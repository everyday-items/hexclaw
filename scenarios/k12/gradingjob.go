package k12

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
)

// CollectionGradingJob 统一批改任务记录集名（架构设计 §6.7 / §5.4 GradingJob）。
//
// 底座说明：本聚合落 k12_grading_jobs 类型化表（§6.9，迁移 V9）——状态机仍由
// RecordSchema.Transitions 声明（六缝契约面不变），§5.4 领域字段拍平为 typed 列，
// 乐观锁用记录 version，幂等键作 dedupe_key（§4.10 同一幂等键只产生一个 GradingJob）。
const CollectionGradingJob = "批改任务"

// GradingJob 阶段（§6.7 状态机图节点）。stage 即记录 status。
const (
	GradingStageQueued               = "queued"
	GradingStageNormalizing          = "normalizing"
	GradingStageRecognizing          = "recognizing"
	GradingStageLocating             = "locating" // 锚点增强并行分支（规则 1）；主链常驻 awaiting_confirmation + anchor_state 正交字段
	GradingStageAwaitingConfirmation = "awaiting_confirmation"
	GradingStageAssessing            = "assessing"
	GradingStageRendering            = "rendering"
	GradingStageProjecting           = "projecting"
	GradingStageCompleted            = "completed"
	GradingStageCancelled            = "cancelled"
	GradingStageFailedRetryable      = "failed_retryable"
	GradingStageFailedTerminal       = "failed_terminal"
)

// 等待态正交拆分（2026-07-18 裁决，外部评审 #4a）：awaiting_confirmation 阶段内
// 「等家长确认」与「等锚点定位」是两个正交子态，防止未确认即批改 / 锚点无限等待 / 崩溃恢复错位。
const (
	// confirmation_state：批改执行的前置条件只看它 = confirmed。
	GradingConfirmationPending   = "pending"
	GradingConfirmationConfirmed = "confirmed"
	// anchor_state：锚点定位超时（默认 60s）显式 degraded，走 §4.9 无坐标降级（该题只出文字结果），
	// 不阻塞文字批改。枚举取值以 §6.7 拆分裁决为准：pending / located / degraded。
	GradingAnchorPending  = "pending"
	GradingAnchorLocated  = "located"
	GradingAnchorDegraded = "degraded"
)

// GradingAnchorTimeoutSeconds 锚点定位默认超时（§6.7 拆分裁决 60s）。
// 超时判定与 degraded 落库由编排器执行（下一轮接线），领域只固化常量与降级语义。
const GradingAnchorTimeoutSeconds = 60

// GradingMaxStageAttempts 每阶段重试上限（规则 4：达上限进 failed_terminal，不允许无限循环）。
// 工程默认值；将来若各阶段需差异化上限，在此扩展为按阶段映射。
const GradingMaxStageAttempts = 3

// GradingModelSnapshot 实际模型路由快照（§5.4 model_snapshot：provider/model/capability/timeout/fallback）。
type GradingModelSnapshot struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Capability string `json:"capability,omitempty"`
	TimeoutMS  int    `json:"timeout_ms,omitempty"`
	Fallback   string `json:"fallback,omitempty"`
}

// GradingStageCheckpoint 已完成阶段的产物摘要（§5.4 stage_checkpoints）。
// 规则 3：每个阶段完成即持久化摘要，失败重试从最近成功阶段恢复，禁止重跑已成功阶段/重复调模型。
type GradingStageCheckpoint struct {
	Stage          string `json:"stage"`
	ArtifactDigest string `json:"artifact_digest,omitempty"` // 阶段产物摘要（输入输出摘要）
	RecordedAt     int64  `json:"recorded_at,omitempty"`     // unix 秒
	Degraded       bool   `json:"degraded,omitempty"`        // 规则 2：rendering 失败降级续跑时 true
}

// GradingJobFields GradingJob 领域字段（§5.4 字段表 + 等待态正交拆分两字段）。
// job_id = 记录 record_id；stage = 记录 status；乐观锁 = 记录 version。
type GradingJobFields struct {
	SubmissionID      string                   `json:"submission_id"`
	SourceKind        string                   `json:"source_kind"` // 创建来源：desktop request_id / http idempotency_key / im message_id / workflow execution_id（§4.10）
	IdempotencyKey    string                   `json:"idempotency_key"`
	ConfirmedVersion  int                      `json:"confirmed_version"` // 家长确认版本；修正 +1 产新 Job（规则 6）
	ConfirmationState string                   `json:"confirmation_state"`
	AnchorState       string                   `json:"anchor_state"`
	Deadline          int64                    `json:"deadline,omitempty"` // 当前自动阶段截止（unix 秒）；awaiting_confirmation 人工等待不计入 = 0（规则 7）
	ModelSnapshot     GradingModelSnapshot     `json:"model_snapshot"`
	StageCheckpoints  []GradingStageCheckpoint `json:"stage_checkpoints,omitempty"`
	AttemptCount      int                      `json:"attempt_count,omitempty"` // 当前阶段重试计数（规则 4）
	FailureKind       string                   `json:"failure_kind,omitempty"`  // 仅失败态有值
	Retryable         bool                     `json:"retryable,omitempty"`     // 是否安全重试（由阶段和错误决定）
	FailedStage       string                   `json:"failed_stage,omitempty"`  // 失败发生的自动阶段（审计/恢复取证）
}

