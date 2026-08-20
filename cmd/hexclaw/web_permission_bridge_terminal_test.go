package main

import (
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/engine"
)

var _ engine.PermissionTerminalSender = (*webPermissionBridge)(nil)

// REG-TOOL-APPROVAL-TRANSPORT-001
func TestWebPermissionBridgePreservesDurableTerminalIdentity(t *testing.T) {
	deadline := time.Unix(1_900_000_000, 123).UTC()
	terminal := &engine.PermissionTerminal{
		RequestID: "approval-terminal-bridge", SessionID: "session-terminal-bridge",
		OwnerID: "owner-terminal-bridge", InvocationID: "invocation-terminal-bridge",
		ArgumentsDigest: "args-terminal-bridge", SecurityScopeDigest: "scope-terminal-bridge",
		ScopeSchemaVersion: 1, DeadlineAt: deadline, TerminalResult: "expired",
	}

	got := webPermissionTerminalData(terminal)
	if got == nil || got.RequestID != terminal.RequestID || got.SessionID != terminal.SessionID ||
		got.OwnerID != terminal.OwnerID || got.InvocationID != terminal.InvocationID ||
		got.ArgumentsDigest != terminal.ArgumentsDigest || got.SecurityScopeDigest != terminal.SecurityScopeDigest ||
		got.ScopeSchemaVersion != terminal.ScopeSchemaVersion || !got.DeadlineAt.Equal(terminal.DeadlineAt) ||
		got.TerminalResult != terminal.TerminalResult {
		t.Fatalf("bridge terminal projection = %+v, want %+v", got, terminal)
	}
}
