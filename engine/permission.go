package engine

import (
	"context"
	"fmt"
	"github.com/hexagon-codes/toolkit/util/logger"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

// PermissionRequest is sent to the frontend for user approval.
type PermissionRequest struct {
	ID        string         `json:"id"`
	ToolName  string         `json:"tool_name"`
	Arguments map[string]any `json:"arguments"`
	Risk      string         `json:"risk"` // "safe" | "sensitive" | "dangerous"
	Reason    string         `json:"reason"`
}

// PermissionResponse is the user's decision.
type PermissionResponse struct {
	RequestID string `json:"request_id"`
	Approved  bool   `json:"approved"`
	Remember  bool   `json:"remember"` // "always allow this tool" for session
}

// PermissionSender pushes approval requests to the frontend.
// Implemented by WebAdapter or CLI adapter.
type PermissionSender interface {
	SendPermissionRequest(ctx context.Context, sessionID string, req *PermissionRequest) error
}

// PermissionHub manages pending approval requests and their responses.
type PermissionHub struct {
	mu      sync.Mutex
	pending map[string]chan PermissionResponse // requestID → response channel
	allowed map[string]map[string]bool         // sessionID → set of always-allowed tool names
	sender  PermissionSender
	timeout time.Duration
}

// NewPermissionHub creates a permission hub.
func NewPermissionHub(timeout time.Duration) *PermissionHub {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &PermissionHub{
		pending: make(map[string]chan PermissionResponse),
		allowed: make(map[string]map[string]bool),
		timeout: timeout,
	}
}

// SetSender sets the adapter that can push messages to the frontend.
func (h *PermissionHub) SetSender(s PermissionSender) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sender = s
}

// ClearSession removes all remembered permissions for a session.
// Should be called when a session is deleted or user disconnects.
func (h *PermissionHub) ClearSession(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.allowed, sessionID)
}

// RequestApproval sends an approval request and blocks until the user responds or timeout.
func (h *PermissionHub) RequestApproval(ctx context.Context, sessionID string, req *PermissionRequest) (bool, error) {
	// Check if this tool is already allowed for this session
	h.mu.Lock()
	if tools, ok := h.allowed[sessionID]; ok && tools[req.ToolName] {
		h.mu.Unlock()
		return true, nil
	}

	if h.sender == nil {
		h.mu.Unlock()
		// No frontend connected — use default policy (deny)
		logger.Info("[permission] no sender available, denying", "tool_name", req.ToolName)
		return false, nil
	}

	ch := make(chan PermissionResponse, 1)
	h.pending[req.ID] = ch
	sender := h.sender
	h.mu.Unlock()

	// Send request to frontend
	if err := sender.SendPermissionRequest(ctx, sessionID, req); err != nil {
		h.mu.Lock()
		delete(h.pending, req.ID)
		h.mu.Unlock()
		return false, fmt.Errorf("failed to send permission request: %w", err)
	}

	// Wait for response
	select {
	case resp := <-ch:
		if resp.Remember && resp.Approved {
			h.mu.Lock()
			if h.allowed[sessionID] == nil {
				h.allowed[sessionID] = make(map[string]bool)
			}
			h.allowed[sessionID][req.ToolName] = true
			h.mu.Unlock()
		}
		return resp.Approved, nil
	case <-time.After(h.timeout):
		h.mu.Lock()
		delete(h.pending, req.ID)
		h.mu.Unlock()
		return false, fmt.Errorf("permission request timed out after %v", h.timeout)
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.pending, req.ID)
		h.mu.Unlock()
		return false, ctx.Err()
	}
}

// HandleResponse is called when the frontend sends back an approval decision.
func (h *PermissionHub) HandleResponse(resp PermissionResponse) {
	h.mu.Lock()
	ch, ok := h.pending[resp.RequestID]
	if ok {
		delete(h.pending, resp.RequestID)
	}
	h.mu.Unlock()

	if ok {
		ch <- resp
	}
}

// PermissionHook is a BeforeToolHook that asks for user approval on sensitive/dangerous tools.
//
// v0.4.0 H2：当 feature flag tool.policy.engine 启用时，先调 PermissionPolicy.Evaluate
// 做声明式决策；命中 ActionDeny 立即拒绝，命中 ActionRequireApproval 走 PermissionHub，
// ActionAllow 直接放行。flag 关闭或 policy=nil 时退化为 classifyRisk 黑名单路径，
// 行为与 v0.3 完全一致。
type PermissionHook struct {
	hub            *PermissionHub
	dangerousTools map[string]bool // tools that always require approval
	sensitiveTools map[string]bool // tools that require approval on first use
	policy         *PermissionPolicy
}

// PermissionHookOption configures a PermissionHook.
type PermissionHookOption func(*PermissionHook)

