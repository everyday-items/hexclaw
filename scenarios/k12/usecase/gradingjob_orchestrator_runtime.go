package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 本文件是 GradingOrchestrator 的单机运行时扩展（架构设计 §6.15）：
//
//   - 阶段中间产物落盘（runDir/<jobID>/image.bin + run.json）：检查点只固化摘要（§5.4），
//     真实产物（原图/识别题目/锚点结论/批改结果）落本地文件——崩溃恢复据此回放，
//     不重复调用模型（§6.7 规则 3 / K12-INV-021）。
//     图片载体设计申报：submission 原图不入 records 的 Fields JSON（base64 过大），
//     以本地文件承载；新 SubmissionID 同时包含图片摘要和 canonical source command
//     摘要，旧 "photo-"+sha1(原图) 仍可加载；两种格式恢复时都校验图片内容。
//   - 异步执行模型：StartAsync 用与请求解耦的进程级 context + 有界并发信号量 + panic recover
//     推进任务（任务 goroutine 不挂 HTTP 请求 context，请求结束不误杀在途批改）。
//   - 崩溃恢复扫描：RecoverGradingJobs 列非终态 Job，从落盘运行时重建 run；
//     自动阶段重新入列续跑，awaiting_confirmation 保持等待，failed_retryable(可重试) 回队。

// GradingOrchestratorOption 编排器组装可选项。
type GradingOrchestratorOption func(*GradingOrchestrator)

// WithGradingRunDir 启用阶段产物落盘目录（空 = 仅内存 run，崩溃后不可恢复）。
func WithGradingRunDir(dir string) GradingOrchestratorOption {
	return func(o *GradingOrchestrator) { o.runDir = dir }
}

// WithGradingBaseContext 异步推进的基座 context（默认 context.Background()；
// 传进程生命周期 ctx 可让退出信号取消在途阶段）。
func WithGradingBaseContext(ctx context.Context) GradingOrchestratorOption {
	return func(o *GradingOrchestrator) { o.baseCtx = ctx }
}

// WithGradingConcurrency 异步推进的有界并发上限（默认 2，与整卷逐题批改并发一致）。
func WithGradingConcurrency(n int) GradingOrchestratorOption {
	return func(o *GradingOrchestrator) {
		if n > 0 {
			o.sem = make(chan struct{}, n)
		}
	}
}

// WithGradingAnchorTimeout 配置锚点增强分支的独立超时。未配置时默认 60 秒；
// 非正数不覆盖默认值，避免误配造成无 deadline 或立即降级。
func WithGradingAnchorTimeout(timeout time.Duration) GradingOrchestratorOption {
	return func(o *GradingOrchestrator) {
		if timeout > 0 {
			o.anchorTimeout = timeout
		}
	}
}

func (o *GradingOrchestrator) gradingBaseContext() context.Context {
	if o.baseCtx != nil {
		return o.baseCtx
	}
	return context.Background()
}

// registerGradingModelCall makes a provider call independently cancellable by
// the public per-Job cancel command. A Job can temporarily own both the main
// grading call and its locating branch, so the registry is one-to-many.
func (o *GradingOrchestrator) registerGradingModelCall(jobID string, cancel context.CancelFunc) func() {
	o.mu.Lock()
	if o.modelCancels == nil {
		o.modelCancels = map[string]map[uint64]context.CancelFunc{}
	}
	o.nextModelCallID++
	callID := o.nextModelCallID
	if o.modelCancels[jobID] == nil {
		o.modelCancels[jobID] = map[uint64]context.CancelFunc{}
	}
	o.modelCancels[jobID][callID] = cancel
	o.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			delete(o.modelCancels[jobID], callID)
			if len(o.modelCancels[jobID]) == 0 {
				delete(o.modelCancels, jobID)
			}
			o.mu.Unlock()
		})
	}
}

func (o *GradingOrchestrator) cancelGradingModelCalls(jobID string) {
	o.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(o.modelCancels[jobID]))
	for _, cancel := range o.modelCancels[jobID] {
		cancels = append(cancels, cancel)
	}
	o.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// beginWorkerLocked registers one process-owned operation. The seal check and
// the 0->1 transition share o.mu with Shutdown, so a worker can never appear
// after Shutdown captured the idle generation it waits on.
func (o *GradingOrchestrator) beginWorkerLocked() bool {
	if o.sealed {
		return false
	}
	if o.workerCount == 0 {
		o.workerIdle = make(chan struct{})
	}
	o.workerCount++
	return true
}

func (o *GradingOrchestrator) finishWorker() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.workerCount <= 0 {
		panic("grading orchestrator worker counter underflow")
	}
	o.workerCount--
	if o.workerCount == 0 {
		close(o.workerIdle)
	}
}

// WaitForIdle waits for the current worker generation. It is observational and
// does not seal the orchestrator; callers closing dependencies must use Shutdown.
// Waiting selects directly on the generation channel and never creates a waiter
// goroutine that could survive a context timeout.
func (o *GradingOrchestrator) WaitForIdle(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	o.mu.Lock()
	done := o.workerIdle
	o.mu.Unlock()
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown seals the orchestrator, cancels all process-owned work, and waits
// until grading, anchor, and recovery operations have returned. It is safe to
// call repeatedly. Once sealed, StartAsync and recovery reject new work.
func (o *GradingOrchestrator) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	o.mu.Lock()
	o.sealed = true
	done := o.workerIdle
	cancel := o.runCancel
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// beginTrackedContext registers synchronous recovery work and merges the
// caller's cancellation with the orchestrator lifecycle without starting a
// waiter goroutine. Values and deadlines continue to come from the caller.
func (o *GradingOrchestrator) beginTrackedContext(parent context.Context) (context.Context, func(), bool) {
	if parent == nil {
		parent = context.Background()
	}
	o.mu.Lock()
	if !o.beginWorkerLocked() {
		o.mu.Unlock()
		return nil, nil, false
	}
	runtimeCtx := o.baseCtx
	o.mu.Unlock()

	ctx, cancel := context.WithCancelCause(parent)
	stopRuntimeCancel := context.AfterFunc(runtimeCtx, func() {
		cancel(context.Cause(runtimeCtx))
	})
	if cause := context.Cause(runtimeCtx); cause != nil {
		cancel(cause)
	}
	return ctx, func() {
		stopRuntimeCancel()
		cancel(context.Canceled)
		o.finishWorker()
	}, true
}

// StartAsync 异步推进一个 Job 到下一停点/终态。与调用方（HTTP 请求）context 解耦；
// 同一 Job 已在推进中时记录一次 rerun 信号，当前轮退出前至少再读一次状态机；这样确认
// 与锚点同时回位时不会因 active 守卫吞掉最后一次续跑。panic 不逃逸（§6.15）。
func (o *GradingOrchestrator) StartAsync(jobID string) bool {
	o.mu.Lock()
	if o.sealed {
		o.mu.Unlock()
		return false
	}
	if o.active == nil {
		o.active = map[string]bool{}
	}
	if o.active[jobID] {
		if o.rerun == nil {
			o.rerun = map[string]bool{}
		}
		o.rerun[jobID] = true
		o.mu.Unlock()
		return true
	}
	o.active[jobID] = true
	if !o.beginWorkerLocked() {
		delete(o.active, jobID)
		o.mu.Unlock()
		return false
	}
	o.mu.Unlock()

	go func() {
		defer o.finishWorker()
		for {
			reconciledForReplay := false
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("K12 批改任务异步推进 panic（已捕获，任务留在当前落库状态）", "job", jobID, "panic", r)
					}
				}()
				select {
				case o.sem <- struct{}{}:
				case <-o.gradingBaseContext().Done():
					return
				}
				defer func() { <-o.sem }()
				view, err := o.RunGradingJob(o.gradingBaseContext(), jobID)
				if err != nil {
					if errors.Is(err, ErrGradingSourceRecognitionPending) {
						// Source worker owns the V73 transition. This is a durable
						// scheduling stop, not a failed model attempt and not a retry.
						slog.Info("K12 批改任务等待 V73 来源识别结果，保持 assessing", "job", jobID)
					} else {
						// 阶段失败已由状态机安全落 failed_retryable/failed_terminal；此处仅取证。
						slog.Warn("K12 批改任务异步推进结束于错误（状态机已落库对应失败态）", "job", jobID, "err", err)
					}
				}
				if view.Record != nil && view.Record.Status == k12.GradingStageOutcomeUnknown {
					run := o.lookup(jobID)
					reconciled, next, reconcileErr := o.reconcileDurableGradingOutcome(
						o.gradingBaseContext(), run, view,
					)
					if reconcileErr != nil {
						slog.Warn("K12 批改任务即时结果对账未收敛，保持结果未知且禁止重发",
							"job", jobID, "stage", view.Fields.FailedStage, "err", reconcileErr)
					} else if reconciled && next.Record != nil &&
						next.Record.Status == k12.GradingStageQueued {
						reconciledForReplay = true
					}
				}
			}()

			o.mu.Lock()
			if reconciledForReplay || o.rerun[jobID] {
				delete(o.rerun, jobID)
				o.mu.Unlock()
				continue
			}
			delete(o.active, jobID)
			o.mu.Unlock()
			return
		}
	}()
	return true
}

