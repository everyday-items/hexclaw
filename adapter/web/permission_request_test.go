package web

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func TestWebAdapter_SendPermissionRequestBroadcastsWhenSessionBindingMissing(t *testing.T) {
	a := New()
	conn, ctx, _ := dialWebAdapter(t, a)
	waitForWebAdapterConnections(t, a, 1)

	err := a.SendPermissionRequest(ctx, "sess-missing-binding", &PermissionRequestData{
		ID:       "approval-1",
		ToolName: "code_exec",
		Risk:     "high",
		Reason:   "execute code",
	})
	if err != nil {
		t.Fatalf("SendPermissionRequest returned error: %v", err)
	}

	var frame wsMessage
	if err := wsjson.Read(ctx, conn, &frame); err != nil {
		t.Fatalf("read approval frame failed: %v", err)
	}
	if frame.Type != "tool_approval_request" {
		t.Fatalf("frame type = %q, want tool_approval_request", frame.Type)
	}
	if frame.SessionID != "sess-missing-binding" {
		t.Fatalf("session_id = %q, want sess-missing-binding", frame.SessionID)
	}
	if frame.RequestID != "approval-1" {
		t.Fatalf("request_id = %q, want approval-1", frame.RequestID)
	}
	if frame.Metadata["tool_name"] != "code_exec" {
		t.Fatalf("tool_name = %q, want code_exec", frame.Metadata["tool_name"])
	}
}

