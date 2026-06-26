package builtin

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// CronHealer 是 app_heal 依赖的窄能力（cron 自愈白名单）。*cron.Scheduler 结构化满足。
type CronHealer interface {
	ResumeJob(ctx context.Context, jobID string) error
	PauseJob(ctx context.Context, jobID string) error
	TriggerJob(ctx context.Context, jobID string) error
}

// WorkflowRunner 是 app_heal 触发工作流的窄能力（P2）。*api.Server 结构化满足。
type WorkflowRunner interface {
	RunWorkflowByID(id, userID string) (string, error)
}

// AppHealSkill 让 Agent 对本应用做**白名单内、可逆**的自愈写操作
// （如重试/恢复卡死的定时任务）。这是"建议→审批后自愈"的执行端。
//
// 安全（硬约束）：
//   - 仅白名单动作（cron retry/resume/pause），**不接受任意 config 改写**（LLM 幻觉改坏 config 是灾难）。
//   - 写操作 → 由 engine PermissionPolicy 的 heal-approve 规则**强制人工审批**。
//   - 每次执行 log 记录；动作可逆（resume↔pause）。
type AppHealSkill struct {
	cron CronHealer
	wf   WorkflowRunner

	// 限频（P10 #11）：workflow_run 即便已过人工审批，也要防"审批后被循环重复触发 / 误连点"
	// 导致同一工作流短时间内多次启动。按 workflow_id 记最近一次触发时刻，窗口内拒绝。
	mu      sync.Mutex
	lastRun map[string]time.Time
}

func NewAppHealSkill(cron CronHealer, wf WorkflowRunner) *AppHealSkill {
	return &AppHealSkill{cron: cron, wf: wf, lastRun: make(map[string]time.Time)}
}

// minWorkflowRunInterval 同一工作流两次 agent 触发之间的最小间隔（防循环/误连点重复启动）。
const minWorkflowRunInterval = 5 * time.Second

// rollbackHint 返回某个自愈动作的逆操作提示（写进审计 + 回给用户，便于一键撤销）。
// 不可逆动作返回 ""。
func rollbackHint(action, target string) string {
	switch action {
	case "cron_pause":
		return "app_heal cron_resume job=" + target
	case "cron_resume":
		return "app_heal cron_pause job=" + target
	default: // cron_retry / workflow_run：触发即生效，无对称逆操作
		return ""
	}
}

// auditHeal 结构化审计轨：改了什么 + 目标 + 结果 + 回滚指针 + 操作者。
// 自愈是少数会改状态的写操作，光 slog 一行 "execute" 不够取证 —— 这里固定字段，便于日后聚合/回放。
func auditHeal(ctx context.Context, action, target, outcome, rollback string) {
	slog.Info("[app_heal] audit",
		"action", action,
		"target", target,
		"outcome", outcome,
		"rollback", rollback,
		"user", skill.AuthenticatedUserID(ctx),
	)
}

// allowWorkflowRun 限频闸：窗口内同一工作流拒绝重复触发。返回 false 时调用方应报错而不执行。
func (s *AppHealSkill) allowWorkflowRun(wfID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, ok := s.lastRun[wfID]; ok && time.Since(last) < minWorkflowRunInterval {
		return false
	}
	s.lastRun[wfID] = time.Now()
	return true
}

func (s *AppHealSkill) Name() string { return "app_heal" }

func (s *AppHealSkill) Description() string {
	return "Apply a whitelisted, reversible self-heal action (e.g. retry/resume a stuck scheduled task). Requires user approval."
}

func (s *AppHealSkill) Match(string) bool { return false }

func (s *AppHealSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("app_heal",
		"Apply a SAFE, whitelisted, reversible self-heal action after diagnosing via app_query (domain=logs/cron). "+
			"Requires user approval. Only cron job remediation is supported: cron_retry (run a stuck/failed task now), "+
			"cron_resume (un-pause), cron_pause. Never edits config or credentials. Always tell the user what you will "+
			"do and why (cite the log/diagnosis) before calling.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"action": {
					Type:        "string",
					Description: "Remediation",
					Enum:        []any{"cron_retry", "cron_resume", "cron_pause", "workflow_run"},
				},
				"job_id": {
					Type:        "string",
					Description: "Target cron job id (required for cron_* actions; use app_query domain=cron to find it)",
				},
				"workflow_id": {
					Type:        "string",
					Description: "Target workflow id (required for workflow_run; use app_query domain=workflows to find it)",
				},
			},
			Required: []string{"action"},
		})
}

func (s *AppHealSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	action := cronArgString(args, "action")
	slog.Info("[app_heal] execute", "action", action, "user", skill.AuthenticatedUserID(ctx))

	if action == "workflow_run" {
		if s.wf == nil {
			return nil, fmt.Errorf("workflow run is not available")
		}
		wfID := cronArgString(args, "workflow_id")
		if wfID == "" {
			return nil, fmt.Errorf("workflow_id is required for workflow_run")
		}
		if !s.allowWorkflowRun(wfID) {
			auditHeal(ctx, action, wfID, "rate_limited", "")
			return nil, fmt.Errorf("工作流 %s 刚刚已触发，请等待 %s 后再试（防重复启动）", wfID, minWorkflowRunInterval)
		}
		runID, err := s.wf.RunWorkflowByID(wfID, skill.AuthenticatedUserID(ctx))
		if err != nil {
			auditHeal(ctx, action, wfID, "error: "+err.Error(), "")
			return nil, fmt.Errorf("workflow_run failed: %w", err)
		}
		auditHeal(ctx, action, wfID, "ok run="+runID, "")
		return &skill.Result{Content: fmt.Sprintf("已触发工作流 %s（run=%s）。", wfID, runID)}, nil
	}

	if s.cron == nil {
		return nil, fmt.Errorf("self-heal (cron) is not enabled")
	}
	jobID := cronArgString(args, "job_id")
	if jobID == "" {
		return nil, fmt.Errorf("job_id is required for cron actions")
	}
	rollback := rollbackHint(action, jobID)
	var err error
	var did string
	switch action {
	case "cron_retry":
		err, did = s.cron.TriggerJob(ctx, jobID), "已立即重试任务"
	case "cron_resume":
		err, did = s.cron.ResumeJob(ctx, jobID), "已恢复任务（可用 cron_pause 回滚）"
	case "cron_pause":
		err, did = s.cron.PauseJob(ctx, jobID), "已暂停任务（可用 cron_resume 回滚）"
	default:
		return nil, fmt.Errorf("unsupported heal action %q (cron_retry/cron_resume/cron_pause/workflow_run)", action)
	}
	if err != nil {
		auditHeal(ctx, action, jobID, "error: "+err.Error(), rollback)
		return nil, fmt.Errorf("self-heal %s failed: %w", action, err)
	}
	auditHeal(ctx, action, jobID, "ok", rollback)
	return &skill.Result{Content: fmt.Sprintf("%s（job=%s）。", did, jobID)}, nil
}