// GradingQuestionCorrection 家长对识别结果的逐题确认/修正（§6.7 公共命令③的结构化载荷）。
// 空字段 = 该维度按识别结果确认不改。
type GradingQuestionCorrection struct {
	Index     int    `json:"index"`
	ProblemID string `json:"problem_id,omitempty"`
	// Confirmed 必须由客户端逐题显式提交；高风险 OCR 不接受“整卷默认确认”。
	Confirmed bool `json:"confirmed,omitempty"`
	// Question / StudentAnswer 是旧客户端的 canonical 修正别名；新客户端直接使用
	// CanonicalMarkdown / AnswerCanonicalMarkdown。两组字段都只改 canonical，永不改 raw。
	Question                string      `json:"question,omitempty"`
	CanonicalMarkdown       string      `json:"canonical_markdown,omitempty"`
	StudentAnswer           string      `json:"student_answer,omitempty"`
	AnswerCanonicalMarkdown string      `json:"answer_canonical_markdown,omitempty"`
	AnswerState             AnswerState `json:"answer_state,omitempty"`
	Subject                 string      `json:"subject,omitempty"`
}

// ConfirmPhotoGradingInput 桌面确认输入：整卷学科/年级 + 逐题修正。
type ConfirmPhotoGradingInput struct {
	Subject     string
	Grade       string
	Corrections []GradingQuestionCorrection
}

// ConfirmPhotoGradingJob 结构化确认（桌面入口）：把家长修正应用到已固化的识别产物
// （确认冻结 canonical 输入，§6.7 规则 1/6 语义），落确认检查点后**异步**续跑到终态。
// ok=false 表示该 Job 无在途运行时（非编排器创建），调用方回退纯状态机确认。
func (o *GradingOrchestrator) ConfirmPhotoGradingJob(ctx context.Context, jobID string, in ConfirmPhotoGradingInput) (GradingJobView, bool, error) {
	run, err := o.ensureRun(ctx, jobID)
	if err != nil || run.textOnly {
		return GradingJobView{}, false, nil
	}
	return o.confirmRegisteredGradingJob(ctx, jobID, run, in)
}

// RegisterPersistedTextGradingJob installs the text-specific worker state for
// a trusted webhook Submission. The typed Problem/Attempt snapshot remains the
// source of truth; no image is fabricated or persisted.
func (o *GradingOrchestrator) RegisterPersistedTextGradingJob(
	ctx context.Context,
	agentName, jobID, subject, grade string,
) error {
	run, ok, err := o.newPersistedTextRun(ctx, agentName, jobID, subject, grade)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("usecase: Job %s 不是 webhook 文本 Submission", jobID)
	}
	o.mu.Lock()
	if existing := o.runs[jobID]; existing != nil {
		if !existing.textOnly || existing.agentName != strings.TrimSpace(agentName) {
			o.mu.Unlock()
			return fmt.Errorf("usecase: Job %s 运行时类型或 owner 冲突", jobID)
		}
		run = existing
	} else {
		o.runs[jobID] = run
	}
	o.mu.Unlock()
	if err := o.persistRun(jobID, run); err != nil {
		return fmt.Errorf("usecase: 固化 webhook 文本运行时: %w", err)
	}
	return nil
}

// ConfirmPersistedTextGradingJob applies the same structured confirmation as
// photo grading, then schedules the text-only worker through rendering and
// projection to completed. It can reconstruct legacy typed text Jobs that
// predate the in-memory registration.
func (o *GradingOrchestrator) ConfirmPersistedTextGradingJob(
	ctx context.Context,
	agentName, jobID string,
	in ConfirmPhotoGradingInput,
) (GradingJobView, bool, error) {
	run := o.lookup(jobID)
	if run == nil {
		if restored, err := o.ensureRun(ctx, jobID); err == nil {
			run = restored
		}
	}
	if run == nil {
		candidate, ok, err := o.newPersistedTextRun(ctx, agentName, jobID, in.Subject, in.Grade)
		if err != nil || !ok {
			return GradingJobView{}, ok, err
		}
		o.mu.Lock()
		if existing := o.runs[jobID]; existing != nil {
			run = existing
		} else {
			o.runs[jobID] = candidate
			run = candidate
		}
		o.mu.Unlock()
		if err := o.persistRun(jobID, run); err != nil {
			return GradingJobView{}, true, fmt.Errorf("usecase: 固化 webhook 文本运行时: %w", err)
		}
	}
	if !run.textOnly || run.agentName != strings.TrimSpace(agentName) {
		return GradingJobView{}, false, nil
	}
	return o.confirmRegisteredGradingJob(ctx, jobID, run, in)
}

func (o *GradingOrchestrator) newPersistedTextRun(
	ctx context.Context,
	agentName, jobID, subject, grade string,
) (*gradingRun, bool, error) {
	agentName = strings.TrimSpace(agentName)
	job, err := o.deps.GetGradingJob(ctx, agentName, jobID)
	if err != nil {
		return nil, false, err
	}
	if job.Fields.SourceKind != "webhook" || !strings.HasPrefix(job.Fields.SubmissionID, "webhook-receipt:") {
		return nil, false, nil
	}
	if o.deps.Records == nil {
		return nil, true, fmt.Errorf("usecase: typed Problem/Attempt store 未配置")
	}
	snapshot, err := o.deps.Records.GetProblemAttemptSnapshot(ctx, agentName, job.Fields.SubmissionID)
	if err != nil {
		return nil, true, fmt.Errorf("usecase: 读取 webhook 文本 Problem/Attempt: %w", err)
	}
	questions, err := RecognizedQuestionsFromProblemAttemptSnapshot(snapshot)
	if err != nil {
		return nil, true, err
	}
	if strings.TrimSpace(subject) == "" {
		for _, question := range questions {
			if strings.TrimSpace(question.Subject) != "" {
				subject = question.Subject
				break
			}
		}
	}
	return &gradingRun{
		agentName: agentName,
		textOnly:  true,
		req: PhotoGradeRequest{
			AgentName: agentName, Subject: strings.TrimSpace(subject), Grade: strings.TrimSpace(grade),
			SourceSession: job.Record.SourceSession, Image: k12TextPipelineToken(job.Fields.SubmissionID),
		},
		questions: questions,
	}, true, nil
}