// BuildGradingIdempotencyKey 统一批改幂等键（§4.10）：创建来源 + 来源键 + confirmed_version。
// 修正确认输入后 confirmed_version+1 → 新键新 Job，不复用旧结论（规则 6）。
func BuildGradingIdempotencyKey(sourceKind, sourceKey string, confirmedVersion int) string {
	return fmt.Sprintf("%s|%s|v%d", sourceKind, sourceKey, confirmedVersion)
}

// GradingStageCancellable 可取消态判定（规则 5）：rendering/projecting 是不可取消的短收尾阶段；
// awaiting_confirmation 无自动过期，但家长可随时显式取消（规则 7）。
func GradingStageCancellable(stage string) bool {
	switch stage {
	case GradingStageQueued, GradingStageNormalizing, GradingStageRecognizing,
		GradingStageLocating, GradingStageAwaitingConfirmation, GradingStageAssessing:
		return true
	}
	return false
}

// GradingStageTerminal 终态判定。
func GradingStageTerminal(stage string) bool {
	switch stage {
	case GradingStageCompleted, GradingStageCancelled, GradingStageFailedTerminal:
		return true
	}
	return false
}

// gradingResumeOrder 主链检查点 → 后继阶段（规则 3 恢复；locating 是并行增强不占主链）。
var gradingResumeOrder = []string{
	GradingStageNormalizing, GradingStageRecognizing,
	GradingStageAwaitingConfirmation, GradingStageAssessing,
	GradingStageRendering, GradingStageProjecting,
}

// GradingResumeStage 计算 queued 起跑目标 = 最近成功主链阶段的后继（规则 3/6）：
// 无检查点从 normalizing 从头跑；确认检查点已预置（修正重批）则直达 assessing 捷径；
// 禁止重跑已成功阶段或重复调用模型。
func GradingResumeStage(checkpoints []GradingStageCheckpoint) string {
	done := map[string]bool{}
	for _, cp := range checkpoints {
		done[cp.Stage] = true
	}
	next := GradingStageNormalizing
	for i, s := range gradingResumeOrder {
		if !done[s] {
			break
		}
		if i+1 < len(gradingResumeOrder) {
			next = gradingResumeOrder[i+1]
		} else {
			next = GradingStageCompleted
		}
	}
	return next
}

// GradingStageBudgetSeconds 各自动阶段预算（§5.4 deadline 按阶段预算计算）。工程默认值；
// awaiting_confirmation 返回 0 = 人工等待不计入（规则 7），确认恢复后按剩余阶段预算重新起算。
func GradingStageBudgetSeconds(stage string) int64 {
	switch stage {
	case GradingStageQueued:
		return 60
	case GradingStageNormalizing:
		return 60
	case GradingStageRecognizing:
		return 120
	case GradingStageLocating:
		return GradingAnchorTimeoutSeconds
	case GradingStageAssessing:
		return 180
	case GradingStageRendering, GradingStageProjecting:
		return 60
	}
	return 0
}

