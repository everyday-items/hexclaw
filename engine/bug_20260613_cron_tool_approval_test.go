package engine

// BUG-20260613: cron-dispatched agent jobs run with no interactive session, so
// every sensitive tool (browser/file_edit/...) hit requestApproval with an
// empty session and was auto-denied ("requires approval but no session context
// available"). The hot-search job could therefore never call browser. A
// scheduled task is pre-authorized at creation time, so system dispatches
// (source=cron) auto-approve SENSITIVE tools — but DANGEROUS tools
// (shell/code_exec) still require a real session and are denied.

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

func TestBug20260613_CronDispatchStillDeniesDangerousTool(t *testing.T) {
	hook := NewPermissionHook(nil)

	ctx := withSystemDispatch(context.Background(), "cron")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "shell"}); err == nil {
		t.Error("cron dispatch must NOT auto-approve dangerous tool shell")
	}
}

func TestBug20260613_NonCronSensitiveStillNeedsSession(t *testing.T) {
	hook := NewPermissionHook(nil)

	// No system-dispatch marker, no session → unchanged behavior: denied.
	if err := hook.BeforeToolCall(context.Background(), &ToolCallInfo{Name: "browser"}); err == nil {
		t.Error("interactive sensitive tool without session must still be denied")
	}
}

// H3: capability-mutating sensitive tools must NOT auto-run from a system
// dispatch — a webhook/spawn could otherwise register an MCP server or edit
// files with no human in the loop.
func TestBug20260613_SystemDispatchDeniesCapabilityMutatingTools(t *testing.T) {
	hook := NewPermissionHook(nil)
	for _, tool := range []string{"create_skill", "manage_mcp_server", "file_edit"} {
		ctx := withSystemDispatch(context.Background(), "webhook")
		if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: tool}); err == nil {
			t.Errorf("system dispatch must NOT auto-approve capability-mutating tool %q", tool)
		}
	}
}

// H2: floor tools (browser/knowledge_ingest/cron_task) must be moved to the
// front for system dispatch so a later MaxTools cap cannot drop them.
func TestBug20260613_SystemDispatchToolFloorSurvivesCap(t *testing.T) {
	mkTool := func(name string) llm.ToolDefinition {
		return llm.ToolDefinition{Type: "function", Function: llm.ToolFunctionDef{Name: name}}
	}
	// Marketplace skills out-rank builtins; floor tools sit at the tail.
	tools := []llm.ToolDefinition{
		mkTool("news"), mkTool("weather"), mkTool("translate"),
		mkTool("browser"), mkTool("knowledge_ingest"), mkTool("cron_task"),
	}
	e := &ReActEngine{}
	cronMsg := &adapter.Message{Metadata: map[string]string{"source": cronDispatchSource}}

	got := e.ensureSystemDispatchToolFloor(tools, cronMsg)
	// After reorder, simulate a tight MaxTools=3 cap.
	capped := got[:3]
	names := map[string]bool{}
	for _, tdef := range capped {
		names[tdef.Function.Name] = true
	}
	for _, must := range []string{"browser", "knowledge_ingest", "cron_task"} {
		if !names[must] {
			t.Errorf("floor tool %q must survive the cap for cron dispatch, capped=%v", must, capped)
		}
	}

	// Interactive messages are untouched (no reorder).
	userMsg := &adapter.Message{Metadata: map[string]string{}}
	plain := e.ensureSystemDispatchToolFloor(tools, userMsg)
	if plain[0].Function.Name != "news" {
		t.Errorf("interactive tool order must be unchanged, got first=%q", plain[0].Function.Name)
	}
}

// Review H1: cron_task auto-approves ONLY for source=cron, never webhook/spawn.
func TestBug20260613_CronTaskAutoApproveCronOnly(t *testing.T) {
	hook := NewPermissionHook(nil)
	// cron source: allowed.
	if err := hook.BeforeToolCall(withSystemDispatch(context.Background(), "cron"), &ToolCallInfo{Name: "cron_task"}); err != nil {
		t.Errorf("cron dispatch must auto-approve cron_task, got: %v", err)
	}
	// webhook/spawn: denied (must not manage schedules autonomously).
	for _, src := range []string{"webhook", "spawn", "heartbeat"} {
		if err := hook.BeforeToolCall(withSystemDispatch(context.Background(), src), &ToolCallInfo{Name: "cron_task"}); err == nil {
			t.Errorf("%s dispatch must NOT auto-approve cron_task", src)
		}
	}
}

// Review H2: floor tools survive even a tight MaxTools cap below the floor size.
func TestBug20260613_EffectiveMaxToolsRaisesCapForSystemDispatch(t *testing.T) {
	cronMsg := &adapter.Message{Metadata: map[string]string{"source": cronDispatchSource}}
	userMsg := &adapter.Message{Metadata: map[string]string{}}

	if got := effectiveMaxTools(3, cronMsg); got < len(systemDispatchToolFloor) {
		t.Errorf("system dispatch cap must be >= floor size %d, got %d", len(systemDispatchToolFloor), got)
	}
	if got := effectiveMaxTools(3, userMsg); got != 3 {
		t.Errorf("interactive cap unchanged, want 3 got %d", got)
	}
	if got := effectiveMaxTools(0, cronMsg); got != 0 {
		t.Errorf("unlimited (0) stays unlimited, got %d", got)
	}
}