func k12TextPipelineToken(submissionID string) []byte {
	return []byte("hexclaw:k12:text-run:v1\x00" + strings.TrimSpace(submissionID))
}

func (o *GradingOrchestrator) confirmRegisteredGradingJob(
	ctx context.Context,
	jobID string,
	run *gradingRun,
	in ConfirmPhotoGradingInput,
) (GradingJobView, bool, error) {
	l := o.jobLock(jobID)
	l.Lock()
	// 先在副本上完成修正与风险校验。拒绝的命令不得污染内存或 run.json 中的冻结事实。
	candidate := *run
	candidate.req = run.req
	candidate.questions = cloneRecognizedQuestions(run.questions)
	candidate.anchored = cloneRecognizedQuestions(run.anchored)
	if err := applyAndValidateGradingConfirmation(&candidate, in); err != nil {
		l.Unlock()
		return GradingJobView{}, true, err
	}
	job, jerr := o.deps.GetGradingJob(ctx, run.agentName, jobID)
	if jerr != nil {
		l.Unlock()
		return GradingJobView{}, true, jerr
	}
	// Validate the state transition before writing candidate canonical facts.
	// Otherwise a duplicate/late confirm could bump ConfirmedVersion even though
	// the GradingJob command is rejected as a conflict.
	if job.Record.Status != k12.GradingStageAwaitingConfirmation ||
		job.Fields.ConfirmationState != k12.GradingConfirmationPending {
		l.Unlock()
		return GradingJobView{}, true, errGradingStageConflict(
			"阶段 %s 不可确认（仅 awaiting_confirmation/pending）", job.Record.Status,
		)
	}
	confirmedFacts := candidate.questions
	if candidate.anchored != nil {
		confirmedFacts = candidate.anchored
	}
	if perr := o.persistProblemAttemptFacts(ctx, run.agentName, job.Fields.SubmissionID, confirmedFacts); perr != nil {
		l.Unlock()
		return GradingJobView{}, true, fmt.Errorf("usecase: 固化确认后的 Problem/Attempt: %w", perr)
	}
	if perr := o.persistRun(jobID, &candidate); perr != nil {
		l.Unlock()
		return GradingJobView{}, true, fmt.Errorf("usecase: 固化确认后的识别产物: %w", perr)
	}
	canonicalDigest := CanonicalRecognizedQuestionsDigest(candidate.questions)
	v, err := o.deps.ConfirmGradingJob(ctx, run.agentName, jobID, []string{"canonical-recognition:" + canonicalDigest})
	if err == nil {
		run.req = candidate.req
		run.questions = candidate.questions
		run.anchored = candidate.anchored
	}
	l.Unlock()
	if err != nil {
		return GradingJobView{}, true, err
	}
	o.StartAsync(jobID)
	return v, true, nil
}

// PhotoGradingJobRuntimeRetrier is the process-local half of the shared
// retry command. A false handled result means the durable Job state machine
// must be used instead (for example after a sidecar restart).
type PhotoGradingJobRuntimeRetrier interface {
	RetryPhotoGradingJob(context.Context, string) (GradingJobView, bool, error)
}

// RetryPhotoGradingJobWithDurableFallback keeps every public parent-facing
// retry on one contract: reuse a live runtime when available, otherwise retry
// the same durable Job from its checkpoint. Callers must already have applied
// their state-specific authorization; this helper never creates a Job or
// retries an outcome-unknown invocation.
func RetryPhotoGradingJobWithDurableFallback(
	ctx context.Context,
	deps Deps,
	runtime PhotoGradingJobRuntimeRetrier,
	agentName, jobID string,
) (GradingJobView, error) {
	if runtime != nil {
		if view, handled, err := runtime.RetryPhotoGradingJob(ctx, jobID); handled {
			return view, err
		}
	}
	return deps.RetryGradingJob(ctx, agentName, jobID)
}

// RetryPhotoGradingJob 安全重试（桌面入口）：回 queued 后**异步**从检查点续跑。
// ok=false 表示无在途运行时，调用方回退纯状态机重试。
func (o *GradingOrchestrator) RetryPhotoGradingJob(ctx context.Context, jobID string) (GradingJobView, bool, error) {
	run, err := o.ensureRun(ctx, jobID)
	if err != nil {
		return GradingJobView{}, false, nil
	}
	l := o.jobLock(jobID)
	l.Lock()
	v, err := o.deps.RetryGradingJob(ctx, run.agentName, jobID)
	l.Unlock()
	if err != nil {
		return GradingJobView{}, true, err
	}
	o.StartAsync(jobID)
	return v, true, nil
}

func (o *GradingOrchestrator) CanRetryPhotoGradingWithParentAutomaticWindow(
	ctx context.Context,
	jobID string,
) (bool, error) {
	run, err := o.ensureRun(ctx, jobID)
	if err != nil {
		return false, err
	}
	v, err := o.deps.GetGradingJob(ctx, run.agentName, jobID)
	if err != nil {
		return false, err
	}
	return gradingInteractiveDeadlineRetryEligible(v), nil
}

func (o *GradingOrchestrator) RetryPhotoGradingJobWithParentAutomaticWindow(
	ctx context.Context,
	jobID, parentAutomaticAttemptID string,
	parentAutomaticDeadlineAt int64,
) (GradingJobView, bool, error) {
	run, err := o.ensureRun(ctx, jobID)
	if err != nil {
		return GradingJobView{}, false, nil
	}
	l := o.jobLock(jobID)
	l.Lock()
	v, err := o.deps.RetryGradingJobWithParentAutomaticWindow(
		ctx, run.agentName, jobID,
		parentAutomaticAttemptID, parentAutomaticDeadlineAt,
	)
	l.Unlock()
	if err != nil {
		return GradingJobView{}, true, err
	}
	o.StartAsync(jobID)
	return v, true, nil
}

// CancelPhotoGradingJob persists the public cancellation first, then interrupts
// every provider call currently owned by the Job. It deliberately does not take
// jobLock: cancellation must remain responsive while a provider call is in flight.
// ok=false lets callers retain the state-machine-only path for Jobs without a
// registered runtime.
func (o *GradingOrchestrator) CancelPhotoGradingJob(ctx context.Context, agentName, jobID string) (GradingJobView, bool, error) {
	run := o.lookup(jobID)
	if run == nil {
		return GradingJobView{}, false, nil
	}
	v, err := o.deps.CancelGradingJob(ctx, agentName, jobID)
	if err != nil {
		return GradingJobView{}, true, err
	}
	o.cancelGradingModelCalls(jobID)
	return v, true, nil
}

// RecognizedQuestions 取识别停点产物（锚点已回位时含 BBox），供确认界面回显。
func (o *GradingOrchestrator) RecognizedQuestions(ctx context.Context, jobID string) ([]RecognizedQuestion, bool) {
	return o.recognizedQuestions(ctx, "", jobID)
}

// RecognizedQuestionsForOwner is the HTTP/public projection boundary. Text
// webhook submissions have durable typed facts but deliberately no photo
// run.json; the explicit owner lets this lookup recover those facts without a
// cross-Tutor record scan.
func (o *GradingOrchestrator) RecognizedQuestionsForOwner(ctx context.Context, agentName, jobID string) ([]RecognizedQuestion, bool) {
	return o.recognizedQuestions(ctx, strings.TrimSpace(agentName), jobID)
}

