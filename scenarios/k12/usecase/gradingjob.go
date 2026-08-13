package usecase

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 统一 GradingJob 应用命令（架构设计 §6.7 公共命令清单 + §4.10 幂等 + §6.15 单机可靠性）。
//
// 本轮范围：领域状态机 + 持久化 + 应用命令 + HTTP 契约。真实编排器（有界 worker pool、
// 阶段级 context 超时、取消传播到在途模型调用、崩溃恢复扫描）在下一轮接线——
// AdvanceGradingStage 即编排器届时唯一的推进入口（带检查点写入）。

// GradingJobView 批改任务视图（记录 + 领域字段）。
type GradingJobView struct {
	Record *records.AgentRecord
	Fields k12.GradingJobFields
}

// CreateGradingJobInput 创建输入。SourceKind/SourceKey 映射统一幂等键（§4.10：
// 桌面 request_id / HTTP idempotency_key / IM message_id / 工作流 execution_id）。
type CreateGradingJobInput struct {
	SubmissionID              string
	SourceKind                string
	SourceKey                 string
	ConfirmedVersion          int
	ModelSnapshot             k12.GradingModelSnapshot
	ParentAutomaticAttemptID  string
	ParentAutomaticDeadlineAt int64
	// BudgetSnapshot is reserved for trusted internal callers/tests. When zero,
	// CreateGradingJob freezes Deps.GradingBudgetSnapshot. API payloads do not
	// expose this field, so a client cannot select a release policy per request.
	BudgetSnapshot k12.GradingBudgetSnapshot
	// MaterializesProblemAttempts is a trusted caller contract: before this Job
	// can leave confirmation, the caller will persist typed Problem/Attempt facts.
	// Frozen policies reject generic placeholder Jobs that have no such path.
	MaterializesProblemAttempts bool
	// trustedRecognitionPolicy 仅由服务端持有的创建边界注入，该边界已为新 Job
	// 选定方案。直接调用 CreateGradingJob 时继续沿用既有的全局策略行为。
	trustedRecognitionPolicy *k12.GradingBudgetSnapshot
}

func validateGradingSourceIdentity(sourceKind, sourceKey string) error {
	if strings.TrimSpace(sourceKind) == "" || strings.TrimSpace(sourceKey) == "" {
		return fmt.Errorf(
			"%w: source_kind/source_key 不可空（统一幂等键，§4.10）",
			ErrInvalidInput,
		)
	}
	if strings.ContainsRune(sourceKind, '|') ||
		strings.IndexFunc(sourceKind, unicode.IsControl) >= 0 {
		return fmt.Errorf(
			"%w: source_kind 含幂等 wire 保留分隔符或控制字符",
			ErrInvalidInput,
		)
	}
	return nil
}

// AdvanceGradingStage 的 outcome 枚举。
const (
	GradingOutcomeOK      = "ok"              // 当前阶段成功完成：写检查点、推进后继阶段
	GradingOutcomeFailed  = "failed"          // 当前自动阶段失败：按规则 2/4 降级或进失败态
	GradingOutcomeAnchor  = "anchor"          // 锚点增强结果回位：located / degraded（规则 1）
	GradingOutcomeUnknown = "outcome_unknown" // 外部请求已发送但结果无法判定：禁止普通重试
)

// AdvanceGradingInput 阶段推进输入（编排器专用，见 AdvanceGradingStage）。
type AdvanceGradingInput struct {
	Outcome        string
	ArtifactDigest string // outcome=ok/anchor：阶段产物摘要（§5.4 stage_checkpoints）
	FailureKind    string // outcome=failed：可理解的失败类别
	Retryable      bool   // outcome=failed：是否安全重试（由阶段和错误决定）
	AnchorState    string // outcome=anchor：located / degraded
}

// errGradingStageConflict 命令与当前阶段不符（HTTP 层映射 409）。
func errGradingStageConflict(format string, args ...any) error {
	return fmt.Errorf("%w: %s", records.ErrIllegalTransition, fmt.Sprintf(format, args...))
}

