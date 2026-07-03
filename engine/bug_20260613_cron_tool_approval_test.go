package engine

// BUG-20260613 evolved under the function-first profile: cron/webhook/spawn/
// heartbeat/workflow run without an interactive session, so permission-required
// tools must auto-approve only when the source/tool matrix allows them.

import (
	"context"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestBug20260613_CronDispatchAutoApprovesSensitiveTool(t *testing.T) {
	hook := NewPermissionHook(nil) // no hub: any approval attempt would fail

	ctx := withSystemDispatch(context.Background(), "cron")
	err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "browser"})
	if err != nil {
		t.Fatalf("cron dispatch must auto-approve sensitive tool browser, got: %v", err)
	}
}

// BUG-20260702：exec 拆宿主/沙箱后，外部源 cron 的宿主直执行 shell 不再自动批准
// （需审批），但沙箱执行 code_exec 仍自动批准。
func TestBug20260613_CronDispatchSandboxedExecOnly(t *testing.T) {
	ctx := withSystemDispatch(context.Background(), "cron")

	// 无 hub：任何审批尝试都失败 → 若 shell 仍走自动批准之外的审批路径必然报错。
	if err := NewPermissionHook(nil).BeforeToolCall(ctx, &ToolCallInfo{Name: "shell"}); err == nil {
		t.Error("cron 宿主直执行 shell 应需审批（exec_host），不应自动放行")
	}
	if err := NewPermissionHook(nil).BeforeToolCall(ctx, &ToolCallInfo{Name: "code_exec"}); err != nil {
		t.Errorf("cron 沙箱执行 code_exec(exec_sandboxed) 应自动批准: %v", err)
	}
}

func TestBug20260613_NonCronSensitiveStillNeedsSession(t *testing.T) {
	hook := NewPermissionHook(nil)

	// No system-dispatch marker, no session → unchanged behavior: denied.
	if err := hook.BeforeToolCall(context.Background(), &ToolCallInfo{Name: "browser"}); err == nil {
		t.Error("interactive sensitive tool without session must still be denied")
	}
}

func TestBug20260613_SystemDispatchDeniesCapabilityMutatingToolsByDefault(t *testing.T) {
	hook := NewPermissionHook(nil, WithPolicy(DefaultBaselinePolicy()))
	for _, tool := range []string{"create_skill", "manage_skill", "manage_mcp_server"} {
		ctx := withSystemDispatch(context.Background(), "webhook")
		if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: tool}); err == nil {
			t.Errorf("function_first webhook dispatch must deny capability-mutating tool %q by default", tool)
		}
	}
}

// H2: functional floor tools must be moved to the front for system dispatch so
// a later MaxTools cap cannot drop core automation capabilities.
func TestBug20260613_SystemDispatchToolFloorSurvivesCap(t *testing.T) {
	mkTool := func(name string) llm.ToolDefinition {
		return llm.ToolDefinition{Type: "function", Function: llm.ToolFunctionDef{Name: name}}
	}
	// Marketplace skills out-rank builtins; floor tools sit at the tail.
	tools := []llm.ToolDefinition{
		mkTool("news"), mkTool("weather"), mkTool("translate"),
		mkTool("browser"), mkTool("knowledge_ingest"), mkTool("cron_task"),
		mkTool("search"), mkTool("web_search"), mkTool("code"), mkTool("code_exec"),
		mkTool("shell"), mkTool("file_edit"), mkTool("send_message"), mkTool("media_generate"),
		mkTool("manage_skill"),
	}
	e := &ReActEngine{}
	cronMsg := &adapter.Message{Metadata: map[string]string{"source": cronDispatchSource}}

	got := e.ensureSystemDispatchToolFloor(tools, cronMsg)
	// After reorder, simulate a tight configured cap; effectiveMaxTools must lift it
	// enough to keep the functional floor intact.
	cap := effectiveMaxTools(3, cronMsg)
	if cap > len(got) {
		cap = len(got)
	}
	capped := got[:cap]
	names := map[string]bool{}
	for _, tdef := range capped {
		names[tdef.Function.Name] = true
	}
	// BUG-20260702：cron(外部源)地板不再含宿主直执行 shell/code（矩阵已不放行它们），
	// 只保沙箱执行 code_exec 等；宿主 shell/code 应被 cap 挤到尾部截掉。
	for _, must := range []string{"browser", "knowledge_ingest", "cron_task", "search", "web_search", "code_exec", "file_edit", "send_message", "media_generate"} {
		if !names[must] {
			t.Errorf("floor tool %q must survive the cap for cron dispatch, capped=%v", must, capped)
		}
	}
	for _, hostExec := range []string{"shell", "code"} {
		if names[hostExec] {
			t.Errorf("host-exec tool %q must NOT be in the cron(external) floor after BUG-20260702, capped=%v", hostExec, capped)
		}
	}
	if names["manage_skill"] {
		t.Errorf("capability tool manage_skill must not be part of the cron default floor, capped=%v", capped)
	}

	// Interactive messages are untouched (no reorder).
	userMsg := &adapter.Message{Metadata: map[string]string{}}
	plain := e.ensureSystemDispatchToolFloor(tools, userMsg)
	if plain[0].Function.Name != "news" {
		t.Errorf("interactive tool order must be unchanged, got first=%q", plain[0].Function.Name)
	}
}

func TestBug20260613_CronTaskAutoApproveOnlyAutomationSources(t *testing.T) {
	hook := NewPermissionHook(nil, WithPolicy(NewPermissionPolicy(ActionAllow,
		PolicyRule{Name: "all-approve", ToolPattern: "*", Action: ActionRequireApproval, Risk: "sensitive"})))
	// GO-9（BUG-20260703）：cron 源不再自动放行 cron_task——cron job 里的 agent
	// 再建 cron job 是自我复制回路（job 造 job，无总量兜底），转审批。
	if err := hook.BeforeToolCall(withSystemDispatch(context.Background(), "cron"), &ToolCallInfo{Name: "cron_task"}); err == nil {
		t.Error("[GO-9] cron dispatch must NOT auto-approve cron_task (self-scheduling loop)")
	}
	// workflow 是用户显式编排、非自我复制回路，仍自动放行。
	if err := hook.BeforeToolCall(withSystemDispatch(context.Background(), "workflow"), &ToolCallInfo{Name: "cron_task"}); err != nil {
		t.Errorf("workflow dispatch must auto-approve cron_task, got: %v", err)
	}
	for _, src := range []string{"webhook", "spawn", "heartbeat"} {
		if err := hook.BeforeToolCall(withSystemDispatch(context.Background(), src), &ToolCallInfo{Name: "cron_task"}); err == nil {
			t.Errorf("%s dispatch must not auto-approve cron_task by default", src)
		}
	}
}

// Review H2: floor tools survive even a tight MaxTools cap below the floor size.
func TestBug20260613_EffectiveMaxToolsRaisesCapForSystemDispatch(t *testing.T) {
	cronMsg := &adapter.Message{Metadata: map[string]string{"source": cronDispatchSource}}
	userMsg := &adapter.Message{Metadata: map[string]string{}}

	floor := systemDispatchToolFloorForSource(cronDispatchSource)
	if got := effectiveMaxTools(3, cronMsg); got < len(floor) {
		t.Errorf("system dispatch cap must be >= floor size %d, got %d", len(floor), got)
	}
	if got := effectiveMaxTools(3, userMsg); got != 3 {
		t.Errorf("interactive cap unchanged, want 3 got %d", got)
	}
	if got := effectiveMaxTools(0, cronMsg); got != 0 {
		t.Errorf("unlimited (0) stays unlimited, got %d", got)
	}
}
