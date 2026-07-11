package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/toolkit/lang/mapx"
	"github.com/hexagon-codes/toolkit/lang/stringx"
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
	hub                  *PermissionHub
	dangerousTools       map[string]bool // tools that always require approval
	sensitiveTools       map[string]bool // tools that require approval on first use
	policy               *PermissionPolicy
	reviewer             UnattendedReviewer
	systemDispatchPolicy atomic.Pointer[SystemDispatchPolicy]
	taskGrants           TaskGrantChecker
	recorder             PermissionDecisionRecorder
}

// PermissionDecision is one auditable outcome of the unattended permission
// gate. It is emitted for system-dispatch runs only (cron/webhook/heartbeat/
// workflow/spawn/solve) — interactive sessions have their own approval UX.
type PermissionDecision struct {
	Source     string // dispatch source (cron/webhook/…)
	TaskRef    string // "cron:<id>" / "webhook:<id>" / "workflow:<id>", "" when unknown
	Tool       string // tool name that hit the gate
	Capability string // matrix category of the tool, "" for uncategorized (e.g. MCP)
	Profile    string // active autonomy profile at decision time
	Decision   string // "allow" | "pending" | "deny"
	Via        string // "matrix" | "task_grant" | "solve_grant" | "policy"
	Reason     string
}

// PermissionDecisionRecorder persists permission decisions. Implementations
// must never block the tool path on failure (log and continue).
type PermissionDecisionRecorder interface {
	RecordPermissionDecision(ctx context.Context, d PermissionDecision)
}

// TaskGrantChecker answers whether a persisted task-scoped grant authorizes a
// tool for the given dispatch source and task reference. It sits between the
// unforgeable solve grant and the profile matrix in the evaluation order.
type TaskGrantChecker interface {
	GrantAllows(source, taskRef, toolName string) bool
}

// WithTaskGrants injects the task-scoped grant store.
func WithTaskGrants(c TaskGrantChecker) PermissionHookOption {
	return func(h *PermissionHook) {
		h.taskGrants = c
	}
}

// WithPermissionDecisionRecorder injects the persistent decision audit sink.
func WithPermissionDecisionRecorder(r PermissionDecisionRecorder) PermissionHookOption {
	return func(h *PermissionHook) {
		h.recorder = r
	}
}

// UnattendedReviewer 判定一个 consequential 动作在无人值守（cron/webhook/heartbeat/
// spawn/workflow）下是否安全到可自动放行。当前默认策略不再依赖它做隐式全开；
// 自动放行由 SystemDispatchPolicy 的 profile + source/tool 矩阵决定。
type UnattendedReviewer interface {
	AssessLowRisk(ctx context.Context, action, payload string) bool
}

// WithUnattendedReviewer 注入无人值守风险顾问。保留给调用方扩展更严格策略；
// 默认权限闸以显式矩阵为准，不用 reviewer 覆盖矩阵结果。
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

// WithPolicy 注入 PermissionPolicy；注入后即作为统一权限闸生效。
// 未注入时才走 classifyRisk legacy 路径。
func WithPolicy(p *PermissionPolicy) PermissionHookOption {
	return func(h *PermissionHook) {
		h.policy = p
	}
}

// WithSystemDispatchPolicy injects the unattended auto-approval matrix.
func WithSystemDispatchPolicy(p *SystemDispatchPolicy) PermissionHookOption {
	return func(h *PermissionHook) {
		if p != nil {
			h.systemDispatchPolicy.Store(p)
		}
	}
}

// SetSystemDispatchPolicy hot-swaps the unattended auto-approval matrix at
// runtime (profile changes from the settings UI take effect without a
// restart). Safe for concurrent use with in-flight tool calls.
func (h *PermissionHook) SetSystemDispatchPolicy(p *SystemDispatchPolicy) {
	if p != nil {
		h.systemDispatchPolicy.Store(p)
	}
}

// DispatchPolicy returns the currently effective unattended matrix.
func (h *PermissionHook) DispatchPolicy() *SystemDispatchPolicy {
	if p := h.systemDispatchPolicy.Load(); p != nil {
		return p
	}
	return DefaultSystemDispatchPolicy()
}