// CreateGradingJob 创建统一批改任务（§6.7 公共命令①）。幂等（§4.10）：同一
// source_kind+source_key+confirmed_version 只产生一个 Job，同键返回既有 Job（created=false）。
func (d Deps) CreateGradingJob(ctx context.Context, agentName, sourceSession string, in CreateGradingJobInput) (GradingJobView, bool, error) {
	if strings.TrimSpace(in.SubmissionID) == "" {
		return GradingJobView{}, false, fmt.Errorf("%w: submission_id 不可空", ErrInvalidInput)
	}
	if err := validateGradingSourceIdentity(in.SourceKind, in.SourceKey); err != nil {
		return GradingJobView{}, false, err
	}
	if in.ConfirmedVersion < 0 {
		return GradingJobView{}, false, fmt.Errorf("%w: confirmed_version 不可为负", ErrInvalidInput)
	}
	// 幂等重放会先读取已经冻结的 Job，再查询可变创建策略。
	// 后续发布策略变更只影响新的幂等键。
	if existing, found, err := d.findGradingJobByIdempotency(
		ctx, agentName, in.SourceKind, in.SourceKey, in.ConfirmedVersion,
	); err != nil {
		return GradingJobView{}, false, err
	} else if found {
		if existing.Fields.SubmissionID != in.SubmissionID {
			return GradingJobView{}, false, fmt.Errorf(
				"%w: idempotency key %q is already bound to submission %q, requested %q",
				ErrInvalidInput,
				existing.Fields.IdempotencyKey,
				existing.Fields.SubmissionID,
				in.SubmissionID,
			)
		}
		return existing, false, nil
	}
	in.ParentAutomaticAttemptID = strings.TrimSpace(in.ParentAutomaticAttemptID)
	if in.ParentAutomaticAttemptID == "" {
		if in.ParentAutomaticDeadlineAt != 0 {
			return GradingJobView{}, false, fmt.Errorf(
				"%w: parent automatic deadline requires attempt identity", ErrInvalidInput,
			)
		}
	} else if in.ParentAutomaticDeadlineAt <= d.now() {
		return GradingJobView{}, false, fmt.Errorf(
			"%w: parent automatic deadline must be in the future", ErrInvalidInput,
		)
	}
	in.ModelSnapshot = k12.NormalizeGradingModelSnapshot(in.ModelSnapshot)
	if err := k12.ValidateGradingRecognizingRequestPolicy(in.ModelSnapshot); err != nil {
		return GradingJobView{}, false, fmt.Errorf(
			"%w: invalid recognizing request policy: %v",
			ErrInvalidInput,
			err,
		)
	}
	budgetSnapshot := in.BudgetSnapshot
	if !budgetSnapshot.IsFrozen() {
		if err := budgetSnapshot.Validate(); err != nil {
			return GradingJobView{}, false, fmt.Errorf("%w: invalid grading budget snapshot: %v", ErrInvalidInput, err)
		}
		budgetSnapshot = d.GradingBudgetSnapshot
	}
	if budgetSnapshot.IsFrozen() {
		trustedPolicy := d.GradingBudgetSnapshot
		if in.trustedRecognitionPolicy != nil {
			trustedPolicy = *in.trustedRecognitionPolicy
		}
		if !trustedPolicy.IsFrozen() {
			return GradingJobView{}, false, fmt.Errorf(
				"%w: frozen grading job requires a trusted recognition creation policy",
				ErrInvalidInput,
			)
		}
		budgetSnapshot = budgetSnapshot.WithRecognitionPolicyFrom(trustedPolicy)
	}
	if err := budgetSnapshot.Validate(); err != nil {
		return GradingJobView{}, false, fmt.Errorf("%w: invalid grading budget snapshot: %v", ErrInvalidInput, err)
	}
	if budgetSnapshot.IsFrozen() && !in.MaterializesProblemAttempts {
		return GradingJobView{}, false, fmt.Errorf("%w: frozen grading jobs require a typed Problem/Attempt materialization path", ErrInvalidInput)
	}
	f := k12.GradingJobFields{
		SubmissionID:              in.SubmissionID,
		SourceKind:                in.SourceKind,
		IdempotencyKey:            k12.BuildGradingIdempotencyKey(in.SourceKind, in.SourceKey, in.ConfirmedVersion),
		ConfirmedVersion:          in.ConfirmedVersion,
		ConfirmationState:         k12.GradingConfirmationPending,
		AnchorState:               k12.GradingAnchorPending,
		ParentAutomaticAttemptID:  in.ParentAutomaticAttemptID,
		ParentAutomaticDeadlineAt: in.ParentAutomaticDeadlineAt,
		ModelSnapshot:             in.ModelSnapshot,
		BudgetSnapshot:            budgetSnapshot,
	}
	if in.ParentAutomaticAttemptID != "" {
		f.ParentAutomaticRemainingSeconds = in.ParentAutomaticDeadlineAt - d.now()
	}
	if err := d.setGradingDeadline(ctx, agentName, &f, k12.GradingStageQueued); err != nil {
		return GradingJobView{}, false, err
	}
	return d.putGradingJob(ctx, agentName, sourceSession, f)
}

// putGradingJob 幂等入库；去重命中时回读既有 Job。
func (d Deps) putGradingJob(ctx context.Context, agentName, sourceSession string, f k12.GradingJobFields) (GradingJobView, bool, error) {
	rec, err := k12.NewGradingJobRecord(agentName, sourceSession, f)
	if err != nil {
		return GradingJobView{}, false, err
	}
	created, err := d.Records.Put(ctx, rec)
	if err != nil {
		return GradingJobView{}, false, fmt.Errorf("usecase: 批改任务入库: %w", err)
	}
	v, err := d.GetGradingJob(ctx, agentName, rec.RecordID)
	if err != nil {
		return GradingJobView{}, false, err
	}
	if !created {
		if v.Fields.SubmissionID != f.SubmissionID {
			return GradingJobView{}, false, fmt.Errorf(
				"%w: idempotency key %q is already bound to submission %q, requested %q",
				ErrInvalidInput,
				f.IdempotencyKey,
				v.Fields.SubmissionID,
				f.SubmissionID,
			)
		}
		storedRoute := k12.NormalizeGradingModelSnapshot(v.Fields.ModelSnapshot)
		requestedRoute := k12.NormalizeGradingModelSnapshot(f.ModelSnapshot)
		if storedRoute != requestedRoute {
			return GradingJobView{}, false, fmt.Errorf(
				"%w: idempotency key %q is already bound to model route %q, requested %q",
				ErrInvalidInput,
				f.IdempotencyKey,
				storedRoute.Route,
				requestedRoute.Route,
			)
		}
	}
	return v, created, nil
}