func (o *GradingOrchestrator) recognizedQuestions(ctx context.Context, agentName, jobID string) ([]RecognizedQuestion, bool) {
	run, err := o.ensureRun(ctx, jobID)
	if err != nil {
		if agentName != "" {
			if typed, ok := o.typedRecognizedQuestions(ctx, jobID, agentName); ok {
				return typed, true
			}
		}
		archived, ok := o.archivedRecognizedQuestions(jobID)
		if !ok || len(archived) == 0 {
			return nil, false
		}
		if agentName != "" {
			audit, auditOK := o.readRecognitionAudit(jobID)
			if !auditOK || audit.AgentName != agentName {
				return nil, false
			}
		}
		if typed, typedOK := o.typedRecognizedQuestions(ctx, jobID, ""); typedOK {
			return typed, true
		}
		return archived, true
	}
	if agentName != "" && run.agentName != agentName {
		return nil, false
	}
	// Recognition facts are committed before the durable Job advances to the
	// confirmation stop. Prefer that persisted projection without taking the
	// process-local execution lock: a poll can observe awaiting_confirmation in
	// the small window before RunGradingJob releases the lock, and the response
	// must still contain the facts needed by the confirmation command.
	if typed, ok := o.typedRecognizedQuestions(ctx, jobID, run.agentName); ok {
		return typed, true
	}
	l := o.jobLock(jobID)
	if !l.TryLock() {
		// The durable Job row remains independently readable while an automatic
		// stage owns the execution lock. A poll must not wait for provider latency;
		// the recognition projection becomes available at the confirmation stop.
		return nil, false
	}
	defer l.Unlock()
	if len(run.questions) == 0 {
		return nil, false
	}
	if typed, ok := o.typedRecognizedQuestions(ctx, jobID, run.agentName); ok {
		return typed, true
	}
	qs := run.questions
	if run.anchored != nil {
		qs = run.anchored
	}
	return cloneRecognizedQuestions(qs), true
}

func (o *GradingOrchestrator) typedRecognizedQuestions(ctx context.Context, jobID, agentName string) ([]RecognizedQuestion, bool) {
	if o == nil || o.deps.Records == nil {
		return nil, false
	}
	// Terminal release may remove run.json. The append-only audit keeps the owner
	// needed to scope the typed lookup without ever querying across Tutor instances.
	if agentName == "" {
		audit, ok := o.readRecognitionAudit(jobID)
		if !ok {
			return nil, false
		}
		agentName = audit.AgentName
	}
	job, err := o.deps.GetGradingJob(ctx, agentName, jobID)
	if err != nil {
		return nil, false
	}
	snapshot, err := o.deps.Records.GetProblemAttemptSnapshot(ctx, agentName, job.Fields.SubmissionID)
	if err != nil {
		return nil, false
	}
	questions, err := RecognizedQuestionsFromProblemAttemptSnapshot(snapshot)
	return questions, err == nil
}

// RecoverGradingJobs 启动扫描（§6.15 崩溃恢复）：逐实例列非终态 GradingJob，从落盘运行时
// 重建 run 并按阶段处置——awaiting_confirmation（锚点已回位）保持等待；failed_retryable
// 且可重试回 queued 重新入列；其余自动阶段直接异步续跑（RunGradingJob 从当前 stage 起，
// 已固化检查点的阶段回放产物，不重复调模型）。恢复不产生重复 Submission（同 Job 续跑）。
const gradingFailureInteractiveDeadlineExceeded = "interactive_deadline_exceeded"

func (o *GradingOrchestrator) RecoverGradingJobs(ctx context.Context, agents []string) (int, error) {
	trackedCtx, finish, ok := o.beginTrackedContext(ctx)
	if !ok {
		return 0, ErrGradingOrchestratorShutdown
	}
	defer finish()
	ctx = trackedCtx
	if o.runDir == "" {
		return 0, nil
	}
	recovered := 0
	for _, agent := range agents {
		views, err := o.deps.ListGradingJobs(ctx, agent, "")
		if err != nil {
			return recovered, fmt.Errorf("usecase: 崩溃恢复扫描列任务(%s): %w", agent, err)
		}
		for _, v := range views {
			if k12.GradingStageTerminal(v.Record.Status) {
				continue
			}
			jobID := v.Record.RecordID
			run, err := o.ensureRun(ctx, jobID)
			if err != nil {
				slog.Warn("K12 批改任务崩溃恢复: 运行时产物缺失/校验失败，任务留在当前状态等人工处置",
					"job", jobID, "stage", v.Record.Status, "err", err)
				continue
			}
			recovered++
			switch v.Record.Status {
			case k12.GradingStageAwaitingConfirmation:
				// 真实风险继续等待家长；ImageTask 清晰事实还会补跑自动冻结。锚点未回位
				// 时同一次续跑也会恢复独立锚点分支。
				if v.Fields.AnchorState == k12.GradingAnchorPending ||
					(automaticPhotoConfirmationSource(v.Fields.SourceKind) &&
						v.Fields.ConfirmationState == k12.GradingConfirmationPending) {
					o.StartAsync(jobID)
				}
			case k12.GradingStageOutcomeUnknown:
				reconciled, next, reconcileErr := o.reconcileDurableGradingOutcome(ctx, run, v)
				if reconcileErr != nil {
					slog.Warn("K12 批改任务崩溃恢复: 持久结果自动对账失败，保持结果未知且禁止重发",
						"job", jobID, "stage", v.Fields.FailedStage, "err", reconcileErr)
					continue
				}
				if !reconciled {
					// 没有结论性持久证据时绝不把查询/重启变成第二次模型调用。
					continue
				}
				if next.Record.Status == k12.GradingStageQueued {
					o.StartAsync(jobID)
				}
			case k12.GradingStageFailedRetryable:
				if !v.Fields.Retryable ||
					v.Fields.FailureKind == gradingFailureInteractiveDeadlineExceeded {
					continue // 不可重试残留（正常应已收敛 failed_terminal）：留人工处置
				}
				l := o.jobLock(jobID)
				l.Lock()
				_, rerr := o.deps.RetryGradingJob(ctx, run.agentName, jobID)
				l.Unlock()
				if rerr != nil {
					slog.Warn("K12 批改任务崩溃恢复: 重新入列失败", "job", jobID, "err", rerr)
					continue
				}
				o.StartAsync(jobID)
			default:
				o.StartAsync(jobID)
			}
		}
	}
	return recovered, nil
}