// WithCodeExecApproval controls whether code_exec requires user approval.
// When disabled, code_exec is removed from the dangerous tools list.
func WithCodeExecApproval(require bool) PermissionHookOption {
	return func(h *PermissionHook) {
		if !require {
			delete(h.dangerousTools, "code_exec")
		}
	}
}

// WithPolicy 注入 v0.4.0 H2 PermissionPolicy；仅在 feature flag tool.policy.engine
// 开启时生效，flag 关闭仍走 classifyRisk 老路径。
func WithPolicy(p *PermissionPolicy) PermissionHookOption {
	return func(h *PermissionHook) {
		h.policy = p
	}
}

// DefaultBaselinePolicy 返回与老 classifyRisk 黑名单等价的 PermissionPolicy。
//
// 用法（cmd/hexclaw 启动时）：
//
//	hook := engine.NewPermissionHook(hub, engine.WithPolicy(engine.DefaultBaselinePolicy()))
//
// flag tool.policy.engine 开启时此 policy 生效；关闭时被忽略，老 classifyRisk 黑名单
// 继续工作。这样一行代码同时启用了 H2 + 默认安全模型。
func DefaultBaselinePolicy() *PermissionPolicy {
	return NewPermissionPolicy(ActionAllow,
		// dangerous：必须用户审批
		PolicyRule{Name: "shell-dangerous", ToolPattern: "shell", Action: ActionRequireApproval, Risk: "dangerous", Reason: "shell 命令执行"},
		PolicyRule{Name: "code-dangerous", ToolPattern: "code", Action: ActionRequireApproval, Risk: "dangerous", Reason: "代码执行"},
		PolicyRule{Name: "code-exec-dangerous", ToolPattern: "code_exec", Action: ActionRequireApproval, Risk: "dangerous", Reason: "代码执行（exec）"},
		// sensitive：首次需用户审批
		PolicyRule{Name: "browser-sensitive", ToolPattern: "browser", Action: ActionRequireApproval, Risk: "sensitive", Reason: "浏览器自动化"},
		PolicyRule{Name: "create-skill-sensitive", ToolPattern: "create_skill", Action: ActionRequireApproval, Risk: "sensitive", Reason: "创建新 Skill"},
		PolicyRule{Name: "manage-mcp-sensitive", ToolPattern: "manage_mcp_server", Action: ActionRequireApproval, Risk: "sensitive", Reason: "管理 MCP server"},
		PolicyRule{Name: "file-edit-sensitive", ToolPattern: "file_edit", Action: ActionRequireApproval, Risk: "sensitive", Reason: "文件编辑"},
		// patch_skill / manage_skill_pending（v0.4.0 F2）也归 sensitive
		PolicyRule{Name: "patch-skill-sensitive", ToolPattern: "patch_skill", Action: ActionRequireApproval, Risk: "sensitive", Reason: "修改既有 Skill"},
		PolicyRule{Name: "manage-pending-sensitive", ToolPattern: "manage_skill_pending", Action: ActionRequireApproval, Risk: "sensitive", Reason: "审批 Skill 草稿"},
	)
}

// Priority 把 PermissionHook 排在最前（before 链）—— 拒绝必须最早发生，避免
// 后续 hook 先做副作用再被否决。flag tool.lifecycle.v2 OFF 时不参与排序。
func (h *PermissionHook) Priority() int { return 10 }

// NewPermissionHook creates a permission hook.
// systemDispatchAutoApprove is the allowlist of read/collect tools any
// pre-authorized system dispatch may use without a human approver.
// Capability-mutating and dangerous tools still require interactive approval.
var systemDispatchAutoApprove = map[string]bool{
	"browser":          true,
	"knowledge_ingest": true,
	"search":           true,
	"web_search":       true,
}

// cronOnlyAutoApprove are tools auto-approved ONLY for the cron source, not for
// webhook/spawn: cron_task manages scheduled executions, which an externally
// triggered webhook or an LLM-decided spawn must not do autonomously
// (BUG-20260613 review H1).
var cronOnlyAutoApprove = map[string]bool{
	"cron_task": true,
}

