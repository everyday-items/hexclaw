package usecase

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

var ErrModelInvocationRequiresReconciliation = errors.New("model invocation requires reconciliation")

// GradingOrchestrator 统一 GradingJob 编排器（架构设计 §6.7 状态机 / §6.15 单机执行模型·二阶段）。
//
// 职责：让真实批改流程通过 GradingJob 状态机推进——按 Job 当前 stage 顺序执行
// normalizing → recognizing →（locating 并行分支）→ awaiting_confirmation（停点）→
// assessing → rendering → projecting → completed。每阶段只调用现有用例的公开入口
// （RecognizeHomework / AnchorHomeworkAnswers / GradeHomeworkPhoto），阶段产物摘要经
// AdvanceGradingStage 写入 stage_checkpoints，失败按 retryable 语义收敛（规则 2/3/4）。
//
// 单机形态（§6.15）：进程内顺序推进，阶段中间产物（原图/识别结果/批改产物）保存在
// 内存 run 登记表——检查点只固化摘要。崩溃后 run 丢失属预期：非终态 Job 的启动扫描
// 恢复（重新入队从检查点续跑）在下一轮接线，本轮 RunGradingJob 对无 run 的 Job 明确报错。
//
// 不重复调模型的关键（规则 3）：assessing 阶段复用 recognizing/locating 已固化的产物——
// 通过给 GradeHomeworkPhoto 注入「预置结果」的 Recognizer/AnswerAnchorer 替身实现，
// 识别/锚点模型不再被二次调用，而分流、证据门禁、逐题批改、Markdown 汇总等内部
// 逻辑全部复用现网实现（pipeline/photo_grade 只读不改）。
type GradingOrchestrator struct {
	deps Deps
	// snapshotFn 提供实际模型路由快照（§6.12：GradingJob 保存不可变路由快照）。
	// StartPhotoGradingInput 未显式给出快照时由它兜底。
	snapshotFn func() k12.GradingModelSnapshot

	// runDir 阶段产物落盘目录（§6.15 崩溃恢复载体；空 = 仅内存，见 *_runtime.go）。
	runDir string
	// baseCtx 异步推进基座 context（与 HTTP 请求解耦，§6.15 任务执行模型）。
	baseCtx context.Context
	// sem 异步推进有界并发信号量。
	sem chan struct{}
	// anchorTimeout 锚点增强分支的独立预算（默认 60s，可通过 option 配置）。
	anchorTimeout time.Duration

	mu           sync.Mutex
	runs         map[string]*gradingRun
	active       map[string]bool          // 在途异步推进守卫（同 Job 不并发双跑）
	rerun        map[string]bool          // active 期间收到续跑信号时，退出前至少再检查一次状态机
	anchorActive map[string]bool          // 独立锚点分支守卫（外部调用不占 Job 锁）
	anchorDone   map[string]chan struct{} // 同步入口只等待分支完成，不让模型调用占用 Job 锁
	locks        map[string]*sync.Mutex   // 每 Job 执行互斥：状态合并/写回与确认/重试/读产物串行化
}

// jobLock 取（或建）某 Job 的执行互斥。run 内存状态与状态机写回都只在持锁下发生，
// 消灭「HTTP 确认 vs 异步推进」并发下的乐观锁冲突与 run 数据竞争。
func (o *GradingOrchestrator) jobLock(jobID string) *sync.Mutex {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.locks == nil {
		o.locks = map[string]*sync.Mutex{}
	}
	l, ok := o.locks[jobID]
	if !ok {
		l = &sync.Mutex{}
		o.locks[jobID] = l
	}
	return l
}

// gradingRun 一个在途 Job 的进程内运行时状态（阶段中间产物）。
type gradingRun struct {
	agentName string
	req       PhotoGradeRequest
	// textOnly marks a trusted text Submission. req.Image contains only a
	// deterministic internal pipeline token so the shared grading pipeline can
	// keep its non-empty input invariant; it is never persisted or sent to a
	// recognizer/annotator.
	textOnly bool
	// questions recognizing 阶段产物（已 Normalize、BBox 已剥离）。
	questions []RecognizedQuestion
	// anchored locating 并行分支成功产物；nil = 未定位（缺席或失败）。
	anchored []RecognizedQuestion
	// anchorFailed 锚点分支调用失败（区分「能力缺席」：影响 assessing 阶段的降级文案复现）。
	anchorFailed bool
	// result assessing+rendering 产物（GradeHomeworkPhoto 完整返回值）。
	result *PhotoGradeResult
	// renderFailure 批注渲染失败原因（"" = 成功或未触发渲染），由 recordingAnnotator 捕获。
	renderFailure string
}