// reconcileDurableGradingOutcome only handles ambiguity that can be settled
// from immutable local evidence. Provider transport ambiguity without such
// evidence deliberately remains parked; recovery must never turn a read or
// restart into a duplicate paid request.
func (o *GradingOrchestrator) reconcileDurableGradingOutcome(
	ctx context.Context,
	run *gradingRun,
	job GradingJobView,
) (bool, GradingJobView, error) {
	if o == nil || run == nil || o.deps.Records == nil ||
		job.Record.Status != k12.GradingStageOutcomeUnknown {
		return false, GradingJobView{}, nil
	}
	invocations, err := o.deps.Records.ListModelInvocations(ctx, run.agentName, job.Record.RecordID)
	if err != nil {
		return false, GradingJobView{}, err
	}
	var invocation *k12.ModelInvocation
	for i := range invocations {
		candidate := &invocations[i]
		if candidate.Stage != job.Fields.FailedStage {
			continue
		}
		if invocation == nil || candidate.Attempt > invocation.Attempt {
			invocation = candidate
		}
	}
	if invocation == nil {
		return false, GradingJobView{}, nil
	}
	switch invocation.Status {
	case k12.ModelInvocationSent, k12.ModelInvocationSucceeded,
		k12.ModelInvocationOutcomeUnknown, k12.ModelInvocationReconciled:
	default:
		return false, GradingJobView{}, nil
	}
	wantRoute := k12.NormalizeGradingModelSnapshot(job.Fields.ModelSnapshot)
	gotRoute := k12.NormalizeGradingModelSnapshot(invocation.RouteSnapshot)
	if gotRoute.Provider != wantRoute.Provider || gotRoute.Model != wantRoute.Model ||
		gotRoute.Route != wantRoute.Route {
		return false, GradingJobView{}, fmt.Errorf("model invocation route drift")
	}

	switch job.Fields.FailedStage {
	case k12.GradingStageRecognizing:
		wantPolicy := k12.NormalizeModelRequestPolicySnapshot(
			job.Fields.ModelSnapshot.RecognizingRequestPolicy,
		)
		if err := k12.ValidateModelInvocationRequestPolicy(
			k12.GradingStageRecognizing,
			job.Fields.ModelSnapshot,
			invocation.RequestPolicySnapshot,
		); err != nil {
			return false, GradingJobView{}, fmt.Errorf(
				"recognition invocation request policy drift: %w",
				err,
			)
		}
		if invocation.RequestPolicySnapshot != wantPolicy {
			return false, GradingJobView{}, fmt.Errorf(
				"recognition invocation request policy drift",
			)
		}
		wantRequestDigest := recognizingInvocationDigest(
			run.req.Image,
			job.Fields.ModelSnapshot,
			wantPolicy,
		)
		if invocation.RequestDigest != wantRequestDigest {
			return false, GradingJobView{}, fmt.Errorf("recognition invocation request digest drift")
		}
		physicalChildren, physicalErr := o.recognitionPhysicalSuccessSet(
			ctx,
			*invocation,
			run.req.Image,
		)
		if physicalErr != nil {
			// A local recognition result file cannot prove that the ambiguous
			// provider operation completed. Reconciliation is eligible only
			// when the exact approved 1-or-7 physical child set is durable and
			// every child is terminal succeeded; otherwise keep the same Job
			// parked without resending.
			return false, GradingJobView{}, nil
		}
		// A source-scoped Problem/Attempt snapshot can still contain facts from an
		// earlier invocation of this Job. Only this Job's append-only recognition
		// receipt proves which result belongs to the ambiguous invocation.
		receipt, ok := o.readRecognitionReceipt(job.Record.RecordID)
		if !ok || receipt.AgentName != run.agentName ||
			receipt.InvocationID != invocation.InvocationID ||
			!sameRecognitionPhysicalReceiptSet(
				receipt.PhysicalInvocations,
				recognitionPhysicalReceiptSet(physicalChildren),
			) {
			return false, GradingJobView{}, nil
		}
		questions := cloneRecognizedQuestions(receipt.Questions)
		if len(questions) == 0 {
			return false, GradingJobView{}, nil
		}
		candidate := *run
		candidate.questions = questions
		if persistErr := o.persistRecognizedPhotoFacts(
			ctx, &candidate, job.Fields.SubmissionID,
		); persistErr != nil {
			return false, GradingJobView{}, persistErr
		}
		if persistErr := o.persistRun(job.Record.RecordID, &candidate); persistErr != nil {
			return false, GradingJobView{}, persistErr
		}
		resultDigest := modelInvocationResultDigest(candidate.questions)
		reconciled, reconcileErr := o.deps.ReconcileGradingInvocationSucceeded(
			ctx, run.agentName, job.Record.RecordID, invocation.InvocationID,
			resultDigest, fmt.Sprintf("questions:%d", len(candidate.questions)), "",
		)
		if reconcileErr != nil {
			return false, GradingJobView{}, reconcileErr
		}
		run.questions = candidate.questions
		if reconciled.Record.Status != k12.GradingStageFailedRetryable {
			return true, reconciled, nil
		}
		queued, retryErr := o.deps.RetryGradingJob(
			ctx, run.agentName, job.Record.RecordID,
		)
		return true, queued, retryErr
	case k12.GradingStageAssessing:
		requestRaw, marshalErr := json.Marshal(struct {
			Request   PhotoGradeRequest    `json:"request"`
			Questions []RecognizedQuestion `json:"questions"`
			Anchored  []RecognizedQuestion `json:"anchored,omitempty"`
		}{run.req, run.questions, run.anchored})
		if marshalErr != nil {
			return false, GradingJobView{}, marshalErr
		}
		wantRequestDigest := modelInvocationDigest(
			[]byte(k12.GradingStageAssessing), requestRaw,
		)
		if invocation.RequestDigest != wantRequestDigest {
			return false, GradingJobView{}, fmt.Errorf("assessment invocation request digest drift")
		}
		result, durable, resultErr := o.durableAssessmentResult(ctx, run, job)
		if resultErr != nil {
			return false, GradingJobView{}, resultErr
		}
		if !durable {
			return false, GradingJobView{}, nil
		}
		candidate := *run
		candidate.result = result
		if persistErr := o.persistRun(job.Record.RecordID, &candidate); persistErr != nil {
			return false, GradingJobView{}, persistErr
		}
		resultDigest := modelInvocationResultDigest(*result)
		reconciled, reconcileErr := o.deps.ReconcileGradingInvocationSucceeded(
			ctx, run.agentName, job.Record.RecordID, invocation.InvocationID,
			resultDigest, fmt.Sprintf("items:%d mode:%s", len(result.Items), result.Mode), "",
		)
		if reconcileErr != nil {
			return false, GradingJobView{}, reconcileErr
		}
		run.result = result
		if reconciled.Record.Status != k12.GradingStageFailedRetryable {
			return true, reconciled, nil
		}
		queued, retryErr := o.deps.RetryGradingJob(
			ctx, run.agentName, job.Record.RecordID,
		)
		return true, queued, retryErr
	default:
		return false, GradingJobView{}, nil
	}
}