// DefaultBaselinePolicy 返回与老 classifyRisk 黑名单等价的 PermissionPolicy。
//
// 用法（cmd/hexclaw 启动时）：
//
//	hook := engine.NewPermissionHook(hub, engine.WithPolicy(engine.DefaultBaselinePolicy()))
//
// 注入后此 policy 生效；未注入 policy 时老 classifyRisk 黑名单继续工作。
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
		// 它已在 cronRecursiveToolDenylist（历史 cron 剥离清单）中，证明它是能力注入向量。
		// webhook/spawn 不应绕过审批；无人值守是否自动放行由 autonomy 矩阵显式决定。
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
// solveAutoApprove are tools auto-approved for a genuine "solve" dispatch
// (SolveSkill's internal solver/verifier/grader). code_exec there is sandboxed
// homework computation, tool-scoped to code_exec only. Authorization is proven by
// the unforgeable solve grant on ctx (see withSolveGrant) — NOT by the
// LLM-forgeable metadata source string. It remains useful for non-system solve
// execution paths and does not broaden shell or other tools.
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
	h.systemDispatchPolicy.Store(DefaultSystemDispatchPolicy())
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// recordDecision persists one gate outcome for system-dispatch runs. No-op
// without a recorder; the recorder itself must swallow storage errors.
func (h *PermissionHook) recordDecision(ctx context.Context, tool, decision, via, reason string) {
	if h.recorder == nil {
		return
	}
	src := systemDispatchSource(ctx)
	if src == "" {
		return // interactive sessions are out of the unattended audit scope
	}
	h.recorder.RecordPermissionDecision(ctx, PermissionDecision{
		Source:     src,
		TaskRef:    systemDispatchTaskRef(ctx),
		Tool:       tool,
		Capability: SystemDispatchToolCategory(tool),
		Profile:    h.DispatchPolicy().Profile(),
		Decision:   decision,
		Via:        via,
		Reason:     reason,
	})
}