func NewPermissionHook(hub *PermissionHub, opts ...PermissionHookOption) *PermissionHook {
	h := &PermissionHook{
		hub: hub,
		dangerousTools: map[string]bool{
			"shell":     true,
			"code":      true,
			"code_exec": true,
		},
		sensitiveTools: map[string]bool{
			"browser":           true,
			"create_skill":      true,
			"manage_mcp_server": true,
			"file_edit":         true,
		},
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func (h *PermissionHook) BeforeToolCall(ctx context.Context, call *ToolCallInfo) error {
	// Schedule-management tools must never run autonomously from an
	// externally-triggered webhook or an LLM-decided spawn (only from cron
	// itself, or interactively). cron_task is otherwise "safe" and ungated,
	// so this explicit guard precedes risk classification (review H1).
	if src := systemDispatchSource(ctx); src != "" && src != "cron" && cronOnlyAutoApprove[call.Name] {
		logger.Warn("[permission] schedule-management tool denied for non-cron system dispatch",
			"tool_name", call.Name, "source", src)
		return fmt.Errorf("tool %q cannot run autonomously from %s", call.Name, src)
	}

	// v0.4.0 H2: feature-flag-gated PolicyEngine 优先
	if h.policy != nil && featureflag.Enabled(ctx, FlagToolPolicyEngine) {
		dec := h.policy.Evaluate(call)
		switch dec.Action {
		case ActionAllow:
			return nil
		case ActionDeny:
			logger.Info("[permission] policy deny",
				"tool", call.Name, "rule", dec.MatchedRule, "reason", dec.Reason)
			reason := dec.Reason
			if reason == "" {
				reason = "policy denies execution"
			}
			return fmt.Errorf("tool %q blocked by policy %q: %s", call.Name, dec.MatchedRule, reason)
		case ActionRequireApproval:
			risk := dec.Risk
			if risk == "" {
				risk = "sensitive"
			}
			reason := dec.Reason
			if reason == "" {
				reason = fmt.Sprintf("Agent wants to execute %s(%s)", call.Name, summarizeArgs(call.Arguments))
			}
			return h.requestApproval(ctx, call, risk, reason)
		default:
			logger.Warn("[permission] unknown policy action, falling back to legacy path",
				"action", dec.Action, "tool", call.Name)
		}
	}

	// Legacy path: hardcoded dangerous/sensitive lists
	risk := h.classifyRisk(call.Name)
	if risk == "safe" {
		return nil
	}
	return h.requestApproval(ctx, call, risk,
		fmt.Sprintf("Agent wants to execute %s(%s)", call.Name, summarizeArgs(call.Arguments)))
}

// requestApproval 抽出来给 policy / classifyRisk 两条路径共用。
func (h *PermissionHook) requestApproval(ctx context.Context, call *ToolCallInfo, risk, reason string) error {
	// System dispatches (cron/heartbeat/webhook/spawn) run without an
	// interactive session to approve through. A scheduled task is
	// pre-authorized at creation, so auto-approve the read/collect sensitive
	// tools it structurally needs — but never the capability-mutating ones
	// (create_skill/manage_mcp_server/file_edit) or dangerous ones
	// (shell/code_exec): those still require a human, which with no session
	// means deny. This keeps an externally-influenced webhook/spawn from
	// silently registering an MCP server or editing files (BUG-20260613).
	if src := systemDispatchSource(ctx); src != "" {
		if systemDispatchAutoApprove[call.Name] || (src == "cron" && cronOnlyAutoApprove[call.Name]) {
			logger.Info("[permission] collect tool auto-approved for pre-authorized system dispatch",
				"tool_name", call.Name, "source", src, "risk", risk)
			return nil
		}
		logger.Warn("[permission] tool denied for system dispatch — needs interactive approval",
			"tool_name", call.Name, "source", src, "risk", risk)
		return fmt.Errorf("tool %q cannot auto-run from %s; it requires interactive approval", call.Name, src)
	}

	sessionID, _ := ctx.Value(ctxKeySessionID).(string)
	if sessionID == "" {
		if risk == "dangerous" {
			return fmt.Errorf("tool %q requires approval but no session context", call.Name)
		}
		logger.Warn("[permission] sensitive tool called without session context, denying", "tool_name", call.Name)
		return fmt.Errorf("tool %q requires approval but no session context available", call.Name)
	}

	reqID := fmt.Sprintf("perm-%s-%d", call.Name, time.Now().UnixNano())
	req := &PermissionRequest{
		ID:        reqID,
		ToolName:  call.Name,
		Arguments: call.Arguments,
		Risk:      risk,
		Reason:    reason,
	}

	approved, err := h.hub.RequestApproval(ctx, sessionID, req)
	if err != nil {
		logger.Error("[permission] approval error for", "name", call.Name, "error", err)
		return fmt.Errorf("tool %q: approval failed: %w", call.Name, err)
	}
	if !approved {
		return fmt.Errorf("tool %q: user denied execution", call.Name)
	}
	return nil
}

func (h *PermissionHook) classifyRisk(toolName string) string {
	if h.dangerousTools[toolName] {
		return "dangerous"
	}
	if h.sensitiveTools[toolName] {
		return "sensitive"
	}
	return "safe"
}

func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable, reproducible audit/approval line regardless of map order
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		s := fmt.Sprintf("%v", args[k])
		if len(s) > 50 {
			s = s[:47] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
	}
	return strings.Join(parts, ", ")
}