// durableAssessmentResult accepts only a complete run artifact or an exact
// set of immutable per-item receipts. The latter is replayed through a
// receipt-only assessor, so rebuilding the page result cannot invoke a model.
func (o *GradingOrchestrator) durableAssessmentResult(
	ctx context.Context,
	run *gradingRun,
	job GradingJobView,
) (*PhotoGradeResult, bool, error) {
	if run.result != nil {
		receipts, err := o.deps.Records.ListGradingAssessmentItems(
			ctx, run.agentName, job.Record.RecordID,
		)
		// Process-local run.result is not a durable assessment boundary.
		// Recovery may advance only from the exact canonical receipt set;
		// otherwise outcome_unknown remains parked and is never resent.
		if err != nil || validateGradingAssessmentExactSet(*run.result, receipts) != nil {
			return nil, false, nil
		}
		result := *run.result
		return &result, true, nil
	}
	if !job.Fields.BudgetSnapshot.IsFrozen() {
		return nil, false, nil
	}
	receipts, err := o.deps.Records.ListGradingAssessmentItems(
		ctx, run.agentName, job.Record.RecordID,
	)
	if err != nil || validateFrozenAssessReceiptSet(run, receipts) != nil {
		return nil, false, nil
	}
	byProblem := make(map[string]k12.GradingAssessmentItem, len(receipts))
	for _, receipt := range receipts {
		byProblem[receipt.ProblemID] = receipt
	}
	replayDeps := o.deps
	replayDeps.Recognizer = presetRecognizer{questions: run.questions}
	switch {
	case run.anchored != nil:
		replayDeps.AnswerAnchorer = presetAnchorer{questions: run.anchored}
	case run.anchorFailed:
		replayDeps.AnswerAnchorer = presetAnchorer{err: fmt.Errorf("anchor checkpoint degraded")}
	default:
		replayDeps.AnswerAnchorer = nil
	}
	if run.textOnly {
		replayDeps.PhotoAnnotator = nil
	}
	result, err := replayDeps.gradeFrozenHomeworkPhotoWithAssessor(
		ctx,
		run.req,
		job.Fields.BudgetSnapshot.ItemConcurrency,
		func(_ context.Context, _ PhotoGradeRequest, _ PhotoMode, question RecognizedQuestion) (PhotoGradeItem, error) {
			receipt, ok := byProblem[question.ProblemID]
			if !ok {
				return PhotoGradeItem{}, fmt.Errorf(
					"%w: missing receipt problem=%s",
					ErrGradingAssessmentExactSet, question.ProblemID,
				)
			}
			return replayGradingAssessmentItem(question, receipt)
		},
	)
	if err != nil {
		return nil, false, err
	}
	return &result, true, nil
}

// --- 家长修正应用 ---

// applyGradingCorrections 把逐题修正同时应用到核心识别产物与锚点增强产物（assessing 消费
// 的是两者回放，必须同步改）；整卷 subject/grade 覆盖批改请求。
func applyGradingCorrections(run *gradingRun, in ConfirmPhotoGradingInput) (map[string]bool, error) {
	if strings.TrimSpace(in.Subject) != "" {
		run.req.Subject = in.Subject
	}
	if strings.TrimSpace(in.Grade) != "" {
		run.req.Grade = in.Grade
	}
	confirmed := make(map[string]bool, len(in.Corrections))
	seenTargets := make(map[string]struct{}, len(in.Corrections))
	for _, c := range in.Corrections {
		index := gradingCorrectionIndex(run.questions, c)
		if index < 0 || index >= len(run.questions) {
			return nil, fmt.Errorf("%w: question_correction target 不存在（problem_id=%q index=%d）", ErrInvalidInput, c.ProblemID, c.Index)
		}
		targetID := run.questions[index].ProblemID
		if _, duplicate := seenTargets[targetID]; duplicate {
			return nil, fmt.Errorf("%w: problem %s 存在重复 question_correction", ErrInvalidInput, targetID)
		}
		seenTargets[targetID] = struct{}{}
		if c.AnswerState != "" && c.AnswerState != AnswerStateBlank && c.AnswerState != AnswerStatePresent && c.AnswerState != AnswerStateUnclear {
			return nil, fmt.Errorf("%w: problem %s answer_state 非法: %q", ErrInvalidInput, targetID, c.AnswerState)
		}
		for _, list := range [][]RecognizedQuestion{run.questions, run.anchored} {
			index := gradingCorrectionIndex(list, GradingQuestionCorrection{ProblemID: targetID, Index: index})
			if index < 0 || index >= len(list) {
				continue
			}
			q := &list[index]
			canonicalChanged := false
			canonicalQuestion := firstNonEmpty(c.CanonicalMarkdown, c.Question)
			if strings.TrimSpace(canonicalQuestion) != "" && canonicalQuestion != q.CanonicalMarkdown {
				q.CanonicalMarkdown = canonicalQuestion
				canonicalChanged = true
			}
			canonicalAnswer := firstNonEmpty(c.AnswerCanonicalMarkdown, c.StudentAnswer)
			if canonicalAnswer != "" && canonicalAnswer != q.AnswerCanonicalMarkdown {
				q.AnswerCanonicalMarkdown = canonicalAnswer
				canonicalChanged = true
				q.AnswerState = AnswerStatePresent
			}
			if c.AnswerState != "" {
				q.AnswerState = c.AnswerState
			}
			if strings.TrimSpace(c.Subject) != "" && q.Subject != strings.TrimSpace(c.Subject) {
				q.Subject = c.Subject
				canonicalChanged = true
			}
			if canonicalChanged {
				q.CanonicalVersion++
			}
			*q = NormalizeRecognizedQuestion(*q)
			if c.Confirmed {
				confirmed[q.ProblemID] = true
			}
		}
	}
	return confirmed, nil
}

func gradingCorrectionIndex(questions []RecognizedQuestion, correction GradingQuestionCorrection) int {
	if id := strings.TrimSpace(correction.ProblemID); id != "" {
		for i := range questions {
			if questions[i].ProblemID == id {
				return i
			}
		}
		return -1
	}
	return correction.Index
}

func applyAndValidateGradingConfirmation(run *gradingRun, in ConfirmPhotoGradingInput) error {
	confirmed, err := applyGradingCorrections(run, in)
	if err != nil {
		return err
	}
	for i := range run.questions {
		q := NormalizeRecognizedQuestion(run.questions[i])
		if !CanonicalMarkdownValid(q.CanonicalMarkdown) ||
			(q.AnswerState == AnswerStatePresent && !CanonicalMarkdownValid(q.AnswerCanonicalMarkdown)) {
			return fmt.Errorf("%w: problem %s canonical Markdown/LaTeX 无法解析，请先逐题修正", ErrInvalidInput, q.ProblemID)
		}
		if q.ConfirmationRequired && !confirmed[q.ProblemID] {
			return fmt.Errorf("%w: problem %s 需逐题确认（%s）", ErrInvalidInput, q.ProblemID, joinOCRRiskReasons(q.ConfirmationReasons))
		}
		q.ConfirmedVersion++
		run.questions[i] = q
	}
	run.questions = FreezeRecognizedQuestionInputDigests(run.questions, run.req.Grade)
	// anchored 是 geometry 投影，确认版本必须与 canonical 同步，且禁止反向覆盖事实。
	if run.anchored != nil {
		run.anchored = mergeAnchorGeometry(run.questions, run.anchored)
		for i := range run.anchored {
			run.anchored[i].ConfirmedVersion = run.questions[i].ConfirmedVersion
		}
	}
	return nil
}

// applyProgressiveGradingConfirmation freezes every syntactically valid item
// independently. An item that still lacks trustworthy source evidence remains
// gated, while its clear siblings may continue without confirming the page.
func applyProgressiveGradingConfirmation(run *gradingRun, in ConfirmPhotoGradingInput) (bool, error) {
	confirmed, err := applyGradingCorrections(run, in)
	if err != nil {
		return false, err
	}
	awaitingSource := false
	for i := range run.questions {
		q := NormalizeRecognizedQuestion(run.questions[i])
		if !CanonicalMarkdownValid(q.CanonicalMarkdown) ||
			(q.AnswerState == AnswerStatePresent && !CanonicalMarkdownValid(q.AnswerCanonicalMarkdown)) {
			return false, fmt.Errorf(
				"%w: problem %s canonical Markdown/LaTeX 无法解析，请先逐题修正",
				ErrInvalidInput, q.ProblemID,
			)
		}
		if q.ConfirmationRequired && !confirmed[q.ProblemID] {
			awaitingSource = true
		}
		q.ConfirmedVersion++
		run.questions[i] = q
	}
	run.questions = FreezeRecognizedQuestionInputDigests(run.questions, run.req.Grade)
	if run.anchored != nil {
		run.anchored = mergeAnchorGeometry(run.questions, run.anchored)
		for i := range run.anchored {
			run.anchored[i].ConfirmedVersion = run.questions[i].ConfirmedVersion
		}
	}
	return awaitingSource, nil
}

