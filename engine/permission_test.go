package engine

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/skill"
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

type scriptedPermissionSender struct {
	mu                 sync.Mutex
	hub                *PermissionHub
	responses          []PermissionResponse
	calls              int
	sawDeadline        bool
	sawRequestDeadline bool
}

type memoryRememberedGrantStore struct {
	mu     sync.Mutex
	grants map[string]bool
}

func (s *memoryRememberedGrantStore) key(ownerID, sessionID, toolName, scopeDigest string) string {
	return ownerID + "\x00" + sessionID + "\x00" + toolName + "\x00" + scopeDigest
}

func (s *memoryRememberedGrantStore) HasRememberedGrant(_ context.Context, ownerID, sessionID, toolName, scopeDigest string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.grants[s.key(ownerID, sessionID, toolName, scopeDigest)], nil
}

func (s *memoryRememberedGrantStore) RememberGrant(_ context.Context, ownerID, sessionID, toolName, scopeDigest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.grants == nil {
		s.grants = make(map[string]bool)
	}
	s.grants[s.key(ownerID, sessionID, toolName, scopeDigest)] = true
	return nil
}

func (s *memoryRememberedGrantStore) DeleteRememberedGrants(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.grants {
		if strings.Contains(key, "\x00"+sessionID+"\x00") {
			delete(s.grants, key)
		}
	}
	return nil
}

func (s *scriptedPermissionSender) SendPermissionRequest(ctx context.Context, sessionID string, req *PermissionRequest) error {
	s.mu.Lock()
	s.calls++
	_, s.sawDeadline = ctx.Deadline()
	requestValue := reflect.ValueOf(req)
	if requestValue.Kind() == reflect.Pointer && !requestValue.IsNil() {
		deadlineField := requestValue.Elem().FieldByName("DeadlineAt")
		s.sawRequestDeadline = deadlineField.IsValid() && !deadlineField.IsZero()
	}
	var response PermissionResponse
	if len(s.responses) > 0 {
		response = s.responses[0]
		s.responses = s.responses[1:]
	}
	s.mu.Unlock()

	response.RequestID = req.ID
	response.OwnerID = req.OwnerID
	response.SessionID = sessionID
	response.InvocationID = req.InvocationID
	response.ArgumentsDigest = req.ArgumentsDigest
	response.SecurityScopeDigest = req.SecurityScopeDigest
	s.hub.HandleResponse(response)
	return nil
}

func permissionTestContext(ctx context.Context) context.Context {
	return skill.WithAuthenticatedUser(ctx, "permission-test-owner")
}

func exactPermissionTestResponse(req *PermissionRequest, sessionID string, approved, remember bool) PermissionResponse {
	return PermissionResponse{
		RequestID: req.ID, OwnerID: req.OwnerID, SessionID: sessionID,
		InvocationID: req.InvocationID, ArgumentsDigest: req.ArgumentsDigest,
		SecurityScopeDigest: req.SecurityScopeDigest, Approved: approved, Remember: remember,
	}
}

func (s *scriptedPermissionSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestPermissionHub_ApproveFlow(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	sender := &mockSender{}
	hub.SetSender(sender)

	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(10 * time.Millisecond)
			if req := sender.getLastReq(); req != nil {
				hub.HandleResponse(exactPermissionTestResponse(req, "sess-1", true, false))
				return
			}
		}
	}()

	ctx := context.WithValue(permissionTestContext(context.Background()), ctxKeySessionID, "sess-1")
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
				hub.HandleResponse(exactPermissionTestResponse(req, "sess-1", false, false))
				return
			}
		}
	}()

	req := &PermissionRequest{ID: "perm-test-2", ToolName: "shell", Risk: "dangerous"}
	approved, err := hub.RequestApproval(permissionTestContext(context.Background()), "sess-1", req)
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
	_, err := hub.RequestApproval(permissionTestContext(context.Background()), "sess-1", req)
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
				hub.HandleResponse(exactPermissionTestResponse(req, "sess-1", true, true))
				return
			}
		}
	}()

	req := &PermissionRequest{ID: "perm-test-4", ToolName: "browser", Risk: "sensitive"}
	approved, err := hub.RequestApproval(permissionTestContext(context.Background()), "sess-1", req)
	if err != nil || !approved {
		t.Fatalf("first call should be approved: err=%v, approved=%v", err, approved)
	}

	// Second call should auto-approve (remembered)
	req2 := &PermissionRequest{ID: "perm-test-5", ToolName: "browser", Risk: "sensitive"}
	approved2, err2 := hub.RequestApproval(permissionTestContext(context.Background()), "sess-1", req2)
	if err2 != nil || !approved2 {
		t.Fatalf("remembered call should auto-approve: err=%v, approved=%v", err2, approved2)
	}
}