// findGradingJobByIdempotency reads the immutable Job before any mutable
// creation dependency (notably the model catalog resolver) is consulted.
// records.Store derives its typed dedupe key from Fields.IdempotencyKey, so the
// probe intentionally contains only the owner, collection, and canonical key.
func (d Deps) findGradingJobByIdempotency(
	ctx context.Context,
	agentName, sourceKind, sourceKey string,
	confirmedVersion int,
) (GradingJobView, bool, error) {
	fields, err := json.Marshal(k12.GradingJobFields{
		IdempotencyKey: k12.BuildGradingIdempotencyKey(sourceKind, sourceKey, confirmedVersion),
	})
	if err != nil {
		return GradingJobView{}, false, fmt.Errorf("usecase: 编码批改任务幂等探针: %w", err)
	}
	rec, err := d.Records.FindDuplicate(ctx, &records.AgentRecord{
		AgentName:  agentName,
		Collection: k12.CollectionGradingJob,
		Fields:     string(fields),
	})
	if errors.Is(err, records.ErrNotFound) {
		return GradingJobView{}, false, nil
	}
	if err != nil {
		return GradingJobView{}, false, fmt.Errorf("usecase: 查找幂等批改任务: %w", err)
	}
	v, err := d.GetGradingJob(ctx, agentName, rec.RecordID)
	if err != nil {
		return GradingJobView{}, false, err
	}
	return v, true, nil
}

// ListGradingJobs 列批改任务（§6.7 公共命令②查询；stage 空 = 全部）。始终按 agent 圈定归属。
func (d Deps) ListGradingJobs(ctx context.Context, agentName, stage string) ([]GradingJobView, error) {
	recs, err := d.Records.ListByScope(ctx, agentName, k12.CollectionGradingJob, stage)
	if err != nil {
		return nil, fmt.Errorf("usecase: 列批改任务: %w", err)
	}
	out := make([]GradingJobView, 0, len(recs))
	for _, r := range recs {
		f, _ := k12.ParseGradingJobFields(r.Fields)
		out = append(out, GradingJobView{Record: r, Fields: f})
	}
	return out, nil
}

// GetGradingJob 取单个批改任务（带 owner 校验：跨实例按不存在处理 = 多孩隔离硬边界）。
func (d Deps) GetGradingJob(ctx context.Context, agentName, recordID string) (GradingJobView, error) {
	rec, err := d.Records.Get(ctx, recordID)
	if err != nil {
		return GradingJobView{}, fmt.Errorf("usecase: 取批改任务: %w", err)
	}
	if rec == nil || rec.AgentName != agentName || rec.Collection != k12.CollectionGradingJob {
		return GradingJobView{}, fmt.Errorf("usecase: 批改任务不存在或不属于该实例: %w", records.ErrNotFound)
	}
	f, err := k12.ParseGradingJobFields(rec.Fields)
	if err != nil {
		return GradingJobView{}, fmt.Errorf("usecase: 解析批改任务字段: %w", err)
	}
	return GradingJobView{Record: rec, Fields: f}, nil
}

// AdvanceGradingStage 阶段推进（编排器专用命令）。桌面/钉钉不得直接调用——它们只使用
// §6.7 公共命令（创建/查询/确认/取消/重试）；本命令是执行侧（worker）完成/失败一个阶段后的
// 回写入口，本轮先以命令+HTTP 契约钉死语义，真实编排器下一轮接线。
//
//   - ok：写当前阶段检查点（规则 3），推进到后继阶段并按阶段预算重算 deadline；
//     queued 起跑目标 = 最近成功阶段的后继（规则 3 恢复 / 规则 6 修正重批 queued→assessing 捷径）。
//   - failed：rendering 失败按规则 2 降级续跑 projecting（不进失败态）；其余自动阶段
//     attempt_count+1 进 failed_retryable，不可重试或达 GradingMaxStageAttempts 上限时
//     立即收敛 failed_terminal（规则 4）。
//   - anchor：锚点增强结果回位（规则 1 并行分支）：anchor_state=located/degraded；
//     若确认已冻结（confirmation_state=confirmed）则汇合进入 assessing。
func (d Deps) AdvanceGradingStage(ctx context.Context, agentName, recordID string, in AdvanceGradingInput) (GradingJobView, error) {
	v, err := d.GetGradingJob(ctx, agentName, recordID)
	if err != nil {
		return GradingJobView{}, err
	}
	switch in.Outcome {
	case GradingOutcomeOK:
		return d.advanceGradingOK(ctx, v, in)
	case GradingOutcomeFailed:
		return d.advanceGradingFailed(ctx, v, in)
	case GradingOutcomeAnchor:
		return d.advanceGradingAnchor(ctx, v, in)
	case GradingOutcomeUnknown:
		return d.advanceGradingOutcomeUnknown(ctx, v, in)
	}
	return GradingJobView{}, fmt.Errorf("%w: outcome 非法: %q（允许 ok/failed/anchor/outcome_unknown）", ErrInvalidInput, in.Outcome)
}

func (d Deps) advanceGradingOutcomeUnknown(ctx context.Context, v GradingJobView, in AdvanceGradingInput) (GradingJobView, error) {
	if strings.TrimSpace(in.FailureKind) == "" {
		return GradingJobView{}, fmt.Errorf("%w: outcome_unknown 必须写明 failure_kind", ErrInvalidInput)
	}
	switch v.Record.Status {
	case k12.GradingStageRecognizing, k12.GradingStageLocating, k12.GradingStageAssessing:
	default:
		return GradingJobView{}, errGradingStageConflict("阶段 %s 无 outcome_unknown 转移", v.Record.Status)
	}
	v.Fields.FailedStage = v.Record.Status
	v.Fields.FailureKind = in.FailureKind
	v.Fields.Retryable = false
	v.Fields.Deadline = 0
	return d.saveGradingJob(ctx, v, k12.GradingStageOutcomeUnknown)
}

