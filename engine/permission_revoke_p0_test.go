package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
)

// REG-TOOL-APPROVAL-REVOKE-001 工程子门（engine 层）：
// 1) RevokeToolGrant 是生产可达公共 API：durable 撤销 + 进程内 cache 同次清理、幂等；
// 2) PermissionHook 命中 ActionDeny 时先撤销该 owner+tool 的 remembered grant 再拒绝。

// memoryRememberedGrantStore 夹具：按 owner+canonical tool 维度撤销（与 SQLite 语义一致）。
func (s *memoryRememberedGrantStore) RevokeToolGrants(_ context.Context, ownerID, canonicalToolName, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.grants {
		if strings.HasPrefix(key, ownerID+"\x00") && strings.Contains(key, "\x00"+canonicalToolName+"\x00") {
			delete(s.grants, key)
		}
	}
	return nil
}

// durable 模式：RevokeToolGrant 后 durable grant 失效、进程内 cache 清理、再次调用重新审批。
func TestPermissionHubRevokeToolGrantDurable(t *testing.T) {
	store := newDurableApprovalTestStore(t, filepath.Join(t.TempDir(), "revoke-durable.db"), "owner-revoke", "session-revoke")
	t.Cleanup(func() { _ = store.Close() })
	hub, err := NewDurablePermissionHub(context.Background(), 5*time.Second, store)
	if err != nil {
		t.Fatalf("new durable hub: %v", err)
	}
	observed := make(chan *PermissionRequest, 4)
	hub.SetSender(&durableApprovalSender{t: t, hub: hub, store: store, requestObserved: observed})
	ctx := approvalOwnerContext("owner-revoke", "session-revoke")

	// 第一次请求：remember 批准。
	result := make(chan error, 1)
	go func() {
		_, err := hub.RequestApproval(ctx, "session-revoke", &PermissionRequest{
			ID: "revoke-durable-1", ToolName: "file_edit", Risk: "sensitive", Arguments: map[string]any{"path": "a.txt"},
		})
		result <- err
	}()
	first := <-observed
	if first.SecurityScopeDigest == "" {
		t.Fatal("first approval request lacks canonical security scope digest")
	}
	hub.HandleResponseReceipt(PermissionResponse{
		RequestID: first.ID, OwnerID: first.OwnerID, SessionID: "session-revoke",
		InvocationID: first.InvocationID, ArgumentsDigest: first.ArgumentsDigest,
		SecurityScopeDigest: first.SecurityScopeDigest, ScopeSchemaVersion: first.ScopeSchemaVersion,
		DecisionID: "decision-revoke-1", Decision: storage.ToolApprovalDecisionApprovedRemember,
		Approved: true, Remember: true, IdempotencyKey: "idem-revoke-1",
	})
	if err := <-result; err != nil {
		t.Fatalf("first request: %v", err)
	}
	allowed, err := store.HasRememberedGrant(ctx, "owner-revoke", "session-revoke", "file_edit", first.SecurityScopeDigest)
	if err != nil || !allowed {
		t.Fatalf("active grant before revoke = (%v, %v), want (true, nil)", allowed, err)
	}

	if err := hub.RevokeToolGrant(ctx, "owner-revoke", "file_edit"); err != nil {
		t.Fatalf("revoke tool grant: %v", err)
	}

	// durable 失效：同一四元 key 不再命中。
	allowed, err = store.HasRememberedGrant(ctx, "owner-revoke", "session-revoke", "file_edit", first.SecurityScopeDigest)
	if err != nil || allowed {
		t.Fatalf("durable grant after revoke = (%v, %v), want (false, nil)", allowed, err)
	}

	// 再次调用必须重新走审批（sender 必须观察到新的 approval request），而不是复用 grant。
	go func() {
		_, err := hub.RequestApproval(ctx, "session-revoke", &PermissionRequest{
			ID: "revoke-durable-2", ToolName: "file_edit", Risk: "sensitive", Arguments: map[string]any{"path": "a.txt"},
		})
		result <- err
	}()
	second := <-observed
	if second.ID == first.ID {
		t.Fatalf("reused the pre-revoke approval identity %q", second.ID)
	}
	hub.HandleResponseReceipt(PermissionResponse{
		RequestID: second.ID, OwnerID: second.OwnerID, SessionID: "session-revoke",
		InvocationID: second.InvocationID, ArgumentsDigest: second.ArgumentsDigest,
		SecurityScopeDigest: second.SecurityScopeDigest, ScopeSchemaVersion: second.ScopeSchemaVersion,
		DecisionID: "decision-revoke-2", Decision: storage.ToolApprovalDecisionDenied,
		IdempotencyKey: "idem-revoke-2",
	})
	if err := <-result; err != nil {
		t.Fatalf("second request: %v", err)
	}

	// 幂等：重复撤销不报错。
	if err := hub.RevokeToolGrant(ctx, "owner-revoke", "file_edit"); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}
}

