package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

type mockSender struct {
	mu      sync.Mutex
	lastReq *PermissionRequest
}

func (m *mockSender) SendPermissionRequest(_ context.Context, _ string, req *PermissionRequest) error {
	m.mu.Lock()
	m.lastReq = req
	m.mu.Unlock()
	return nil
}

func (m *mockSender) getLastReq() *PermissionRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastReq
}

func TestPermissionHub_ApproveFlow(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	sender := &mockSender{}
	hub.SetSender(sender)

	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(10 * time.Millisecond)
			if req := sender.getLastReq(); req != nil {
				hub.HandleResponse(PermissionResponse{RequestID: req.ID, Approved: true})
				return
			}
		}
	}()

	ctx := context.WithValue(context.Background(), "session_id", "sess-1")
	req := &PermissionRequest{ID: "perm-test-1", ToolName: "shell", Risk: "dangerous", Reason: "test"}
	approved, err := hub.RequestApproval(ctx, "sess-1", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !approved {
		t.Fatal("expected approval")
	}
}

func TestPermissionHub_DenyFlow(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	sender := &mockSender{}
	hub.SetSender(sender)

	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(10 * time.Millisecond)
			if req := sender.getLastReq(); req != nil {
				hub.HandleResponse(PermissionResponse{RequestID: req.ID, Approved: false})
				return
			}
		}
	}()

	req := &PermissionRequest{ID: "perm-test-2", ToolName: "shell", Risk: "dangerous"}
	approved, err := hub.RequestApproval(context.Background(), "sess-1", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if approved {
		t.Fatal("expected denial")
	}
}

func TestPermissionHub_Timeout(t *testing.T) {
	hub := NewPermissionHub(100 * time.Millisecond)
	sender := &mockSender{}
	hub.SetSender(sender)

	req := &PermissionRequest{ID: "perm-test-3", ToolName: "shell", Risk: "dangerous"}
	_, err := hub.RequestApproval(context.Background(), "sess-1", req)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestPermissionHub_RememberAllow(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	sender := &mockSender{}
	hub.SetSender(sender)

	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(10 * time.Millisecond)
			if req := sender.getLastReq(); req != nil {
				hub.HandleResponse(PermissionResponse{RequestID: req.ID, Approved: true, Remember: true})
				return
			}
		}
	}()

	req := &PermissionRequest{ID: "perm-test-4", ToolName: "browser", Risk: "sensitive"}
	approved, err := hub.RequestApproval(context.Background(), "sess-1", req)
	if err != nil || !approved {
		t.Fatalf("first call should be approved: err=%v, approved=%v", err, approved)
	}

	// Second call should auto-approve (remembered)
	req2 := &PermissionRequest{ID: "perm-test-5", ToolName: "browser", Risk: "sensitive"}
	approved2, err2 := hub.RequestApproval(context.Background(), "sess-1", req2)
	if err2 != nil || !approved2 {
		t.Fatalf("remembered call should auto-approve: err=%v, approved=%v", err2, approved2)
	}
}

func TestPermissionHook_SafeToolSkipped(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	hook := NewPermissionHook(hub)
	ctx := context.WithValue(context.Background(), "session_id", "sess-1")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "search", Source: "skill"}); err != nil {
		t.Fatalf("safe tool should not require approval: %v", err)
	}
}

func TestPermissionHook_NoSenderDenied(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	hook := NewPermissionHook(hub)
	ctx := context.WithValue(context.Background(), "session_id", "sess-1")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "shell", Source: "skill"}); err == nil {
		t.Fatal("dangerous tool with no sender should be denied")
	}
}