// gradingNextStage 主链后继（ok 推进）。
func gradingNextStage(v GradingJobView) (string, error) {
	switch v.Record.Status {
	case k12.GradingStageQueued:
		return k12.GradingResumeStage(v.Fields.StageCheckpoints), nil
	case k12.GradingStageNormalizing:
		return k12.GradingStageRecognizing, nil
	case k12.GradingStageRecognizing:
		// 规则 1：识别完成即进 awaiting_confirmation，家长确认不等待锚点；
		// 锚点增强同时异步启动（anchor_state=pending，由 outcome=anchor 回位）。
		return k12.GradingStageAwaitingConfirmation, nil
	case k12.GradingStageAssessing:
		return k12.GradingStageRendering, nil
	case k12.GradingStageRendering:
		return k12.GradingStageProjecting, nil
	case k12.GradingStageProjecting:
		return k12.GradingStageCompleted, nil
	case k12.GradingStageAwaitingConfirmation:
		return "", errGradingStageConflict("awaiting_confirmation 只能经 confirm 命令进入 assessing（规则 1：不得跳过家长确认）")
	}
	return "", errGradingStageConflict("阶段 %s 不可推进", v.Record.Status)
}

func (d Deps) advanceGradingOK(ctx context.Context, v GradingJobView, in AdvanceGradingInput) (GradingJobView, error) {
	next, err := gradingNextStage(v)
	if err != nil {
		return GradingJobView{}, err
	}
	if v.Record.Status == k12.GradingStageQueued && v.Fields.FailedStage != "" {
		for _, checkpoint := range v.Fields.StageCheckpoints {
			if checkpoint.Stage != v.Fields.FailedStage {
				continue
			}
			// A conclusive reconciliation checkpoint proves the formerly
			// ambiguous stage completed. Clear its transient failure projection
			// before exposing the resumed successor stage.
			v.Fields.AttemptCount = 0
			v.Fields.FailureKind = ""
			v.Fields.Retryable = false
			v.Fields.FailedStage = ""
			break
		}
	}
	// queued 只是起跑位，不产阶段产物；其余阶段成功即写检查点（规则 3）。
	if v.Record.Status != k12.GradingStageQueued {
		v.Fields.StageCheckpoints = append(v.Fields.StageCheckpoints, k12.GradingStageCheckpoint{
			Stage: v.Record.Status, ArtifactDigest: in.ArtifactDigest, RecordedAt: d.now(),
		})
		// 阶段成功：清理失败痕迹、重置当前阶段重试计数。
		v.Fields.AttemptCount = 0
		v.Fields.FailureKind = ""
		v.Fields.Retryable = false
		v.Fields.FailedStage = ""
	}
	if err := d.setGradingDeadline(ctx, v.Record.AgentName, &v.Fields, next); err != nil {
		return GradingJobView{}, err
	}
	return d.saveGradingJob(ctx, v, next)
}

func (d Deps) advanceGradingFailed(ctx context.Context, v GradingJobView, in AdvanceGradingInput) (GradingJobView, error) {
	stage := v.Record.Status
	if strings.TrimSpace(in.FailureKind) == "" {
		return GradingJobView{}, fmt.Errorf("%w: failed 必须写明 failure_kind", ErrInvalidInput)
	}
	// 规则 2：rendering 失败降级为「原图 + 文字列表」，任务继续 projecting；
	// failed_terminal 只由重试耗尽或明确不可重试错误进入，不与渲染阶段绑定。
	if stage == k12.GradingStageRendering {
		v.Fields.StageCheckpoints = append(v.Fields.StageCheckpoints, k12.GradingStageCheckpoint{
			Stage: stage, ArtifactDigest: "degraded:" + in.FailureKind, RecordedAt: d.now(), Degraded: true,
		})
		if err := d.setGradingDeadline(ctx, v.Record.AgentName, &v.Fields, k12.GradingStageProjecting); err != nil {
			return GradingJobView{}, err
		}
		return d.saveGradingJob(ctx, v, k12.GradingStageProjecting)
	}
	switch stage {
	case k12.GradingStageQueued, k12.GradingStageNormalizing, k12.GradingStageRecognizing,
		k12.GradingStageLocating, k12.GradingStageAssessing, k12.GradingStageProjecting:
	default:
		return GradingJobView{}, errGradingStageConflict("阶段 %s 无失败转移", stage)
	}
	v.Fields.AttemptCount++
	v.Fields.FailureKind = in.FailureKind
	v.Fields.Retryable = in.Retryable
	v.Fields.FailedStage = stage
	v.Fields.Deadline = 0
	v, err := d.saveGradingJob(ctx, v, k12.GradingStageFailedRetryable)
	if err != nil {
		return GradingJobView{}, err
	}
	// 规则 4：达阶段重试上限或不可重试错误 → failed_terminal，不允许无限循环。
	if !in.Retryable || v.Fields.AttemptCount >= k12.GradingMaxStageAttempts {
		v.Fields.Retryable = false
		return d.saveGradingJob(ctx, v, k12.GradingStageFailedTerminal)
	}
	return v, nil
}

func (d Deps) advanceGradingAnchor(ctx context.Context, v GradingJobView, in AdvanceGradingInput) (GradingJobView, error) {
	switch in.AnchorState {
	case k12.GradingAnchorLocated, k12.GradingAnchorDegraded:
	default:
		return GradingJobView{}, fmt.Errorf("%w: anchor_state 只能回位 located/degraded", ErrInvalidInput)
	}
	stage := v.Record.Status
	if stage != k12.GradingStageAwaitingConfirmation && stage != k12.GradingStageLocating {
		return GradingJobView{}, errGradingStageConflict("阶段 %s 无锚点回位语义", stage)
	}
	v.Fields.AnchorState = in.AnchorState
	v.Fields.StageCheckpoints = append(v.Fields.StageCheckpoints, k12.GradingStageCheckpoint{
		Stage: k12.GradingStageLocating, ArtifactDigest: in.ArtifactDigest, RecordedAt: d.now(),
		Degraded: in.AnchorState == k12.GradingAnchorDegraded, // 超时降级：该题走 §4.9 文字结果降级
	})
	// 规则 1 汇合：确认已冻结 canonical 输入且锚点已回位（located 或 degraded）→ assessing。
	next := stage
	if gradingJoinReady(v.Fields) {
		next = k12.GradingStageAssessing
		if err := d.setGradingDeadline(ctx, v.Record.AgentName, &v.Fields, next); err != nil {
			return GradingJobView{}, err
		}
	}
	return d.saveGradingJob(ctx, v, next)
}