// REG-TOOL-APPROVAL-REUSE-001 / REG-TOOL-APPROVAL-ARGS-001
func TestPermissionHub_RememberedGrantReusesOnlySameSecurityScope(t *testing.T) {
	hub := NewPermissionHub(time.Second)
	sender := &scriptedPermissionSender{
		hub: hub,
		responses: []PermissionResponse{
			{Approved: true, Remember: true},
			{Approved: false},
		},
	}
	hub.SetSender(sender)

	first := &PermissionRequest{
		ID:       "approval-scope-1",
		ToolName: "file_edit",
		Arguments: map[string]any{
			"path":    "/workspace/report.md",
			"content": map[string]any{"title": "中文", "tags": []any{"a", nil}},
		},
		Risk: "sensitive",
	}
	approved, err := hub.RequestApproval(permissionTestContext(context.Background()), "session-1", first)
	if err != nil || !approved {
		t.Fatalf("first remembered approval = (%v, %v), want (true, nil)", approved, err)
	}

	sameScope := &PermissionRequest{
		ID:       "approval-scope-2",
		ToolName: "file_edit",
		Arguments: map[string]any{
			"content": map[string]any{"tags": []any{"a", nil}, "title": "中文"},
			"path":    "/workspace/report.md",
		},
		Risk: "sensitive",
	}
	approved, err = hub.RequestApproval(permissionTestContext(context.Background()), "session-1", sameScope)
	if err != nil || !approved {
		t.Fatalf("same scope reuse = (%v, %v), want (true, nil)", approved, err)
	}
	if got := sender.callCount(); got != 1 {
		t.Fatalf("same scope approval request count = %d, want 1 total", got)
	}

	expandedScope := &PermissionRequest{
		ID:       "approval-scope-3",
		ToolName: "file_edit",
		Arguments: map[string]any{
			"path":    "/workspace/private/credentials.md",
			"content": map[string]any{"title": "中文", "tags": []any{"a", nil}},
		},
		Risk: "sensitive",
	}
	approved, err = hub.RequestApproval(permissionTestContext(context.Background()), "session-1", expandedScope)
	if err != nil {
		t.Fatalf("expanded scope approval returned error: %v", err)
	}
	if approved {
		t.Fatal("expanded security scope reused the old remembered grant; want reapproval and denial")
	}
	if got := sender.callCount(); got != 2 {
		t.Fatalf("expanded scope approval request count = %d, want 2 total", got)
	}
}

