package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestToolApprovalGate_SafeToolPassesThrough(t *testing.T) {
	callbackCalled := false
	gate := NewToolApprovalGate(func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
		callbackCalled = true
		return ApprovalResponse{Approved: true}, nil
	}, 5*time.Second)

	ctx := context.WithValue(context.Background(), ctxKeySessionID, "sess-1")
	call := &ToolCallInfo{Name: "search", Arguments: map[string]any{"q": "test"}}

	err := gate.BeforeToolCall(ctx, call)
	if err != nil {
		t.Errorf("safe tool should pass, got: %v", err)
	}
	if callbackCalled {
		t.Error("callback should NOT be called for safe tools")
	}
}

func TestToolApprovalGate_DangerousToolApproved(t *testing.T) {
	gate := NewToolApprovalGate(func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
		if req.ToolName != "shell" {
			t.Errorf("expected tool name 'shell', got %q", req.ToolName)
		}
		if req.RiskLevel != "dangerous" {
			t.Errorf("expected risk level 'dangerous', got %q", req.RiskLevel)
		}
		return ApprovalResponse{Approved: true}, nil
	}, 5*time.Second)

	ctx := context.WithValue(context.Background(), ctxKeySessionID, "sess-1")
	call := &ToolCallInfo{Name: "shell", Arguments: map[string]any{"cmd": "ls"}}

	err := gate.BeforeToolCall(ctx, call)
	if err != nil {
		t.Errorf("approved dangerous tool should pass, got: %v", err)
	}
}

func TestToolApprovalGate_DangerousToolDenied(t *testing.T) {
	gate := NewToolApprovalGate(func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
		return ApprovalResponse{Approved: false, Reason: "too risky"}, nil
	}, 5*time.Second)

	ctx := context.WithValue(context.Background(), ctxKeySessionID, "sess-1")
	call := &ToolCallInfo{Name: "shell", Arguments: map[string]any{"cmd": "rm -rf /"}}

	err := gate.BeforeToolCall(ctx, call)
	if err == nil {
		t.Fatal("denied tool should return error")
	}
	if !strings.Contains(err.Error(), "too risky") {
		t.Errorf("error should contain denial reason, got: %v", err)
	}
}

func TestToolApprovalGate_DangerousToolDenied_NoReason(t *testing.T) {
	gate := NewToolApprovalGate(func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
		return ApprovalResponse{Approved: false}, nil
	}, 5*time.Second)

	ctx := context.WithValue(context.Background(), ctxKeySessionID, "sess-1")
	call := &ToolCallInfo{Name: "shell"}

	err := gate.BeforeToolCall(ctx, call)
	if err == nil {
		t.Fatal("denied tool should return error")
	}
	if !strings.Contains(err.Error(), "user denied") {
		t.Errorf("error should contain 'user denied' default, got: %v", err)
	}
}

func TestToolApprovalGate_CallbackTimeout(t *testing.T) {
	gate := NewToolApprovalGate(func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
		// Simulate slow user - wait for context to be cancelled
		<-ctx.Done()
		return ApprovalResponse{}, ctx.Err()
	}, 50*time.Millisecond) // very short timeout

	ctx := context.WithValue(context.Background(), ctxKeySessionID, "sess-1")
	call := &ToolCallInfo{Name: "code_exec"}

	err := gate.BeforeToolCall(ctx, call)
	if err == nil {
		t.Fatal("timed-out callback should return error")
	}
	if !strings.Contains(err.Error(), "approval failed") {
		t.Errorf("error should mention approval failure, got: %v", err)
	}
}

func TestToolApprovalGate_CallbackError(t *testing.T) {
	gate := NewToolApprovalGate(func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
		return ApprovalResponse{}, errors.New("websocket disconnected")
	}, 5*time.Second)

	ctx := context.WithValue(context.Background(), ctxKeySessionID, "sess-1")
	call := &ToolCallInfo{Name: "shell"}

	err := gate.BeforeToolCall(ctx, call)
	if err == nil {
		t.Fatal("callback error should propagate")
	}
	if !strings.Contains(err.Error(), "websocket disconnected") {
		t.Errorf("error should contain original error, got: %v", err)
	}
}

func TestToolApprovalGate_AlwaysAllow(t *testing.T) {
	callCount := 0
	gate := NewToolApprovalGate(func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
		callCount++
		return ApprovalResponse{Approved: true, AlwaysAllow: true}, nil
	}, 5*time.Second)

	ctx := context.WithValue(context.Background(), ctxKeySessionID, "sess-1")
	call := &ToolCallInfo{Name: "shell"}

	// First call: callback should be invoked
	if err := gate.BeforeToolCall(ctx, call); err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Errorf("callback should be called once, got %d", callCount)
	}

	// Second call: should skip callback due to AlwaysAllow
	if err := gate.BeforeToolCall(ctx, call); err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Errorf("callback should NOT be called again (AlwaysAllow), got %d calls", callCount)
	}
}