// ConfirmGradingJob 批量确认/修正识别结果（§6.7 公共命令③，completed 前的等待态确认）。
// corrections 为家长的逐题确认/修正输入摘要；确认冻结 canonical 输入并写确认检查点。
// 规则 1：进入 assessing 须与锚点汇合——锚点未回位（pending）时保持等待，
// 锚点超时会显式 degraded（不阻塞）；只有 confirmed ∧ (located ∨ degraded) 才能批改。
func (d Deps) ConfirmGradingJob(ctx context.Context, agentName, recordID string, corrections []string) (GradingJobView, error) {
	v, err := d.GetGradingJob(ctx, agentName, recordID)
	if err != nil {
		return GradingJobView{}, err
	}
	if v.Record.Status != k12.GradingStageAwaitingConfirmation {
		return GradingJobView{}, errGradingStageConflict("阶段 %s 不可确认（仅 awaiting_confirmation）", v.Record.Status)
	}
	v.Fields.ConfirmationState = k12.GradingConfirmationConfirmed
	v.Fields.StageCheckpoints = append(v.Fields.StageCheckpoints, k12.GradingStageCheckpoint{
		Stage:          k12.GradingStageAwaitingConfirmation,
		ArtifactDigest: gradingCorrectionsDigest(corrections),
		RecordedAt:     d.now(),
	})
	resumeParentAutomaticWindow(&v.Fields, d.now())
	next := v.Record.Status
	if gradingJoinReady(v.Fields) {
		next = k12.GradingStageAssessing
		// §5.4：确认恢复后按剩余阶段预算重新起算 deadline。
		if err := d.setGradingDeadline(ctx, v.Record.AgentName, &v.Fields, next); err != nil {
			return GradingJobView{}, err
		}
	}
	return d.saveGradingJob(ctx, v, next)
}

// gradingJoinReady 是确认分支与定位分支的唯一汇合判定。显式列出终止锚点态，避免
// 未知/损坏值被“非 pending”宽松判断误放行到 assessing。
func gradingJoinReady(fields k12.GradingJobFields) bool {
	if fields.ConfirmationState != k12.GradingConfirmationConfirmed {
		return false
	}
	switch fields.AnchorState {
	case k12.GradingAnchorLocated, k12.GradingAnchorDegraded:
		return true
	default:
		return false
	}
}

// ReviseGradingJob 修正重批（§6.7 规则 6）：completed 之后家长修改确认输入 → 不回退旧 Job，
// 携带 confirmed_version+1 创建新 GradingJob（同 submission_id），复用已固化的识别与定位
// 检查点，走状态机 queued→assessing 捷径起跑。同一修正重复提交幂等命中既有新 Job。
//
// TODO(Assessment 聚合)：旧 Assessment/Evidence 标记 is_current=false 在 Assessment 聚合
// 落库（§6.9 k12_assessments，本批未建，见切换申报）时同事务执行；当前 Assessment 尚未持久化，无可标记对象。
func (d Deps) ReviseGradingJob(ctx context.Context, agentName, recordID, sourceSession string, corrections []string) (GradingJobView, bool, error) {
	old, err := d.GetGradingJob(ctx, agentName, recordID)
	if err != nil {
		return GradingJobView{}, false, err
	}
	if old.Record.Status != k12.GradingStageCompleted {
		return GradingJobView{}, false, errGradingStageConflict("仅 completed 后可修正重批（当前 %s；等待态修正走 confirm）", old.Record.Status)
	}
	// 幂等键来源维度复用旧 Job 的 source_kind + 原始来源键（从旧键剥出），版本 +1。
	sourceKey := gradingSourceKeyFromIdempotencyKey(old.Fields)
	newVersion := old.Fields.ConfirmedVersion + 1
	f := k12.GradingJobFields{
		SubmissionID:      old.Fields.SubmissionID,
		SourceKind:        old.Fields.SourceKind,
		IdempotencyKey:    k12.BuildGradingIdempotencyKey(old.Fields.SourceKind, sourceKey, newVersion),
		ConfirmedVersion:  newVersion,
		ConfirmationState: k12.GradingConfirmationConfirmed, // 修正即新确认输入已冻结
		AnchorState:       old.Fields.AnchorState,           // 复用已固化定位结论（located/degraded）
		Deadline:          0,
		ModelSnapshot:     old.Fields.ModelSnapshot,
		BudgetSnapshot:    old.Fields.BudgetSnapshot,
	}
	queuedBudget, ok := gradingBudgetSeconds(f.BudgetSnapshot, k12.GradingStageQueued, 0)
	if !ok {
		return GradingJobView{}, false, fmt.Errorf("%w: missing queued grading budget", ErrInvalidInput)
	}
	f.Deadline = d.now() + queuedBudget
	// 检查点预置：复用识别/定位产物 + 新确认输入摘要 → GradingResumeStage 直达 assessing。
	for _, cp := range old.Fields.StageCheckpoints {
		switch cp.Stage {
		case k12.GradingStageNormalizing, k12.GradingStageRecognizing, k12.GradingStageLocating:
			f.StageCheckpoints = append(f.StageCheckpoints, cp)
		}
	}
	f.StageCheckpoints = append(f.StageCheckpoints, k12.GradingStageCheckpoint{
		Stage:          k12.GradingStageAwaitingConfirmation,
		ArtifactDigest: gradingCorrectionsDigest(corrections),
		RecordedAt:     d.now(),
	})
	return d.putGradingJob(ctx, agentName, sourceSession, f)
}

