package web

import (
	"context"
	"testing"
	"time"

	"nhooyr.io/websocket/wsjson"
)

// REG-TOOL-APPROVAL-RECONCILE-004
func TestWebAdapterToolApprovalReconcileReturnsCompleteDurableACK(t *testing.T) {
	a := New()
	deadline := time.Unix(1_900_000_001, 123).UTC()
	called := false
	a.SetApprovalReconciliationHandler(func(_ context.Context, data ApprovalReconciliationData) (ApprovalReconciliationResult, error) {
		called = true
		if data.RequestID != "approval-reconcile-wire" || data.OwnerID != "desktop-user" ||
			data.SessionID != "session-reconcile-wire" || data.InvocationID != "invocation-reconcile-wire" ||
			data.ArgumentsDigest != "arguments-reconcile-wire" || data.SecurityScopeDigest != "scope-reconcile-wire" ||
			data.ScopeSchemaVersion != 1 || !data.DeadlineAt.Equal(deadline) {
			t.Errorf("reconciliation input = %+v, want exact Desktop identity", data)
		}
		return ApprovalReconciliationResult{Receipt: &ApprovalDecisionReceipt{
			RequestID: data.RequestID, OwnerID: data.OwnerID, SessionID: data.SessionID,
			InvocationID: data.InvocationID, ArgumentsDigest: data.ArgumentsDigest,
			SecurityScopeDigest: data.SecurityScopeDigest, ScopeSchemaVersion: data.ScopeSchemaVersion,
			DeadlineAt: data.DeadlineAt, DecisionID: "decision-reconcile-wire",
			Decision: "denied", IdempotencyKey: "idem-reconcile-wire",
			TerminalResult: "denied", ACKStatus: "accepted", Replayed: true,
		}}, nil
	})
	conn, ctx, _ := dialWebAdapter(t, a)
	if err := wsjson.Write(ctx, conn, wsMessage{
		Type: "tool_approval_reconcile", RequestID: "approval-reconcile-wire",
		OwnerID: "desktop-user", SessionID: "session-reconcile-wire",
		InvocationID: "invocation-reconcile-wire", ArgumentsDigest: "arguments-reconcile-wire",
		SecurityScopeDigest: "scope-reconcile-wire", ScopeSchemaVersion: 1,
		DeadlineAt: deadline.Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("write reconciliation: %v", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var ack wsMessage
	if err := wsjson.Read(readCtx, conn, &ack); err != nil {
		t.Fatalf("read reconciliation ACK: %v", err)
	}
	if !called {
		t.Fatal("reconciliation did not reach durable backend handler")
	}
	if ack.Type != "tool_approval_ack" || ack.Status != "already_accepted" ||
		ack.RequestID != "approval-reconcile-wire" || ack.SessionID != "session-reconcile-wire" ||
		ack.OwnerID != "desktop-user" || ack.InvocationID != "invocation-reconcile-wire" ||
		ack.ArgumentsDigest != "arguments-reconcile-wire" || ack.SecurityScopeDigest != "scope-reconcile-wire" ||
		ack.ScopeSchemaVersion != 1 || ack.DeadlineAt != deadline.Format(time.RFC3339Nano) ||
		ack.DecisionID != "decision-reconcile-wire" || ack.Decision != "denied" ||
		ack.IdempotencyKey != "idem-reconcile-wire" {
		t.Fatalf("reconciliation ACK = %+v, want complete durable wire", ack)
	}
	for key, want := range map[string]string{
		"session_id": "session-reconcile-wire", "owner_id": "desktop-user",
		"invocation_id": "invocation-reconcile-wire", "arguments_digest": "arguments-reconcile-wire",
		"security_scope_digest": "scope-reconcile-wire", "scope_schema_version": "1",
		"deadline_at": deadline.Format(time.RFC3339Nano), "decision_id": "decision-reconcile-wire",
		"decision": "denied", "idempotency_key": "idem-reconcile-wire", "terminal_result": "denied",
	} {
		if got := ack.Metadata[key]; got != want {
			t.Errorf("ACK metadata %s = %q, want %q", key, got, want)
		}
	}
}

// REG-TOOL-APPROVAL-RECONCILE-005
func TestWebAdapterToolApprovalReconcileReturnsTerminalWithoutDecisionFields(t *testing.T) {
	a := New()
	deadline := time.Unix(1_900_000_002, 456).UTC()
	a.SetApprovalReconciliationHandler(func(_ context.Context, data ApprovalReconciliationData) (ApprovalReconciliationResult, error) {
		return ApprovalReconciliationResult{Receipt: &ApprovalDecisionReceipt{
			RequestID: data.RequestID, OwnerID: data.OwnerID, SessionID: data.SessionID,
			InvocationID: data.InvocationID, ArgumentsDigest: data.ArgumentsDigest,
			SecurityScopeDigest: data.SecurityScopeDigest, ScopeSchemaVersion: data.ScopeSchemaVersion,
			DeadlineAt: data.DeadlineAt,
			// A late decision may exist in durable storage, but terminal wire must never expose it.
			DecisionID: "late-decision", Decision: "approved_once", IdempotencyKey: "late-idempotency",
			TerminalResult: "fenced", ACKStatus: "rejected",
		}}, nil
	})
	conn, ctx, _ := dialWebAdapter(t, a)
	if err := wsjson.Write(ctx, conn, wsMessage{
		Type: "tool_approval_reconcile", RequestID: "approval-reconcile-terminal",
		OwnerID: "desktop-user", SessionID: "session-reconcile-terminal",
		InvocationID: "invocation-reconcile-terminal", ArgumentsDigest: "arguments-reconcile-terminal",
		SecurityScopeDigest: "scope-reconcile-terminal", ScopeSchemaVersion: 1,
		DeadlineAt: deadline.Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("write terminal reconciliation: %v", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var terminal wsMessage
	if err := wsjson.Read(readCtx, conn, &terminal); err != nil {
		t.Fatalf("read reconciliation terminal: %v", err)
	}
	if terminal.Type != "tool_approval_terminal" || terminal.TerminalResult != "fenced" ||
		terminal.RequestID != "approval-reconcile-terminal" || terminal.SessionID != "session-reconcile-terminal" ||
		terminal.OwnerID != "desktop-user" || terminal.InvocationID != "invocation-reconcile-terminal" ||
		terminal.ArgumentsDigest != "arguments-reconcile-terminal" || terminal.SecurityScopeDigest != "scope-reconcile-terminal" ||
		terminal.ScopeSchemaVersion != 1 || terminal.DeadlineAt != deadline.Format(time.RFC3339Nano) {
		t.Fatalf("reconciliation terminal = %+v, want complete fenced terminal", terminal)
	}
	if terminal.DecisionID != "" || terminal.Decision != "" || terminal.IdempotencyKey != "" {
		t.Fatalf("terminal leaked durable decision fields: %+v", terminal)
	}
	for _, key := range []string{"decision_id", "decision", "idempotency_key"} {
		if _, ok := terminal.Metadata[key]; ok {
			t.Errorf("terminal metadata unexpectedly contains %s", key)
		}
	}
}

// REG-TOOL-APPROVAL-RECONCILE-006
func TestWebAdapterToolApprovalReconcileReplaysExactPendingRequest(t *testing.T) {
	a := New()
	deadline := time.Now().UTC().Add(time.Minute)
	a.SetApprovalReconciliationHandler(func(_ context.Context, data ApprovalReconciliationData) (ApprovalReconciliationResult, error) {
		return ApprovalReconciliationResult{Request: &PermissionRequestData{
			ID: data.RequestID, OwnerID: data.OwnerID, InvocationID: data.InvocationID,
			ToolName: "shell", Arguments: map[string]any{"command": "printf pending"},
			ArgumentsDigest: data.ArgumentsDigest, SecurityScopeDigest: data.SecurityScopeDigest,
			ScopeSchemaVersion: data.ScopeSchemaVersion, DeadlineAt: data.DeadlineAt,
			Risk: "dangerous", Reason: "still pending",
		}}, nil
	})
	conn, ctx, _ := dialWebAdapter(t, a)
	if err := wsjson.Write(ctx, conn, wsMessage{
		Type: "tool_approval_reconcile", RequestID: "approval-reconcile-pending",
		OwnerID: "desktop-user", SessionID: "session-reconcile-pending",
		InvocationID: "invocation-reconcile-pending", ArgumentsDigest: "arguments-reconcile-pending",
		SecurityScopeDigest: "scope-reconcile-pending", ScopeSchemaVersion: 1,
		DeadlineAt: deadline.Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("write pending reconciliation: %v", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var request wsMessage
	if err := wsjson.Read(readCtx, conn, &request); err != nil {
		t.Fatalf("read pending reconciliation replay: %v", err)
	}
	if request.Type != "tool_approval_request" || request.RequestID != "approval-reconcile-pending" ||
		request.SessionID != "session-reconcile-pending" || request.OwnerID != "desktop-user" ||
		request.InvocationID != "invocation-reconcile-pending" ||
		request.ArgumentsDigest != "arguments-reconcile-pending" ||
		request.SecurityScopeDigest != "scope-reconcile-pending" || request.ScopeSchemaVersion != 1 ||
		request.DeadlineAt != deadline.Format(time.RFC3339Nano) {
		t.Fatalf("pending reconciliation replay = %+v, want exact pending request", request)
	}
}

// REG-TOOL-APPROVAL-RECONCILE-008
func TestWebAdapterOwnerSocketACKRequiresAndReturnsCompleteIdentity(t *testing.T) {
	a := New()
	deadline := time.Now().UTC().Add(time.Minute)
	calls := 0
	a.SetDurableApprovalDecisionHandler(func(data ApprovalResponseData) ApprovalDecisionReceipt {
		calls++
		if data.RequestID != "approval-owner-ack" || data.OwnerID != "desktop-user" ||
			data.SessionID != "session-owner-ack" || data.InvocationID != "invocation-owner-ack" ||
			data.ArgumentsDigest != "arguments-owner-ack" || data.SecurityScopeDigest != "scope-owner-ack" ||
			data.ScopeSchemaVersion != 1 || !data.DeadlineAt.Equal(deadline) {
			t.Errorf("owner-socket response identity = %+v, want exact complete identity", data)
		}
		return ApprovalDecisionReceipt{
			RequestID: data.RequestID, OwnerID: data.OwnerID, SessionID: data.SessionID,
			InvocationID: data.InvocationID, ArgumentsDigest: data.ArgumentsDigest,
			SecurityScopeDigest: data.SecurityScopeDigest, ScopeSchemaVersion: data.ScopeSchemaVersion,
			DeadlineAt: data.DeadlineAt, DecisionID: data.DecisionID, Decision: data.Decision,
			IdempotencyKey: data.IdempotencyKey, TerminalResult: "approved_once", ACKStatus: "accepted",
		}
	})
	conn, ctx, _ := dialWebAdapter(t, a)
	chatID := onlyWebAdapterChatID(t, a)
	a.approvalBindings.Store("approval-owner-ack", approvalTransportBinding{
		requestID: "approval-owner-ack", ownerID: "desktop-user", sessionID: "session-owner-ack", chatID: chatID,
		invocationID: "invocation-owner-ack", argumentsDigest: "arguments-owner-ack",
		securityScopeDigest: "scope-owner-ack", scopeSchemaVersion: 1, expiresAt: deadline,
	})
	response := wsMessage{
		Type: "tool_approval_response", RequestID: "approval-owner-ack", OwnerID: "desktop-user",
		SessionID: "session-owner-ack", InvocationID: "invocation-owner-ack",
		ArgumentsDigest: "arguments-owner-ack", SecurityScopeDigest: "scope-owner-ack",
		ScopeSchemaVersion: 1, DeadlineAt: deadline.Format(time.RFC3339Nano),
		DecisionID: "decision-owner-ack", Decision: "approved_once", IdempotencyKey: "idem-owner-ack",
	}
	if err := wsjson.Write(ctx, conn, response); err != nil {
		t.Fatalf("write complete owner-socket response: %v", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var ack wsMessage
	if err := wsjson.Read(readCtx, conn, &ack); err != nil {
		t.Fatalf("read complete owner-socket ACK: %v", err)
	}
	if ack.Type != "tool_approval_ack" || ack.Status != "accepted" ||
		ack.RequestID != response.RequestID || ack.OwnerID != response.OwnerID || ack.SessionID != response.SessionID ||
		ack.InvocationID != response.InvocationID || ack.ArgumentsDigest != response.ArgumentsDigest ||
		ack.SecurityScopeDigest != response.SecurityScopeDigest || ack.ScopeSchemaVersion != response.ScopeSchemaVersion ||
		ack.DeadlineAt != response.DeadlineAt || ack.DecisionID != response.DecisionID ||
		ack.Decision != response.Decision || ack.IdempotencyKey != response.IdempotencyKey {
		t.Fatalf("owner-socket ACK = %+v, want complete immutable identity", ack)
	}
	for key, want := range map[string]string{
		"request_id": response.RequestID, "session_id": response.SessionID, "owner_id": response.OwnerID,
		"invocation_id": response.InvocationID, "arguments_digest": response.ArgumentsDigest,
		"security_scope_digest": response.SecurityScopeDigest, "scope_schema_version": "1",
		"deadline_at": response.DeadlineAt, "decision_id": response.DecisionID,
		"decision": response.Decision, "idempotency_key": response.IdempotencyKey,
	} {
		if got := ack.Metadata[key]; got != want {
			t.Errorf("owner-socket ACK metadata %s = %q, want %q", key, got, want)
		}
	}

	response.DeadlineAt = ""
	if err := wsjson.Write(ctx, conn, response); err != nil {
		t.Fatalf("write incomplete owner-socket response: %v", err)
	}
	var rejected wsMessage
	if err := wsjson.Read(readCtx, conn, &rejected); err != nil {
		t.Fatalf("read rejected incomplete owner-socket ACK: %v", err)
	}
	if rejected.Status != "rejected" || rejected.Metadata["terminal_result"] != "identity_mismatch" {
		t.Fatalf("incomplete owner-socket ACK = %+v, want rejected identity mismatch", rejected)
	}
	if calls != 1 {
		t.Fatalf("incomplete owner-socket response reached durable handler %d times, want 1 total", calls)
	}
}
