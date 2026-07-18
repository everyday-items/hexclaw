package usecase

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

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

	mu     sync.Mutex
	runs   map[string]*gradingRun
	active map[string]bool        // 在途异步推进守卫（同 Job 不并发双跑）
	locks  map[string]*sync.Mutex // 每 Job 执行互斥：异步推进与确认/重试/读产物串行化
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
		runs: map[string]*gradingRun{}, active: map[string]bool{}, locks: map[string]*sync.Mutex{},
		sem: make(chan struct{}, 2), baseCtx: context.Background(),
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
			// 规则 1：锚点是与确认并行的增强分支。识别一到位就先把锚点跑掉再停——
			// 单机顺序编排下这等价于「并行启动、确认前汇合」，且保证 Confirm 命令
			// 到达时 anchor_state 必已回位（located/degraded），不会卡等待。
			if v.Fields.AnchorState == k12.GradingAnchorPending {
				if v, err = o.runAnchor(ctx, run, jobID); err != nil {
					return v, err
				}
				continue
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
	defer l.Unlock()
	if _, err := o.deps.ConfirmGradingJob(ctx, run.agentName, jobID, corrections); err != nil {
		return GradingJobView{}, err
	}
	return o.runLoop(ctx, run, jobID)
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

// ReleaseGradingRun 投递完成后释放进程内运行时状态与落盘产物
// （检查点已在 Job 持久化，摘要不丢；终态任务不再需要恢复载体）。
func (o *GradingOrchestrator) ReleaseGradingRun(jobID string) {
	o.mu.Lock()
	delete(o.runs, jobID)
	delete(o.locks, jobID)
	o.mu.Unlock()
	o.releaseRunFiles(jobID)
}

// --- 阶段执行 ---

// runRecognize recognizing 阶段：调现有识题用例（公开入口），成功写检查点进
// awaiting_confirmation；失败按 retryable 语义落 failed_*（规则 4）并原样返回下游错误。
func (o *GradingOrchestrator) runRecognize(ctx context.Context, run *gradingRun, jobID string) (GradingJobView, error) {
	questions, err := o.deps.RecognizeHomework(ctx, run.req.Image)
	if err == nil && len(questions) == 0 {
		// 与 GradeHomeworkPhoto 同口径：空清单是输入问题，不可重试。
		err = fmt.Errorf("%w: 未识别到可处理的题目", ErrInvalidInput)
	}
	if err != nil {
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
	run.questions = questions
	// 先固化产物再写检查点（§6.15：检查点存在即产物可回放，崩溃窗口不产生"有检查点无产物"）。
	if perr := o.persistRun(jobID, run); perr != nil {
		return o.failStage(ctx, run, jobID, "persist_failed", perr)
	}
	return o.advanceOK(ctx, run, jobID, fmt.Sprintf("questions:%d", len(questions)))
}

// runAnchor locating 并行分支：锚点回位 located / degraded（规则 1）。失败或能力缺席
// 显式 degraded（§4.9 无坐标文字降级），绝不阻塞确认与批改。
func (o *GradingOrchestrator) runAnchor(ctx context.Context, run *gradingRun, jobID string) (GradingJobView, error) {
	state, digest := k12.GradingAnchorDegraded, "anchor:absent"
	if o.deps.AnswerAnchorer != nil && hasAnswerCandidate(run.questions) {
		anchored, err := o.deps.AnchorHomeworkAnswers(ctx, run.req.Image, run.questions)
		if err != nil {
			run.anchorFailed = true
			digest = "anchor:failed"
		} else {
			run.anchored = anchored
			state, digest = k12.GradingAnchorLocated, "anchor:located"
		}
	}
	if perr := o.persistRun(jobID, run); perr != nil {
		// 锚点回位发生在 awaiting_confirmation/locating：无失败转移边，保持 anchor pending
		// 原样返回错误——下次推进重跑锚点分支（幂等）。
		return GradingJobView{}, fmt.Errorf("usecase: 固化锚点产物: %w", perr)
	}
	return o.deps.AdvanceGradingStage(ctx, run.agentName, jobID, AdvanceGradingInput{
		Outcome: GradingOutcomeAnchor, AnchorState: state, ArtifactDigest: digest,
	})
}

// runAssess assessing 阶段：复用识别/锚点检查点产物调 GradeHomeworkPhoto（公开入口）——
// 分流、证据门禁、逐题批改、错题入库、批注渲染、Markdown 汇总全走现网实现。
// 渲染结果由 recordingAnnotator 捕获，rendering 阶段据此推进/降级。
func (o *GradingOrchestrator) runAssess(ctx context.Context, run *gradingRun, jobID string) (GradingJobView, error) {
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
	if o.deps.PhotoAnnotator != nil {
		recorder = &recordingAnnotator{inner: o.deps.PhotoAnnotator}
		assessDeps.PhotoAnnotator = recorder
	}

	result, err := assessDeps.GradeHomeworkPhoto(ctx, run.req)
	if err != nil {
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
		return o.failStage(ctx, run, jobID, "persist_failed", perr)
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

func shortSHA1(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:8])
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