// CancelGradingJob 取消（§6.7 公共命令④）。规则 5：rendering/projecting 是不可取消的
// 短收尾阶段，其余阶段（含 awaiting_confirmation，规则 7 家长可随时显式取消）均可取消。
// 取消传播语义：编排器接线后必须把取消传播到在途模型调用（阶段 context 派生自任务 context，
// cancelled 落库即 cancel 该 context）；已产生的阶段检查点保留可追溯——本命令不清空 checkpoints。
func (d Deps) CancelGradingJob(ctx context.Context, agentName, recordID string) (GradingJobView, error) {
	v, err := d.GetGradingJob(ctx, agentName, recordID)
	if err != nil {
		return GradingJobView{}, err
	}
	if !k12.GradingStageCancellable(v.Record.Status) {
		return GradingJobView{}, errGradingStageConflict("阶段 %s 不可取消（规则 5：rendering/projecting 为不可取消收尾阶段）", v.Record.Status)
	}
	v.Fields.Deadline = 0
	return d.saveGradingJob(ctx, v, k12.GradingStageCancelled)
}

// RetryGradingJob 安全重试（§6.7 公共命令④）：仅 failed_retryable 且 retryable=true 可重试，
// 回 queued 从最近成功阶段的检查点恢复（规则 3；恢复目标 = GradingResumeStage，由编排器
// 经 AdvanceGradingStage(ok) 起跑）。重试上限在失败落库时已收敛 failed_terminal（规则 4）。
func (d Deps) RetryGradingJob(ctx context.Context, agentName, recordID string) (GradingJobView, error) {
	v, err := d.GetGradingJob(ctx, agentName, recordID)
	if err != nil {
		return GradingJobView{}, err
	}
	if v.Record.Status != k12.GradingStageFailedRetryable {
		return GradingJobView{}, errGradingStageConflict("阶段 %s 不可重试（仅 failed_retryable）", v.Record.Status)
	}
	if !v.Fields.Retryable {
		return GradingJobView{}, errGradingStageConflict("该失败不可安全重试（failure_kind=%s）", v.Fields.FailureKind)
	}
	if err := d.setGradingDeadline(ctx, agentName, &v.Fields, k12.GradingStageQueued); err != nil {
		return GradingJobView{}, err
	}
	return d.saveGradingJob(ctx, v, k12.GradingStageQueued)
}

func gradingInteractiveDeadlineRetryEligible(v GradingJobView) bool {
	return v.Record.Status == k12.GradingStageFailedRetryable &&
		v.Fields.Retryable &&
		v.Fields.FailureKind == gradingFailureInteractiveDeadlineExceeded
}

func (d Deps) RetryGradingJobWithParentAutomaticWindow(
	ctx context.Context,
	agentName, recordID, parentAutomaticAttemptID string,
	parentAutomaticDeadlineAt int64,
) (GradingJobView, error) {
	v, err := d.GetGradingJob(ctx, agentName, recordID)
	if err != nil {
		return GradingJobView{}, err
	}
	if !gradingInteractiveDeadlineRetryEligible(v) {
		return GradingJobView{}, errGradingStageConflict(
			"阶段 %s/failure_kind=%s 不可刷新 parent automatic window",
			v.Record.Status, v.Fields.FailureKind,
		)
	}
	parentAutomaticAttemptID = strings.TrimSpace(parentAutomaticAttemptID)
	now := d.now()
	if parentAutomaticAttemptID == "" || parentAutomaticAttemptID == v.Fields.ParentAutomaticAttemptID ||
		parentAutomaticDeadlineAt <= now {
		return GradingJobView{}, fmt.Errorf(
			"%w: fresh parent automatic attempt identity/deadline required", ErrInvalidInput,
		)
	}
	v.Fields.ParentAutomaticAttemptID = parentAutomaticAttemptID
	v.Fields.ParentAutomaticDeadlineAt = parentAutomaticDeadlineAt
	v.Fields.ParentAutomaticRemainingSeconds = parentAutomaticDeadlineAt - now
	if err := d.setGradingDeadline(ctx, agentName, &v.Fields, k12.GradingStageQueued); err != nil {
		return GradingJobView{}, err
	}
	return d.saveGradingJob(ctx, v, k12.GradingStageQueued)
}

// ReconcileGradingInvocationNotExecuted is an operator/application-service
// command, not the parent-facing ordinary retry action. Provider evidence must
// prove the ambiguous request did not execute before this command exposes a
// same-route retryable state.
func (d Deps) ReconcileGradingInvocationNotExecuted(ctx context.Context, agentName, recordID, invocationID string) (GradingJobView, error) {
	v, err := d.GetGradingJob(ctx, agentName, recordID)
	if err != nil {
		return GradingJobView{}, err
	}
	invocation, err := d.Records.GetModelInvocation(ctx, agentName, invocationID)
	if err != nil {
		return GradingJobView{}, err
	}
	if invocation.JobID != recordID || invocation.Stage != v.Fields.FailedStage {
		return GradingJobView{}, errGradingStageConflict("invocation 不属于当前 Job/失败阶段")
	}
	if v.Record.Status == k12.GradingStageFailedRetryable &&
		invocation.Status == k12.ModelInvocationReconciled && invocation.FailureKind == "reconciled_not_executed" {
		return v, nil
	}
	if v.Record.Status != k12.GradingStageOutcomeUnknown {
		return GradingJobView{}, errGradingStageConflict("阶段 %s 不可执行模型调用对账", v.Record.Status)
	}
	if invocation.Status == k12.ModelInvocationOutcomeUnknown {
		invocation, err = d.Records.ReconcileModelInvocationNotExecuted(ctx, agentName, invocationID)
		if err != nil {
			return GradingJobView{}, err
		}
	}
	if invocation.Status != k12.ModelInvocationReconciled || invocation.FailureKind != "reconciled_not_executed" {
		return GradingJobView{}, errGradingStageConflict("invocation %s 尚未证实未执行", invocationID)
	}
	v.Fields.AttemptCount = invocation.Attempt
	v.Fields.FailureKind = "reconciled_not_executed"
	v.Fields.Retryable = true
	v.Fields.Deadline = 0
	return d.saveGradingJob(ctx, v, k12.GradingStageFailedRetryable)
}

