package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
)

// REG-TOOL-APPROVAL-RECONCILE-001
func TestPermissionHubReconcileApprovalReceiptReturnsCommittedACKWithoutSecondRelease(t *testing.T) {
	store := newDurableApprovalTestStore(
		t, filepath.Join(t.TempDir(), "reconcile-ack.db"), "owner-reconcile-ack", "session-reconcile-ack",
	)
	defer store.Close()
	hub := NewPermissionHubWithRememberedGrantStore(time.Second, store)
	sender := &durableApprovalSender{t: t, hub: hub, store: store, responseKey: "reconcile-ack"}
	hub.SetSender(sender)
	req := &PermissionRequest{
		ID: "approval-reconcile-ack", ToolName: "shell",
		Arguments: map[string]any{"command": "printf reconciled"}, Risk: "dangerous",
	}
	if approved, err := hub.RequestApproval(
		approvalOwnerContext("owner-reconcile-ack", "session-reconcile-ack"), "session-reconcile-ack", req,
	); err != nil || !approved {
		t.Fatalf("seed approval = (%v, %v), want (true, nil)", approved, err)
	}
	before, err := store.GetToolApprovalReceipt(context.Background(), req.ID)
	if err != nil {
		t.Fatalf("read seed durable receipt: %v", err)
	}
	if before.ConsumedAt.IsZero() {
		t.Fatalf("seed receipt = %+v, want already consumed one release", before)
	}

	result, err := hub.ReconcileApprovalReceipt(context.Background(), reconciliationIdentity(req, "session-reconcile-ack"))
	if err != nil {
		t.Fatalf("reconcile committed ACK: %v", err)
	}
	if result == nil || result.Request != nil || result.Receipt == nil {
		t.Fatalf("reconcile result = %+v, want durable ACK only", result)
	}
	if result.Receipt.TerminalResult != storage.ToolApprovalDecisionApprovedOnce ||
		result.Receipt.ACKStatus != storage.ToolApprovalACKAccepted || !result.Receipt.Replayed {
		t.Fatalf("reconciled ACK = %+v, want replayed approved_once/accepted receipt", result.Receipt)
	}
	after, err := store.GetToolApprovalReceipt(context.Background(), req.ID)
	if err != nil {
		t.Fatalf("read reconciled durable receipt: %v", err)
	}
	if !after.ConsumedAt.Equal(before.ConsumedAt) || after.ReleaseState != before.ReleaseState {
		t.Fatalf("reconciliation repeated or changed release: before=%+v after=%+v", before, after)
	}
}

// REG-TOOL-APPROVAL-RECONCILE-002
func TestPermissionHubReconcileApprovalReceiptReturnsOnlyExactDurableTerminal(t *testing.T) {
	store := newDurableApprovalTestStore(
		t, filepath.Join(t.TempDir(), "reconcile-terminal.db"), "owner-reconcile-terminal", "session-reconcile-terminal",
	)
	defer store.Close()
	deadline := time.Now().UTC().Add(time.Minute)
	req := &storage.ToolApprovalRequest{
		RequestID: "approval-reconcile-terminal", InvocationID: "invocation-reconcile-terminal",
		OwnerID: "owner-reconcile-terminal", ResolvedSessionID: "session-reconcile-terminal",
		CanonicalToolName: "file_edit", ArgumentsDigest: "arguments-reconcile-terminal",
		SecurityScopeDigest: "scope-reconcile-terminal", ScopeSchemaVersion: storage.CurrentToolApprovalScopeSchemaVersion,
		DeadlineAt: deadline,
	}
	if created, err := store.CreateToolApprovalRequest(context.Background(), req); err != nil || !created {
		t.Fatalf("create durable request = (%v, %v)", created, err)
	}
	if _, err := store.FenceToolApprovalRequest(context.Background(), req.RequestID, "disconnect", time.Now().UTC()); err != nil {
		t.Fatalf("fence durable request: %v", err)
	}
	hub := NewPermissionHubWithRememberedGrantStore(time.Second, store)

	result, err := hub.ReconcileApprovalReceipt(context.Background(), PermissionReceiptReconciliation{
		RequestID: req.RequestID, OwnerID: req.OwnerID, SessionID: req.ResolvedSessionID,
		InvocationID: req.InvocationID, ArgumentsDigest: req.ArgumentsDigest,
		SecurityScopeDigest: req.SecurityScopeDigest, ScopeSchemaVersion: req.ScopeSchemaVersion,
		DeadlineAt: req.DeadlineAt,
	})
	if err != nil {
		t.Fatalf("reconcile fenced terminal: %v", err)
	}
	if result == nil || result.Request != nil || result.Receipt == nil ||
		result.Receipt.TerminalResult != storage.ToolApprovalTerminalFenced ||
		!result.Receipt.DeadlineAt.Equal(req.DeadlineAt) {
		t.Fatalf("reconciled terminal = %+v, want exact fenced receipt", result)
	}

	wrong := reconciliationIdentityFromStorageRequest(req)
	wrong.ArgumentsDigest = "other-arguments-digest"
	if result, err := hub.ReconcileApprovalReceipt(context.Background(), wrong); !errors.Is(err, storage.ErrToolApprovalIdentityMismatch) || result != nil {
		t.Fatalf("mismatched reconciliation = (%+v, %v), want nil identity mismatch", result, err)
	}
}