func joinOCRRiskReasons(reasons []OCRRiskReason) string {
	parts := make([]string, len(reasons))
	for i := range reasons {
		parts[i] = string(reasons[i])
	}
	return strings.Join(parts, ",")
}

// --- 运行时落盘 ---

// gradingRunFile run.json 结构（原图独立存 image.bin，避免每次改写都重写大字节）。
type gradingRunFile struct {
	AgentName     string               `json:"agent_name"`
	TextOnly      bool                 `json:"text_only,omitempty"`
	Subject       string               `json:"subject,omitempty"`
	Grade         string               `json:"grade,omitempty"`
	SourceSession string               `json:"source_session,omitempty"`
	Questions     []RecognizedQuestion `json:"questions,omitempty"`
	Anchored      []RecognizedQuestion `json:"anchored,omitempty"`
	AnchorFailed  bool                 `json:"anchor_failed,omitempty"`
	RenderFailure string               `json:"render_failure,omitempty"`
	Result        *PhotoGradeResult    `json:"result,omitempty"`
}

type gradingRecognitionAuditFile struct {
	JobID           string               `json:"job_id"`
	AgentName       string               `json:"agent_name"`
	CanonicalDigest string               `json:"canonical_digest"`
	Questions       []RecognizedQuestion `json:"questions"`
	ArchivedAt      int64                `json:"archived_at"`
}

type gradingRecognitionReceiptFile struct {
	JobID               string                              `json:"job_id"`
	InvocationID        string                              `json:"invocation_id"`
	AgentName           string                              `json:"agent_name"`
	CanonicalDigest     string                              `json:"canonical_digest"`
	Questions           []RecognizedQuestion                `json:"questions"`
	PhysicalInvocations []gradingRecognitionPhysicalReceipt `json:"physical_invocations,omitempty"`
	CreatedAt           int64                               `json:"created_at"`
}

type gradingRecognitionPhysicalReceipt struct {
	PhysicalInvocationID string                      `json:"physical_invocation_id"`
	PhysicalUnit         k12.RecognitionPhysicalUnit `json:"physical_unit"`
	RequestDigest        string                      `json:"request_digest"`
	ResultDigest         string                      `json:"result_digest"`
}

func (o *GradingOrchestrator) runPath(jobID string, file string) string {
	return filepath.Join(o.runDir, jobID, file)
}

func (o *GradingOrchestrator) persistProblemAttemptFacts(ctx context.Context, agentName, submissionID string, questions []RecognizedQuestion) error {
	snapshot, err := RecognizedQuestionsProblemAttemptSnapshot(agentName, submissionID, questions, o.deps.now())
	if err != nil {
		return err
	}
	return o.deps.Records.PutProblemAttemptSnapshot(ctx, snapshot)
}

// persistRun 落盘运行时状态（runDir 未启用时为 no-op）。原图只写一次（内容不变）。
func (o *GradingOrchestrator) persistRun(jobID string, run *gradingRun) error {
	if o.runDir == "" {
		return nil
	}
	dir := filepath.Join(o.runDir, jobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("建运行时目录: %w", err)
	}
	if !run.textOnly {
		imgPath := o.runPath(jobID, "image.bin")
		if _, err := os.Stat(imgPath); err != nil {
			if werr := atomicWriteFile(imgPath, run.req.Image); werr != nil {
				return fmt.Errorf("固化原图: %w", werr)
			}
		}
	}
	meta := gradingRunFile{
		AgentName: run.agentName, TextOnly: run.textOnly, Subject: run.req.Subject, Grade: run.req.Grade,
		SourceSession: run.req.SourceSession,
		Questions:     run.questions, Anchored: run.anchored, AnchorFailed: run.anchorFailed,
		RenderFailure: run.renderFailure, Result: run.result,
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal 运行时状态: %w", err)
	}
	if err := atomicWriteFile(o.runPath(jobID, "run.json"), raw); err != nil {
		return fmt.Errorf("固化运行时状态: %w", err)
	}
	return nil
}

// ensureRun 取（或从落盘恢复）Job 的运行时状态。
func (o *GradingOrchestrator) ensureRun(ctx context.Context, jobID string) (*gradingRun, error) {
	if run := o.lookup(jobID); run != nil {
		return run, nil
	}
	if o.runDir == "" {
		return nil, fmt.Errorf("usecase: 批改任务 %s 无在途运行时状态（未启用落盘恢复）", jobID)
	}
	raw, err := os.ReadFile(o.runPath(jobID, "run.json"))
	if err != nil {
		return nil, fmt.Errorf("usecase: 批改任务 %s 运行时状态不可读: %w", jobID, err)
	}
	var meta gradingRunFile
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("usecase: 批改任务 %s 运行时状态损坏: %w", jobID, err)
	}
	v, err := o.deps.GetGradingJob(ctx, meta.AgentName, jobID)
	if err != nil {
		return nil, err
	}
	var image []byte
	questions := meta.Questions
	if meta.TextOnly {
		if v.Fields.SourceKind != "webhook" || !strings.HasPrefix(v.Fields.SubmissionID, "webhook-receipt:") || o.deps.Records == nil {
			return nil, fmt.Errorf("usecase: 批改任务 %s text-only 身份无效", jobID)
		}
		typed, err := o.deps.Records.GetProblemAttemptSnapshot(ctx, meta.AgentName, v.Fields.SubmissionID)
		if err != nil {
			return nil, fmt.Errorf("usecase: 批改任务 %s typed 文本事实不可读: %w", jobID, err)
		}
		questions, err = RecognizedQuestionsFromProblemAttemptSnapshot(typed)
		if err != nil {
			return nil, fmt.Errorf("usecase: 批改任务 %s typed 文本事实损坏: %w", jobID, err)
		}
		image = k12TextPipelineToken(v.Fields.SubmissionID)
	} else {
		image, err = os.ReadFile(o.runPath(jobID, "image.bin"))
		if err != nil {
			return nil, fmt.Errorf("usecase: 批改任务 %s 原图不可读: %w", jobID, err)
		}
		// 内容校验：兼容 legacy photo-sha1 与 source-scoped photo-v2；两者都把
		// 原图摘要冻结进 SubmissionID，防落盘文件被替换或半写。
		if !photoSubmissionMatchesImage(v.Fields.SubmissionID, image) {
			return nil, fmt.Errorf(
				"usecase: 批改任务 %s 原图校验失败（submission=%s 实测摘要=%s）",
				jobID,
				v.Fields.SubmissionID,
				photoImageDigest(image),
			)
		}
	}
	run := &gradingRun{
		agentName: meta.AgentName, textOnly: meta.TextOnly,
		req: PhotoGradeRequest{
			AgentName: meta.AgentName, Subject: meta.Subject, Grade: meta.Grade,
			SourceSession: meta.SourceSession, Image: image,
		},
		questions: questions, anchored: meta.Anchored, anchorFailed: meta.AnchorFailed,
		renderFailure: meta.RenderFailure, result: meta.Result,
	}
	o.mu.Lock()
	if existing, ok := o.runs[jobID]; ok {
		run = existing // 并发恢复竞态：先注册者赢
	} else {
		o.runs[jobID] = run
	}
	o.mu.Unlock()
	return run, nil
}

func (o *GradingOrchestrator) recognitionAuditPath(jobID string) string {
	return filepath.Join(o.runDir, "recognition-audit", jobID+".json")
}