// NewGradingOrchestrator 组装编排器。snapshotFn 可为 nil（调用方须在 Start 输入里给快照）。
// opts 见 *_runtime.go（落盘恢复目录 / 异步基座 context / 有界并发）。
func NewGradingOrchestrator(deps Deps, snapshotFn func() k12.GradingModelSnapshot, opts ...GradingOrchestratorOption) *GradingOrchestrator {
	o := &GradingOrchestrator{
		deps: deps, snapshotFn: snapshotFn,
		runs: map[string]*gradingRun{}, active: map[string]bool{}, rerun: map[string]bool{},
		anchorActive: map[string]bool{}, anchorDone: map[string]chan struct{}{}, locks: map[string]*sync.Mutex{},
		sem: make(chan struct{}, 2), baseCtx: context.Background(),
		anchorTimeout: time.Duration(k12.GradingAnchorTimeoutSeconds) * time.Second,
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// StartPhotoGradingInput 照片批改 Job 创建输入。SourceKind/SourceKey 映射统一幂等键
// （§4.10：IM = "im"/message_id）。ModelSnapshot 为空时取 snapshotFn()。
type StartPhotoGradingInput struct {
	Photo         PhotoGradeRequest
	SourceKind    string
	SourceKey     string
	ModelSnapshot k12.GradingModelSnapshot
}

// StartPhotoGradingJob 创建照片批改 Job 并登记进程内运行时。幂等：同键命中既有 Job
// （created=false）且不覆盖已登记的运行时状态。
func (o *GradingOrchestrator) StartPhotoGradingJob(ctx context.Context, in StartPhotoGradingInput) (GradingJobView, bool, error) {
	if len(in.Photo.Image) == 0 {
		return GradingJobView{}, false, fmt.Errorf("%w: Image 不可空", ErrInvalidInput)
	}
	snap := in.ModelSnapshot
	if strings.TrimSpace(snap.Provider) == "" && o.snapshotFn != nil {
		snap = o.snapshotFn()
	}
	sum := sha1.Sum(in.Photo.Image)
	v, created, err := o.deps.CreateGradingJob(ctx, in.Photo.AgentName, in.Photo.SourceSession, CreateGradingJobInput{
		// Submission 聚合未落库（§6.9 类型化存储待接线）：以原图内容摘要为提交标识，
		// 同图重投可追溯到同一 submission。
		SubmissionID:  "photo-" + hex.EncodeToString(sum[:]),
		SourceKind:    in.SourceKind,
		SourceKey:     in.SourceKey,
		ModelSnapshot: snap,
	})
	if err != nil {
		return GradingJobView{}, false, err
	}
	o.mu.Lock()
	run, ok := o.runs[v.Record.RecordID]
	if !ok {
		run = &gradingRun{agentName: in.Photo.AgentName, req: in.Photo}
		o.runs[v.Record.RecordID] = run
	}
	o.mu.Unlock()
	// §6.15：原图与运行时状态即刻落盘（崩溃后从检查点恢复的载体；见 *_runtime.go 设计申报）。
	if err := o.persistRun(v.Record.RecordID, run); err != nil {
		return GradingJobView{}, false, fmt.Errorf("usecase: 固化批改任务运行时: %w", err)
	}
	return v, created, nil
}

// RunGradingJob 按 Job 当前 stage 顺序推进，直到停点（awaiting_confirmation 等确认命令）、
// 终态或失败态。返回推进后的视图；阶段执行的下游错误原样返回（Job 已安全落
// failed_retryable/failed_terminal），保持与旧自编排路径一致的错误面。
func (o *GradingOrchestrator) RunGradingJob(ctx context.Context, jobID string) (GradingJobView, error) {
	run, err := o.ensureRun(ctx, jobID)
	if err != nil {
		return GradingJobView{}, err
	}
	l := o.jobLock(jobID)
	l.Lock()
	defer l.Unlock()
	return o.runLoop(ctx, run, jobID)
}

// runLoop 持 Job 锁的推进循环（调用方必须已持 jobLock）。
func (o *GradingOrchestrator) runLoop(ctx context.Context, run *gradingRun, jobID string) (GradingJobView, error) {
	for {
		v, err := o.deps.GetGradingJob(ctx, run.agentName, jobID)
		if err != nil {
			return GradingJobView{}, err
		}
		// DD-018: every model boundary receives the immutable route stored on
		// this Job. Mutable global defaults are only consulted for new Jobs.
		ctx = k12.WithGradingModelSnapshot(ctx, v.Fields.ModelSnapshot)
		switch v.Record.Status {
		case k12.GradingStageQueued:
			// 起跑/恢复：AdvanceGradingStage(ok) 落到最近成功检查点的后继（规则 3/6）。
			if v, err = o.advanceOK(ctx, run, jobID, ""); err != nil {
				return v, err
			}
		case k12.GradingStageNormalizing:
			// 现网无独立图片预处理用例（EXIF/旋转在识别 adapter 内的可追溯变换链处理，
			// §6.8 步骤 1）→ 直通，检查点固化原图摘要作追溯锚。
			if v, err = o.advanceOK(ctx, run, jobID, "image:"+shortSHA1(run.req.Image)); err != nil {
				return v, err
			}
		case k12.GradingStageRecognizing:
			if v, err = o.runRecognize(ctx, run, jobID); err != nil {
				return v, err
			}
		case k12.GradingStageAwaitingConfirmation:
			// 规则 1：识别冻结后，确认与锚点是两个独立分支。锚点模型调用在 Job 锁外
			// 异步执行，主链立即返回确认停点；二者任意顺序到达，均由状态机显式汇合。
			if v.Fields.AnchorState == k12.GradingAnchorPending {
				o.startAnchorAsync(jobID, run, v.Fields.ModelSnapshot)
			}
			// 停点：等家长确认（规则 7 不自动过期）。ConfirmAndRun 续跑。
			return v, nil
		case k12.GradingStageAssessing:
			if v, err = o.runAssess(ctx, run, jobID); err != nil {
				return v, err
			}
		case k12.GradingStageRendering:
			if v, err = o.runRender(ctx, run, jobID); err != nil {
				return v, err
			}
		case k12.GradingStageProjecting:
			if v, err = o.runProject(ctx, run, jobID); err != nil {
				return v, err
			}
		default:
			// completed / cancelled / failed_retryable / failed_terminal：推进结束。
			return v, nil
		}
	}
}

// ConfirmAndRun 家长确认（公共命令③）后继续推进到下一停点/终态。
func (o *GradingOrchestrator) ConfirmAndRun(ctx context.Context, jobID string, corrections []string) (GradingJobView, error) {
	run, err := o.ensureRun(ctx, jobID)
	if err != nil {
		return GradingJobView{}, err
	}
	l := o.jobLock(jobID)
	l.Lock()
	candidate := *run
	candidate.questions = cloneRecognizedQuestions(run.questions)
	candidate.anchored = cloneRecognizedQuestions(run.anchored)
	if err := applyAndValidateGradingConfirmation(&candidate, ConfirmPhotoGradingInput{}); err != nil {
		l.Unlock()
		return GradingJobView{}, err
	}
	if err := o.persistRun(jobID, &candidate); err != nil {
		l.Unlock()
		return GradingJobView{}, fmt.Errorf("usecase: 固化确认后的识别产物: %w", err)
	}
	digestInputs := []string{"canonical-recognition:" + CanonicalRecognizedQuestionsDigest(candidate.questions)}
	// 旧同步入口的自由文本只留作审计附录；结论身份始终由 canonical digest 决定。
	for _, correction := range corrections {
		digestInputs = append(digestInputs, "legacy-note:"+correction)
	}
	if _, err := o.deps.ConfirmGradingJob(ctx, run.agentName, jobID, digestInputs); err != nil {
		l.Unlock()
		return GradingJobView{}, err
	}
	run.questions = candidate.questions
	run.anchored = candidate.anchored
	for {
		v, err := o.runLoop(ctx, run, jobID)
		if err != nil || v.Record.Status != k12.GradingStageAwaitingConfirmation ||
			v.Fields.ConfirmationState != k12.GradingConfirmationConfirmed ||
			v.Fields.AnchorState != k12.GradingAnchorPending {
			l.Unlock()
			return v, err
		}
		// IM 等同步入口保留“确认并跑完”的语义，但先释放 Job 锁再等 anchor：确认已经
		// 持久化，HTTP/桌面读写不被模型延迟阻塞；锚点回位后重新读状态机继续主链。
		done := o.anchorDoneChannel(jobID)
		l.Unlock()
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return GradingJobView{}, ctx.Err()
			}
		}
		l.Lock()
	}
}