// REG-TOOL-APPROVAL-RECONCILE-003
func TestPermissionHubReconcileApprovalReceiptReplaysOnlyLiveExactPendingRequest(t *testing.T) {
	store := newDurableApprovalTestStore(
		t, filepath.Join(t.TempDir(), "reconcile-pending.db"), "owner-reconcile-pending", "session-reconcile-pending",
	)
	defer store.Close()
	hub := NewPermissionHubWithRememberedGrantStore(2*time.Second, store)
	observed := make(chan *PermissionRequest, 1)
	hub.SetSender(&durableApprovalSender{t: t, hub: hub, store: store, requestObserved: observed})
	resultCh := make(chan error, 1)
	go func() {
		_, err := hub.RequestApproval(
			approvalOwnerContext("owner-reconcile-pending", "session-reconcile-pending"), "session-reconcile-pending",
			&PermissionRequest{ID: "approval-reconcile-pending", ToolName: "browser", Arguments: map[string]any{"url": "https://example.test"}},
		)
		resultCh <- err
	}()
	req := <-observed

	result, err := hub.ReconcileApprovalReceipt(context.Background(), reconciliationIdentity(req, "session-reconcile-pending"))
	if err != nil {
		t.Fatalf("reconcile pending request: %v", err)
	}
	if result == nil || result.Receipt != nil || result.Request == nil ||
		result.Request.ID != req.ID || result.Request.OwnerID != req.OwnerID ||
		result.Request.InvocationID != req.InvocationID ||
		result.Request.ArgumentsDigest != req.ArgumentsDigest ||
		result.Request.SecurityScopeDigest != req.SecurityScopeDigest ||
		result.Request.ScopeSchemaVersion != req.ScopeSchemaVersion ||
		!result.Request.DeadlineAt.Equal(req.DeadlineAt) {
		t.Fatalf("pending reconciliation = %+v, want exact live request", result)
	}
	hub.HandleResponseReceipt(PermissionResponse{
		RequestID: req.ID, OwnerID: req.OwnerID, SessionID: "session-reconcile-pending",
		InvocationID: req.InvocationID, ArgumentsDigest: req.ArgumentsDigest,
		SecurityScopeDigest: req.SecurityScopeDigest, ScopeSchemaVersion: req.ScopeSchemaVersion,
		DecisionID: "decision-reconcile-pending", Decision: storage.ToolApprovalDecisionDenied,
		IdempotencyKey: "idem-reconcile-pending",
	})
	if err := <-resultCh; err != nil {
		t.Fatalf("complete pending approval: %v", err)
	}
}

func reconciliationIdentity(req *PermissionRequest, sessionID string) PermissionReceiptReconciliation {
	return PermissionReceiptReconciliation{
		RequestID: req.ID, OwnerID: req.OwnerID, SessionID: sessionID,
		InvocationID: req.InvocationID, ArgumentsDigest: req.ArgumentsDigest,
		SecurityScopeDigest: req.SecurityScopeDigest, ScopeSchemaVersion: req.ScopeSchemaVersion,
		DeadlineAt: req.DeadlineAt,
	}
}

func reconciliationIdentityFromStorageRequest(req *storage.ToolApprovalRequest) PermissionReceiptReconciliation {
	return PermissionReceiptReconciliation{
		RequestID: req.RequestID, OwnerID: req.OwnerID, SessionID: req.ResolvedSessionID,
		InvocationID: req.InvocationID, ArgumentsDigest: req.ArgumentsDigest,
		SecurityScopeDigest: req.SecurityScopeDigest, ScopeSchemaVersion: req.ScopeSchemaVersion,
		DeadlineAt: req.DeadlineAt,
	}
}