func TestToolApprovalGate_AlwaysAllow_DifferentSession(t *testing.T) {
	callCount := 0
	gate := NewToolApprovalGate(func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
		callCount++
		return ApprovalResponse{Approved: true, AlwaysAllow: true}, nil
	}, 5*time.Second)

	ctx1 := context.WithValue(context.Background(), ctxKeySessionID, "sess-1")
	ctx2 := context.WithValue(context.Background(), ctxKeySessionID, "sess-2")
	call := &ToolCallInfo{Name: "shell"}

	gate.BeforeToolCall(ctx1, call)
	if callCount != 1 {
		t.Fatalf("expected 1 call, got %d", callCount)
	}

	// Different session should still call callback
	gate.BeforeToolCall(ctx2, call)
	if callCount != 2 {
		t.Errorf("different session should call callback again, got %d calls", callCount)
	}
}

func TestToolApprovalGate_ClearSession(t *testing.T) {
	callCount := 0
	gate := NewToolApprovalGate(func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
		callCount++
		return ApprovalResponse{Approved: true, AlwaysAllow: true}, nil
	}, 5*time.Second)

	ctx := context.WithValue(context.Background(), ctxKeySessionID, "sess-1")
	call := &ToolCallInfo{Name: "shell"}

	// First call sets AlwaysAllow
	gate.BeforeToolCall(ctx, call)
	if callCount != 1 {
		t.Fatalf("expected 1 call, got %d", callCount)
	}

	// Clear session
	gate.ClearSession("sess-1")

	// After clear, callback should be called again
	gate.BeforeToolCall(ctx, call)
	if callCount != 2 {
		t.Errorf("after ClearSession, callback should be called again, got %d calls", callCount)
	}
}

func TestToolApprovalGate_NilContextSessionID(t *testing.T) {
	gate := NewToolApprovalGate(func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
		if req.SessionID != "" {
			t.Errorf("session ID should be empty string for nil context value, got %q", req.SessionID)
		}
		return ApprovalResponse{Approved: true}, nil
	}, 5*time.Second)

	// Context without session_id key
	ctx := context.Background()
	call := &ToolCallInfo{Name: "shell"}

	// Should not panic
	err := gate.BeforeToolCall(ctx, call)
	if err != nil {
		t.Errorf("should not error with nil session_id, got: %v", err)
	}
}

func TestToolApprovalGate_SensitiveTool(t *testing.T) {
	gate := NewToolApprovalGate(func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
		if req.RiskLevel != "sensitive" {
			t.Errorf("browser should be classified as sensitive, got %q", req.RiskLevel)
		}
		return ApprovalResponse{Approved: true}, nil
	}, 5*time.Second)

	ctx := context.WithValue(context.Background(), ctxKeySessionID, "sess-1")
	call := &ToolCallInfo{Name: "browser"}

	err := gate.BeforeToolCall(ctx, call)
	if err != nil {
		t.Errorf("approved sensitive tool should pass, got: %v", err)
	}
}

func TestToolApprovalGate_CustomClassifier(t *testing.T) {
	gate := NewToolApprovalGate(func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
		return ApprovalResponse{Approved: true}, nil
	}, 5*time.Second)

	// Custom classifier that marks "my_tool" as dangerous
	gate.SetClassifier(func(toolName string) string {
		if toolName == "my_tool" {
			return "dangerous"
		}
		return "safe"
	})

	callbackInvoked := false
	gate.callback = func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error) {
		callbackInvoked = true
		return ApprovalResponse{Approved: true}, nil
	}

	ctx := context.WithValue(context.Background(), ctxKeySessionID, "sess-1")

	// "shell" should now be safe (custom classifier)
	gate.BeforeToolCall(ctx, &ToolCallInfo{Name: "shell"})
	if callbackInvoked {
		t.Error("shell should be safe with custom classifier, callback should not be called")
	}

	// "my_tool" should be dangerous
	gate.BeforeToolCall(ctx, &ToolCallInfo{Name: "my_tool"})
	if !callbackInvoked {
		t.Error("my_tool should be dangerous with custom classifier, callback should be called")
	}
}

func TestToolApprovalGate_DefaultTimeout(t *testing.T) {
	// timeout <= 0 should default to 30s
	gate := NewToolApprovalGate(nil, 0)
	if gate.timeout != 30*time.Second {
		t.Errorf("default timeout should be 30s, got %v", gate.timeout)
	}

	gate2 := NewToolApprovalGate(nil, -1)
	if gate2.timeout != 30*time.Second {
		t.Errorf("negative timeout should default to 30s, got %v", gate2.timeout)
	}
}