// RetryAndRun 安全重试（公共命令④）后从最近检查点续跑（规则 3）。
func (o *GradingOrchestrator) RetryAndRun(ctx context.Context, jobID string) (GradingJobView, error) {
	run, err := o.ensureRun(ctx, jobID)
	if err != nil {
		return GradingJobView{}, err
	}
	l := o.jobLock(jobID)
	l.Lock()
	defer l.Unlock()
	if _, err := o.deps.RetryGradingJob(ctx, run.agentName, jobID); err != nil {
		return GradingJobView{}, err
	}
	return o.runLoop(ctx, run, jobID)
}

// PhotoResult 取 completed 后的批改产物（Markdown + 批改图），供入口按现有投递逻辑发送。
func (o *GradingOrchestrator) PhotoResult(jobID string) (PhotoGradeResult, bool) {
	l := o.jobLock(jobID)
	l.Lock()
	defer l.Unlock()
	o.mu.Lock()
	run, ok := o.runs[jobID]
	o.mu.Unlock()
	if !ok || run.result == nil {
		return PhotoGradeResult{}, false
	}
	return *run.result, true
}

// ReleaseGradingRun 投递完成后释放进程内运行时与原图/结果临时产物；识别阶段的
// raw/canonical/结构/确认原因先写只追加审计归档，DD-012 要求 raw 不随任务清理消失。
func (o *GradingOrchestrator) ReleaseGradingRun(jobID string) {
	o.mu.Lock()
	run := o.runs[jobID]
	delete(o.runs, jobID)
	delete(o.active, jobID)
	delete(o.rerun, jobID)
	delete(o.anchorActive, jobID)
	delete(o.anchorDone, jobID)
	delete(o.locks, jobID)
	o.mu.Unlock()
	if err := o.archiveRecognitionFacts(jobID, run); err != nil {
		// 归档失败时保留原 run 目录；宁可暂不回收原图，也不能违反 raw 永久留存。
		slog.Error("K12 识别事实终态归档失败（保留原运行目录）", "job", jobID, "err", err)
		return
	}
	o.releaseRunFiles(jobID)
}

// --- 阶段执行 ---

