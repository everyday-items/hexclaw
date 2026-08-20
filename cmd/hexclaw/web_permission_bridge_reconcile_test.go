package main

import (
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/storage"
)

// REG-TOOL-APPROVAL-RECONCILE-007
func TestWebPermissionReconciliationBridgePreservesCompleteDurableReceipt(t *testing.T) {
	deadline := time.Unix(1_900_000_003, 789).UTC()
	input := &engine.PermissionReceiptReconciliationResult{Receipt: &storage.ToolApprovalReceipt{
		RequestID: "approval-reconcile-bridge", OwnerID: "owner-reconcile-bridge",
		ResolvedSessionID: "session-reconcile-bridge", InvocationID: "invocation-reconcile-bridge",
		ArgumentsDigest: "arguments-reconcile-bridge", SecurityScopeDigest: "scope-reconcile-bridge",
		ScopeSchemaVersion: 1, DeadlineAt: deadline, DecisionID: "decision-reconcile-bridge",
		Decision: storage.ToolApprovalDecisionDenied, IdempotencyKey: "idem-reconcile-bridge",
		TerminalResult: storage.ToolApprovalDecisionDenied, ACKStatus: storage.ToolApprovalACKAccepted,
		Replayed: true,
	}}
	got := webPermissionReconciliationResult(input)
	if got.Receipt == nil || got.Request != nil ||
		got.Receipt.RequestID != input.Receipt.RequestID || got.Receipt.OwnerID != input.Receipt.OwnerID ||
		got.Receipt.SessionID != input.Receipt.ResolvedSessionID ||
		got.Receipt.InvocationID != input.Receipt.InvocationID ||
		got.Receipt.ArgumentsDigest != input.Receipt.ArgumentsDigest ||
		got.Receipt.SecurityScopeDigest != input.Receipt.SecurityScopeDigest ||
		got.Receipt.ScopeSchemaVersion != input.Receipt.ScopeSchemaVersion ||
		!got.Receipt.DeadlineAt.Equal(input.Receipt.DeadlineAt) ||
		got.Receipt.DecisionID != input.Receipt.DecisionID || got.Receipt.Decision != input.Receipt.Decision ||
		got.Receipt.IdempotencyKey != input.Receipt.IdempotencyKey ||
		got.Receipt.TerminalResult != input.Receipt.TerminalResult || got.Receipt.ACKStatus != input.Receipt.ACKStatus ||
		!got.Receipt.Replayed {
		t.Fatalf("reconciliation bridge = %+v, want exact durable receipt", got)
	}
}