func (h *PermissionHook) BeforeToolCall(ctx context.Context, call *ToolCallInfo) error {
	// v0.4.3 §11.10 统一安全闸：PermissionPolicy 是单一权限闸，配置即生效（不再 flag-gated）——
	// cron/webhook 的 ctx 不携带 flags，flag-gating 会让无人值守路径漏过闸。policy==nil 时
	// （未注入策略的调用方）才退化到 classifyRisk 黑名单兜底。
	if h.policy != nil {
		dec := h.policy.Evaluate(call)
		switch dec.Action {
		case ActionAllow:
			return h.gateUnattendedConnectorTool(ctx, call)
		case ActionDeny:
			logger.Info("[permission] policy deny",
				"tool", call.Name, "rule", dec.MatchedRule, "reason", dec.Reason)
			reason := dec.Reason
			if reason == "" {
				reason = "policy denies execution"
			}
			h.recordDecision(ctx, call.Name, "deny", "policy", "显式 deny 规则 "+dec.MatchedRule)
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
		return h.gateUnattendedConnectorTool(ctx, call)
	}
	return h.requestApproval(ctx, call, risk,
		fmt.Sprintf("Agent wants to execute %s(%s)", call.Name, summarizeArgs(call.Arguments)))
}

// gateUnattendedConnectorTool 收口无人值守下的连接器（MCP）工具。
//
// 基线 policy 对未命中规则的工具默认放行——交互会话保持该手感不变；但外部/
// 无人值守触发（cron/webhook/…）下的 MCP 连接器工具是「外部写入」风险面
// （GitHub/Jira/DB 等第三方系统），矩阵没有它们的内置类别，必须由任务级
// grant 或显式矩阵条目（含全功能 profile 的 "*"）授权，否则转待审批。
// skill/builtin 工具不经此闸（它们有类别与 policy 规则治理）。
func (h *PermissionHook) gateUnattendedConnectorTool(ctx context.Context, call *ToolCallInfo) error {
	src := systemDispatchSource(ctx)
	if src == "" {
		// 交互会话（无派发源）保持自动放行手感，不引入审批摩擦。
		return nil
	}
	// 无人值守下需矩阵/grant 授权的两类工具：
	//   - 连接器（MCP）工具：外部写入风险面，矩阵无内置类别。
	//   - 自排程自动化 builtin（cron_task，automation 类别）：GO-9——cron_task 是
	//     builtin(Source≠mcp)、DefaultBaselinePolicy 无其规则 → 走 ActionAllow 分支
	//     直达本闸，若仍按「非 mcp 早退」放行，则 SystemDispatchPolicy 矩阵对 cron_task
	//     形同虚设，cron agent 可自建 cron job 成自我复制回路。故 automation 类别
	//     builtin 在此一并按矩阵/grant 求值（read/files/media 等其它类别不受影响）。
	if call.Source != "mcp" && SystemDispatchToolCategory(call.Name) != "automation" {
		return nil
	}
	if taskRef := systemDispatchTaskRef(ctx); taskRef != "" && h.taskGrants != nil && h.taskGrants.GrantAllows(src, taskRef, call.Name) {
		logger.Info("[permission] connector tool auto-approved by task-scoped grant",
			"tool_name", call.Name, "source", src, "task_ref", taskRef)
		h.recordDecision(ctx, call.Name, "allow", "task_grant", "任务级授权命中（连接器工具）")
		return nil
	}
	policy := h.DispatchPolicy()
	if policy.Allows(src, call.Name) {
		h.recordDecision(ctx, call.Name, "allow", "matrix", "矩阵显式条目放行（连接器工具）")
		return nil
	}
	logger.Warn("[permission] unattended connector tool requires explicit authorization",
		"tool_name", call.Name, "source", src, "profile", policy.Profile())
	h.recordDecision(ctx, call.Name, "pending", "matrix", "连接器工具在无人值守下需显式授权")
	return fmt.Errorf("connector tool %q requires explicit authorization for unattended %s dispatch: grant it to this task or add an explicit autonomy matrix entry", call.Name, src)
}

// requestApproval 抽出来给 policy / classifyRisk 两条路径共用。
func (h *PermissionHook) requestApproval(ctx context.Context, call *ToolCallInfo, risk, reason string) error {
	// P0 solve（设计⑤根治）：内部解题验证的 sandboxed code_exec 自动放行——**仅凭不可伪造的 solve
	// grant**（typed ctx value，外部消息注入不进来），不再认可可伪造的 metadata source+spawn_depth。
	// grant 也只授权 code_exec(solveAutoApprove)，不放宽任何其它工具。放在 systemDispatch 判定之前：
	// grant 是最权威的授权，与 metadata 无关。
	if solveGrantFromContext(ctx) && solveAutoApprove[call.Name] {
		logger.Info("[permission] solve-internal code_exec auto-approved (unforgeable grant)",
			"tool_name", call.Name)
		h.recordDecision(ctx, call.Name, "allow", "solve_grant", "solve 内部沙箱执行（不可伪造 grant）")
		return nil
	}

	if src := systemDispatchSource(ctx); src != "" {
		// 任务级 grant 优先于 Profile 矩阵：范围最小的显式授权（用户在创建流/
		// 阻断处理里点出来的）应当赢过全局默认，且不放宽其他任务。
		if taskRef := systemDispatchTaskRef(ctx); taskRef != "" && h.taskGrants != nil {
			if h.taskGrants.GrantAllows(src, taskRef, call.Name) {
				logger.Info("[permission] tool auto-approved by task-scoped grant",
					"tool_name", call.Name, "source", src, "task_ref", taskRef)
				h.recordDecision(ctx, call.Name, "allow", "task_grant", "任务级授权命中")
				return nil
			}
		}

		policy := h.DispatchPolicy()
		if policy.Allows(src, call.Name) {
			logger.Info("[permission] tool auto-approved by system dispatch matrix",
				"tool_name", call.Name, "source", src, "risk", risk, "profile", policy.Profile())
			h.recordDecision(ctx, call.Name, "allow", "matrix", "命中 Profile 矩阵自动放行")
			return nil
		}
		logger.Warn("[permission] system dispatch requires explicit autonomy switch",
			"tool_name", call.Name, "source", src, "risk", risk, "profile", policy.Profile())
		h.recordDecision(ctx, call.Name, "pending", "matrix", "Profile 矩阵未放行，转待审批")
		return fmt.Errorf("tool %q requires approval but %s dispatch profile %q does not auto-approve it; configure security.autonomy.system_dispatch.%s to include a matching category/tool",
			call.Name, src, policy.Profile(), src)
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
			// rune-safe 截断，避免 CJK 字节切断产生乱码（AP-141，审批/审计行用户可见）。
			s = stringx.TruncateBytes(s, 47, "...")
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
	}
	return strings.Join(parts, ", ")
}
