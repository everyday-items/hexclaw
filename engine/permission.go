package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/toolkit/lang/mapx"
	"github.com/hexagon-codes/toolkit/util/idgen"
	"github.com/hexagon-codes/toolkit/util/logger"
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
	reviewer       UnattendedReviewer
}

// UnattendedReviewer 判定一个 consequential 动作在无人值守（cron/webhook/heartbeat/
// spawn）下是否安全到可自动放行。仅 low-risk 明确放行；medium/high/出错一律拒
// （fail-closed）。这是 §11.10 统一安全闸的无人值守顾问 —— 单一风险闸，取代原先
// send_message 自管的 cronConfirmer。
type UnattendedReviewer interface {
	AssessLowRisk(ctx context.Context, action, payload string) bool
}

// WithUnattendedReviewer 注入无人值守风险顾问：当 require_approval 命中而当前是无交互
// 会话的系统派发时，改问顾问，仅 low 放行一次，否则 fail-closed 拒。
func WithUnattendedReviewer(r UnattendedReviewer) PermissionHookOption {
	return func(h *PermissionHook) {
		h.reviewer = r
	}
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
		// manage_skill 安装/卸载市场技能 = 引入新代码 = 能力注入；与 create_skill 同级。
		// 它已在 cronRecursiveToolDenylist（cron 剥离），但 webhook/spawn 不剥离，必须靠此规则
		// 兜住无人值守自动放行（BUG-F1，review 2026-06-21）。
		PolicyRule{Name: "manage-skill-sensitive", ToolPattern: "manage_skill", Action: ActionRequireApproval, Risk: "sensitive", Reason: "安装/卸载 Skill（能力变更）"},
		PolicyRule{Name: "manage-mcp-sensitive", ToolPattern: "manage_mcp_server", Action: ActionRequireApproval, Risk: "sensitive", Reason: "管理 MCP server"},
		PolicyRule{Name: "file-edit-sensitive", ToolPattern: "file_edit", Action: ActionRequireApproval, Risk: "sensitive", Reason: "文件编辑"},
		// patch_skill / manage_skill_pending（v0.4.0 F2）也归 sensitive
		PolicyRule{Name: "patch-skill-sensitive", ToolPattern: "patch_skill", Action: ActionRequireApproval, Risk: "sensitive", Reason: "修改既有 Skill"},
		PolicyRule{Name: "manage-pending-sensitive", ToolPattern: "manage_skill_pending", Action: ActionRequireApproval, Risk: "sensitive", Reason: "审批 Skill 草稿"},
		// consequential 动作：发送到外部渠道 / 媒体生成 / 发布，执行前需用户审批。
		PolicyRule{Name: "send-approve", ToolPattern: "send_message", Action: ActionRequireApproval, Risk: "sensitive", Reason: "发送到外部渠道"},
		PolicyRule{Name: "heal-approve", ToolPattern: "app_heal", Action: ActionRequireApproval, Risk: "sensitive", Reason: "自愈写操作（cron 重试/恢复/暂停）"},
		PolicyRule{Name: "media-approve", ToolPattern: "media_generate", Action: ActionRequireApproval, Risk: "sensitive", Reason: "媒体生成"},
		PolicyRule{Name: "publish-approve", ToolPattern: "publish_*", Action: ActionRequireApproval, Risk: "dangerous", Reason: "发布到外部平台"},
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

// solveAutoApprove are tools auto-approved for a genuine "solve" dispatch
// (SolveSkill's internal solver/verifier/grader). code_exec there is sandboxed
// homework computation, tool-scoped to code_exec only. Authorization is proven by
// the unforgeable solve grant on ctx (see withSolveGrant) — NOT by the
// LLM-forgeable metadata source string — so it is safe to run without an
// approver, unlike a generic spawn whose code_exec stays hard-denied.
var solveAutoApprove = map[string]bool{
	"code_exec": true,
}

// ── 不可伪造的 solve 内部授权令牌（设计⑤根治）──
//
// 旧判据「metadata source==solve && spawn_depth>0」两个字段都源自 msg.Metadata，由 LLM/外部消息
// 可伪造（react.go 把 metadata 原样灌进 ctx）。伪造顶层消息同塞两字段即可骗过闸放行 code_exec。
// 改用一个 **typed context value**：只有 SolveSkill 在真派 solver/verifier/grader 时用 withSolveGrant
// 盖它；它不是 metadata，外部消息无从注入。permission 据它（而非字符串 source）放行沙箱 code_exec。
type ctxKeySolveGrantType struct{}

var ctxKeySolveGrant = ctxKeySolveGrantType{}

// withSolveGrant 盖受信 solve 授权令牌。SolveSkill 派子 Agent 时盖在传给 executeFunc 的 ctx 上，
// 经 executeFunc→eng.Process→工具执行一路透传到 permission 闸。
func withSolveGrant(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeySolveGrant, true)
}

// solveGrantFromContext 报告 ctx 是否携带受信 solve 授权令牌。
func solveGrantFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(ctxKeySolveGrant).(bool)
	return v
}