// GradingJobSchema 返回批改任务记录集 schema（注册进 RecordSchemaRegistry）。
//
// Transitions 逐边对照 §6.7 状态机图；额外的 queued→{recognizing,locating,awaiting_confirmation}
// 恢复边由规则 3 授权（failed_retryable→queued 后从最近成功阶段的后继起跑，
// 图中 queued→assessing 捷径即其中修正重批/批改阶段恢复的特例）。
// 规则 2：rendering 无 failed_* 出边——失败降级续跑 projecting，不进失败态。
// 规则 5：rendering/projecting 无 cancelled 出边。
// 规则 6：completed 无出边——修正 = confirmed_version+1 的新 Job。
// 规则 7：awaiting_confirmation 无自动过期出边，仅 assessing（汇合）与 cancelled（显式取消）。
func GradingJobSchema() *records.RecordSchema {
	return &records.RecordSchema{
		Collection:    CollectionGradingJob,
		Version:       1,
		InitialStatus: GradingStageQueued,
		Statuses: []string{
			GradingStageQueued, GradingStageNormalizing, GradingStageRecognizing,
			GradingStageLocating, GradingStageAwaitingConfirmation, GradingStageAssessing,
			GradingStageRendering, GradingStageProjecting, GradingStageCompleted,
			GradingStageCancelled, GradingStageFailedRetryable, GradingStageFailedTerminal,
		},
		Transitions: map[string][]string{
			GradingStageQueued: {
				GradingStageNormalizing,
				// 规则 3 检查点恢复边 + 规则 6 修正重批捷径（assessing）
				GradingStageRecognizing, GradingStageLocating,
				GradingStageAwaitingConfirmation, GradingStageAssessing,
				GradingStageCancelled,
			},
			GradingStageNormalizing: {GradingStageRecognizing, GradingStageCancelled, GradingStageFailedRetryable},
			GradingStageRecognizing: {GradingStageAwaitingConfirmation, GradingStageLocating, GradingStageCancelled, GradingStageFailedRetryable},
			GradingStageLocating:    {GradingStageAssessing, GradingStageCancelled, GradingStageFailedRetryable},
			GradingStageAwaitingConfirmation: {
				GradingStageAssessing, // 与 locating 汇合后进入（规则 1）
				GradingStageCancelled,
			},
			GradingStageAssessing:       {GradingStageRendering, GradingStageCancelled, GradingStageFailedRetryable},
			GradingStageRendering:       {GradingStageProjecting},
			GradingStageProjecting:      {GradingStageCompleted, GradingStageFailedRetryable},
			GradingStageFailedRetryable: {GradingStageQueued, GradingStageFailedTerminal},
			GradingStageCompleted:       {},
			GradingStageCancelled:       {},
			GradingStageFailedTerminal:  {},
		},
		DedupeKey:      gradingJobDedupeKey,
		ValidateFields: validateGradingJobFields,
	}
}

// gradingJobDedupeKey 幂等键即去重键（§4.10 同一幂等键只产生一个 GradingJob）。
func gradingJobDedupeKey(r *records.AgentRecord) string {
	var f GradingJobFields
	_ = json.Unmarshal([]byte(r.Fields), &f)
	return f.IdempotencyKey
}

func validateGradingJobFields(fieldsJSON string) error {
	var f GradingJobFields
	if err := json.Unmarshal([]byte(fieldsJSON), &f); err != nil {
		return fmt.Errorf("批改任务字段非法 JSON: %w", err)
	}
	if strings.TrimSpace(f.SubmissionID) == "" {
		return fmt.Errorf("批改任务缺少 submission_id")
	}
	if strings.TrimSpace(f.IdempotencyKey) == "" {
		return fmt.Errorf("批改任务缺少 idempotency_key")
	}
	if strings.TrimSpace(f.SourceKind) == "" {
		return fmt.Errorf("批改任务缺少 source_kind（创建来源，§4.10）")
	}
	switch f.ConfirmationState {
	case GradingConfirmationPending, GradingConfirmationConfirmed:
	default:
		return fmt.Errorf("confirmation_state 非法: %q（允许 pending/confirmed）", f.ConfirmationState)
	}
	switch f.AnchorState {
	case GradingAnchorPending, GradingAnchorLocated, GradingAnchorDegraded:
	default:
		return fmt.Errorf("anchor_state 非法: %q（允许 pending/located/degraded）", f.AnchorState)
	}
	if f.ConfirmedVersion < 0 {
		return fmt.Errorf("confirmed_version 不可为负: %d", f.ConfirmedVersion)
	}
	if strings.TrimSpace(f.ModelSnapshot.Provider) == "" || strings.TrimSpace(f.ModelSnapshot.Model) == "" {
		return fmt.Errorf("model_snapshot 缺少 provider/model（§5.4 不可空）")
	}
	valid := map[string]bool{}
	for _, s := range GradingJobSchema().Statuses {
		valid[s] = true
	}
	for i, cp := range f.StageCheckpoints {
		if !valid[cp.Stage] {
			return fmt.Errorf("检查点 #%d 阶段非法: %q", i, cp.Stage)
		}
	}
	return nil
}

// NewGradingJobRecord 从领域字段构造一条批改任务记录（初始 queued）。
func NewGradingJobRecord(agentName, sourceSession string, f GradingJobFields) (*records.AgentRecord, error) {
	if f.ConfirmationState == "" {
		f.ConfirmationState = GradingConfirmationPending
	}
	if f.AnchorState == "" {
		f.AnchorState = GradingAnchorPending
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("marshal 批改任务字段: %w", err)
	}
	return &records.AgentRecord{
		AgentName:     agentName,
		Collection:    CollectionGradingJob,
		Fields:        string(raw),
		Status:        GradingStageQueued,
		SourceSession: sourceSession,
	}, nil
}

// ParseGradingJobFields 解析批改任务字段。
func ParseGradingJobFields(fieldsJSON string) (GradingJobFields, error) {
	var f GradingJobFields
	err := json.Unmarshal([]byte(fieldsJSON), &f)
	return f, err
}
