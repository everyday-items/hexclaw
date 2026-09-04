package usecase

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

var (
	ErrModelInvocationRequiresReconciliation = errors.New("model invocation requires reconciliation")
	ErrModelRequestPolicyInvalid             = errors.New("model request policy invalid")
	ErrGradingOrchestratorShutdown           = errors.New("grading orchestrator is shut down")
	errGradingBudgetMissing                  = errors.New("grading budget missing")
	errGradingBudgetPolicyInvalid            = errors.New("grading budget policy invalid")
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
	// 它既校验调用方显式选择，也为未显式选择的入口解析默认能力路由。
	snapshotFn GradingModelSnapshotResolver

	// runDir 阶段产物落盘目录（§6.15 崩溃恢复载体；空 = 仅内存，见 *_runtime.go）。
	runDir string
	// baseCtx 异步推进基座 context（与 HTTP 请求解耦，§6.15 任务执行模型）。
	baseCtx context.Context
	// runCancel 取消全部进程持有的 grading/anchor/recovery 工作；只由 Shutdown 调用。
	runCancel context.CancelFunc
	// sem 异步推进有界并发信号量。
	sem chan struct{}
	// anchorTimeout 锚点增强分支的独立预算（默认 60s，可通过 option 配置）。
	anchorTimeout time.Duration

	mu              sync.Mutex
	runs            map[string]*gradingRun
	active          map[string]bool                          // 在途异步推进守卫（同 Job 不并发双跑）
	rerun           map[string]bool                          // active 期间收到续跑信号时，退出前至少再检查一次状态机
	anchorActive    map[string]bool                          // 独立锚点分支守卫（外部调用不占 Job 锁）
	anchorDone      map[string]chan struct{}                 // 同步入口只等待分支完成，不让模型调用占用 Job 锁
	locks           map[string]*sync.Mutex                   // 每 Job 执行互斥：状态合并/写回与确认/重试/读产物串行化
	modelCancels    map[string]map[uint64]context.CancelFunc // 每 Job 在途模型调用；取消命令向 provider 传播
	nextModelCallID uint64
	sealed          bool          // Shutdown 后拒绝新的异步推进/锚点/恢复工作
	workerCount     int           // mu 下维护，覆盖 grading + anchor + recovery
	workerIdle      chan struct{} // workerCount 归零时关闭；新一轮 0→1 时替换
	// pageAssetLocks serializes the file+V19 compensated command by owner and
	// image content digest. Distinct source-scoped submissions can still share one
	// page asset, so two same-photo Jobs must not race one failure cleanup against
	// another successful reference.
	pageAssetLocks map[string]*pageAssetLockEntry
}

