package web

import (
	"testing"
	"time"

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