// runRecognize recognizing 阶段：调现有识题用例（公开入口），成功写检查点进
// awaiting_confirmation；失败按 retryable 语义落 failed_*（规则 4）并原样返回下游错误。
func (o *GradingOrchestrator) runRecognize(ctx context.Context, run *gradingRun, jobID string) (GradingJobView, error) {
	job, err := o.deps.GetGradingJob(ctx, run.agentName, jobID)
	if err != nil {
		return GradingJobView{}, err
	}
	invocation, err := o.beginModelInvocation(ctx, job, k12.GradingStageRecognizing,
		modelInvocationDigest([]byte(k12.GradingStageRecognizing), run.req.Image))
	if err != nil {
		if recovered, v, recoverErr := o.recoverRecognizeInvocation(ctx, run, jobID, invocation); recovered {
			return v, recoverErr
		}
		if invocation.Status == k12.ModelInvocationSent {
			invocation, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(context.WithoutCancel(ctx),
				run.agentName, invocation.InvocationID, "recovered_after_send")
		}
		unknown, advanceErr := o.markGradingOutcomeUnknown(ctx, run, jobID, "invocation_reconciliation_required")
		if advanceErr != nil {
			return unknown, advanceErr
		}
		return unknown, err
	}
	questions, err := o.deps.RecognizeHomework(ctx, run.req.Image)
	if err != nil {
		if invocationOutcomeUnknown(err) {
			_, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(context.WithoutCancel(ctx), run.agentName, invocation.InvocationID, "provider_outcome_unknown")
			v, aerr := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, jobID, "provider_outcome_unknown")
			if aerr != nil {
				return v, aerr
			}
			return v, err
		}
		if _, ledgerErr := o.deps.Records.MarkModelInvocationFailed(context.WithoutCancel(ctx), run.agentName, invocation.InvocationID, "recognize_failed"); ledgerErr != nil {
			v, aerr := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, jobID, "invocation_ledger_write_failed")
			if aerr != nil {
				return v, aerr
			}
			return v, ledgerErr
		}
		v, aerr := o.deps.AdvanceGradingStage(ctx, run.agentName, jobID, AdvanceGradingInput{
			Outcome:     GradingOutcomeFailed,
			FailureKind: "recognize_failed",
			Retryable:   gradingErrRetryable(err),
		})
		if aerr != nil {
			return v, aerr
		}
		return v, err
	}
	if len(questions) == 0 {
		_, _ = o.deps.Records.MarkModelInvocationSucceeded(context.WithoutCancel(ctx), run.agentName,
			invocation.InvocationID, modelInvocationResultDigest(questions), "")
		err = fmt.Errorf("%w: 未识别到可处理的题目", ErrInvalidInput)
		v, aerr := o.deps.AdvanceGradingStage(ctx, run.agentName, jobID, AdvanceGradingInput{
			Outcome: GradingOutcomeFailed, FailureKind: "recognize_empty", Retryable: false,
		})
		if aerr != nil {
			return v, aerr
		}
		return v, err
	}
	run.questions, err = NormalizeRecognizedProblems(job.Fields.SubmissionID, cloneRecognizedQuestions(questions))
	if err != nil {
		_, _ = o.deps.Records.MarkModelInvocationSucceeded(context.WithoutCancel(ctx), run.agentName,
			invocation.InvocationID, modelInvocationResultDigest(questions), "")
		v, aerr := o.deps.AdvanceGradingStage(ctx, run.agentName, jobID, AdvanceGradingInput{
			Outcome: GradingOutcomeFailed, FailureKind: "recognize_structure_invalid", Retryable: false,
		})
		if aerr != nil {
			return v, aerr
		}
		return v, err
	}
	for i := range run.questions {
		// 核心识别冻结事实不接纳 geometry；BBox 只能由独立 anchor 分支补入。
		run.questions[i].BBox = nil
	}
	if perr := o.persistProblemAttemptFacts(ctx, run.agentName, job.Fields.SubmissionID, run.questions); perr != nil {
		_, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(context.WithoutCancel(ctx), run.agentName,
			invocation.InvocationID, "typed_result_not_durable")
		v, aerr := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, jobID, "typed_result_not_durable")
		if aerr != nil {
			return v, aerr
		}
		return v, perr
	}
	// 先固化产物再写检查点（§6.15：检查点存在即产物可回放，崩溃窗口不产生"有检查点无产物"）。
	if perr := o.persistRun(jobID, run); perr != nil {
		_, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(context.WithoutCancel(ctx), run.agentName,
			invocation.InvocationID, "result_not_durable")
		v, aerr := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, jobID, "result_not_durable")
		if aerr != nil {
			return v, aerr
		}
		return v, perr
	}
	if _, err := o.deps.Records.MarkModelInvocationSucceeded(context.WithoutCancel(ctx), run.agentName,
		invocation.InvocationID, modelInvocationResultDigest(run.questions), ""); err != nil {
		v, aerr := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, jobID, "invocation_ledger_write_failed")
		if aerr != nil {
			return v, aerr
		}
		return v, err
	}
	return o.advanceOK(ctx, run, jobID, fmt.Sprintf("questions:%d", len(questions)))
}

