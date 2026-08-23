package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/skill"
)

type permissionIdentitySender struct {
	request chan *PermissionRequest
}

func (s *permissionIdentitySender) SendPermissionRequest(_ context.Context, _ string, req *PermissionRequest) error {
	s.request <- req
	return nil
}

func TestPermissionIdentityP0_ResponseRequiresCompleteExactBinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PermissionResponse)
	}{
		{name: "missing owner", mutate: func(r *PermissionResponse) { r.OwnerID = "" }},
		{name: "missing session", mutate: func(r *PermissionResponse) { r.SessionID = "" }},
		{name: "missing invocation", mutate: func(r *PermissionResponse) { r.InvocationID = "" }},
		{name: "missing arguments digest", mutate: func(r *PermissionResponse) { r.ArgumentsDigest = "" }},
		{name: "missing security scope digest", mutate: func(r *PermissionResponse) { r.SecurityScopeDigest = "" }},
		{name: "wrong owner", mutate: func(r *PermissionResponse) { r.OwnerID = "other-owner" }},
		{name: "wrong session", mutate: func(r *PermissionResponse) { r.SessionID = "other-session" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := NewPermissionHub(time.Second)
			sender := &permissionIdentitySender{request: make(chan *PermissionRequest, 1)}
			hub.SetSender(sender)
			ctx, cancel := context.WithCancel(skill.WithAuthenticatedUser(context.Background(), "owner-1"))
			defer cancel()

			result := make(chan error, 1)
			go func() {
				approved, err := hub.RequestApproval(ctx, "session-1", &PermissionRequest{
					ID: "approval-1", ToolName: "code_exec", Arguments: map[string]any{"code": "1+1"},
				})
				if err == nil && !approved {
					err = context.Canceled
				}
				result <- err
			}()

			req := <-sender.request
			exact := PermissionResponse{
				RequestID: req.ID, OwnerID: req.OwnerID, SessionID: "session-1",
				InvocationID: req.InvocationID, ArgumentsDigest: req.ArgumentsDigest,
				SecurityScopeDigest: req.SecurityScopeDigest, Approved: true,
			}
			invalid := exact
			tt.mutate(&invalid)
			if got := hub.HandleResponseResult(invalid); got != "identity_mismatch" {
				t.Fatalf("incomplete/mismatched response result=%q, want identity_mismatch", got)
			}
			select {
			case err := <-result:
				t.Fatalf("invalid response released pending execution: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
			if got := hub.HandleResponseResult(exact); got != "approved_once" {
				t.Fatalf("exact response result=%q, want approved_once", got)
			}
			if err := <-result; err != nil {
				t.Fatalf("exact response did not approve execution: %v", err)
			}
		})
	}
}

func TestPermissionIdentityP0_RequestWithoutAuthenticatedOwnerFailsClosed(t *testing.T) {
	hub := NewPermissionHub(time.Second)
	hub.SetSender(&permissionIdentitySender{request: make(chan *PermissionRequest, 1)})
	approved, err := hub.RequestApproval(context.Background(), "session-1", &PermissionRequest{
		ID: "approval-no-owner", ToolName: "code_exec", Arguments: map[string]any{"code": "1+1"},
	})
	if err == nil || approved {
		t.Fatalf("ownerless approval request approved=%v err=%v, want fail-closed error", approved, err)
	}
}

// REG-TOOL-APPROVAL-LIFECYCLE-OWNER-NA：审批 owner 是认证用户，不能把 routed Agent
// 的注销误当成 owner delete。当前产品没有用户删除操作，因此不接 Agent 注销生命周期。
func TestToolApprovalOwnerIsAuthenticatedPrincipalNotRoutedAgent(t *testing.T) {
	hub := NewPermissionHub(time.Second)
	sender := &permissionIdentitySender{request: make(chan *PermissionRequest, 1)}
	hub.SetSender(sender)
	ctx, cancel := context.WithCancel(skill.WithRoutedAgent(
		skill.WithAuthenticatedUser(context.Background(), "desktop-owner"),
		"routed-agent",
	))
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, err := hub.RequestApproval(ctx, "session-owner-boundary", &PermissionRequest{
			ID: "approval-owner-boundary", ToolName: "code_exec", Arguments: map[string]any{"code": "1+1"},
		})
		result <- err
	}()

	req := <-sender.request
	if req.OwnerID != "desktop-owner" {
		t.Fatalf("approval owner = %q, want authenticated principal desktop-owner", req.OwnerID)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel pending approval error = %v, want context.Canceled", err)
	}
}

// REG-TOOL-APPROVAL-LIFECYCLE-DISABLE-NA：当前 DisableForSession 只是进程内
// session policy override，不是产品级工具禁用或 durable grant 撤销操作。
func TestToolDisableForSessionIsTransientPolicyOnly(t *testing.T) {
	perms := NewToolPermissions(nil, nil)
	perms.DisableForSession("session-disabled", "code_exec")
	if err := perms.Check("code_exec", "session-disabled"); err == nil {
		t.Fatal("disabled session unexpectedly retained tool permission")
	}
	if err := perms.Check("code_exec", "other-session"); err != nil {
		t.Fatalf("session-local disable leaked to another session: %v", err)
	}
	perms.ClearSession("session-disabled")
	if err := perms.Check("code_exec", "session-disabled"); err != nil {
		t.Fatalf("cleared transient disable remained active: %v", err)
	}
}

func TestNormalizePermissionResponseDecisionRejectsContradictoryFlags(t *testing.T) {
	for _, response := range []PermissionResponse{
		{Decision: "approved_once", Approved: false, Remember: false},
		{Decision: "approved_once", Approved: true, Remember: true},
		{Decision: "approved_remember", Approved: true, Remember: false},
		{Decision: "approved_remember", Approved: false, Remember: true},
		{Decision: "denied", Approved: true, Remember: false},
		{Decision: "denied", Approved: false, Remember: true},
	} {
		if decision, ok := normalizePermissionResponseDecision(response); ok {
			t.Errorf("contradictory response %+v normalized as %q", response, decision)
		}
	}
	for _, response := range []PermissionResponse{
		{Decision: "approved_once", Approved: true},
		{Decision: "approved_remember", Approved: true, Remember: true},
		{Decision: "denied"},
	} {
		if _, ok := normalizePermissionResponseDecision(response); !ok {
			t.Errorf("exact response %+v was rejected", response)
		}
	}
}