// ReconcileGradingInvocationSucceeded is an internal recovery command for the
// opposite conclusive outcome: the exact provider result is already durable
// locally, but the Job was parked because its acknowledgement/checkpoint write
// was interrupted. It records the frozen stage checkpoint and exposes only a
// checkpoint replay; it never authorizes another provider request.
func (d Deps) ReconcileGradingInvocationSucceeded(
	ctx context.Context,
	agentName, recordID, invocationID, resultDigest, artifactDigest, externalRequestID string,
) (GradingJobView, error) {
	if d.Records == nil || strings.TrimSpace(resultDigest) == "" {
		return GradingJobView{}, fmt.Errorf("%w: durable model result evidence is required", ErrInvalidInput)
	}
	v, err := d.GetGradingJob(ctx, agentName, recordID)
	if err != nil {
		return GradingJobView{}, err
	}
	invocation, err := d.Records.GetModelInvocation(ctx, agentName, invocationID)
	if err != nil {
		return GradingJobView{}, err
	}
	if invocation.JobID != recordID || invocation.Stage != v.Fields.FailedStage {
		return GradingJobView{}, errGradingStageConflict("invocation 不属于当前 Job/失败阶段")
	}
	wantRoute := k12.NormalizeGradingModelSnapshot(v.Fields.ModelSnapshot)
	gotRoute := k12.NormalizeGradingModelSnapshot(invocation.RouteSnapshot)
	if gotRoute.Provider != wantRoute.Provider || gotRoute.Model != wantRoute.Model ||
		gotRoute.Route != wantRoute.Route {
		return GradingJobView{}, errGradingStageConflict("invocation 模型路由与冻结 Job 不一致")
	}
	if v.Record.Status == k12.GradingStageFailedRetryable &&
		invocation.Status == k12.ModelInvocationReconciled &&
		invocation.FailureKind == "reconciled_succeeded" &&
		invocation.ResultDigest == resultDigest {
		return v, nil
	}
	if v.Record.Status != k12.GradingStageOutcomeUnknown {
		return GradingJobView{}, errGradingStageConflict("阶段 %s 不可执行成功结果对账", v.Record.Status)
	}
	switch invocation.Status {
	case k12.ModelInvocationOutcomeUnknown:
		invocation, err = d.Records.ReconcileModelInvocationSucceeded(
			ctx, agentName, invocationID, resultDigest, externalRequestID,
		)
	case k12.ModelInvocationSent:
		invocation, err = d.Records.MarkModelInvocationSucceeded(
			ctx, agentName, invocationID, resultDigest, externalRequestID,
		)
	case k12.ModelInvocationSucceeded, k12.ModelInvocationReconciled:
		// Validate the already-written conclusive ledger row below.
	default:
		return GradingJobView{}, errGradingStageConflict(
			"invocation %s 状态 %s 不能按成功结果对账", invocationID, invocation.Status,
		)
	}
	if err != nil {
		return GradingJobView{}, err
	}
	if invocation.ResultDigest != resultDigest ||
		(invocation.Status != k12.ModelInvocationSucceeded &&
			(invocation.Status != k12.ModelInvocationReconciled ||
				invocation.FailureKind != "reconciled_succeeded")) {
		return GradingJobView{}, errGradingStageConflict("invocation %s 的持久结果证据不一致", invocationID)
	}
	checkpointReady := false
	for _, checkpoint := range v.Fields.StageCheckpoints {
		if checkpoint.Stage != invocation.Stage {
			continue
		}
		if checkpoint.ArtifactDigest != artifactDigest {
			return GradingJobView{}, errGradingStageConflict("阶段 %s 已存在不同结果检查点", invocation.Stage)
		}
		checkpointReady = true
		break
	}
	if !checkpointReady {
		v.Fields.StageCheckpoints = append(v.Fields.StageCheckpoints, k12.GradingStageCheckpoint{
			Stage: invocation.Stage, ArtifactDigest: artifactDigest, RecordedAt: d.now(),
		})
	}
	// failed_retryable is a recoverable state-machine bridge only. Recovery
	// immediately returns it to queued, whose checkpoint replay skips the
	// already-completed provider stage.
	v.Fields.AttemptCount = 0
	v.Fields.FailureKind = "reconciled_succeeded"
	v.Fields.Retryable = true
	v.Fields.Deadline = 0
	return d.saveGradingJob(ctx, v, k12.GradingStageFailedRetryable)
}

// --- 内部辅助 ---