// startAnchorAsync 启动 locating 独立分支。昂贵的模型调用不持 Job 锁，因此家长确认可
// 同时持久化；只有几何合并、run 落盘与 anchor 检查点写回在 Job 锁内串行化。
func (o *GradingOrchestrator) startAnchorAsync(jobID string, run *gradingRun, snapshot k12.GradingModelSnapshot) {
	o.mu.Lock()
	if o.anchorActive == nil {
		o.anchorActive = map[string]bool{}
	}
	if o.anchorActive[jobID] {
		o.mu.Unlock()
		return
	}
	o.anchorActive[jobID] = true
	done := make(chan struct{})
	if o.anchorDone == nil {
		o.anchorDone = map[string]chan struct{}{}
	}
	o.anchorDone[jobID] = done
	o.mu.Unlock()

	image := append([]byte(nil), run.req.Image...)
	frozen := cloneRecognizedQuestions(run.questions)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("K12 批改任务锚点分支 panic（任务保留 anchor pending）", "job", jobID, "panic", r)
			}
			o.mu.Lock()
			delete(o.anchorActive, jobID)
			if current := o.anchorDone[jobID]; current == done {
				delete(o.anchorDone, jobID)
				close(done)
			}
			o.mu.Unlock()
		}()

		anchored, state, digest, failed := o.executeAnchor(jobID, run.agentName, image, frozen, snapshot)
		ctx := o.gradingBaseContext()
		l := o.jobLock(jobID)
		l.Lock()
		v, err := o.deps.GetGradingJob(ctx, run.agentName, jobID)
		if err != nil || v.Fields.AnchorState != k12.GradingAnchorPending ||
			(v.Record.Status != k12.GradingStageAwaitingConfirmation && v.Record.Status != k12.GradingStageLocating) {
			l.Unlock()
			return
		}
		run.anchorFailed = failed
		if state == k12.GradingAnchorLocated {
			// Adapter 只拥有 geometry 权限：以当前 canonical（可能已被家长确认修正）为底，
			// 按索引拷贝 BBox，绝不接纳题干/答案/作答态/学科/知识点的反向覆盖。
			run.anchored = mergeAnchorGeometry(run.questions, anchored)
		} else {
			run.anchored = nil
		}
		facts := run.questions
		if run.anchored != nil {
			facts = run.anchored
		}
		if err = o.persistProblemAttemptFacts(ctx, run.agentName, v.Fields.SubmissionID, facts); err == nil {
			err = o.persistRun(jobID, run)
		}
		if err == nil {
			v, err = o.deps.AdvanceGradingStage(ctx, run.agentName, jobID, AdvanceGradingInput{
				Outcome: GradingOutcomeAnchor, AnchorState: state, ArtifactDigest: digest,
			})
		}
		l.Unlock()
		if err != nil {
			slog.Warn("K12 批改任务锚点结果写回失败（保留当前状态供恢复）", "job", jobID, "err", err)
			return
		}
		if v.Record.Status == k12.GradingStageAssessing {
			o.StartAsync(jobID)
		}
	}()
}

func (o *GradingOrchestrator) anchorDoneChannel(jobID string) <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.anchorDone[jobID]
}

// executeAnchor 只包围锚点外部调用的 deadline。超时是可审计的 degraded，而不是任务失败；
// 后续仍使用冻结的文字事实完成 assessing/rendering/projecting。
func (o *GradingOrchestrator) executeAnchor(jobID, agentName string, image []byte, frozen []RecognizedQuestion, snapshot k12.GradingModelSnapshot) ([]RecognizedQuestion, string, string, bool) {
	if o.deps.AnswerAnchorer == nil || !hasAnswerCandidate(frozen) {
		return nil, k12.GradingAnchorDegraded, "anchor:absent", false
	}
	ctx := k12.WithGradingModelSnapshot(o.gradingBaseContext(), snapshot)
	ctx, cancel := context.WithTimeout(ctx, o.anchorTimeout)
	job, err := o.deps.GetGradingJob(ctx, agentName, jobID)
	if err != nil {
		cancel()
		return nil, k12.GradingAnchorDegraded, "anchor:ledger_job_missing", true
	}
	requestRaw, _ := json.Marshal(struct {
		ImageDigest string               `json:"image_digest"`
		Questions   []RecognizedQuestion `json:"questions"`
	}{modelInvocationDigest(image), frozen})
	invocation, err := o.beginModelInvocation(ctx, job, k12.GradingStageLocating,
		modelInvocationDigest([]byte(k12.GradingStageLocating), requestRaw))
	if err != nil {
		cancel()
		return nil, k12.GradingAnchorDegraded, "anchor:outcome_unknown", true
	}
	anchored, err := o.deps.AnchorHomeworkAnswers(ctx, image, frozen)
	ctxErr := ctx.Err()
	cancel()
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctxErr, context.DeadlineExceeded) {
		_, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(context.WithoutCancel(ctx), agentName,
			invocation.InvocationID, "provider_outcome_unknown")
		return nil, k12.GradingAnchorDegraded, "anchor:timeout", true
	}
	if err != nil {
		_, _ = o.deps.Records.MarkModelInvocationFailed(context.WithoutCancel(ctx), agentName,
			invocation.InvocationID, "anchor_failed")
		return nil, k12.GradingAnchorDegraded, "anchor:failed", true
	}
	if _, err := o.deps.Records.MarkModelInvocationSucceeded(context.WithoutCancel(ctx), agentName,
		invocation.InvocationID, modelInvocationResultDigest(anchored), ""); err != nil {
		return nil, k12.GradingAnchorDegraded, "anchor:ledger_write_failed", true
	}
	return anchored, k12.GradingAnchorLocated, "anchor:located", false
}