type pageAssetLockEntry struct {
	mutex sync.Mutex
	refs  int
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

// withProblemSourceActionJobFence linearizes a source-changing command with
// the canonical grading runner and the source reprocess worker. All three use
// the same per-Job mutex, so a command cannot commit a new immutable V72 head
// after runAssessItems passed its pending-source gate but before/during a
// provider operation in this process.
func (o *GradingOrchestrator) withProblemSourceActionJobFence(
	jobID string,
	command func() error,
) error {
	l := o.jobLock(strings.TrimSpace(jobID))
	l.Lock()
	defer l.Unlock()
	return command()
}

func (o *GradingOrchestrator) acquirePageAssetLock(agentName, imageDigest string) func() {
	key := agentName + "\x00" + imageDigest
	o.mu.Lock()
	if o.pageAssetLocks == nil {
		o.pageAssetLocks = map[string]*pageAssetLockEntry{}
	}
	entry, ok := o.pageAssetLocks[key]
	if !ok {
		entry = &pageAssetLockEntry{}
		o.pageAssetLocks[key] = entry
	}
	entry.refs++
	o.mu.Unlock()

	entry.mutex.Lock()
	return func() {
		entry.mutex.Unlock()
		o.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(o.pageAssetLocks, key)
		}
		o.mu.Unlock()
	}
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

// GradingModelSnapshotResolver validates an exact caller-selected route or,
// for an empty request, resolves the configured automatic route. It runs
// before persistence so an incapable model cannot create a doomed Job.
type GradingModelSnapshotResolver func(
	requested k12.GradingModelSnapshot,
) (k12.GradingModelSnapshot, error)

// NewGradingOrchestrator 组装编排器。snapshotFn 可为 nil（调用方须在 Start 输入里给快照）。
// opts 见 *_runtime.go（落盘恢复目录 / 异步基座 context / 有界并发）。
func NewGradingOrchestrator(deps Deps, snapshotFn GradingModelSnapshotResolver, opts ...GradingOrchestratorOption) *GradingOrchestrator {
	o := &GradingOrchestrator{
		deps: deps, snapshotFn: snapshotFn,
		runs: map[string]*gradingRun{}, active: map[string]bool{}, rerun: map[string]bool{},
		anchorActive: map[string]bool{}, anchorDone: map[string]chan struct{}{}, locks: map[string]*sync.Mutex{},
		modelCancels:   map[string]map[uint64]context.CancelFunc{},
		pageAssetLocks: map[string]*pageAssetLockEntry{},
		sem:            make(chan struct{}, 2), baseCtx: context.Background(),
		anchorTimeout: time.Duration(k12.GradingAnchorTimeoutSeconds) * time.Second,
	}
	for _, opt := range opts {
		opt(o)
	}
	if o.baseCtx == nil {
		o.baseCtx = context.Background()
	}
	o.baseCtx, o.runCancel = context.WithCancel(o.baseCtx)
	o.workerIdle = make(chan struct{})
	close(o.workerIdle)
	return o
}

// StartPhotoGradingInput 照片批改 Job 创建输入。SourceKind/SourceKey 映射统一幂等键
// （§4.10：IM = "im"/message_id）。ModelSnapshot 为空时取 snapshotFn()。
type StartPhotoGradingInput struct {
	Photo                     PhotoGradeRequest
	SourceKind                string
	SourceKey                 string
	ModelSnapshot             k12.GradingModelSnapshot
	BudgetSnapshot            k12.GradingBudgetSnapshot
	ParentAutomaticAttemptID  string
	ParentAutomaticDeadlineAt int64
}

// StartPhotoGradingJob 创建照片批改 Job 并登记进程内运行时。幂等：同键命中既有 Job
// （created=false）且不覆盖已登记的运行时状态。
func (o *GradingOrchestrator) StartPhotoGradingJob(ctx context.Context, in StartPhotoGradingInput) (GradingJobView, bool, error) {
	if len(in.Photo.Image) == 0 {
		return GradingJobView{}, false, fmt.Errorf("%w: Image 不可空", ErrInvalidInput)
	}
	if err := validateGradingSourceIdentity(in.SourceKind, in.SourceKey); err != nil {
		return GradingJobView{}, false, err
	}
	if strings.TrimSpace(in.SourceKind) == "image_task" {
		if in.BudgetSnapshot.PolicyVersion == 0 {
			return GradingJobView{}, false, fmt.Errorf(
				"%w: %w: public image_task requires a frozen grading budget policy",
				ErrInvalidInput,
				errGradingBudgetMissing,
			)
		}
		if err := in.BudgetSnapshot.Validate(); err != nil {
			return GradingJobView{}, false, fmt.Errorf(
				"%w: %w: invalid public image_task grading budget: %v",
				ErrInvalidInput,
				errGradingBudgetPolicyInvalid,
				err,
			)
		}
		if strings.TrimSpace(in.ParentAutomaticAttemptID) == "" ||
			in.ParentAutomaticDeadlineAt <= o.deps.now() {
			return GradingJobView{}, false, fmt.Errorf(
				"%w: public image_task requires a live parent automatic attempt",
				ErrInvalidInput,
			)
		}
	}
	submissionID := scopedPhotoSubmissionID(
		in.Photo.Image,
		in.SourceKind,
		in.SourceKey,
	)
	v, found, err := o.deps.findGradingJobByIdempotency(
		ctx, in.Photo.AgentName, in.SourceKind, in.SourceKey, 0,
	)
	if err != nil {
		return GradingJobView{}, false, err
	}
	created := false
	if found {
		if policyErr := k12.ValidateGradingRecognizingRequestPolicy(
			v.Fields.ModelSnapshot,
		); policyErr != nil {
			return GradingJobView{}, false, fmt.Errorf(
				"%w: %w: stored grading model request policy is invalid: %v",
				ErrInvalidInput,
				ErrModelRequestPolicyInvalid,
				policyErr,
			)
		}
		if !photoSubmissionMatchesRequest(
			v.Fields.SubmissionID,
			in.Photo.Image,
			in.SourceKind,
			in.SourceKey,
		) {
			return GradingJobView{}, false, fmt.Errorf(
				"%w: idempotency key %q is already bound to submission %q, requested %q",
				ErrInvalidInput, v.Fields.IdempotencyKey, v.Fields.SubmissionID, submissionID,
			)
		}
		providerAuto := strings.TrimSpace(in.ModelSnapshot.Provider) == "" ||
			strings.EqualFold(strings.TrimSpace(in.ModelSnapshot.Provider), "auto")
		modelAuto := strings.TrimSpace(in.ModelSnapshot.Model) == "" ||
			strings.EqualFold(strings.TrimSpace(in.ModelSnapshot.Model), "auto")
		if !providerAuto || !modelAuto {
			storedRoute := k12.NormalizeGradingModelSnapshot(v.Fields.ModelSnapshot)
			requestedRoute := k12.NormalizeGradingModelSnapshot(in.ModelSnapshot)
			if storedRoute.Provider != requestedRoute.Provider ||
				storedRoute.Model != requestedRoute.Model ||
				storedRoute.Route != requestedRoute.Route {
				return GradingJobView{}, false, fmt.Errorf(
					"%w: idempotency key %q is already bound to model route %q, requested %q",
					ErrInvalidInput, v.Fields.IdempotencyKey, storedRoute.Route, requestedRoute.Route,
				)
			}
		}
		if strings.TrimSpace(in.ParentAutomaticAttemptID) != "" &&
			v.Fields.ParentAutomaticAttemptID != strings.TrimSpace(in.ParentAutomaticAttemptID) {
			return GradingJobView{}, false, fmt.Errorf(
				"%w: idempotency key %q is already bound to parent automatic attempt %q, requested %q",
				ErrInvalidInput, v.Fields.IdempotencyKey,
				v.Fields.ParentAutomaticAttemptID, strings.TrimSpace(in.ParentAutomaticAttemptID),
			)
		}
		if strings.TrimSpace(in.ParentAutomaticAttemptID) != "" &&
			v.Fields.ParentAutomaticDeadlineAt != in.ParentAutomaticDeadlineAt {
			return GradingJobView{}, false, fmt.Errorf(
				"%w: idempotency key %q is already bound to parent automatic deadline %d, requested %d",
				ErrInvalidInput, v.Fields.IdempotencyKey,
				v.Fields.ParentAutomaticDeadlineAt, in.ParentAutomaticDeadlineAt,
			)
		}
	} else {
		var trustedRecognitionPolicy *k12.GradingBudgetSnapshot
		if o.deps.GradingBudgetSnapshot.IsFrozen() {
			var taskIntent PhotoTaskIntent
			if strings.TrimSpace(in.SourceKind) == "image_task" {
				taskIntent = in.Photo.TaskIntent
			}
			selected, selectErr := trustedPhotoRecognitionCreationPolicy(
				o.deps.GradingBudgetSnapshot,
				in.Photo.Image,
				taskIntent,
			)
			if selectErr != nil {
				return GradingJobView{}, false, fmt.Errorf(
					"%w: select trusted photo recognition policy: %v",
					ErrInvalidInput,
					selectErr,
				)
			}
			trustedRecognitionPolicy = &selected
		}
		snap := in.ModelSnapshot
		if o.snapshotFn != nil {
			snap, err = o.snapshotFn(snap)
			if err != nil {
				return GradingJobView{}, false, fmt.Errorf("%w: resolve grading model snapshot: %w", ErrInvalidInput, err)
			}
		} else if strings.TrimSpace(snap.Provider) == "" || strings.TrimSpace(snap.Model) == "" {
			return GradingJobView{}, false, fmt.Errorf("%w: grading model snapshot resolver 未配置且 provider/model 不完整", ErrInvalidInput)
		}
		snap = k12.NormalizeGradingModelSnapshot(snap)
		if snap.Provider == "" || snap.Model == "" {
			return GradingJobView{}, false, fmt.Errorf("%w: grading model snapshot 缺少 provider/model", ErrInvalidInput)
		}
		if policyErr := k12.ValidateGradingRecognizingRequestPolicy(snap); policyErr != nil {
			return GradingJobView{}, false, fmt.Errorf(
				"%w: %w: invalid recognizing request policy: %v",
				ErrInvalidInput,
				ErrModelRequestPolicyInvalid,
				policyErr,
			)
		}
		v, created, err = o.deps.CreateGradingJob(ctx, in.Photo.AgentName, in.Photo.SourceSession, CreateGradingJobInput{
			// Submission 聚合未落库（§6.9 类型化存储待接线）：新任务以图片摘要和
			// canonical source command 的组合摘要为提交标识。同一来源命令重放稳定
			// 命中；不同来源即使图片字节相同也必须隔离 Problem/Attempt 事实域。
			SubmissionID:                submissionID,
			SourceKind:                  in.SourceKind,
			SourceKey:                   in.SourceKey,
			ModelSnapshot:               snap,
			ParentAutomaticAttemptID:    in.ParentAutomaticAttemptID,
			ParentAutomaticDeadlineAt:   in.ParentAutomaticDeadlineAt,
			BudgetSnapshot:              in.BudgetSnapshot,
			MaterializesProblemAttempts: true,
			trustedRecognitionPolicy:    trustedRecognitionPolicy,
		})
		if err != nil {
			return GradingJobView{}, false, err
		}
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

func trustedPhotoRecognitionCreationPolicy(
	trusted k12.GradingBudgetSnapshot,
	image []byte,
	taskIntent PhotoTaskIntent,
) (k12.GradingBudgetSnapshot, error) {
	if err := trusted.Validate(); err != nil {
		return k12.GradingBudgetSnapshot{}, err
	}
	switch trusted.RecognitionPlanVersion {
	case k12.RecognitionPlanVersionV1:
		return trusted, nil
	case k12.RecognitionPlanVersionV2:
		if taskIntent != PhotoTaskBlankWorksheet &&
			k12.ClassifyRecognitionPage(image) == k12.RecognitionPageDense {
			return trusted, nil
		}
		selected := trusted
		selected.RecognitionPlanVersion = k12.RecognitionPlanVersionV1
		selected.StageSeconds.Recognizing =
			(selected.PhysicalCallCapMillis + 999) / 1000
		selected.RecognizingBuckets = k12.RecognitionLayoutBudgetBucketsV2{}
		selected.PhysicalCallCapMillis = 0
		selected.WorkerHardCap = 0
		selected.EffectiveConcurrency = 0
		if err := selected.Validate(); err != nil {
			return k12.GradingBudgetSnapshot{}, err
		}
		return selected, nil
	default:
		return k12.GradingBudgetSnapshot{}, fmt.Errorf(
			"unknown recognition plan version %d",
			trusted.RecognitionPlanVersion,
		)
	}
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
		if parentAutomaticDeadlineExceeded(v.Fields, o.deps.now()) &&
			gradingStageMayStartPhysicalWork(v.Record.Status) {
			handled, expired, expiryErr := o.expireParentAutomaticStageBeforeSend(ctx, run, v)
			if handled {
				return expired, expiryErr
			}
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
			// 规则 1：识别冻结后，确认与锚点是两个独立分支。锚点模型调用在 Job 锁外
			// 异步执行，主链立即返回确认停点；二者任意顺序到达，均由状态机显式汇合。
			if v.Fields.AnchorState == k12.GradingAnchorPending {
				o.startAnchorAsync(jobID, run, v.Fields.ModelSnapshot)
			}
			// ImageTask facade 对清晰、证据充足的事实直接自动冻结；只有稳定风险原因
			// 存在时才停下来等家长确认。旧 GradingJob 入口保持原显式确认语义。
			if automaticPhotoConfirmationSource(v.Fields.SourceKind) &&
				v.Fields.ConfirmationState == k12.GradingConfirmationPending &&
				!recognizedQuestionsRequireGuardianConfirmation(run.questions, run.req.TaskIntent) {
				if v, err = o.autoFreezeClearRecognition(ctx, run, v); err != nil {
					return v, err
				}
				if v.Record.Status == k12.GradingStageAssessing {
					continue
				}
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

func recognizedQuestionsRequireGuardianConfirmation(
	questions []RecognizedQuestion,
	taskIntent PhotoTaskIntent,
) bool {
	for _, question := range questions {
		if recognizedQuestionRequiresGuardianConfirmation(question, taskIntent) {
			return true
		}
	}
	return false
}

func recognizedQuestionRequiresGuardianConfirmation(
	question RecognizedQuestion,
	taskIntent PhotoTaskIntent,
) bool {
	question = NormalizeRecognizedQuestion(question)
	if !question.ConfirmationRequired {
		return false
	}
	if taskIntent != PhotoTaskBlankWorksheet ||
		question.AnswerState != AnswerStateUnclear ||
		photoHasExplicitUnclearAnswerEvidence([]RecognizedQuestion{question}) {
		return true
	}
	// 空白卷没有独立答案证据时，纯擦痕和字迹信号不构成已作答事实；
	// 题干低置信仍需确认，避免把误识运算符直接交给确定性求解器。
	for _, reason := range question.ConfirmationReasons {
		switch reason {
		case OCRRiskErasure, OCRRiskUnclearHandwriting:
		default:
			return true
		}
	}
	return false
}

func automaticPhotoConfirmationSource(sourceKind string) bool {
	return sourceKind == "image_task" || sourceKind == PracticeReturnGradingSourceKind
}

// autoFreezeClearRecognition runs with the Job lock held. It follows the same
// persist-before-checkpoint order as an explicit confirmation, but supplies no
// parent correction because every recognized fact passed the risk policy.
func (o *GradingOrchestrator) autoFreezeClearRecognition(
	ctx context.Context,
	run *gradingRun,
	job GradingJobView,
) (GradingJobView, error) {
	candidate := *run
	candidate.questions = cloneRecognizedQuestions(run.questions)
	candidate.anchored = cloneRecognizedQuestions(run.anchored)
	if err := applyAndValidateGradingConfirmation(&candidate, ConfirmPhotoGradingInput{}); err != nil {
		return GradingJobView{}, err
	}
	confirmedFacts := candidate.questions
	if candidate.anchored != nil {
		confirmedFacts = candidate.anchored
	}
	if err := o.persistProblemAttemptFacts(
		ctx, run.agentName, job.Fields.SubmissionID, confirmedFacts,
	); err != nil {
		return GradingJobView{}, fmt.Errorf("usecase: 自动冻结 Problem/Attempt: %w", err)
	}
	if err := o.persistRun(job.Record.RecordID, &candidate); err != nil {
		return GradingJobView{}, fmt.Errorf("usecase: 自动冻结识别产物: %w", err)
	}
	canonicalDigest := CanonicalRecognizedQuestionsDigest(candidate.questions)
	view, err := o.deps.ConfirmGradingJob(
		ctx,
		run.agentName,
		job.Record.RecordID,
		[]string{"canonical-recognition:" + canonicalDigest},
	)
	if err != nil {
		return view, err
	}
	run.questions = candidate.questions
	run.anchored = candidate.anchored
	return view, nil
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
	job, err := o.deps.GetGradingJob(ctx, run.agentName, jobID)
	if err != nil {
		l.Unlock()
		return GradingJobView{}, err
	}
	awaitingItemSource := false
	if job.Fields.BudgetSnapshot.IsFrozen() {
		awaitingItemSource, err = applyProgressiveGradingConfirmation(
			&candidate,
			ConfirmPhotoGradingInput{},
		)
	} else {
		err = applyAndValidateGradingConfirmation(&candidate, ConfirmPhotoGradingInput{})
	}
	if err != nil {
		l.Unlock()
		return GradingJobView{}, err
	}
	confirmedFacts := candidate.questions
	if candidate.anchored != nil {
		confirmedFacts = candidate.anchored
	}
	if err := o.persistProblemAttemptFacts(ctx, run.agentName, job.Fields.SubmissionID, confirmedFacts); err != nil {
		l.Unlock()
		return GradingJobView{}, fmt.Errorf("usecase: 固化确认后的 Problem/Attempt: %w", err)
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
	if awaitingItemSource {
		for _, q := range candidate.questions {
			q = NormalizeRecognizedQuestion(q)
			if recognizedQuestionRequiresGuardianConfirmation(q, candidate.req.TaskIntent) {
				continue
			}
			if _, itemErr := o.assessDurablePhotoItem(
				ctx, o.deps, job, run.req, PhotoModeGrade, q,
			); itemErr != nil {
				l.Unlock()
				return GradingJobView{}, itemErr
			}
		}
		current, getErr := o.deps.GetGradingJob(ctx, run.agentName, jobID)
		l.Unlock()
		if getErr != nil {
			return GradingJobView{}, getErr
		}
		return current, nil
	}
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
	_ = os.Remove(o.recognitionReceiptPath(jobID))
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
	policy := k12.NormalizeModelRequestPolicySnapshot(
		job.Fields.ModelSnapshot.RecognizingRequestPolicy,
	)
	if policyErr := k12.ValidateModelInvocationRequestPolicy(
		k12.GradingStageRecognizing,
		job.Fields.ModelSnapshot,
		policy,
	); policyErr != nil {
		return o.failModelInvocationBeforeSend(
			ctx,
			run,
			jobID,
			fmt.Errorf("%w: %v", ErrModelRequestPolicyInvalid, policyErr),
		)
	}
	recognitionPlanVersion, planVersionErr :=
		frozenRecognitionPlanVersion(job.Fields.BudgetSnapshot)
	if planVersionErr != nil {
		return o.failModelInvocationBeforeSend(
			ctx,
			run,
			jobID,
			planVersionErr,
		)
	}
	if recognitionPlanVersion == k12.RecognitionPlanVersionV2 && policy.IsZero() {
		return o.failModelInvocationBeforeSend(
			ctx,
			run,
			jobID,
			fmt.Errorf(
				"%w: recognition plan v2 requires the frozen recognizing request policy",
				ErrModelRequestPolicyInvalid,
			),
		)
	}
	var invocation k12.ModelInvocation
	if policy.IsZero() {
		invocation, err = o.beginModelInvocationWithPolicy(
			ctx,
			job,
			k12.GradingStageRecognizing,
			recognizingInvocationDigest(
				run.req.Image,
				job.Fields.ModelSnapshot,
				policy,
			),
			policy,
		)
	} else {
		invocation, err = o.beginRecognizingModelInvocationWithPolicy(
			ctx,
			job,
			run.req.Image,
			recognizingInvocationDigest(
				run.req.Image,
				job.Fields.ModelSnapshot,
				policy,
			),
			policy,
		)
	}
	if err != nil {
		if invocation.InvocationID == "" {
			return o.failModelInvocationBeforeSend(ctx, run, jobID, err)
		}
		if errors.Is(err, ErrRecognitionPhysicalCallObservedInFlight) {
			current, readErr := o.deps.GetGradingJob(
				context.WithoutCancel(ctx),
				run.agentName,
				jobID,
			)
			if readErr != nil {
				return current, errors.Join(err, readErr)
			}
			return current, err
		}
		if handled, settled, settleErr :=
			o.settleConclusiveRecognitionRecovery(
				ctx,
				run,
				job,
				invocation,
			); handled {
			return settled, errors.Join(err, settleErr)
		} else if settleErr != nil {
			err = errors.Join(err, settleErr)
		}
		resumePrepared, resumeErr :=
			o.preparedWholePageRecognitionCanResume(
				context.WithoutCancel(ctx),
				invocation,
				run.req.Image,
			)
		if resumeErr != nil {
			err = errors.Join(err, resumeErr)
		}
		if !resumePrepared {
			if recovered, v, recoverErr := o.recoverRecognizeInvocation(
				ctx,
				run,
				jobID,
				invocation,
			); recovered {
				return v, recoverErr
			}
			if invocation.Status == k12.ModelInvocationSent {
				invocation, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(
					context.WithoutCancel(ctx),
					run.agentName,
					invocation.InvocationID,
					"recovered_after_send",
				)
			}
			unknown, advanceErr := o.markGradingOutcomeUnknown(
				ctx,
				run,
				jobID,
				"invocation_reconciliation_required",
			)
			if advanceErr != nil {
				return unknown, advanceErr
			}
			return unknown, err
		}
	}
	// An exact prepared whole-page child is restart-safe only while the frozen
	// stage deadline is still live. The atomic initial publication path can
	// replay that parent+child pair without returning an error, so enforce the
	// same conclusive recovery gate before entering the Provider on both the
	// error and successful replay paths.
	if handled, settled, settleErr :=
		o.settleConclusiveRecognitionRecovery(
			ctx,
			run,
			job,
			invocation,
		); handled {
		return settled, settleErr
	} else if settleErr != nil {
		return GradingJobView{}, settleErr
	}
	// Fields.Deadline is the durable budget for the current automatic stage.
	// The provider call must use that persisted cutoff rather than a shorter
	// transient request context; durableGradingStageContext still binds the
	// returned cancel to the process lifecycle and public Job cancellation.
	providerCtx, cancelProvider := o.durableGradingStageContext(ctx, job.Fields.Deadline)
	if !invocation.RequestPolicySnapshot.IsZero() {
		providerCtx = k12.WithGradingModelRequestPolicy(
			providerCtx,
			invocation.RequestPolicySnapshot,
		)
	}
	if recognitionPlanVersion == k12.RecognitionPlanVersionV2 {
		runtime, runtimeErr := o.loadInitialRecognitionLayoutRuntimeV2(
			context.WithoutCancel(ctx),
			job,
			invocation,
			run.req.Image,
		)
		if runtimeErr != nil {
			cancelProvider()
			return GradingJobView{}, runtimeErr
		}
		providerCtx = k12.WithRecognitionLayoutPlanV2(
			providerCtx,
			runtime.HeaderDigest,
		)
		if runtime.Status == "succeeded" {
			// 最终化是持久化精确集合的提交。如果进程在将其投影到父 Job 前退出，
			// 则通过显式的只读重放标记进入适配器。适配器必须在任何图像拆分或
			// Provider 边界之前解码已有回执。
			providerCtx = k12.WithRecognitionLayoutFinalizationReplayV2(
				providerCtx,
			)
		}
	}
	physicalExecutor := newDurableRecognitionPhysicalCallExecutor(
		o,
		invocation,
	)
	providerCtx = k12.WithRecognitionPhysicalCallExecutor(
		providerCtx,
		physicalExecutor,
	)
	unregisterProvider := o.registerGradingModelCall(jobID, cancelProvider)
	if current, readErr := o.deps.GetGradingJob(context.WithoutCancel(ctx), run.agentName, jobID); readErr != nil {
		cancelProvider()
		unregisterProvider()
		return GradingJobView{}, readErr
	} else if current.Record.Status == k12.GradingStageCancelled {
		cancelProvider()
		unregisterProvider()
		_, _ = o.deps.Records.MarkModelInvocationFailed(context.WithoutCancel(ctx), run.agentName,
			invocation.InvocationID, "cancelled_before_provider_call")
		return current, nil
	}
	questions, err := o.deps.RecognizeHomework(providerCtx, run.req.Image)
	cancelProvider()
	unregisterProvider()
	if err != nil {
		if errors.Is(err, ErrRecognitionPhysicalCallObservedInFlight) {
			// Another worker owns the exact same durable child and may already
			// be inside the Provider request. This worker is a passive observer:
			// it must not project its local no-send result onto the shared
			// parent invocation or Job.
			current, readErr := o.deps.GetGradingJob(
				context.WithoutCancel(ctx),
				run.agentName,
				jobID,
			)
			if readErr != nil {
				return current, errors.Join(err, readErr)
			}
			return current, err
		}
		beforePhysicalSend := errors.Is(
			err,
			ErrRecognitionPhysicalCallBeforeSend,
		)
		if !invocation.RequestPolicySnapshot.IsZero() &&
			physicalExecutor.localCallEntries.Load() == 0 {
			definiteNoSend, observedOtherWorker, inspectErr :=
				o.settleRecognitionFailureBeforeLocalPhysicalCall(
					context.WithoutCancel(ctx),
					invocation,
					run.req.Image,
				)
			if inspectErr != nil {
				current, readErr := o.deps.GetGradingJob(
					context.WithoutCancel(ctx),
					run.agentName,
					jobID,
				)
				return current, errors.Join(err, inspectErr, readErr)
			}
			if observedOtherWorker {
				current, readErr := o.deps.GetGradingJob(
					context.WithoutCancel(ctx),
					run.agentName,
					jobID,
				)
				return current, errors.Join(
					err,
					ErrRecognitionPhysicalCallObservedInFlight,
					readErr,
				)
			}
			beforePhysicalSend = definiteNoSend
		}
		if !beforePhysicalSend &&
			!invocation.RequestPolicySnapshot.IsZero() {
			physicalStarted, inspectErr :=
				o.recognitionPhysicalCallStarted(
					context.WithoutCancel(ctx),
					invocation,
				)
			if inspectErr == nil && !physicalStarted {
				// Under the approved DD-036 policy every provider request must
				// cross a durable child prepared→sent boundary first. Therefore
				// an exact zero-child set proves this failure happened before
				// any request could have reached the provider.
				beforePhysicalSend = true
			}
		}
		protocolInvalid := errors.Is(
			err,
			k12.ErrRecognitionProtocolInvalid,
		)
		if !beforePhysicalSend &&
			!protocolInvalid &&
			sentProviderOutcomeUnknown(err, nil) {
			_, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(context.WithoutCancel(ctx), run.agentName, invocation.InvocationID, "provider_outcome_unknown")
			if current, readErr := o.deps.GetGradingJob(context.WithoutCancel(ctx), run.agentName, jobID); readErr == nil && current.Record.Status == k12.GradingStageCancelled {
				return current, nil
			}
			v, aerr := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, jobID, "provider_outcome_unknown")
			if aerr != nil {
				return v, aerr
			}
			return v, err
		}
		failureKind := "recognize_failed"
		if beforePhysicalSend {
			failureKind = "physical_invocation_prepare_failed"
		}
		if _, ledgerErr := o.deps.Records.MarkModelInvocationFailed(
			context.WithoutCancel(ctx),
			run.agentName,
			invocation.InvocationID,
			failureKind,
		); ledgerErr != nil {
			v, aerr := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, jobID, "invocation_ledger_write_failed")
			if aerr != nil {
				return v, aerr
			}
			return v, ledgerErr
		}
		// A public Job cancellation owns the Job terminal state even when the
		// queued physical child has only just converged to a definite no-send
		// failure. Do not race that persisted cancellation with a failed-stage
		// transition; retain the settled parent/child facts and return the
		// canonical cancellation to the synchronous caller.
		if current, readErr := o.deps.GetGradingJob(
			context.WithoutCancel(ctx),
			run.agentName,
			jobID,
		); readErr != nil {
			return current, errors.Join(err, readErr)
		} else if current.Record != nil && current.Record.Status == k12.GradingStageCancelled {
			return current, context.Canceled
		}
		v, aerr := o.deps.AdvanceGradingStage(
			context.WithoutCancel(ctx),
			run.agentName,
			jobID,
			AdvanceGradingInput{
				Outcome:     GradingOutcomeFailed,
				FailureKind: failureKind,
				Retryable:   gradingErrRetryable(err),
			},
		)
		if aerr != nil {
			return v, aerr
		}
		return v, err
	}
	_, physicalErr := o.recognitionPhysicalSuccessSet(
		context.WithoutCancel(ctx),
		invocation,
		run.req.Image,
	)
	if physicalErr != nil {
		_, ledgerErr := o.deps.Records.MarkModelInvocationOutcomeUnknown(
			context.WithoutCancel(ctx),
			run.agentName,
			invocation.InvocationID,
			"physical_receipt_invalid",
		)
		v, advanceErr := o.markGradingOutcomeUnknown(
			context.WithoutCancel(ctx),
			run,
			jobID,
			"physical_receipt_invalid",
		)
		return v, errors.Join(physicalErr, ledgerErr, advanceErr)
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
	if automaticPhotoConfirmationSource(job.Fields.SourceKind) {
		for i := range run.questions {
			// The current facade requires confidence evidence before automatic
			// freezing. Keep nil compatible for legacy GradingJob callers, but
			// fail closed for every new public image task.
			if run.questions[i].RecognitionConfidence == nil {
				run.questions[i].ConfirmationReasons = append(
					run.questions[i].ConfirmationReasons, OCRRiskLowConfidence,
				)
				run.questions[i] = NormalizeRecognizedQuestion(run.questions[i])
			}
		}
	}
	for i := range run.questions {
		// 核心识别冻结事实不接纳 geometry；BBox 只能由独立 anchor 分支补入。
		run.questions[i].BBox = nil
	}
	// Provider 已返回后先写 Job+Invocation-scoped 不可变结果回执，再把同一事实投影到按
	// SubmissionID 共享的 Problem/Attempt ledger。后者可能来自另一条同图 Job，
	// 不能单独证明当前 invocation 成功；回执是 outcome_unknown 自动对账的 provenance。
	if receiptErr := o.persistRecognitionReceipt(jobID, invocation.InvocationID, run); receiptErr != nil {
		_, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(
			context.WithoutCancel(ctx), run.agentName, invocation.InvocationID,
			"recognition_receipt_not_durable",
		)
		v, aerr := o.markGradingOutcomeUnknown(
			context.WithoutCancel(ctx), run, jobID, "recognition_receipt_not_durable",
		)
		if aerr != nil {
			return v, aerr
		}
		return v, receiptErr
	}
	if perr := o.persistRecognizedPhotoFacts(ctx, run, job.Fields.SubmissionID); perr != nil {
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

func gradingStageContext(parent context.Context, deadline int64) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if deadline <= 0 {
		return context.WithCancel(parent)
	}
	// A GradingJob deadline is a durable stage fact. The caller can be a short
	// lived HTTP/IM request or result poll; retain its values but do not let that
	// transient cancellation/deadline shorten an already persisted stage window.
	return context.WithDeadline(context.WithoutCancel(parent), time.Unix(deadline, 0))
}

// durableGradingStageContext restores the orchestrator lifecycle cancellation
// that gradingStageContext intentionally detaches from transient callers. The
// same returned cancel is registered for explicit public Job cancellation.
func (o *GradingOrchestrator) durableGradingStageContext(parent context.Context, deadline int64) (context.Context, context.CancelFunc) {
	stageCtx, cancelStage := gradingStageContext(parent, deadline)
	if deadline <= 0 {
		return stageCtx, cancelStage
	}
	stopLifecycleCancel := context.AfterFunc(o.gradingBaseContext(), cancelStage)
	var once sync.Once
	return stageCtx, func() {
		once.Do(func() {
			stopLifecycleCancel()
			cancelStage()
		})
	}
}

func gradingStageMayStartPhysicalWork(stage string) bool {
	switch stage {
	case k12.GradingStageQueued, k12.GradingStageNormalizing,
		k12.GradingStageRecognizing, k12.GradingStageLocating,
		k12.GradingStageAssessing:
		return true
	default:
		return false
	}
}

func (o *GradingOrchestrator) expireParentAutomaticStageBeforeSend(
	ctx context.Context,
	run *gradingRun,
	job GradingJobView,
) (bool, GradingJobView, error) {
	invocations, err := o.deps.Records.ListModelInvocations(
		context.WithoutCancel(ctx), run.agentName, job.Record.RecordID,
	)
	if err != nil {
		return true, GradingJobView{}, err
	}
	for _, invocation := range invocations {
		if invocation.Stage != job.Record.Status {
			continue
		}
		switch invocation.Status {
		case k12.ModelInvocationSent, k12.ModelInvocationOutcomeUnknown,
			k12.ModelInvocationSucceeded, k12.ModelInvocationReconciled:
			return false, GradingJobView{}, nil
		case k12.ModelInvocationPrepared:
			if _, markErr := o.deps.Records.MarkModelInvocationFailed(
				context.WithoutCancel(ctx), run.agentName, invocation.InvocationID,
				gradingFailureInteractiveDeadlineExceeded,
			); markErr != nil {
				current, readErr := o.deps.Records.GetModelInvocation(
					context.WithoutCancel(ctx), run.agentName, invocation.InvocationID,
				)
				if readErr == nil {
					switch current.Status {
					case k12.ModelInvocationSent, k12.ModelInvocationOutcomeUnknown,
						k12.ModelInvocationSucceeded, k12.ModelInvocationReconciled:
						return false, GradingJobView{}, nil
					}
				}
				return true, GradingJobView{}, markErr
			}
		}
	}
	expired, err := o.deps.AdvanceGradingStage(ctx, run.agentName, job.Record.RecordID,
		AdvanceGradingInput{
			Outcome:     GradingOutcomeFailed,
			FailureKind: gradingFailureInteractiveDeadlineExceeded,
			Retryable:   true,
		})
	if err != nil {
		current, readErr := o.deps.GetGradingJob(
			context.WithoutCancel(ctx), run.agentName, job.Record.RecordID,
		)
		if readErr == nil && k12.GradingStageTerminal(current.Record.Status) {
			return true, current, nil
		}
		return true, expired, err
	}
	return true, expired, fmt.Errorf(
		"%w: parent automatic attempt %s",
		context.DeadlineExceeded, job.Fields.ParentAutomaticAttemptID,
	)
}

// persistRecognizedPhotoFacts is the compensated local command that promotes a
// source image and its typed recognition together. The file store cannot join a
// SQLite transaction, so a newly-created blob is removed if the atomic V19 write
// fails. Same-owner/content calls are serialized even across distinct submission
// scopes to keep that cleanup race-free in the single-process execution model.
func (o *GradingOrchestrator) persistRecognizedPhotoFacts(
	ctx context.Context,
	run *gradingRun,
	submissionID string,
) error {
	if o.deps.PageAssets == nil {
		// Compatibility for embedded/test compositions and historical page-* facts.
		// Production assembly always injects PageAssets.
		return o.persistProblemAttemptFacts(ctx, run.agentName, submissionID, run.questions)
	}
	release := o.acquirePageAssetLock(run.agentName, photoImageDigest(run.req.Image))
	defer release()

	assetID := strings.TrimSpace(run.req.SourcePageAssetID)
	created := false
	if assetID == "" {
		var err error
		assetID, created, err = o.deps.PageAssets.Ensure(run.agentName, run.req.Image)
		if err != nil {
			return fmt.Errorf("usecase: 固化识题原图资产: %w", err)
		}
	}
	if strings.TrimSpace(assetID) == "" {
		if created {
			_, _ = o.deps.PageAssets.Remove(run.agentName, assetID)
		}
		return fmt.Errorf("usecase: 固化识题原图资产返回空 ID")
	}
	previousPageIDs := make([]string, len(run.questions))
	for i := range run.questions {
		previousPageIDs[i] = run.questions[i].PageAssetID
		run.questions[i].PageAssetID = assetID
	}
	if err := o.persistProblemAttemptFacts(ctx, run.agentName, submissionID, run.questions); err != nil {
		for i := range run.questions {
			run.questions[i].PageAssetID = previousPageIDs[i]
		}
		if created {
			_, removeErr := o.deps.PageAssets.Remove(run.agentName, assetID)
			if removeErr != nil {
				return errors.Join(err, fmt.Errorf("usecase: 补偿识题原图资产 %q: %w", assetID, removeErr))
			}
		}
		return err
	}
	return nil
}

// startAnchorAsync 启动 locating 独立分支。昂贵的模型调用不持 Job 锁，因此家长确认可
// 同时持久化；只有几何合并、run 落盘与 anchor 检查点写回在 Job 锁内串行化。
func (o *GradingOrchestrator) startAnchorAsync(jobID string, run *gradingRun, snapshot k12.GradingModelSnapshot) {
	o.mu.Lock()
	if o.sealed {
		o.mu.Unlock()
		return
	}
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
	if !o.beginWorkerLocked() {
		delete(o.anchorActive, jobID)
		delete(o.anchorDone, jobID)
		o.mu.Unlock()
		return
	}
	o.mu.Unlock()

	image := append([]byte(nil), run.req.Image...)
	frozen := cloneRecognizedQuestions(run.questions)
	taskIntent := run.req.TaskIntent
	go func() {
		defer o.finishWorker()
		startedAt := time.Now()
		terminalStatus := "failed"
		heartbeatCtx, stopHeartbeat := context.WithCancel(o.gradingBaseContext())
		heartbeatStopped := make(chan struct{})
		slog.Info("K12 GradingJob anchor started",
			"job_id", jobID,
			"agent_id", run.agentName,
			"stage", k12.GradingStageLocating,
			"task_intent", taskIntent,
			"questions", frozen,
			"questions_total", len(frozen),
			"image_bytes", len(image),
			"model", k12.NormalizeGradingModelSnapshot(snapshot).Model,
			"elapsed_ms", int64(0),
		)
		go func() {
			defer close(heartbeatStopped)
			defer func() {
				if recovered := recover(); recovered != nil {
					slog.Warn("K12 GradingJob anchor heartbeat stopped",
						"job_id", jobID,
						"agent_id", run.agentName,
						"stage", k12.GradingStageLocating,
						"panic_type", fmt.Sprintf("%T", recovered),
						"panic", recovered,
					)
				}
			}()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-heartbeatCtx.Done():
					return
				case <-ticker.C:
					slog.Info("K12 GradingJob anchor heartbeat",
						"job_id", jobID,
						"agent_id", run.agentName,
						"stage", k12.GradingStageLocating,
						"questions_total", len(frozen),
						"elapsed_ms", time.Since(startedAt).Milliseconds(),
					)
				}
			}
		}()
		defer func() {
			if r := recover(); r != nil {
				terminalStatus = "panicked"
				slog.Error("K12 批改任务锚点分支 panic（任务保留 anchor pending）",
					"job_id", jobID,
					"agent_id", run.agentName,
					"panic_type", fmt.Sprintf("%T", r),
					"panic", r,
				)
			}
			stopHeartbeat()
			select {
			case <-heartbeatStopped:
			case <-time.After(time.Second):
				slog.Warn("K12 GradingJob anchor heartbeat stop timed out",
					"job_id", jobID,
					"agent_id", run.agentName,
					"stage", k12.GradingStageLocating,
					"timeout_ms", int64(time.Second/time.Millisecond),
				)
			}
			logTerminal := slog.Info
			if terminalStatus == "failed" || terminalStatus == "panicked" {
				logTerminal = slog.Warn
			}
			logTerminal("K12 GradingJob anchor finished",
				"job_id", jobID,
				"agent_id", run.agentName,
				"stage", k12.GradingStageLocating,
				"status", terminalStatus,
				"elapsed_ms", time.Since(startedAt).Milliseconds(),
			)
			o.mu.Lock()
			delete(o.anchorActive, jobID)
			if current := o.anchorDone[jobID]; current == done {
				delete(o.anchorDone, jobID)
				close(done)
			}
			o.mu.Unlock()
		}()

		anchored, state, digest, failed := o.executeAnchorForTask(
			jobID, run.agentName, image, frozen, snapshot, taskIntent,
		)
		ctx := o.gradingBaseContext()
		l := o.jobLock(jobID)
		l.Lock()
		v, err := o.deps.GetGradingJob(ctx, run.agentName, jobID)
		if err != nil || v.Fields.AnchorState != k12.GradingAnchorPending ||
			(v.Record.Status != k12.GradingStageAwaitingConfirmation && v.Record.Status != k12.GradingStageLocating) {
			terminalStatus = "superseded"
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
			slog.Warn("K12 批改任务锚点结果写回失败（保留当前状态供恢复）",
				"job_id", jobID,
				"agent_id", run.agentName,
				"error", err,
			)
			return
		}
		terminalStatus = "completed"
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
func (o *GradingOrchestrator) executeAnchor(
	jobID, agentName string,
	image []byte,
	frozen []RecognizedQuestion,
	snapshot k12.GradingModelSnapshot,
) ([]RecognizedQuestion, string, string, bool) {
	return o.executeAnchorForTask(jobID, agentName, image, frozen, snapshot, "")
}

func (o *GradingOrchestrator) executeAnchorForTask(
	jobID, agentName string,
	image []byte,
	frozen []RecognizedQuestion,
	snapshot k12.GradingModelSnapshot,
	taskIntent PhotoTaskIntent,
) ([]RecognizedQuestion, string, string, bool) {
	// 空白卷没有学生作答痕迹，不发起只用于答案区域的定位模型调用。
	if taskIntent == PhotoTaskBlankWorksheet {
		return nil, k12.GradingAnchorDegraded, "anchor:blank_worksheet", false
	}
	if o.deps.AnswerAnchorer == nil || !hasAnswerCandidate(frozen) {
		return nil, k12.GradingAnchorDegraded, "anchor:absent", false
	}
	baseCtx := k12.WithGradingModelSnapshot(o.gradingBaseContext(), snapshot)
	job, err := o.deps.GetGradingJob(baseCtx, agentName, jobID)
	if err != nil {
		return nil, k12.GradingAnchorDegraded, "anchor:ledger_job_missing", true
	}
	policy := k12.ModelRequestPolicySnapshot{}
	if k12.NormalizeGradingModelSnapshot(snapshot).Model == k12.RecognizingPolicyModel {
		policy = k12.ApprovedLocatingRequestPolicy()
	}
	requestRaw, _ := json.Marshal(struct {
		ImageDigest string               `json:"image_digest"`
		Questions   []RecognizedQuestion `json:"questions"`
	}{modelInvocationDigest(image), frozen})
	policyRaw, _ := json.Marshal(policy)
	invocation, err := o.beginModelInvocationWithPolicy(
		baseCtx,
		job,
		k12.GradingStageLocating,
		modelInvocationDigest([]byte(k12.GradingStageLocating), requestRaw, policyRaw),
		policy,
	)
	if err != nil {
		return nil, k12.GradingAnchorDegraded, "anchor:outcome_unknown", true
	}
	// The provider budget starts at the provider boundary. Local ledger reads/writes
	// must not consume the AnswerAnchorer deadline, especially under slow storage.
	anchorTimeout := o.anchorTimeout
	if job.Fields.BudgetSnapshot.IsFrozen() {
		seconds, ok := job.Fields.BudgetSnapshot.StageBudgetSeconds(k12.GradingStageLocating, 0)
		if !ok {
			_, _ = o.deps.Records.MarkModelInvocationFailed(context.WithoutCancel(baseCtx), agentName,
				invocation.InvocationID, "invalid_frozen_locating_budget")
			return nil, k12.GradingAnchorDegraded, "anchor:invalid_budget", true
		}
		anchorTimeout = time.Duration(seconds) * time.Second
	}
	anchorDeadline := time.Now().Add(anchorTimeout)
	if strings.TrimSpace(job.Fields.ParentAutomaticAttemptID) != "" {
		switch {
		case job.Fields.ParentAutomaticDeadlineAt > 0 &&
			time.Unix(job.Fields.ParentAutomaticDeadlineAt, 0).Before(anchorDeadline):
			anchorDeadline = time.Unix(job.Fields.ParentAutomaticDeadlineAt, 0)
		case job.Fields.ParentAutomaticDeadlineAt == 0:
			remainingDeadline := time.Now().Add(
				time.Duration(job.Fields.ParentAutomaticRemainingSeconds) * time.Second,
			)
			if remainingDeadline.Before(anchorDeadline) {
				anchorDeadline = remainingDeadline
			}
		}
	}
	ctx, cancel := context.WithDeadline(baseCtx, anchorDeadline)
	if !invocation.RequestPolicySnapshot.IsZero() {
		ctx = k12.WithGradingModelRequestPolicy(ctx, invocation.RequestPolicySnapshot)
	}
	unregisterProvider := o.registerGradingModelCall(jobID, cancel)
	if current, readErr := o.deps.GetGradingJob(context.WithoutCancel(ctx), agentName, jobID); readErr != nil {
		cancel()
		unregisterProvider()
		return nil, k12.GradingAnchorDegraded, "anchor:job_read_failed", true
	} else if current.Record.Status == k12.GradingStageCancelled {
		cancel()
		unregisterProvider()
		_, _ = o.deps.Records.MarkModelInvocationFailed(context.WithoutCancel(ctx), agentName,
			invocation.InvocationID, "cancelled_before_provider_call")
		return nil, k12.GradingAnchorDegraded, "anchor:cancelled", false
	}
	anchored, err := o.deps.anchorHomeworkGeometry(ctx, image, frozen)
	ctxErr := ctx.Err()
	cancel()
	unregisterProvider()
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(ctxErr, context.DeadlineExceeded) || errors.Is(ctxErr, context.Canceled) {
		_, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(context.WithoutCancel(ctx), agentName,
			invocation.InvocationID, "provider_outcome_unknown")
		artifact := "anchor:cancelled"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctxErr, context.DeadlineExceeded) {
			artifact = "anchor:timeout"
		}
		return nil, k12.GradingAnchorDegraded, artifact, true
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
	// ADR-K12-024 requires persisted text Problem/Attempt facts to reach the one
	// page finalizer through durable per-item receipts. A zero budget snapshot
	// remains an unfrozen legacy deadline policy; this cutover is intentionally
	// limited to the typed text application path so historical photo deadline and
	// cancellation semantics do not change without their own migration contract.
	assessmentQuestions := RecognizedQuestionsForAssessment(run.questions)
	hasDurableItemIdentity := len(assessmentQuestions) > 0
	for _, question := range assessmentQuestions {
		if strings.TrimSpace(question.ProblemID) == "" ||
			strings.TrimSpace(question.AttemptID) == "" ||
			question.ConfirmedVersion < 1 ||
			strings.TrimSpace(question.InputDigest) == "" {
			hasDurableItemIdentity = false
			break
		}
	}
	if job.Fields.BudgetSnapshot.IsFrozen() || (run.textOnly && hasDurableItemIdentity) {
		return o.runAssessItems(ctx, run, job)
	}
	requestRaw, _ := json.Marshal(struct {
		Request   PhotoGradeRequest    `json:"request"`
		Questions []RecognizedQuestion `json:"questions"`
		Anchored  []RecognizedQuestion `json:"anchored,omitempty"`
	}{run.req, run.questions, run.anchored})
	invocation, err := o.beginModelInvocation(ctx, job, k12.GradingStageAssessing,
		modelInvocationDigest([]byte(k12.GradingStageAssessing), requestRaw))
	if err != nil {
		if invocation.InvocationID == "" {
			return o.failModelInvocationBeforeSend(ctx, run, jobID, err)
		}
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

	providerCtx, cancelProvider := o.durableGradingStageContext(ctx, job.Fields.Deadline)
	unregisterProvider := o.registerGradingModelCall(jobID, cancelProvider)
	if current, readErr := o.deps.GetGradingJob(context.WithoutCancel(ctx), run.agentName, jobID); readErr != nil {
		cancelProvider()
		unregisterProvider()
		return GradingJobView{}, readErr
	} else if current.Record.Status == k12.GradingStageCancelled {
		cancelProvider()
		unregisterProvider()
		_, _ = o.deps.Records.MarkModelInvocationFailed(context.WithoutCancel(ctx), run.agentName,
			invocation.InvocationID, "cancelled_before_provider_call")
		return current, nil
	}
	result, err := assessDeps.GradeHomeworkPhoto(providerCtx, run.req)
	// Sample the provider context before invoking our own cancel function. Some
	// adapters sanitize timeout errors into opaque text; after cancelProvider every
	// ordinary failure would look cancelled and could be misclassified as unknown.
	providerCtxErr := providerCtx.Err()
	cancelProvider()
	unregisterProvider()
	if err != nil && invocationOutcomeUnknown(providerCtxErr) && photoHasCompletedItem(result.Items) {
		// An opaque adapter may replace the typed context error with friendly
		// text. Once another item completed, the page is provably partial, so the
		// provider deadline is the authoritative non-retryable cause.
		err = providerCtxErr
	}
	if err == nil && invocationOutcomeUnknown(providerCtxErr) {
		// GradeHomeworkPhoto intentionally keeps ordinary per-item failures as a
		// partial result. If the provider budget also expired, an opaque adapter may
		// have hidden the typed timeout; do not publish that partial result as final.
		for _, item := range result.Items {
			if item.Status == PhotoFailed {
				err = providerCtxErr
				break
			}
		}
	}
	if err != nil {
		if sentProviderOutcomeUnknown(err, providerCtxErr) {
			_, _ = o.deps.Records.MarkModelInvocationOutcomeUnknown(context.WithoutCancel(ctx), run.agentName, invocation.InvocationID, "provider_outcome_unknown")
			if current, readErr := o.deps.GetGradingJob(context.WithoutCancel(ctx), run.agentName, jobID); readErr == nil && current.Record.Status == k12.GradingStageCancelled {
				return current, nil
			}
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

func photoHasCompletedItem(items []PhotoGradeItem) bool {
	for _, item := range items {
		if item.Status != "" && item.Status != PhotoFailed {
			return true
		}
	}
	return false
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

// runProject is the only page-finalization entry. Completion is authorized by
// one durable GradingFinalArtifact, never by the process-local page result.
func (o *GradingOrchestrator) runProject(ctx context.Context, run *gradingRun, jobID string) (GradingJobView, error) {
	job, err := o.deps.GetGradingJob(ctx, run.agentName, jobID)
	if err != nil {
		return GradingJobView{}, err
	}
	finalArtifact, err := o.finalizeGradingPage(ctx, run, job)
	if err != nil {
		return job, err
	}
	return o.completeFinalizedGrading(ctx, run, jobID, finalArtifact.ArtifactDigest)
}

func (o *GradingOrchestrator) completeFinalizedGrading(
	ctx context.Context,
	run *gradingRun,
	jobID string,
	finalArtifactDigest string,
) (GradingJobView, error) {
	return o.advanceOK(ctx, run, jobID, finalArtifactDigest)
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
	retryable := !errors.Is(cause, k12.ErrModelCapabilityUnverified)
	if !retryable {
		kind = "model_capability_unverified"
	}
	v, aerr := o.deps.AdvanceGradingStage(ctx, run.agentName, jobID, AdvanceGradingInput{
		Outcome: GradingOutcomeFailed, FailureKind: kind, Retryable: retryable,
	})
	if aerr != nil {
		return v, aerr
	}
	return v, cause
}

// failModelInvocationBeforeSend handles a durable-ledger preparation failure.
// No provider request can have escaped before PrepareModelInvocation returns,
// so parking this case as outcome_unknown would be both inaccurate and
// unrecoverable. Immutable identity conflicts are terminal for the frozen Job;
// ordinary local storage failures remain safely retryable.
func (o *GradingOrchestrator) failModelInvocationBeforeSend(
	ctx context.Context,
	run *gradingRun,
	jobID string,
	cause error,
) (GradingJobView, error) {
	kind := "invocation_prepare_failed"
	retryable := true
	if current, err := o.deps.GetGradingJob(
		context.WithoutCancel(ctx), run.agentName, jobID,
	); err == nil && parentAutomaticDeadlineExceeded(current.Fields, o.deps.now()) {
		kind = gradingFailureInteractiveDeadlineExceeded
	}
	if errors.Is(cause, k12storage.ErrModelInvocationConflict) {
		kind = "invocation_identity_conflict"
		retryable = false
	}
	if errors.Is(cause, ErrModelRequestPolicyInvalid) {
		kind = "invocation_policy_invalid"
		retryable = false
	}
	if errors.Is(cause, k12.ErrModelCapabilityUnverified) {
		kind = "model_capability_unverified"
		retryable = false
	}
	v, err := o.deps.AdvanceGradingStage(ctx, run.agentName, jobID, AdvanceGradingInput{
		Outcome: GradingOutcomeFailed, FailureKind: kind, Retryable: retryable,
	})
	if err != nil {
		return v, err
	}
	return v, cause
}

// gradingErrRetryable 失败可重试判定：输入类错误重跑必然同败；收到明确 HTTP
// 响应时，仅 408/425/429 和 5xx 属瞬态。其余未类型化错误沿用既有可重试语义。
func gradingErrRetryable(err error) bool {
	if errors.Is(err, ErrInvalidInput) || errors.Is(err, k12.ErrModelCapabilityUnverified) {
		return false
	}
	if statusCode, definitive := definitiveProviderResponseStatus(err); definitive {
		switch statusCode {
		case 408, 425, 429:
			return true
		default:
			return statusCode >= 500 && statusCode <= 599
		}
	}
	return true
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
		if question.SourceRegion != nil {
			region := *question.SourceRegion
			out[i].SourceRegion = &region
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

const photoSubmissionV2Prefix = "photo-v2-"

func photoImageDigest(image []byte) string {
	sum := sha1.Sum(image)
	return hex.EncodeToString(sum[:])
}

func legacyPhotoSubmissionID(image []byte) string {
	return "photo-" + photoImageDigest(image)
}

// scopedPhotoSubmissionID separates the immutable domain identity of a user
// submission from the image content digest. Source kind/key are already the
// canonical idempotency command identity, while the embedded image digest
// remains available for crash-recovery integrity checks.
func scopedPhotoSubmissionID(image []byte, sourceKind, sourceKey string) string {
	sourceHash := sha256.New()
	var frame [8]byte
	binary.BigEndian.PutUint64(frame[:], uint64(len(sourceKind)))
	_, _ = sourceHash.Write(frame[:])
	_, _ = sourceHash.Write([]byte(sourceKind))
	binary.BigEndian.PutUint64(frame[:], uint64(len(sourceKey)))
	_, _ = sourceHash.Write(frame[:])
	_, _ = sourceHash.Write([]byte(sourceKey))
	return photoSubmissionV2Prefix +
		photoImageDigest(image) + "-" +
		hex.EncodeToString(sourceHash.Sum(nil))
}

func photoSubmissionMatchesRequest(
	submissionID string,
	image []byte,
	sourceKind, sourceKey string,
) bool {
	// Accept the legacy content-only identity when an existing idempotency key is
	// replayed after upgrade. New tasks always use the source-scoped v2 identity.
	return submissionID == legacyPhotoSubmissionID(image) ||
		submissionID == scopedPhotoSubmissionID(image, sourceKind, sourceKey)
}

func photoSubmissionMatchesImage(submissionID string, image []byte) bool {
	if submissionID == legacyPhotoSubmissionID(image) {
		return true
	}
	prefix := photoSubmissionV2Prefix + photoImageDigest(image) + "-"
	if !strings.HasPrefix(submissionID, prefix) {
		return false
	}
	sourceDigest := strings.TrimPrefix(submissionID, prefix)
	if len(sourceDigest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(sourceDigest)
	return err == nil
}

func modelInvocationDigest(parts ...[]byte) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(part)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func recognizingInvocationDigest(
	image []byte,
	route k12.GradingModelSnapshot,
	policy k12.ModelRequestPolicySnapshot,
) string {
	route = k12.NormalizeGradingModelSnapshot(route)
	policy = k12.NormalizeModelRequestPolicySnapshot(policy)
	routeJSON, _ := json.Marshal(route)
	policyJSON, _ := json.Marshal(policy)
	return modelInvocationDigest(
		[]byte("k12-recognizing-request-v1"),
		[]byte(k12.GradingStageRecognizing),
		image,
		routeJSON,
		policyJSON,
	)
}

func modelInvocationResultDigest(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return modelInvocationDigest([]byte(fmt.Sprintf("%#v", value)))
	}
	return modelInvocationDigest(raw)
}

func invocationOutcomeUnknown(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrModelInvocationRequiresReconciliation)
}

func (o *GradingOrchestrator) beginModelInvocation(ctx context.Context, job GradingJobView, stage, requestDigest string) (k12.ModelInvocation, error) {
	return o.beginModelInvocationWithPolicy(
		ctx,
		job,
		stage,
		requestDigest,
		k12.ModelRequestPolicySnapshot{},
	)
}

func (o *GradingOrchestrator) beginModelInvocationWithPolicy(
	ctx context.Context,
	job GradingJobView,
	stage, requestDigest string,
	policy k12.ModelRequestPolicySnapshot,
) (k12.ModelInvocation, error) {
	invocation, _, err := o.deps.Records.PrepareModelInvocation(ctx, k12.ModelInvocation{
		InvocationID: "modelinv-" + idgen.ShortID(), AgentName: job.Record.AgentName,
		JobID: job.Record.RecordID, Stage: stage, RequestDigest: requestDigest,
		RouteSnapshot:         job.Fields.ModelSnapshot,
		RequestPolicySnapshot: k12.NormalizeModelRequestPolicySnapshot(policy),
		Attempt:               job.Fields.AttemptCount + 1,
		CreatedAt:             o.deps.now(),
		UpdatedAt:             o.deps.now(),
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

// beginRecognizingModelInvocationWithPolicy publishes the initial physical
// authorization before exposing the stage parent as sent. A prepared parent is
// safe to observe without a child; a sent parent is not.
func (o *GradingOrchestrator) beginRecognizingModelInvocationWithPolicy(
	ctx context.Context,
	job GradingJobView,
	image []byte,
	requestDigest string,
	policy k12.ModelRequestPolicySnapshot,
) (k12.ModelInvocation, error) {
	recognitionPlanVersion, err :=
		frozenRecognitionPlanVersion(job.Fields.BudgetSnapshot)
	if err != nil {
		return k12.ModelInvocation{}, err
	}
	if recognitionPlanVersion == k12.RecognitionPlanVersionV2 {
		return o.beginRecognizingLayoutModelInvocationV2(
			ctx,
			job,
			image,
			requestDigest,
			policy,
		)
	}
	parent, _, err := o.deps.Records.PrepareModelInvocation(
		ctx,
		k12.ModelInvocation{
			InvocationID:  "modelinv-" + idgen.ShortID(),
			AgentName:     job.Record.AgentName,
			JobID:         job.Record.RecordID,
			Stage:         k12.GradingStageRecognizing,
			RequestDigest: requestDigest,
			RouteSnapshot: job.Fields.ModelSnapshot,
			RequestPolicySnapshot: k12.NormalizeModelRequestPolicySnapshot(
				policy,
			),
			Attempt:   job.Fields.AttemptCount + 1,
			CreatedAt: o.deps.now(),
			UpdatedAt: o.deps.now(),
		},
	)
	if err != nil {
		return k12.ModelInvocation{}, err
	}
	call := k12.RecognitionPhysicalCall{
		Unit:  k12.RecognitionPhysicalUnitWholePage,
		Image: image,
	}
	childDigest, err := recognizingPhysicalInvocationDigest(parent, call)
	if err != nil {
		return parent, fmt.Errorf(
			"%w: build initial whole-page digest: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	published, child, _, err :=
		o.deps.Records.PrepareRecognizingInvocationWithInitialWholePage(
			ctx,
			parent,
			k12.ModelPhysicalInvocation{
				PhysicalInvocationID: stableRecognitionPhysicalInvocationID(
					parent.InvocationID,
					call.Unit,
				),
				ParentInvocationID:    parent.InvocationID,
				AgentName:             parent.AgentName,
				JobID:                 parent.JobID,
				Stage:                 parent.Stage,
				PhysicalUnit:          call.Unit,
				RequestDigest:         childDigest,
				RouteSnapshot:         parent.RouteSnapshot,
				RequestPolicySnapshot: parent.RequestPolicySnapshot,
				Attempt:               1,
				CreatedAt:             o.deps.now(),
				UpdatedAt:             o.deps.now(),
			},
		)
	if err != nil {
		return parent, fmt.Errorf(
			"%w: publish initial whole-page child: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	if published.Status == k12.ModelInvocationSent &&
		child.Status == k12.ModelInvocationPrepared {
		return published, nil
	}
	passiveObserver, inspectErr :=
		o.recognitionPhysicalChildIsPassiveObserver(
			context.WithoutCancel(ctx),
			published,
			child,
			call,
		)
	if inspectErr != nil {
		return published, inspectErr
	}
	if passiveObserver {
		return published, recognitionPhysicalObservedInFlightError(child)
	}
	return published, fmt.Errorf(
		"%w: invocation=%s status=%s whole_page=%s",
		ErrModelInvocationRequiresReconciliation,
		published.InvocationID,
		published.Status,
		child.Status,
	)
}

func frozenRecognitionPlanVersion(
	snapshot k12.GradingBudgetSnapshot,
) (int, error) {
	if !snapshot.IsFrozen() {
		if err := snapshot.Validate(); err != nil {
			return 0, fmt.Errorf(
				"%w: invalid legacy grading budget snapshot: %v",
				ErrModelRequestPolicyInvalid,
				err,
			)
		}
		return k12.RecognitionPlanVersionV1, nil
	}
	if err := snapshot.Validate(); err != nil {
		return 0, fmt.Errorf(
			"%w: invalid frozen recognition plan: %v",
			ErrModelRequestPolicyInvalid,
			err,
		)
	}
	switch snapshot.RecognitionPlanVersion {
	case k12.RecognitionPlanVersionV1, k12.RecognitionPlanVersionV2:
		return snapshot.RecognitionPlanVersion, nil
	default:
		return 0, fmt.Errorf(
			"%w: unknown frozen recognition plan version %d",
			ErrModelRequestPolicyInvalid,
			snapshot.RecognitionPlanVersion,
		)
	}
}

func (o *GradingOrchestrator) beginRecognizingLayoutModelInvocationV2(
	ctx context.Context,
	job GradingJobView,
	image []byte,
	requestDigest string,
	policy k12.ModelRequestPolicySnapshot,
) (k12.ModelInvocation, error) {
	canonicalPage, err := k12.CanonicalizeRecognitionPageV2(image)
	if err != nil {
		return k12.ModelInvocation{}, fmt.Errorf(
			"%w: canonicalize recognition page v2: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	parent, _, err := o.deps.Records.PrepareModelInvocation(
		ctx,
		k12.ModelInvocation{
			InvocationID:  "modelinv-" + idgen.ShortID(),
			AgentName:     job.Record.AgentName,
			JobID:         job.Record.RecordID,
			Stage:         k12.GradingStageRecognizing,
			RequestDigest: requestDigest,
			RouteSnapshot: job.Fields.ModelSnapshot,
			RequestPolicySnapshot: k12.NormalizeModelRequestPolicySnapshot(
				policy,
			),
			Attempt:   job.Fields.AttemptCount + 1,
			CreatedAt: o.deps.now(),
			UpdatedAt: o.deps.now(),
		},
	)
	if err != nil {
		return k12.ModelInvocation{}, err
	}
	stageStartedAt, err := recognitionLayoutStageStartedAtV2(job, parent)
	if err != nil {
		return parent, err
	}
	return o.publishInitialRecognitionLayoutV2(
		ctx,
		parent,
		canonicalPage,
		initialRecognitionLayoutContractV2{
			Budget:                   job.Fields.BudgetSnapshot,
			StageStartedAtUnixMillis: stageStartedAt,
		},
	)
}

func recognitionLayoutStageStartedAtV2(
	job GradingJobView,
	parent k12.ModelInvocation,
) (int64, error) {
	budgetMillis := job.Fields.BudgetSnapshot.RecognizingBuckets.
		UpTo32ProblemsMillis
	budgetSeconds := (budgetMillis + 999) / 1000
	stageStartedAtSeconds := parent.CreatedAt
	if budgetMillis <= 0 || budgetSeconds <= 0 ||
		stageStartedAtSeconds <= 0 ||
		job.Fields.Deadline <= stageStartedAtSeconds ||
		job.Fields.Deadline > stageStartedAtSeconds+budgetSeconds {
		return 0, fmt.Errorf(
			"%w: recognizing job deadline exceeds the trusted v2 or parent ceiling",
			ErrModelRequestPolicyInvalid,
		)
	}
	return stageStartedAtSeconds * 1000, nil
}

func stableRecognitionLayoutPlanIDV2(parentInvocationID string) string {
	sum := sha256.Sum256([]byte(
		"k12-recognition-layout-plan-v2\x00" + parentInvocationID,
	))
	return "layoutplan-" + hex.EncodeToString(sum[:16])
}

func (o *GradingOrchestrator) loadInitialRecognitionLayoutRuntimeV2(
	ctx context.Context,
	job GradingJobView,
	parent k12.ModelInvocation,
	image []byte,
) (k12.RecognitionLayoutPlanRuntimeV2, error) {
	canonicalPage, err := k12.CanonicalizeRecognitionPageV2(image)
	if err != nil {
		return k12.RecognitionLayoutPlanRuntimeV2{}, fmt.Errorf(
			"%w: canonicalize recognition page v2: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	stageStartedAt, err := recognitionLayoutStageStartedAtV2(job, parent)
	if err != nil {
		return k12.RecognitionLayoutPlanRuntimeV2{}, err
	}
	return o.loadInitialRecognitionLayoutRuntimeForParentV2(
		ctx,
		parent,
		canonicalPage,
		initialRecognitionLayoutContractV2{
			Budget:                   job.Fields.BudgetSnapshot,
			StageStartedAtUnixMillis: stageStartedAt,
		},
	)
}

func (o *GradingOrchestrator) recoverRecognizeInvocation(ctx context.Context, run *gradingRun, jobID string, invocation k12.ModelInvocation) (bool, GradingJobView, error) {
	if (invocation.Status != k12.ModelInvocationSent && invocation.Status != k12.ModelInvocationSucceeded) || len(run.questions) == 0 {
		return false, GradingJobView{}, nil
	}
	physicalChildren, physicalErr := o.recognitionPhysicalSuccessSet(
		context.WithoutCancel(ctx),
		invocation,
		run.req.Image,
	)
	if physicalErr != nil {
		var ledgerErr error
		if invocation.Status == k12.ModelInvocationSent {
			_, ledgerErr = o.deps.Records.MarkModelInvocationOutcomeUnknown(
				context.WithoutCancel(ctx),
				run.agentName,
				invocation.InvocationID,
				"physical_receipt_invalid",
			)
		}
		v, advanceErr := o.markGradingOutcomeUnknown(
			context.WithoutCancel(ctx),
			run,
			jobID,
			"physical_receipt_invalid",
		)
		return true, v, errors.Join(physicalErr, ledgerErr, advanceErr)
	}
	if !invocation.RequestPolicySnapshot.IsZero() {
		receipt, ok := o.readRecognitionReceipt(jobID)
		if !ok ||
			receipt.AgentName != run.agentName ||
			receipt.InvocationID != invocation.InvocationID ||
			!sameRecognitionPhysicalReceiptSet(
				receipt.PhysicalInvocations,
				recognitionPhysicalReceiptSet(physicalChildren),
			) {
			var ledgerErr error
			if invocation.Status == k12.ModelInvocationSent {
				_, ledgerErr =
					o.deps.Records.MarkModelInvocationOutcomeUnknown(
						context.WithoutCancel(ctx),
						run.agentName,
						invocation.InvocationID,
						"recognition_receipt_invalid",
					)
			}
			v, advanceErr := o.markGradingOutcomeUnknown(
				context.WithoutCancel(ctx),
				run,
				jobID,
				"recognition_receipt_invalid",
			)
			return true, v, errors.Join(
				ErrModelInvocationRequiresReconciliation,
				ledgerErr,
				advanceErr,
			)
		}
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

func (p presetAnchorer) AnchorAnswerGeometry(context.Context, []byte, []RecognizedQuestion) ([]RecognizedQuestion, error) {
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