// REG-TOOL-APPROVAL-ARGS-001
func TestPermissionRequestMessagePreservesFrozenArgumentsAndScopeIdentity(t *testing.T) {
	arguments := map[string]any{
		"target":  "/workspace/报告.md",
		"options": map[string]any{"replace": true, "labels": []any{"a", nil, "中"}},
	}
	data := &PermissionRequestData{
		ID:        "approval-wire-1",
		OwnerID:   "owner-wire-1",
		ToolName:  "file_edit",
		Arguments: arguments,
		Risk:      "sensitive",
		Reason:    "edit one approved file scope",
	}
	setPermissionRequestDataField(t, data, "InvocationID", "invocation-wire-1")
	setPermissionRequestDataField(t, data, "ArgumentsDigest", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	setPermissionRequestDataField(t, data, "SecurityScopeDigest", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	setPermissionRequestDataField(t, data, "DeadlineAt", time.Unix(1_800_000_000, 0).UTC())

	raw, err := json.Marshal(permissionRequestMessage("session-wire-1", data))
	if err != nil {
		t.Fatalf("marshal permission request: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal permission request: %v", err)
	}

	var wantArguments any
	wantRaw, _ := json.Marshal(arguments)
	_ = json.Unmarshal(wantRaw, &wantArguments)
	if !reflect.DeepEqual(payload["arguments"], wantArguments) {
		t.Errorf("wire arguments = %#v, want frozen %#v", payload["arguments"], wantArguments)
	}
	assertWireString(t, payload, "invocation_id", "invocation-wire-1")
	assertWireString(t, payload, "owner_id", "owner-wire-1")
	assertWireString(t, payload, "tool_name", "file_edit")
	assertWireString(t, payload, "arguments_digest", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	assertWireString(t, payload, "security_scope_digest", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if deadline, ok := payload["deadline_at"].(string); !ok || deadline == "" {
		t.Errorf("wire deadline_at = %#v, want non-empty backend timestamp", payload["deadline_at"])
	}
}

// REG-TOOL-APPROVAL-TRANSPORT-001
func TestWebAdapter_ToolApprovalResponseReturnsIdempotentACK(t *testing.T) {
	a := New()
	conn, ctx, _ := dialWebAdapter(t, a)
	a.SetApprovalResponseHandler(func(_ string, _, _ bool) {})

	response := wsMessage{
		Type:    "tool_approval_response",
		Content: "approved_remember",
		Metadata: map[string]string{
			"request_id":            "approval-ack-1",
			"approval_request_id":   "approval-ack-1",
			"invocation_id":         "invocation-ack-1",
			"decision":              "approved_remember",
			"idempotency_key":       "decision-key-1",
			"arguments_digest":      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"security_scope_digest": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}

	first := writeApprovalResponseAndReadACK(t, ctx, conn, response)
	second := writeApprovalResponseAndReadACK(t, ctx, conn, response)
	if first.Type != "tool_approval_ack" {
		t.Fatalf("ACK type = %q, want tool_approval_ack", first.Type)
	}
	for key, want := range map[string]string{
		"approval_request_id": "approval-ack-1",
		"invocation_id":       "invocation-ack-1",
		"decision":            "approved_remember",
		"idempotency_key":     "decision-key-1",
	} {
		if got := first.Metadata[key]; got != want {
			t.Errorf("ACK %s = %q, want %q", key, got, want)
		}
	}
	if first.Metadata["terminal_result"] == "" {
		t.Error("ACK has no durable terminal_result")
	}
	if first.Status != "accepted" {
		t.Errorf("first ACK status = %q, want accepted", first.Status)
	}
	if second.Status != "already_accepted" {
		t.Errorf("duplicate ACK status = %q, want already_accepted", second.Status)
	}
	if !reflect.DeepEqual(first.Metadata, second.Metadata) {
		t.Fatalf("duplicate response changed durable ACK metadata:\nfirst=%#v\nsecond=%#v", first.Metadata, second.Metadata)
	}
}

// Cross-repository contract: hexclaw-desktop sends a stable decision_id on
// the owning request socket and waits for top-level ACK correlation/status.
func TestWebAdapter_DesktopApprovalDecisionWireCompatibility(t *testing.T) {
	a := New()
	firstConn, ctx, _ := dialWebAdapter(t, a)
	secondConn, _, _ := dialWebAdapter(t, a)
	waitForWebAdapterConnections(t, a, 2)

	callbacks := 0
	a.SetApprovalDecisionHandler(func(data ApprovalResponseData) string {
		callbacks++
		if data.IdempotencyKey != "idempotency-desktop-1" {
			if data.RequestID == "approval-desktop-1" {
				t.Errorf("idempotency key = %q, want explicit stable key", data.IdempotencyKey)
			}
		}
		switch data.RequestID {
		case "approval-expired-1":
			return "not_pending"
		case "approval-rejected-1":
			return "identity_mismatch"
		}
		return "approved_remember"
	})

	response := map[string]any{
		"type":        "tool_approval_response",
		"content":     "approved_remember",
		"request_id":  "approval-desktop-1",
		"decision_id": "decision-desktop-1",
		"metadata": map[string]string{
			"request_id":            "approval-desktop-1",
			"decision_id":           "decision-desktop-1",
			"invocation_id":         "invocation-desktop-1",
			"decision":              "approved_remember",
			"idempotency_key":       "idempotency-desktop-1",
			"arguments_digest":      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"security_scope_digest": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}

	first := writeRawApprovalResponseAndReadACK(t, ctx, firstConn, response)
	second := writeRawApprovalResponseAndReadACK(t, ctx, secondConn, response)
	for key, want := range map[string]string{
		"type":        "tool_approval_ack",
		"request_id":  "approval-desktop-1",
		"decision_id": "decision-desktop-1",
	} {
		if got, _ := first[key].(string); got != want {
			t.Errorf("first ACK %s = %q, want %q", key, got, want)
		}
		if got, _ := second[key].(string); got != want {
			t.Errorf("duplicate ACK %s = %q, want %q", key, got, want)
		}
	}
	if got := first["status"]; got != "accepted" {
		t.Errorf("first ACK status = %#v, want accepted", got)
	}
	if got := second["status"]; got != "already_accepted" {
		t.Errorf("duplicate ACK status = %#v, want already_accepted", got)
	}
	for _, tc := range []struct {
		requestID  string
		decisionID string
		wantStatus string
	}{
		{"approval-expired-1", "decision-expired-1", "expired"},
		{"approval-rejected-1", "decision-rejected-1", "rejected"},
	} {
		terminalResponse := map[string]any{
			"type":        "tool_permission_response",
			"content":     "approved_once",
			"request_id":  tc.requestID,
			"decision_id": tc.decisionID,
			"metadata": map[string]string{
				"request_id":      tc.requestID,
				"decision_id":     tc.decisionID,
				"invocation_id":   "invocation-" + tc.requestID,
				"decision":        "approved_once",
				"idempotency_key": "idempotency-" + tc.requestID,
			},
		}
		ack := writeRawApprovalResponseAndReadACK(t, ctx, firstConn, terminalResponse)
		if got := ack["status"]; got != tc.wantStatus {
			t.Errorf("%s ACK status = %#v, want %s", tc.requestID, got, tc.wantStatus)
		}
		duplicate := writeRawApprovalResponseAndReadACK(t, ctx, secondConn, terminalResponse)
		if got := duplicate["status"]; got != tc.wantStatus {
			t.Errorf("duplicate %s ACK status = %#v, want stable %s", tc.requestID, got, tc.wantStatus)
		}
	}
	if callbacks != 3 {
		t.Errorf("durable decision callback count = %d, want 3 with reconnect duplicate invoked once", callbacks)
	}
}

// REG-TOOL-APPROVAL-TRANSPORT-001
func TestWebAdapter_RejectsLegacyDecisionAndMissingIdempotencyKey(t *testing.T) {
	a := New()
	conn, ctx, _ := dialWebAdapter(t, a)
	callbacks := 0
	a.SetApprovalDecisionHandler(func(ApprovalResponseData) string {
		callbacks++
		return "approved_once"
	})

	for _, tc := range []struct {
		name     string
		decision string
		idemKey  string
	}{
		{name: "legacy approved", decision: "approved", idemKey: "legacy-explicit-key"},
		{name: "missing idempotency key", decision: "approved_once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := map[string]any{
				"type":        "tool_approval_response",
				"content":     tc.decision,
				"request_id":  "approval-" + tc.name,
				"decision_id": "decision-" + tc.name,
				"metadata": map[string]string{
					"request_id":      "approval-" + tc.name,
					"decision_id":     "decision-" + tc.name,
					"invocation_id":   "invocation-" + tc.name,
					"decision":        tc.decision,
					"idempotency_key": tc.idemKey,
				},
			}
			ack := writeRawApprovalResponseAndReadACK(t, ctx, conn, response)
			if got := ack["status"]; got != "rejected" {
				t.Fatalf("ACK status = %#v, want rejected", got)
			}
		})
	}
	if callbacks != 0 {
		t.Fatalf("invalid response reached approval coordinator %d time(s), want 0", callbacks)
	}
	cached := 0
	a.approvalACKs.Range(func(_, _ any) bool {
		cached++
		return true
	})
	if cached != 0 {
		t.Fatalf("invalid responses populated %d ACK cache record(s), want 0", cached)
	}
}

func setPermissionRequestDataField(t *testing.T, data *PermissionRequestData, name string, value any) {
	t.Helper()
	field := reflect.ValueOf(data).Elem().FieldByName(name)
	if !field.IsValid() {
		t.Errorf("PermissionRequestData missing %s", name)
		return
	}
	incoming := reflect.ValueOf(value)
	if !incoming.Type().AssignableTo(field.Type()) {
		t.Errorf("PermissionRequestData.%s type = %s, cannot assign %s", name, field.Type(), incoming.Type())
		return
	}
	field.Set(incoming)
}

func assertWireString(t *testing.T, payload map[string]any, key, want string) {
	t.Helper()
	if got, _ := payload[key].(string); got != want {
		t.Errorf("wire %s = %q, want %q", key, got, want)
	}
}

func writeApprovalResponseAndReadACK(t *testing.T, ctx context.Context, conn *websocket.Conn, response wsMessage) wsMessage {
	t.Helper()
	if err := wsjson.Write(ctx, conn, response); err != nil {
		t.Fatalf("write approval response: %v", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	var ack wsMessage
	if err := wsjson.Read(readCtx, conn, &ack); err != nil {
		t.Fatalf("read approval ACK: %v", err)
	}
	return ack
}

func writeRawApprovalResponseAndReadACK(t *testing.T, ctx context.Context, conn *websocket.Conn, response map[string]any) map[string]any {
	t.Helper()
	if err := wsjson.Write(ctx, conn, response); err != nil {
		t.Fatalf("write Desktop approval response: %v", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	var ack map[string]any
	if err := wsjson.Read(readCtx, conn, &ack); err != nil {
		t.Fatalf("read Desktop approval ACK: %v", err)
	}
	return ack
}

func waitForWebAdapterConnections(t *testing.T, a *WebAdapter, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countWebAdapterConnections(a) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("active websocket connections = %d, want at least %d", countWebAdapterConnections(a), want)
}

func countWebAdapterConnections(a *WebAdapter) int {
	count := 0
	a.conns.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}