// runAssess assessing 阶段：复用识别/锚点检查点产物调 GradeHomeworkPhoto（公开入口）——
// 分流、证据门禁、逐题批改、错题入库、批注渲染、Markdown 汇总全走现网实现。
// 渲染结果由 recordingAnnotator 捕获，rendering 阶段据此推进/降级。
func (o *GradingOrchestrator) runAssess(ctx context.Context, run *gradingRun, jobID string) (GradingJobView, error) {
	job, err := o.deps.GetGradingJob(ctx, run.agentName, jobID)
	if err != nil {
		return GradingJobView{}, err
	}
	requestRaw, _ := json.Marshal(struct {
		Request   PhotoGradeRequest    `json:"request"`
		Questions []RecognizedQuestion `json:"questions"`
		Anchored  []RecognizedQuestion `json:"anchored,omitempty"`
	}{run.req, run.questions, run.anchored})
	invocation, err := o.beginModelInvocation(ctx, job, k12.GradingStageAssessing,
		modelInvocationDigest([]byte(k12.GradingStageAssessing), requestRaw))
	if err != nil {
		if recovered, v, recoverErr := o.recoverAssessInvocation(ctx, run, jobID, invocation); recovered {
			return v, recoverErr
		}
		if invocation.Status == k12.ModelInvocationSent {
			invocation, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(context.WithoutCancel(ctx),
				run.agentName, invocation.InvocationID, "recovered_after_send")
		}
		unknown, advanceErr := o.markGradingOutcomeUnknown(ctx, run, jobID, "invocation_reconciliation_required")
		if advanceErr != nil {
			return unknown, advanceErr
		}
		return unknown, err
	}
	assessDeps := o.deps
	// 预置识别产物：识别模型不二次调用（规则 3 禁止重复调模型）。
	assessDeps.Recognizer = presetRecognizer{questions: run.questions}
	// 预置锚点结论：located 回放产物；失败复现失败（触发与现网一致的文字降级文案）；
	// 缺席保持缺席（GradeHomeworkPhoto 按无核验能力的既有语义 fail-closed）。
	switch {
	case run.anchored != nil:
		assessDeps.AnswerAnchorer = presetAnchorer{questions: run.anchored}
	case run.anchorFailed:
		assessDeps.AnswerAnchorer = presetAnchorer{err: errors.New("锚点定位在 locating 阶段已失败（检查点回放）")}
	default:
		assessDeps.AnswerAnchorer = nil
	}
	var recorder *recordingAnnotator
	if run.textOnly {
		// The deterministic text pipeline token is an internal invariant marker,
		// never user media. Fail closed even if a restored/legacy checkpoint
		// unexpectedly carries coordinates.
		assessDeps.PhotoAnnotator = nil
	} else if o.deps.PhotoAnnotator != nil {
		recorder = &recordingAnnotator{inner: o.deps.PhotoAnnotator}
		assessDeps.PhotoAnnotator = recorder
	}

	result, err := assessDeps.GradeHomeworkPhoto(ctx, run.req)
	if err != nil {
		if invocationOutcomeUnknown(err) {
			_, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(context.WithoutCancel(ctx), run.agentName, invocation.InvocationID, "provider_outcome_unknown")
			v, aerr := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, jobID, "provider_outcome_unknown")
			if aerr != nil {
				return v, aerr
			}
			return v, err
		}
		if _, ledgerErr := o.deps.Records.MarkModelInvocationFailed(context.WithoutCancel(ctx), run.agentName, invocation.InvocationID, "assess_failed"); ledgerErr != nil {
			v, aerr := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, jobID, "invocation_ledger_write_failed")
			if aerr != nil {
				return v, aerr
			}
			return v, ledgerErr
		}
		v, aerr := o.deps.AdvanceGradingStage(ctx, run.agentName, jobID, AdvanceGradingInput{
			Outcome:     GradingOutcomeFailed,
			FailureKind: "assess_failed",
			Retryable:   gradingErrRetryable(err),
		})
		if aerr != nil {
			return v, aerr
		}
		return v, err
	}
	run.result = &result
	run.renderFailure = ""
	if recorder != nil {
		run.renderFailure = recorder.failure
	}
	if perr := o.persistRun(jobID, run); perr != nil {
		_, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(context.WithoutCancel(ctx), run.agentName,
			invocation.InvocationID, "result_not_durable")
		v, aerr := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, jobID, "result_not_durable")
		if aerr != nil {
			return v, aerr
		}
		return v, perr
	}
	if _, err := o.deps.Records.MarkModelInvocationSucceeded(context.WithoutCancel(ctx), run.agentName,
		invocation.InvocationID, modelInvocationResultDigest(result), ""); err != nil {
		v, aerr := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, jobID, "invocation_ledger_write_failed")
		if aerr != nil {
			return v, aerr
		}
		return v, err
	}
	return o.advanceOK(ctx, run, jobID, fmt.Sprintf("items:%d mode:%s", len(result.Items), result.Mode))
}

// runRender rendering 阶段：像素合成已在 assessing 内联执行（现网实现），本阶段回写其结果——
// 失败走规则 2 降级（不终态，续跑 projecting），成功/无可信坐标可渲染则正常推进。
func (o *GradingOrchestrator) runRender(ctx context.Context, run *gradingRun, jobID string) (GradingJobView, error) {
	if run.result == nil {
		return GradingJobView{}, fmt.Errorf("usecase: 批改任务 %s rendering 缺批改产物（运行时状态丢失）", jobID)
	}
	if run.renderFailure != "" {
		return o.deps.AdvanceGradingStage(ctx, run.agentName, jobID, AdvanceGradingInput{
			Outcome: GradingOutcomeFailed, FailureKind: "annotate_failed", Retryable: false,
		})
	}
	digest := "render:skipped"
	if run.result.AnnotatedImage != nil && len(run.result.AnnotatedImage.Data) > 0 {
		digest = "render:" + firstNonEmpty(run.result.AnnotatedImage.MIME, "image/png")
	}
	return o.advanceOK(ctx, run, jobID, digest)
}

