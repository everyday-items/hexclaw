package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
)

// REG-TOOL-APPROVAL-REVOKE-001 工程子门：RevokeToolGrants 按 owner+canonical
// tool 维度主动撤销 remembered grants（工具禁用/策略收紧路径）。
func TestToolApprovalV70RevokeToolGrants(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tool-approval-revoke.db")
	store, err := New(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	createToolApprovalTestSession(t, store, "owner-1", "session-1")
	createToolApprovalTestSession(t, store, "owner-2", "session-2")

	deadline := time.Now().UTC().Add(time.Minute)
	req := newPendingToolApproval("owner-1", "session-1", "approval-revoke-1", deadline)
	if created, err := store.CreateToolApprovalRequest(ctx, req); err != nil || !created {
		t.Fatalf("create pending approval = (%v, %v), want (true, nil)", created, err)
	}
	receipt, err := store.DecideToolApproval(ctx, exactToolApprovalDecision(
		req, storage.ToolApprovalDecisionApprovedRemember, "idem-revoke-1", time.Now(),
	))
	if err != nil {
		t.Fatalf("decide remembered approval: %v", err)
	}
	if receipt.Replayed || receipt.TerminalResult != storage.ToolApprovalDecisionApprovedRemember {
		t.Fatalf("durable decision receipt = %+v", receipt)
	}

	// 另一 owner 的 grant 保持 active，作为对照。
	req2 := newPendingToolApproval("owner-2", "session-2", "approval-revoke-2", deadline)
	if created, err := store.CreateToolApprovalRequest(ctx, req2); err != nil || !created {
		t.Fatalf("create pending approval 2 = (%v, %v), want (true, nil)", created, err)
	}
	if _, err := store.DecideToolApproval(ctx, exactToolApprovalDecision(
		req2, storage.ToolApprovalDecisionApprovedRemember, "idem-revoke-2", time.Now(),
	)); err != nil {
		t.Fatalf("decide remembered approval 2: %v", err)
	}

	allowed, err := store.HasRememberedGrant(ctx, "owner-1", "session-1", "file_edit", testApprovalScopeDigest)
	if err != nil || !allowed {
		t.Fatalf("active grant before revoke = (%v, %v), want (true, nil)", allowed, err)
	}

	if err := store.RevokeToolGrants(ctx, "owner-1", "file_edit", "tool_revoked"); err != nil {
		t.Fatalf("revoke tool grants: %v", err)
	}

	// 撤销后同一四元 key 不再命中。
	allowed, err = store.HasRememberedGrant(ctx, "owner-1", "session-1", "file_edit", testApprovalScopeDigest)
	if err != nil || allowed {
		t.Fatalf("grant after revoke = (%v, %v), want (false, nil)", allowed, err)
	}

	// 行级撤销证据：active=0 + revoked_at + revoked_reason，工具重新启用不得复活。
	var active int
	var revokedAt int64
	var revokedReason string
	if err := store.db.QueryRowContext(ctx, `
SELECT active, COALESCE(revoked_at, 0), revoked_reason
FROM remembered_permission_grants
WHERE owner_id = ? AND canonical_tool_name = ?`,
		"owner-1", "file_edit",
	).Scan(&active, &revokedAt, &revokedReason); err != nil {
		t.Fatalf("query revoked grant row: %v", err)
	}
	if active != 0 || revokedAt == 0 || revokedReason != "tool_revoked" {
		t.Fatalf("revoked grant row = (active=%d revoked_at=%d reason=%q), want (0,>0,tool_revoked)",
			active, revokedAt, revokedReason)
	}

	// 其他 owner 的行不受影响。
	allowed, err = store.HasRememberedGrant(ctx, "owner-2", "session-2", "file_edit", testApprovalScopeDigest)
	if err != nil || !allowed {
		t.Fatalf("other owner grant = (%v, %v), want (true, nil)", allowed, err)
	}

	// 重复撤销幂等。
	if err := store.RevokeToolGrants(ctx, "owner-1", "file_edit", "tool_revoked"); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}

	// 撤销后只能经 DecideToolApproval 重新 mint，独立 RememberGrant 必须 fail-closed。
	if err := store.RememberGrant(ctx, "owner-1", "session-1", "file_edit", testApprovalScopeDigest); err != storage.ErrToolApprovalDecisionRequired {
		t.Fatalf("re-mint grant via narrow interface = %v, want ErrToolApprovalDecisionRequired", err)
	}
}

// RevokeToolGrants 缺 owner 或 tool 必须拒绝，不得做全量撤销。
func TestToolApprovalV70RevokeToolGrantsRequiresIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := New(filepath.Join(t.TempDir(), "tool-approval-revoke-identity.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	if err := store.RevokeToolGrants(ctx, "", "file_edit", "tool_revoked"); err != storage.ErrToolApprovalIdentityMismatch {
		t.Fatalf("revoke with empty owner = %v, want ErrToolApprovalIdentityMismatch", err)
	}
	if err := store.RevokeToolGrants(ctx, "owner-1", "", "tool_revoked"); err != storage.ErrToolApprovalIdentityMismatch {
		t.Fatalf("revoke with empty tool = %v, want ErrToolApprovalIdentityMismatch", err)
	}
}
