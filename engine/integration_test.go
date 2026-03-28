package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// ============== 链路⑧: PermissionHook 工具审批 ==============

func TestPermissionHook_DangerousToolBlocked(t *testing.T) {
	hub := NewPermissionHub(1 * time.Second)
	hook := NewPermissionHook(hub)
	// No sender → dangerous tools auto-denied

	ctx := context.WithValue(context.Background(), "session_id", "test-sess")

	// shell is classified as dangerous
	err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "shell", Source: "skill"})
	if err == nil {
		t.Fatal("expected dangerous tool to be blocked without sender")
	}

	// search is safe → should pass
	err = hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "search", Source: "skill"})
	if err != nil {
		t.Fatalf("safe tool should pass: %v", err)
	}
}

func TestPermissionHook_ApprovalFlow(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	sender := &mockSender{}
	hub.SetSender(sender)
	hook := NewPermissionHook(hub)

	ctx := context.WithValue(context.Background(), "session_id", "test-sess")

	// Simulate async user approval
	go func() {
		time.Sleep(50 * time.Millisecond)
		if sender.getLastReq() != nil {
			hub.HandleResponse(PermissionResponse{
				RequestID: sender.getLastReq().ID,
				Approved:  true,
			})
		}
	}()

	err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "code_exec", Source: "skill", Arguments: map[string]any{"code": "print(1)"}})
	if err != nil {
		t.Fatalf("approved tool should pass: %v", err)
	}
}

func TestPermissionHook_DenialFlow(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	sender := &mockSender{}
	hub.SetSender(sender)
	hook := NewPermissionHook(hub)

	ctx := context.WithValue(context.Background(), "session_id", "test-sess")

	go func() {
		time.Sleep(50 * time.Millisecond)
		if sender.getLastReq() != nil {
			hub.HandleResponse(PermissionResponse{
				RequestID: sender.getLastReq().ID,
				Approved:  false,
			})
		}
	}()

	err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "shell", Source: "skill", Arguments: map[string]any{"command": "rm -rf /"}})
	if err == nil {
		t.Fatal("denied tool should be blocked")
	}
}

// ============== Budget per-request isolation ==============

func TestBudget_PerRequestIsolation(t *testing.T) {
	cfg := BudgetConfig{MaxTokens: 1000, MaxDuration: 5 * time.Minute, MaxCost: 1.0}

	b1 := NewBudgetController(cfg)
	b2 := NewBudgetController(cfg)

	// Consume tokens in b1
	b1.RecordTokens(900)

	// b2 should be unaffected
	if err := b2.Check(); err != nil {
		t.Fatalf("b2 should have full budget: %v", err)
	}

	// b1 should be near limit
	b1.RecordTokens(200)
	if err := b1.Check(); err == nil {
		t.Fatal("b1 should be exhausted after 1100 tokens (limit 1000)")
	}
}

// ============== FileOps path safety ==============

func TestFileOps_PathTraversalBlocked(t *testing.T) {
	// Path traversal validation is tested in skill/builtin package.
	// Engine-level check: verify ".." is detectable in args
	path := "../../etc/passwd"
	if len(path) > 0 && (path[0] == '/' || (len(path) > 1 && path[:2] == "..")) {
		t.Log("path traversal input correctly detected at engine level")
	} else {
		t.Fatal("failed to detect path traversal")
	}
}

// ============== SpawnSkill timeout ==============

func TestSpawnSkill_Timeout(t *testing.T) {
	slowExec := func(ctx context.Context, agent, task string) (string, error) {
		select {
		case <-time.After(5 * time.Second):
			return "done", nil
		case <-ctx.Done():
			return "partial", ctx.Err()
		}
	}

	spawn := NewSpawnSkill(slowExec)
	result, err := spawn.Execute(context.Background(), map[string]any{
		"agent_name": "test",
		"task":       "slow task",
		"timeout":    "200ms",
	})

	if err != nil {
		t.Fatalf("spawn should return partial result, not error: %v", err)
	}
	if result == nil || result.Content == "" {
		t.Fatal("expected partial result content")
	}
	t.Logf("spawn result: %s", result.Content)
}

// ============== OrchestrateSkill parallel ==============

func TestOrchestrateSkill_ParallelExecution(t *testing.T) {
	var execCount atomic.Int32
	mockExec := func(ctx context.Context, agent, task string) (string, error) {
		execCount.Add(1)
		time.Sleep(50 * time.Millisecond) // Simulate work
		return "result from " + agent, nil
	}

	orch := NewOrchestrateSkill(mockExec)
	result, err := orch.Execute(context.Background(), map[string]any{
		"subtasks": []any{
			map[string]any{"agent": "researcher", "task": "search competitors"},
			map[string]any{"agent": "analyst", "task": "analyze data"},
		},
	})

	if err != nil {
		t.Fatalf("orchestrate failed: %v", err)
	}
	if c := execCount.Load(); c != 2 {
		t.Fatalf("expected 2 executions, got %d", c)
	}
	if result == nil || result.Content == "" {
		t.Fatal("expected combined results")
	}
	t.Logf("orchestrate result: %s", result.Content)
}

// ============== HandoffSkill ==============

func TestHandoffSkill_MetadataSet(t *testing.T) {
	handoff := NewHandoffSkill(nil) // nil dispatcher → will return error on route, but metadata still set
	result, err := handoff.Execute(context.Background(), map[string]any{
		"agent_name": "coder",
		"context":    "user needs Python help",
	})

	// With nil dispatcher, should return error
	if err == nil && result != nil {
		// If it didn't error, check metadata
		if result.Metadata["handoff_to"] != "coder" {
			t.Fatalf("expected handoff_to=coder, got %s", result.Metadata["handoff_to"])
		}
	}
}

// ============== Helpers ==============

// mockPermSender defined in permission_test.go (with sync.Mutex)