func (o *GradingOrchestrator) recognitionReceiptPath(jobID string) string {
	return filepath.Join(o.runDir, "recognition-results", jobID+".json")
}

// persistRecognitionReceipt writes the immutable provider result before any
// shared Submission projection. The Job+Invocation binding is the only local
// evidence allowed to reconcile an ambiguous recognizing call.
func (o *GradingOrchestrator) persistRecognitionReceipt(jobID, invocationID string, run *gradingRun) error {
	if o.runDir == "" || run == nil || strings.TrimSpace(jobID) == "" ||
		strings.TrimSpace(invocationID) == "" || len(run.questions) == 0 {
		return nil
	}
	var physicalReceipts []gradingRecognitionPhysicalReceipt
	if o.deps.Records != nil {
		parent, err := o.deps.Records.GetModelInvocation(
			context.Background(),
			run.agentName,
			invocationID,
		)
		if err != nil {
			return fmt.Errorf("读取识别调用以绑定物理回执: %w", err)
		}
		if !parent.RequestPolicySnapshot.IsZero() {
			children, err := o.recognitionPhysicalSuccessSet(
				context.Background(),
				parent,
				run.req.Image,
			)
			if err != nil {
				return fmt.Errorf("识别结果回执缺少完整物理调用证据: %w", err)
			}
			physicalReceipts = recognitionPhysicalReceiptSet(children)
		}
	}
	path := o.recognitionReceiptPath(jobID)
	if _, err := os.Stat(path); err == nil {
		existing, ok := o.readRecognitionReceipt(jobID)
		wantDigest := CanonicalRecognizedQuestionsDigest(run.questions)
		if !ok || existing.InvocationID != invocationID ||
			existing.AgentName != run.agentName ||
			existing.CanonicalDigest != wantDigest ||
			!sameRecognitionPhysicalReceiptSet(
				existing.PhysicalInvocations,
				physicalReceipts,
			) {
			return fmt.Errorf("识别结果回执与当前 Job 调用事实冲突")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查识别结果回执: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建识别结果回执目录: %w", err)
	}
	receipt := gradingRecognitionReceiptFile{
		JobID: jobID, InvocationID: invocationID, AgentName: run.agentName,
		CanonicalDigest:     CanonicalRecognizedQuestionsDigest(run.questions),
		Questions:           cloneRecognizedQuestions(run.questions),
		PhysicalInvocations: physicalReceipts,
		CreatedAt:           time.Now().Unix(),
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("marshal 识别结果回执: %w", err)
	}
	if err := atomicWriteFileNoReplace(path, raw); err != nil {
		return fmt.Errorf("写识别结果回执: %w", err)
	}
	return nil
}

func recognitionPhysicalReceiptSet(
	children []k12.ModelPhysicalInvocation,
) []gradingRecognitionPhysicalReceipt {
	if len(children) == 0 {
		return nil
	}
	out := make(
		[]gradingRecognitionPhysicalReceipt,
		0,
		len(children),
	)
	for _, child := range children {
		out = append(out, gradingRecognitionPhysicalReceipt{
			PhysicalInvocationID: child.PhysicalInvocationID,
			PhysicalUnit:         child.PhysicalUnit,
			RequestDigest:        child.RequestDigest,
			ResultDigest:         child.ResultDigest,
		})
	}
	return out
}

func sameRecognitionPhysicalReceiptSet(
	left []gradingRecognitionPhysicalReceipt,
	right []gradingRecognitionPhysicalReceipt,
) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (o *GradingOrchestrator) readRecognitionReceipt(jobID string) (gradingRecognitionReceiptFile, bool) {
	if o.runDir == "" || strings.TrimSpace(jobID) == "" {
		return gradingRecognitionReceiptFile{}, false
	}
	raw, err := os.ReadFile(o.recognitionReceiptPath(jobID))
	if err != nil {
		return gradingRecognitionReceiptFile{}, false
	}
	var receipt gradingRecognitionReceiptFile
	if json.Unmarshal(raw, &receipt) != nil || receipt.JobID != jobID ||
		strings.TrimSpace(receipt.InvocationID) == "" || len(receipt.Questions) == 0 ||
		receipt.CanonicalDigest != CanonicalRecognizedQuestionsDigest(receipt.Questions) {
		return gradingRecognitionReceiptFile{}, false
	}
	for _, physical := range receipt.PhysicalInvocations {
		if strings.TrimSpace(physical.PhysicalInvocationID) == "" ||
			!physical.PhysicalUnit.Valid() ||
			!validModelInvocationDigest(physical.RequestDigest) ||
			!validModelInvocationDigest(physical.ResultDigest) {
			return gradingRecognitionReceiptFile{}, false
		}
	}
	return receipt, true
}

// archiveRecognitionFacts is the terminal append-only audit. Unlike the
// transient invocation receipt it records the final confirmed recognition
// facts and therefore must only be created during ReleaseGradingRun.
func (o *GradingOrchestrator) archiveRecognitionFacts(jobID string, run *gradingRun) error {
	if o.runDir == "" || run == nil || strings.TrimSpace(jobID) == "" || len(run.questions) == 0 {
		return nil
	}
	path := o.recognitionAuditPath(jobID)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查识别审计归档: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建识别审计目录: %w", err)
	}
	audit := gradingRecognitionAuditFile{
		JobID: jobID, AgentName: run.agentName,
		CanonicalDigest: CanonicalRecognizedQuestionsDigest(run.questions),
		Questions:       cloneRecognizedQuestions(run.questions), ArchivedAt: time.Now().Unix(),
	}
	raw, err := json.Marshal(audit)
	if err != nil {
		return fmt.Errorf("marshal 识别审计归档: %w", err)
	}
	if err := atomicWriteFileNoReplace(path, raw); err != nil {
		return fmt.Errorf("写识别审计归档: %w", err)
	}
	return nil
}

func (o *GradingOrchestrator) archivedRecognizedQuestions(jobID string) ([]RecognizedQuestion, bool) {
	audit, ok := o.readRecognitionAudit(jobID)
	if !ok {
		return nil, false
	}
	return cloneRecognizedQuestions(audit.Questions), true
}

func (o *GradingOrchestrator) readRecognitionAudit(jobID string) (gradingRecognitionAuditFile, bool) {
	if o.runDir == "" || strings.TrimSpace(jobID) == "" {
		return gradingRecognitionAuditFile{}, false
	}
	raw, err := os.ReadFile(o.recognitionAuditPath(jobID))
	if err != nil {
		return gradingRecognitionAuditFile{}, false
	}
	var audit gradingRecognitionAuditFile
	if json.Unmarshal(raw, &audit) != nil || audit.JobID != jobID || len(audit.Questions) == 0 ||
		audit.CanonicalDigest != CanonicalRecognizedQuestionsDigest(audit.Questions) {
		return gradingRecognitionAuditFile{}, false
	}
	return audit, true
}

// releaseRunFiles 删除落盘运行时（投递完成/终态清理）。
func (o *GradingOrchestrator) releaseRunFiles(jobID string) {
	if o.runDir == "" || strings.TrimSpace(jobID) == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(o.runDir, jobID))
}

// atomicWriteFile 先写临时文件再原子改名，防中断留半文件（§6.15 不允许阶段产物半写）。
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// atomicWriteFileNoReplace 通过同目录临时文件 + hard link 原子发布只追加事实；目标已存在
// 视为幂等成功，任何重复 Release 都不能覆盖首次归档。
func atomicWriteFileNoReplace(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".audit-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(name, path); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}