// unattendedHardDeny are tools that must NEVER auto-run from a system dispatch —
// not even on a "low" verdict from the unattended risk reviewer: arbitrary code
// execution and capability/host mutation. The reviewer is a single LLM call that
// can be talked into "low"; letting it greenlight `shell` or `manage_skill install`
// from an externally-triggered webhook is a privilege-escalation / RCE vector.
// These require a real interactive approver — with no session that means deny.
// The reviewer continues to gate only lower-risk delivery actions
// (send_message / media_generate / publish_*). (BUG-F5, review 2026-06-21.)
var unattendedHardDeny = map[string]bool{
	"shell": true, "code": true, "code_exec": true, // arbitrary execution
	"create_skill": true, "manage_skill": true, "patch_skill": true,
	"manage_skill_pending": true, "manage_mcp_server": true, // capability mutation
	"file_edit": true, // host filesystem mutation
}

func NewPermissionHook(hub *PermissionHub, opts ...PermissionHookOption) *PermissionHook {
	h := &PermissionHook{
		hub: hub,
		dangerousTools: map[string]bool{
			"shell":     true,
			"code":      true,
			"code_exec": true,
			// 自愈写操作（cron 重试/恢复/暂停、workflow run）——always-approve，
			// 放进 legacy 黑名单使其**与 tool.policy.engine flag 无关**地强制审批（修：默认 flag OFF 时网关本会失效）。
			"app_heal": true,
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

	// v0.4.3 §11.10 统一安全闸：PermissionPolicy 是单一权限闸，配置即生效（不再 flag-gated）——
	// cron/webhook 的 ctx 不携带 flags，flag-gating 会让无人值守路径漏过闸。policy==nil 时
	// （未注入策略的调用方）才退化到 classifyRisk 黑名单兜底。
	if h.policy != nil {
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
	// P0 solve（设计⑤根治）：内部解题验证的 sandboxed code_exec 自动放行——**仅凭不可伪造的 solve
	// grant**（typed ctx value，外部消息注入不进来），不再认可可伪造的 metadata source+spawn_depth。
	// solve 子 Agent 工具被 spec 限定为仅 code_exec、leaf 深度、沙箱算题、无宿主变更/外部触发，故不属
	// unattendedHardDeny 的 RCE 面。grant 也只授权 code_exec(solveAutoApprove)，不放宽任何其它工具。
	// 放在 systemDispatch 判定之前：grant 是最权威的授权，与 metadata 无关。
	if solveGrantFromContext(ctx) && solveAutoApprove[call.Name] {
		logger.Info("[permission] solve-internal code_exec auto-approved (unforgeable grant)",
			"tool_name", call.Name)
		return nil
	}

	if src := systemDispatchSource(ctx); src != "" {
		if systemDispatchAutoApprove[call.Name] || (src == "cron" && cronOnlyAutoApprove[call.Name]) {
			logger.Info("[permission] collect tool auto-approved for pre-authorized system dispatch",
				"tool_name", call.Name, "source", src, "risk", risk)
			return nil
		}
		// BUG-F5：任意代码执行 + 能力/宿主变更类工具一律 fail-closed，风险顾问无权放行——
		// 只能交互式人工审批（无会话即拒）。顾问只兜下面的送达类动作。在问顾问之前拦截。
		if unattendedHardDeny[call.Name] {
			logger.Warn("[permission] exec/capability tool hard-denied for system dispatch (reviewer cannot override)",
				"tool_name", call.Name, "source", src, "risk", risk)
			return fmt.Errorf("tool %q cannot run unattended from %s; exec/capability mutation requires interactive approval", call.Name, src)
		}
		// §11.10 无人值守风险顾问：无交互会话可审批时，consequential 动作（send/media/
		// publish 等命中 require_approval 的）改问 LLM 顾问；仅判定 low 放行一次，
		// medium/high/无顾问/出错一律 fail-closed 拒。这是统一安全闸的单一无人值守闸，
		// 取代原 skill 层 cronConfirmer。
		if h.reviewer != nil && h.reviewer.AssessLowRisk(ctx, call.Name, summarizeArgs(call.Arguments)) {
			logger.Info("[permission] unattended action allowed by low-risk reviewer verdict",
				"tool_name", call.Name, "source", src, "risk", risk)
			return nil
		}
		logger.Warn("[permission] tool denied for system dispatch — needs interactive approval or low-risk verdict",
			"tool_name", call.Name, "source", src, "risk", risk)
		return fmt.Errorf("tool %q cannot auto-run from %s; it requires interactive approval or a low-risk verdict", call.Name, src)
	}

	sessionID, _ := ctx.Value(ctxKeySessionID).(string)
	if sessionID == "" {
		if risk == "dangerous" {
			return fmt.Errorf("tool %q requires approval but no session context", call.Name)
		}
		logger.Warn("[permission] sensitive tool called without session context, denying", "tool_name", call.Name)
		return fmt.Errorf("tool %q requires approval but no session context available", call.Name)
	}

	reqID := fmt.Sprintf("perm-%s-%s", call.Name, idgen.NanoID())
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
	keys := mapx.Keys(args)
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