// REG-TOOL-APPROVAL-LIFECYCLE-001
func TestPermissionHub_RememberedGrantSurvivesCoordinatorRestart(t *testing.T) {
	grants := &memoryRememberedGrantStore{}
	firstHub := NewPermissionHubWithRememberedGrantStore(time.Second, grants)
	firstSender := &scriptedPermissionSender{
		hub:       firstHub,
		responses: []PermissionResponse{{Approved: true, Remember: true}},
	}
	firstHub.SetSender(firstSender)
	req := &PermissionRequest{
		ID:        "approval-restart-1",
		ToolName:  "browser",
		Arguments: map[string]any{"target": "https://example.test/same-scope"},
		Risk:      "sensitive",
	}
	if approved, err := firstHub.RequestApproval(permissionTestContext(context.Background()), "session-restart", req); err != nil || !approved {
		t.Fatalf("seed remembered approval = (%v, %v), want (true, nil)", approved, err)
	}

	restartedHub := NewPermissionHubWithRememberedGrantStore(time.Second, grants)
	replayed := &PermissionRequest{
		ID:        "approval-restart-2",
		ToolName:  "browser",
		Arguments: map[string]any{"target": "https://example.test/same-scope"},
		Risk:      "sensitive",
	}
	approved, err := restartedHub.RequestApproval(permissionTestContext(context.Background()), "session-restart", replayed)
	if err != nil {
		t.Fatalf("durable same-scope lookup after coordinator restart returned error: %v", err)
	}
	if !approved {
		t.Fatal("remembered grant was lost across coordinator/Sidecar restart")
	}
}

// REG-TOOL-APPROVAL-DEADLINE-001
func TestPermissionHub_FreezesOneBackendDeadlineForSenderAndRequest(t *testing.T) {
	hub := NewPermissionHub(250 * time.Millisecond)
	sender := &scriptedPermissionSender{
		hub:       hub,
		responses: []PermissionResponse{{Approved: false}},
	}
	hub.SetSender(sender)

	approved, err := hub.RequestApproval(permissionTestContext(context.Background()), "session-deadline", &PermissionRequest{
		ID:       "approval-deadline-1",
		ToolName: "shell",
		Risk:     "dangerous",
	})
	if err != nil {
		t.Fatalf("denied response returned error: %v", err)
	}
	if approved {
		t.Fatal("denied response unexpectedly approved")
	}
	if !sender.sawDeadline {
		t.Error("PermissionSender context has no authoritative backend deadline")
	}
	if !sender.sawRequestDeadline {
		t.Error("PermissionRequest has no frozen deadline_at matching the sender context")
	}
}

// REG-TOOL-APPROVAL-POLICY-001
func TestPermissionHooks_StaticDenyRunsBeforeInteractiveApproval(t *testing.T) {
	hub := NewPermissionHub(time.Second)
	sender := &scriptedPermissionSender{
		hub:       hub,
		responses: []PermissionResponse{{Approved: true, Remember: true}},
	}
	hub.SetSender(sender)

	executor := NewToolExecutor(nil, nil)
	executor.AddHook(NewPermissionHook(hub, WithPolicy(DefaultBaselinePolicy())))
	executor.AddHook(NewToolPermissionHook(NewToolPermissions(nil, []string{"shell"})))

	executed := false
	ctx := context.WithValue(context.Background(), ctxKeySessionID, "session-static-deny")
	_, err := executor.executeWithHooks(ctx, &ToolCallInfo{
		Name:      "shell",
		Source:    "skill",
		Arguments: map[string]any{"command": "printf forbidden"},
	}, func(context.Context) (string, error) {
		executed = true
		return "unexpected", nil
	})
	if err == nil {
		t.Fatal("static deny should block the tool")
	}
	if got := sender.callCount(); got != 0 {
		t.Fatalf("static deny emitted %d interactive approval request(s), want 0", got)
	}
	if executed {
		t.Fatal("static deny allowed the tool side effect")
	}
}

func TestPermissionHook_SafeToolSkipped(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	hook := NewPermissionHook(hub)
	ctx := context.WithValue(context.Background(), ctxKeySessionID, "sess-1")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "search", Source: "skill"}); err != nil {
		t.Fatalf("safe tool should not require approval: %v", err)
	}
}

func TestPermissionHook_NoSenderDenied(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	hook := NewPermissionHook(hub)
	ctx := context.WithValue(context.Background(), ctxKeySessionID, "sess-1")
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "shell", Source: "skill"}); err == nil {
		t.Fatal("dangerous tool with no sender should be denied")
	}
}
