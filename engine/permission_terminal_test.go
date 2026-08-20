package engine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
)

type terminalCapturePermissionSender struct {
	cancel          context.CancelFunc
	terminal        chan *PermissionTerminal
	terminalContext chan error
}

func (s *terminalCapturePermissionSender) SendPermissionRequest(context.Context, string, *PermissionRequest) error {
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

func (s *terminalCapturePermissionSender) SendPermissionTerminal(ctx context.Context, terminal *PermissionTerminal) error {
	copyOfTerminal := *terminal
	s.terminal <- &copyOfTerminal
	s.terminalContext <- ctx.Err()
	return nil
}

// REG-TOOL-APPROVAL-DEADLINE-001
func TestPermissionHubProjectsDurableExpiredTerminalWithCompleteIdentity(t *testing.T) {
	store := newDurableApprovalTestStore(
		t, filepath.Join(t.TempDir(), "terminal-expired.db"), "owner-terminal-expired", "session-terminal-expired",
	)
	defer store.Close()
	hub := NewPermissionHubWithRememberedGrantStore(30*time.Millisecond, store)
	sender := &terminalCapturePermissionSender{
		terminal:        make(chan *PermissionTerminal, 1),
		terminalContext: make(chan error, 1),
	}
	hub.SetSender(sender)
	req := &PermissionRequest{
		ID: "approval-terminal-expired", ToolName: "shell",
		Arguments: map[string]any{"command": "printf terminal"}, Risk: "dangerous",
	}

	approved, err := hub.RequestApproval(
		approvalOwnerContext("owner-terminal-expired", "session-terminal-expired"),
		"session-terminal-expired", req,
	)
	if approved || err == nil {
		t.Fatalf("expired approval = (%v, %v), want denied timeout", approved, err)
	}

	receipt, err := store.GetToolApprovalReceipt(context.Background(), req.ID)
	if err != nil {
		t.Fatalf("read durable terminal receipt: %v", err)
	}
	terminal := receivePermissionTerminal(t, sender.terminal)
	assertPermissionTerminalMatchesReceipt(t, terminal, receipt)
	if terminal.TerminalResult != storage.ToolApprovalTerminalExpired {
		t.Fatalf("terminal_result = %q, want expired", terminal.TerminalResult)
	}
	if contextErr := receivePermissionTerminalContext(t, sender.terminalContext); contextErr != nil {
		t.Fatalf("terminal transport inherited expired request context: %v", contextErr)
	}
	raw, err := json.Marshal(terminal)
	if err != nil {
		t.Fatalf("marshal terminal projection: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal terminal projection: %v", err)
	}
	if _, ok := payload["decision_id"]; ok {
		t.Fatal("backend expiry terminal fabricated decision_id")
	}
	if _, ok := payload["idempotency_key"]; ok {
		t.Fatal("backend expiry terminal fabricated idempotency_key")
	}
}

// REG-TOOL-APPROVAL-TRANSPORT-001
func TestPermissionHubProjectsDurableFencedTerminalBeforeDeadline(t *testing.T) {
	store := newDurableApprovalTestStore(
		t, filepath.Join(t.TempDir(), "terminal-fenced.db"), "owner-terminal-fenced", "session-terminal-fenced",
	)
	defer store.Close()
	hub := NewPermissionHubWithRememberedGrantStore(time.Minute, store)
	requestCtx, cancel := context.WithCancel(
		approvalOwnerContext("owner-terminal-fenced", "session-terminal-fenced"),
	)
	sender := &terminalCapturePermissionSender{
		cancel:          cancel,
		terminal:        make(chan *PermissionTerminal, 1),
		terminalContext: make(chan error, 1),
	}
	hub.SetSender(sender)
	req := &PermissionRequest{
		ID: "approval-terminal-fenced", ToolName: "file_edit",
		Arguments: map[string]any{"path": "/workspace/report.md"}, Risk: "sensitive",
	}

	approved, err := hub.RequestApproval(requestCtx, "session-terminal-fenced", req)
	if approved || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled approval = (%v, %v), want denied context.Canceled", approved, err)
	}

	receipt, err := store.GetToolApprovalReceipt(context.Background(), req.ID)
	if err != nil {
		t.Fatalf("read durable terminal receipt: %v", err)
	}
	terminal := receivePermissionTerminal(t, sender.terminal)
	assertPermissionTerminalMatchesReceipt(t, terminal, receipt)
	if terminal.TerminalResult != storage.ToolApprovalTerminalFenced {
		t.Fatalf("terminal_result = %q, want fenced", terminal.TerminalResult)
	}
	if !time.Now().Before(terminal.DeadlineAt) {
		t.Fatalf("fenced terminal deadline = %v, want backend deadline still in the future", terminal.DeadlineAt)
	}
	if contextErr := receivePermissionTerminalContext(t, sender.terminalContext); contextErr != nil {
		t.Fatalf("terminal transport inherited cancelled request context: %v", contextErr)
	}
}

func receivePermissionTerminal(t *testing.T, terminals <-chan *PermissionTerminal) *PermissionTerminal {
	t.Helper()
	select {
	case terminal := <-terminals:
		return terminal
	case <-time.After(time.Second):
		t.Fatal("backend durable terminal was not projected")
		return nil
	}
}

func receivePermissionTerminalContext(t *testing.T, contexts <-chan error) error {
	t.Helper()
	select {
	case err := <-contexts:
		return err
	case <-time.After(time.Second):
		t.Fatal("terminal sender context was not observed")
		return nil
	}
}

func assertPermissionTerminalMatchesReceipt(t *testing.T, terminal *PermissionTerminal, receipt *storage.ToolApprovalReceipt) {
	t.Helper()
	if terminal == nil || receipt == nil {
		t.Fatalf("terminal=%+v receipt=%+v, want both non-nil", terminal, receipt)
	}
	if terminal.RequestID != receipt.RequestID || terminal.SessionID != receipt.ResolvedSessionID ||
		terminal.OwnerID != receipt.OwnerID || terminal.InvocationID != receipt.InvocationID ||
		terminal.ArgumentsDigest != receipt.ArgumentsDigest ||
		terminal.SecurityScopeDigest != receipt.SecurityScopeDigest ||
		terminal.ScopeSchemaVersion != receipt.ScopeSchemaVersion ||
		!terminal.DeadlineAt.Equal(receipt.DeadlineAt) || terminal.TerminalResult != receipt.TerminalResult {
		t.Fatalf("terminal projection = %+v, want durable identity %+v", terminal, receipt)
	}
}