// setGradingDeadline reads only the immutable Job snapshot for a frozen Job.
// Assessing selects its measured bucket from the durable Attempt count; legacy
// policy_version=0 keeps the historical compatibility budgets.
func (d Deps) setGradingDeadline(ctx context.Context, agentName string, f *k12.GradingJobFields, next string) error {
	now := d.now()
	if next == k12.GradingStageAwaitingConfirmation {
		pauseParentAutomaticWindow(f, now)
		f.Deadline = 0
		return nil
	}
	problemCount := 0
	if f.BudgetSnapshot.IsFrozen() && next == k12.GradingStageAssessing {
		snapshot, err := d.Records.GetProblemAttemptSnapshot(ctx, agentName, f.SubmissionID)
		if err != nil {
			return fmt.Errorf("usecase: read assessing item count: %w", err)
		}
		problemCount = len(snapshot.Attempts)
	}
	budget, ok := gradingBudgetSeconds(f.BudgetSnapshot, next, problemCount)
	if !ok {
		if k12.GradingStageTerminal(next) || next == k12.GradingStageAwaitingConfirmation {
			f.Deadline = 0
			return nil
		}
		return fmt.Errorf("%w: no grading budget for stage=%s problems=%d", ErrInvalidInput, next, problemCount)
	}
	reserveImageTaskAssessingParentAutomaticWindow(f, now, next, budget)
	f.Deadline = now + budget
	if strings.TrimSpace(f.ParentAutomaticAttemptID) != "" {
		if f.ParentAutomaticDeadlineAt <= 0 {
			f.Deadline = now
		} else if f.ParentAutomaticDeadlineAt < f.Deadline {
			f.Deadline = f.ParentAutomaticDeadlineAt
		}
	}
	return nil
}

// reserveImageTaskAssessingParentAutomaticWindow starts the durable child
// automatic window at the only boundary where the exact Problem/Attempt count
// is known: confirmed image-task assessment.  The ingress 300-second window
// remains authoritative through recognizing/locating; after confirmation a
// frozen policy's selected assessing bucket is the shared ceiling for the Job
// deadline, physical calls, and before-send expiry checks.
func reserveImageTaskAssessingParentAutomaticWindow(
	f *k12.GradingJobFields,
	now int64,
	next string,
	assessingBudget int64,
) {
	if f == nil || next != k12.GradingStageAssessing ||
		f.SourceKind != "image_task" || !f.BudgetSnapshot.IsFrozen() ||
		strings.TrimSpace(f.ParentAutomaticAttemptID) == "" || assessingBudget <= 0 {
		return
	}
	f.ParentAutomaticRemainingSeconds = assessingBudget
	f.ParentAutomaticDeadlineAt = now + assessingBudget
}

func pauseParentAutomaticWindow(f *k12.GradingJobFields, now int64) {
	if f == nil || strings.TrimSpace(f.ParentAutomaticAttemptID) == "" ||
		f.ParentAutomaticDeadlineAt <= 0 {
		return
	}
	remaining := f.ParentAutomaticDeadlineAt - now
	if remaining < 0 {
		remaining = 0
	}
	if f.ParentAutomaticRemainingSeconds > 0 &&
		remaining > f.ParentAutomaticRemainingSeconds {
		remaining = f.ParentAutomaticRemainingSeconds
	}
	f.ParentAutomaticRemainingSeconds = remaining
	f.ParentAutomaticDeadlineAt = 0
}

func resumeParentAutomaticWindow(f *k12.GradingJobFields, now int64) {
	if f == nil || strings.TrimSpace(f.ParentAutomaticAttemptID) == "" ||
		f.ParentAutomaticDeadlineAt > 0 {
		return
	}
	f.ParentAutomaticDeadlineAt = now + f.ParentAutomaticRemainingSeconds
}

func parentAutomaticDeadlineExceeded(f k12.GradingJobFields, now int64) bool {
	return strings.TrimSpace(f.ParentAutomaticAttemptID) != "" &&
		f.ParentAutomaticDeadlineAt > 0 &&
		f.ParentAutomaticDeadlineAt <= now
}

func gradingBudgetSeconds(snapshot k12.GradingBudgetSnapshot, stage string, problemCount int) (int64, bool) {
	if snapshot.IsFrozen() {
		return snapshot.StageBudgetSeconds(stage, problemCount)
	}
	if snapshot.Validate() != nil {
		return 0, false
	}
	budget := k12.GradingStageBudgetSeconds(stage)
	return budget, budget > 0
}

// saveGradingJob 乐观锁写回（状态机合法性由 records schema Transitions 强制）。
func (d Deps) saveGradingJob(ctx context.Context, v GradingJobView, newStatus string) (GradingJobView, error) {
	raw, err := json.Marshal(v.Fields)
	if err != nil {
		return GradingJobView{}, fmt.Errorf("usecase: marshal 批改任务字段: %w", err)
	}
	if err := d.Records.UpdateStatusFields(ctx, v.Record.RecordID, newStatus, v.Record.DueAt, string(raw), v.Record.Version); err != nil {
		return GradingJobView{}, fmt.Errorf("usecase: 批改任务写回: %w", err)
	}
	v.Record.Status = newStatus
	v.Record.Fields = string(raw)
	v.Record.Version++
	return v, nil
}

// gradingCorrectionsDigest 家长确认/修正输入摘要（确认检查点产物；空 = 全部按识别结果确认）。
func gradingCorrectionsDigest(corrections []string) string {
	if len(corrections) == 0 {
		return "confirmed-as-recognized"
	}
	sum := sha1.Sum([]byte(strings.Join(corrections, "\n")))
	return "confirmed:" + hex.EncodeToString(sum[:])
}

// gradingSourceKeyFromIdempotencyKey 从既有幂等键剥出原始来源键（BuildGradingIdempotencyKey 逆向）。
func gradingSourceKeyFromIdempotencyKey(f k12.GradingJobFields) string {
	prefix := f.SourceKind + "|"
	suffix := fmt.Sprintf("|v%d", f.ConfirmedVersion)
	key := strings.TrimPrefix(f.IdempotencyKey, prefix)
	return strings.TrimSuffix(key, suffix)
}
