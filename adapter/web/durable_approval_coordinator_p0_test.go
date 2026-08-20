package web

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket/wsjson"
)

func TestDurableApprovalHandlerOwnsACKReplayWithoutTransportBinding(t *testing.T) {
	a := New()
	deadline := time.Now().UTC().Add(time.Minute)
	var calls atomic.Int32
	a.SetDurableApprovalDecisionHandler(func(data ApprovalResponseData) ApprovalDecisionReceipt {
		calls.Add(1)
		return ApprovalDecisionReceipt{
			RequestID: data.RequestID, InvocationID: data.InvocationID,
			OwnerID: data.OwnerID, SessionID: "session-durable",
			DecisionID: "decision-original", Decision: data.Decision,
			IdempotencyKey:  data.IdempotencyKey,
			ArgumentsDigest: data.ArgumentsDigest, SecurityScopeDigest: data.SecurityScopeDigest,
			ScopeSchemaVersion: data.ScopeSchemaVersion, DeadlineAt: data.DeadlineAt,
			TerminalResult: "approved_once", ACKStatus: "accepted",
			Replayed: calls.Load() > 1,
		}
	})
	response := ApprovalResponseData{
		RequestID: "approval-restart", OwnerID: "owner-durable",
		SessionID: "session-durable", DeadlineAt: deadline,
		DecisionID: "decision-transport-retry", InvocationID: "invocation-restart",
		Decision: "approved_once", IdempotencyKey: "idem-restart",
		ArgumentsDigest: "args-restart", SecurityScopeDigest: "scope-restart",
		ScopeSchemaVersion: 1, responderChatID: "replacement-socket",
	}
	first := a.approvalACK(response)
	second := a.approvalACK(response)
	if calls.Load() != 2 {
		t.Fatalf("durable coordinator calls = %d, want 2 (transport cache must not authorize)", calls.Load())
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same idempotency replay changed ACK:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if first.Status != "accepted" || first.DecisionID != "decision-original" ||
		first.Metadata["terminal_result"] != "approved_once" {
		t.Fatalf("durable ACK = %+v, want canonical persisted receipt", first)
	}
}

func TestDurableApprovalHandlerRejectsCoordinatorIdentityDrift(t *testing.T) {
	a := New()
	deadline := time.Now().UTC().Add(time.Minute)
	a.SetDurableApprovalDecisionHandler(func(data ApprovalResponseData) ApprovalDecisionReceipt {
		return ApprovalDecisionReceipt{
			RequestID: data.RequestID, InvocationID: "different-invocation",
			OwnerID: data.OwnerID, SessionID: "session-durable",
			DecisionID: data.DecisionID, Decision: data.Decision,
			IdempotencyKey:  data.IdempotencyKey,
			ArgumentsDigest: data.ArgumentsDigest, SecurityScopeDigest: data.SecurityScopeDigest,
			ScopeSchemaVersion: data.ScopeSchemaVersion, DeadlineAt: data.DeadlineAt,
			TerminalResult: "approved_once", ACKStatus: "accepted",
		}
	})
	ack := a.approvalACK(ApprovalResponseData{
		RequestID: "approval-drift", OwnerID: "owner-durable", DecisionID: "decision-drift",
		SessionID: "session-durable", DeadlineAt: deadline,
		InvocationID: "invocation-drift", Decision: "approved_once", IdempotencyKey: "idem-drift",
		ArgumentsDigest: "args-drift", SecurityScopeDigest: "scope-drift", ScopeSchemaVersion: 1,
		responderChatID: "socket-drift",
	})
	if ack.Status != "rejected" || ack.Metadata["terminal_result"] != "identity_mismatch" {
		t.Fatalf("identity-drift ACK = %+v, want rejected identity_mismatch", ack)
	}
}

func TestBindSessionRequestsBackendPendingReplayOnlyOnNewPhysicalConnection(t *testing.T) {
	a := New()
	var calls atomic.Int32
	a.SetPendingApprovalReplayHandler(func(_ context.Context, ownerID, sessionID string) []*PermissionRequestData {
		calls.Add(1)
		if ownerID != "owner-reconnect" || sessionID != "session-reconnect" {
			t.Errorf("replay identity = (%q, %q)", ownerID, sessionID)
		}
		return []*PermissionRequestData{{
			ID: "approval-reconnect", OwnerID: ownerID, InvocationID: "invocation-reconnect",
			ToolName: "shell", Arguments: map[string]any{"command": "true"},
			ArgumentsDigest: "args-reconnect", SecurityScopeDigest: "scope-reconnect",
			ScopeSchemaVersion: 1, DeadlineAt: time.Now().Add(time.Minute),
		}}
	})
	if !a.bindSession("session-reconnect", "chat-1", "owner-reconnect") {
		t.Fatal("first session bind failed")
	}
	if !a.bindSession("session-reconnect", "chat-1", "owner-reconnect") {
		t.Fatal("same-connection session rebind failed")
	}
	if !a.bindSession("session-reconnect", "chat-2", "owner-reconnect") {
		t.Fatal("replacement connection session bind failed")
	}
	if calls.Load() != 2 {
		t.Fatalf("pending replay calls = %d, want first bind + replacement bind only", calls.Load())
	}
}

func TestAuthenticatedReconnectReceivesSamePendingApprovalWire(t *testing.T) {
	a := New()
	a.SetPendingApprovalReplayHandler(func(_ context.Context, ownerID, sessionID string) []*PermissionRequestData {
		return []*PermissionRequestData{{
			ID: "approval-wire-reconnect", OwnerID: ownerID, InvocationID: "invocation-wire-reconnect",
			ToolName: "shell", Arguments: map[string]any{"command": "true"},
			ArgumentsDigest: "args-wire-reconnect", SecurityScopeDigest: "scope-wire-reconnect",
			ScopeSchemaVersion: 1, DeadlineAt: time.Now().UTC().Add(time.Minute),
			Risk: "dangerous", Reason: "same durable pending request",
		}}
	})
	conn, ctx, _ := dialWebAdapter(t, a)
	waitForWebAdapterConnections(t, a, 1)
	chatID := onlyWebAdapterChatID(t, a)
	if !a.bindSession("session-wire-reconnect", chatID, "desktop-user") {
		t.Fatal("authenticated reconnect bind failed")
	}
	var replay wsMessage
	if err := wsjson.Read(ctx, conn, &replay); err != nil {
		t.Fatalf("read pending approval replay: %v", err)
	}
	if replay.Type != "tool_approval_request" || replay.RequestID != "approval-wire-reconnect" ||
		replay.InvocationID != "invocation-wire-reconnect" || replay.ArgumentsDigest != "args-wire-reconnect" ||
		replay.SecurityScopeDigest != "scope-wire-reconnect" || replay.ScopeSchemaVersion != 1 {
		t.Fatalf("replayed approval identity drifted: %+v", replay)
	}
}