// runProject projecting 阶段：错题投影已在批改用例内联幂等入库（pipeline.go 判错步骤，
// 含首次复习到期与学情信号），本阶段固化投影摘要检查点后收敛 completed。
// 错题域写与 Outbox 事件已同事务提交（§6.9，k12storage V9）；
// TODO(Assessment 聚合)：k12_assessments 聚合落库后，本阶段改为 Assessment 投影事务。
func (o *GradingOrchestrator) runProject(ctx context.Context, run *gradingRun, jobID string) (GradingJobView, error) {
	if run.result == nil {
		return GradingJobView{}, fmt.Errorf("usecase: 批改任务 %s projecting 缺批改产物（运行时状态丢失）", jobID)
	}
	mistakes := 0
	for _, item := range run.result.Items {
		if item.Status == PhotoWrong && item.Grade.RecordID != "" {
			mistakes++
		}
	}
	return o.advanceOK(ctx, run, jobID, fmt.Sprintf("mistakes:%d", mistakes))
}

// --- 内部辅助 ---

func (o *GradingOrchestrator) lookup(jobID string) *gradingRun {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.runs[jobID]
}

func (o *GradingOrchestrator) advanceOK(ctx context.Context, run *gradingRun, jobID, digest string) (GradingJobView, error) {
	return o.deps.AdvanceGradingStage(ctx, run.agentName, jobID, AdvanceGradingInput{
		Outcome: GradingOutcomeOK, ArtifactDigest: digest,
	})
}

// failStage 当前自动阶段按可重试失败落库并返回原因（运行时落盘失败等本机 IO 类错误可重试）。
func (o *GradingOrchestrator) failStage(ctx context.Context, run *gradingRun, jobID, kind string, cause error) (GradingJobView, error) {
	v, aerr := o.deps.AdvanceGradingStage(ctx, run.agentName, jobID, AdvanceGradingInput{
		Outcome: GradingOutcomeFailed, FailureKind: kind, Retryable: true,
	})
	if aerr != nil {
		return v, aerr
	}
	return v, cause
}

// gradingErrRetryable 失败可重试判定：输入类错误（ErrInvalidInput）重跑必然同败 → 不可重试；
// 下游服务错误（超时/上游不可用等）→ 可安全重试（阶段有幂等检查点，规则 3）。
func gradingErrRetryable(err error) bool {
	return !errors.Is(err, ErrInvalidInput)
}

func hasAnswerCandidate(questions []RecognizedQuestion) bool {
	for _, q := range questions {
		switch NormalizeRecognizedQuestion(q).AnswerState {
		case AnswerStatePresent, AnswerStateUnclear:
			return true
		}
	}
	return false
}

func cloneRecognizedQuestions(questions []RecognizedQuestion) []RecognizedQuestion {
	if questions == nil {
		return nil
	}
	out := make([]RecognizedQuestion, len(questions))
	for i, question := range questions {
		out[i] = question
		out[i].KnowledgePoints = append([]string(nil), question.KnowledgePoints...)
		out[i].OCRSignals = append([]string(nil), question.OCRSignals...)
		out[i].EvidenceTranscriptions = append([]string(nil), question.EvidenceTranscriptions...)
		out[i].AnswerEvidenceTranscriptions = append([]string(nil), question.AnswerEvidenceTranscriptions...)
		out[i].ConfirmationReasons = append([]OCRRiskReason(nil), question.ConfirmationReasons...)
		if question.RecognitionConfidence != nil {
			confidence := *question.RecognitionConfidence
			out[i].RecognitionConfidence = &confidence
		}
		if question.BBox != nil {
			box := *question.BBox
			out[i].BBox = &box
		}
	}
	return out
}

// mergeAnchorGeometry 以 canonical 为唯一事实源，仅允许 anchor 输出补充对应题目的 BBox。
func mergeAnchorGeometry(canonical, anchored []RecognizedQuestion) []RecognizedQuestion {
	out := cloneRecognizedQuestions(canonical)
	byProblemID := make(map[string]*BBox, len(anchored))
	for i := range anchored {
		if anchored[i].ProblemID != "" && anchored[i].BBox != nil {
			byProblemID[anchored[i].ProblemID] = anchored[i].BBox
		}
	}
	for i := range out {
		out[i].BBox = nil
		var source *BBox
		if out[i].ProblemID != "" {
			source = byProblemID[out[i].ProblemID]
		} else if i < len(anchored) {
			// 仅为老的、尚未分配 ProblemID 的内存值对象保留索引兼容；正式 Job
			// 在调用 anchor 前已经冻结 ID，不能因返回顺序变化串到兄弟题。
			source = anchored[i].BBox
		}
		if source == nil {
			continue
		}
		switch NormalizeRecognizedQuestion(out[i]).AnswerState {
		case AnswerStatePresent, AnswerStateUnclear:
			box := *source
			out[i].BBox = &box
		}
	}
	return out
}

func shortSHA1(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:8])
}

func modelInvocationDigest(parts ...[]byte) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(part)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func modelInvocationResultDigest(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return modelInvocationDigest([]byte(fmt.Sprintf("%#v", value)))
	}
	return modelInvocationDigest(raw)
}

