package web

import (
	"context"
	"testing"
	"time"

	"nhooyr.io/websocket/wsjson"
)

// REG-TOOL-APPROVAL-TRANSPORT-001
func TestWebAdapterSendsApprovalTerminalOnlyToCurrentSessionConnection(t *testing.T) {
	a := New()
	oldConn, oldCtx, _ := dialWebAdapter(t, a)
	waitForWebAdapterConnections(t, a, 1)
	oldChatID := onlyWebAdapterChatID(t, a)
	currentConn, currentCtx, _ := dialWebAdapter(t, a)
	waitForWebAdapterConnections(t, a, 2)
	currentChatID := webAdapterChatIDExcept(t, a, oldChatID)

	const (
		requestID       = "approval-terminal-current"
		sessionID       = "session-terminal-current"
		ownerID         = "desktop-user"
		invocationID    = "invocation-terminal-current"
		argumentsDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		scopeDigest     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	deadline := time.Now().UTC().Add(time.Minute)
	a.sessionConns.Store(sessionID, sessionConnectionBinding{chatID: currentChatID, ownerID: ownerID})
	a.approvalBindings.Store(requestID, approvalTransportBinding{
		requestID: requestID, ownerID: ownerID, sessionID: sessionID, chatID: oldChatID,
		invocationID: invocationID, argumentsDigest: argumentsDigest,
		securityScopeDigest: scopeDigest, scopeSchemaVersion: 1, expiresAt: deadline,
	})

	err := a.SendPermissionTerminal(context.Background(), &PermissionTerminalData{
		RequestID: requestID, SessionID: sessionID, OwnerID: ownerID, InvocationID: invocationID,
		ArgumentsDigest: argumentsDigest, SecurityScopeDigest: scopeDigest,
		ScopeSchemaVersion: 1, DeadlineAt: deadline, TerminalResult: "fenced",
	})
	if err != nil {
		t.Fatalf("send approval terminal: %v", err)
	}

	var payload map[string]any
	if err := wsjson.Read(currentCtx, currentConn, &payload); err != nil {
		t.Fatalf("current session connection did not receive terminal: %v", err)
	}
	for key, want := range map[string]string{
		"type":                  "tool_approval_terminal",
		"request_id":            requestID,
		"session_id":            sessionID,
		"owner_id":              ownerID,
		"invocation_id":         invocationID,
		"arguments_digest":      argumentsDigest,
		"security_scope_digest": scopeDigest,
		"deadline_at":           deadline.Format(time.RFC3339Nano),
		"terminal_result":       "fenced",
	} {
		if got, _ := payload[key].(string); got != want {
			t.Errorf("terminal %s = %q, want %q", key, got, want)
		}
	}
	if got := payload["scope_schema_version"]; got != float64(1) {
		t.Errorf("terminal scope_schema_version = %#v, want 1", got)
	}
	metadata, ok := payload["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("terminal metadata = %#v, want object", payload["metadata"])
	}
	for key, want := range map[string]string{
		"request_id":            requestID,
		"session_id":            sessionID,
		"owner_id":              ownerID,
		"invocation_id":         invocationID,
		"arguments_digest":      argumentsDigest,
		"security_scope_digest": scopeDigest,
		"scope_schema_version":  "1",
		"deadline_at":           deadline.Format(time.RFC3339Nano),
		"terminal_result":       "fenced",
	} {
		if got, _ := metadata[key].(string); got != want {
			t.Errorf("terminal metadata %s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"decision_id", "idempotency_key"} {
		if _, ok := payload[key]; ok {
			t.Errorf("backend terminal unexpectedly contains top-level %s", key)
		}
		if _, ok := metadata[key]; ok {
			t.Errorf("backend terminal unexpectedly contains metadata %s", key)
		}
	}
	if _, ok := a.approvalBindings.Load(requestID); ok {
		t.Fatal("durable terminal retained stale approval transport binding")
	}

	readCtx, cancel := context.WithTimeout(oldCtx, 100*time.Millisecond)
	defer cancel()
	var unexpected map[string]any
	if err := wsjson.Read(readCtx, oldConn, &unexpected); err == nil {
		t.Fatalf("stale session connection received terminal broadcast: %#v", unexpected)
	}
}

func webAdapterChatIDExcept(t *testing.T, a *WebAdapter, excluded string) string {
	t.Helper()
	var matched string
	a.conns.Range(func(key, _ any) bool {
		chatID, _ := key.(string)
		if chatID != "" && chatID != excluded {
			matched = chatID
			return false
		}
		return true
	})
	if matched == "" {
		t.Fatalf("no active WebSocket chat ID except %q", excluded)
	}
	return matched
}