// in-memory 模式：RevokeToolGrant 必须同时清理进程内 remembered cache。
func TestPermissionHubRevokeToolGrantClearsProcessCache(t *testing.T) {
	grants := &memoryRememberedGrantStore{grants: map[string]bool{}}
	hub := NewPermissionHubWithRememberedGrantStore(time.Second, grants)
	sender := &scriptedPermissionSender{hub: hub, responses: []PermissionResponse{
		{Approved: true, Remember: true},
		{Approved: true},
	}}
	hub.SetSender(sender)
	ctx := approvalOwnerContext("owner-cache", "session-cache")

	allowed, err := hub.RequestApproval(ctx, "session-cache", &PermissionRequest{
		ID: "revoke-cache-1", ToolName: "browser", Risk: "sensitive",
	})
	if err != nil || !allowed {
		t.Fatalf("first request = (%v, %v), want (true, nil)", allowed, err)
	}
	if sender.calls != 1 {
		t.Fatalf("sender calls after first = %d, want 1", sender.calls)
	}

	// remember 生效：第二次同 key 不再请求审批。
	allowed, err = hub.RequestApproval(ctx, "session-cache", &PermissionRequest{
		ID: "revoke-cache-2", ToolName: "browser", Risk: "sensitive",
	})
	if err != nil || !allowed {
		t.Fatalf("remembered request = (%v, %v), want (true, nil)", allowed, err)
	}
	if sender.calls != 1 {
		t.Fatalf("remembered reuse produced extra approval request: calls=%d", sender.calls)
	}

	if err := hub.RevokeToolGrant(ctx, "owner-cache", "browser"); err != nil {
		t.Fatalf("revoke tool grant: %v", err)
	}

	// 进程内 cache 已清理：第三次必须重新审批。
	allowed, err = hub.RequestApproval(ctx, "session-cache", &PermissionRequest{
		ID: "revoke-cache-3", ToolName: "browser", Risk: "sensitive",
	})
	if err != nil || !allowed {
		t.Fatalf("post-revoke request = (%v, %v), want (true, nil)", allowed, err)
	}
	if sender.calls != 2 {
		t.Fatalf("post-revoke reuse skipped approval: calls=%d, want 2", sender.calls)
	}
}

// 策略收紧：BeforeToolCall 命中 ActionDeny 必须先撤销 remembered grant 再拒绝。
func TestPermissionHookPolicyDenyRevokesRememberedGrant(t *testing.T) {
	store := newDurableApprovalTestStore(t, filepath.Join(t.TempDir(), "revoke-deny.db"), "owner-deny", "session-deny")
	t.Cleanup(func() { _ = store.Close() })
	hub, err := NewDurablePermissionHub(context.Background(), 5*time.Second, store)
	if err != nil {
		t.Fatalf("new durable hub: %v", err)
	}
	observed := make(chan *PermissionRequest, 4)
	hub.SetSender(&durableApprovalSender{t: t, hub: hub, store: store, requestObserved: observed})
	ctx := approvalOwnerContext("owner-deny", "session-deny")

	result := make(chan error, 1)
	go func() {
		_, err := hub.RequestApproval(ctx, "session-deny", &PermissionRequest{
			ID: "deny-1", ToolName: "file_edit", Risk: "sensitive", Arguments: map[string]any{"path": "a.txt"},
		})
		result <- err
	}()
	first := <-observed
	hub.HandleResponseReceipt(PermissionResponse{
		RequestID: first.ID, OwnerID: first.OwnerID, SessionID: "session-deny",
		InvocationID: first.InvocationID, ArgumentsDigest: first.ArgumentsDigest,
		SecurityScopeDigest: first.SecurityScopeDigest, ScopeSchemaVersion: first.ScopeSchemaVersion,
		DecisionID: "decision-deny-1", Decision: storage.ToolApprovalDecisionApprovedRemember,
		Approved: true, Remember: true, IdempotencyKey: "idem-deny-1",
	})
	if err := <-result; err != nil {
		t.Fatalf("remember request: %v", err)
	}
	allowed, err := store.HasRememberedGrant(ctx, "owner-deny", "session-deny", "file_edit", first.SecurityScopeDigest)
	if err != nil || !allowed {
		t.Fatalf("active grant before deny = (%v, %v), want (true, nil)", allowed, err)
	}

	hook := NewPermissionHook(hub, WithPolicy(NewPermissionPolicy(ActionAllow,
		PolicyRule{Name: "deny-fe", ToolPattern: "file_edit", Action: ActionDeny, Reason: "deny file edit"},
	)))
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "file_edit", Arguments: map[string]any{"path": "a.txt"}}); err == nil {
		t.Fatal("policy deny did not block the tool call")
	}

	// deny 即撤销：同一四元 key 的 durable grant 必须失效。
	allowed, err = store.HasRememberedGrant(ctx, "owner-deny", "session-deny", "file_edit", first.SecurityScopeDigest)
	if err != nil || allowed {
		t.Fatalf("grant after policy deny = (%v, %v), want (false, nil)", allowed, err)
	}

	// 撤销后再次请求必须重新审批，grant 不得复活。
	go func() {
		_, err := hub.RequestApproval(ctx, "session-deny", &PermissionRequest{
			ID: "deny-2", ToolName: "file_edit", Risk: "sensitive", Arguments: map[string]any{"path": "a.txt"},
		})
		result <- err
	}()
	second := <-observed
	if second.ID == first.ID {
		t.Fatalf("revived the pre-deny approval identity %q", second.ID)
	}
	hub.HandleResponseReceipt(PermissionResponse{
		RequestID: second.ID, OwnerID: second.OwnerID, SessionID: "session-deny",
		InvocationID: second.InvocationID, ArgumentsDigest: second.ArgumentsDigest,
		SecurityScopeDigest: second.SecurityScopeDigest, ScopeSchemaVersion: second.ScopeSchemaVersion,
		DecisionID: "decision-deny-2", Decision: storage.ToolApprovalDecisionDenied,
		IdempotencyKey: "idem-deny-2",
	})
	if err := <-result; err != nil {
		t.Fatalf("post-deny request: %v", err)
	}
}