func invocationOutcomeUnknown(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrModelInvocationRequiresReconciliation)
}

func (o *GradingOrchestrator) beginModelInvocation(ctx context.Context, job GradingJobView, stage, requestDigest string) (k12.ModelInvocation, error) {
	invocation, _, err := o.deps.Records.PrepareModelInvocation(ctx, k12.ModelInvocation{
		InvocationID: "modelinv-" + idgen.ShortID(), AgentName: job.Record.AgentName,
		JobID: job.Record.RecordID, Stage: stage, RequestDigest: requestDigest,
		RouteSnapshot: job.Fields.ModelSnapshot, Attempt: job.Fields.AttemptCount + 1,
		CreatedAt: o.deps.now(), UpdatedAt: o.deps.now(),
	})
	if err != nil {
		return k12.ModelInvocation{}, err
	}
	switch invocation.Status {
	case k12.ModelInvocationPrepared:
		return o.deps.Records.MarkModelInvocationSent(ctx, invocation.AgentName, invocation.InvocationID, "")
	case k12.ModelInvocationSent, k12.ModelInvocationSucceeded, k12.ModelInvocationOutcomeUnknown,
		k12.ModelInvocationReconciled:
		return invocation, fmt.Errorf("%w: invocation=%s status=%s", ErrModelInvocationRequiresReconciliation,
			invocation.InvocationID, invocation.Status)
	default:
		return invocation, fmt.Errorf("%w: invocation=%s unexpected status=%s", ErrModelInvocationRequiresReconciliation,
			invocation.InvocationID, invocation.Status)
	}
}

func (o *GradingOrchestrator) recoverRecognizeInvocation(ctx context.Context, run *gradingRun, jobID string, invocation k12.ModelInvocation) (bool, GradingJobView, error) {
	if (invocation.Status != k12.ModelInvocationSent && invocation.Status != k12.ModelInvocationSucceeded) || len(run.questions) == 0 {
		return false, GradingJobView{}, nil
	}
	digest := modelInvocationResultDigest(run.questions)
	if invocation.Status == k12.ModelInvocationSucceeded && invocation.ResultDigest != digest {
		return false, GradingJobView{}, nil
	}
	if invocation.Status == k12.ModelInvocationSent {
		if _, err := o.deps.Records.MarkModelInvocationSucceeded(context.WithoutCancel(ctx), run.agentName,
			invocation.InvocationID, digest, ""); err != nil {
			return true, GradingJobView{}, err
		}
	}
	v, err := o.advanceOK(ctx, run, jobID, fmt.Sprintf("questions:%d", len(run.questions)))
	return true, v, err
}

func (o *GradingOrchestrator) recoverAssessInvocation(ctx context.Context, run *gradingRun, jobID string, invocation k12.ModelInvocation) (bool, GradingJobView, error) {
	if (invocation.Status != k12.ModelInvocationSent && invocation.Status != k12.ModelInvocationSucceeded) || run.result == nil {
		return false, GradingJobView{}, nil
	}
	digest := modelInvocationResultDigest(*run.result)
	if invocation.Status == k12.ModelInvocationSucceeded && invocation.ResultDigest != digest {
		return false, GradingJobView{}, nil
	}
	if invocation.Status == k12.ModelInvocationSent {
		if _, err := o.deps.Records.MarkModelInvocationSucceeded(context.WithoutCancel(ctx), run.agentName,
			invocation.InvocationID, digest, ""); err != nil {
			return true, GradingJobView{}, err
		}
	}
	v, err := o.advanceOK(ctx, run, jobID, fmt.Sprintf("items:%d mode:%s", len(run.result.Items), run.result.Mode))
	return true, v, err
}

func (o *GradingOrchestrator) markGradingOutcomeUnknown(ctx context.Context, run *gradingRun, jobID, failureKind string) (GradingJobView, error) {
	return o.deps.AdvanceGradingStage(ctx, run.agentName, jobID, AdvanceGradingInput{
		Outcome: GradingOutcomeUnknown, FailureKind: failureKind,
	})
}

// presetRecognizer 回放 recognizing 阶段已固化的识别产物（不再调识别模型）。
type presetRecognizer struct{ questions []RecognizedQuestion }

func (p presetRecognizer) Recognize(context.Context, []byte) ([]RecognizedQuestion, error) {
	return append([]RecognizedQuestion(nil), p.questions...), nil
}

// presetAnchorer 回放 locating 分支结论（成功回放产物 / 失败复现失败语义）。
type presetAnchorer struct {
	questions []RecognizedQuestion
	err       error
}

func (p presetAnchorer) AnchorAnswers(context.Context, []byte, []RecognizedQuestion) ([]RecognizedQuestion, error) {
	if p.err != nil {
		return nil, p.err
	}
	return append([]RecognizedQuestion(nil), p.questions...), nil
}

// recordingAnnotator 包装现网 PhotoAnnotator，捕获渲染是否失败供 rendering 阶段回写（规则 2）。
type recordingAnnotator struct {
	inner   PhotoAnnotator
	failure string
}

func (r *recordingAnnotator) Annotate(ctx context.Context, image []byte, marks []PhotoAnnotation) (RenderedPhoto, error) {
	rendered, err := r.inner.Annotate(ctx, image, marks)
	if err != nil {
		r.failure = err.Error()
	}
	return rendered, err
}
