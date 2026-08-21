package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
)

type deadlineExceededBeforeWallContext struct {
	context.Context
	deadline time.Time
	done     chan struct{}
}

func (c *deadlineExceededBeforeWallContext) Deadline() (time.Time, bool) {
	return c.deadline, true
}

func (c *deadlineExceededBeforeWallContext) Done() <-chan struct{} {
	return c.done
}

func (c *deadlineExceededBeforeWallContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestPermissionHubDeadlineExceededUsesFrozenDeadlineForExpiredTerminal(t *testing.T) {
	const ownerID = "owner-deadline-frozen"
	const sessionID = "session-deadline-frozen"
	store := newDurableApprovalTestStore(
		t, filepath.Join(t.TempDir(), "deadline-frozen.db"), ownerID, sessionID,
	)
	t.Cleanup(func() { _ = store.Close() })
	hub := NewPermissionHubWithRememberedGrantStore(2*time.Second, store)
	ctx := &deadlineExceededBeforeWallContext{
		Context:  approvalOwnerContext(ownerID, sessionID),
		deadline: time.Now().Add(time.Second),
		done:     make(chan struct{}),
	}
	sender := &terminalCapturePermissionSender{
		cancel:          func() { close(ctx.done) },
		terminal:        make(chan *PermissionTerminal, 1),
		terminalContext: make(chan error, 1),
	}
	hub.SetSender(sender)
	req := &PermissionRequest{
		ID: "approval-deadline-frozen", ToolName: "browser",
		Arguments: map[string]any{"fixture": "deadline-frozen"}, Risk: "sensitive",
	}

	approved, err := hub.RequestApproval(ctx, sessionID, req)
	if approved || err == nil {
		t.Fatalf("deadline-exceeded approval = (%v, %v), want denied timeout", approved, err)
	}

	receipt, err := store.GetToolApprovalReceipt(context.Background(), req.ID)
	if err != nil {
		t.Fatalf("read deadline receipt: %v", err)
	}
	terminal := receivePermissionTerminal(t, sender.terminal)
	assertPermissionTerminalMatchesReceipt(t, terminal, receipt)
	if receipt.TerminalResult != storage.ToolApprovalTerminalExpired ||
		receipt.ReleaseState != storage.ToolApprovalReleaseFenced {
		t.Fatalf("deadline receipt = %+v, want expired/fenced", receipt)
	}
	if contextErr := receivePermissionTerminalContext(t, sender.terminalContext); contextErr != nil {
		t.Fatalf("terminal transport inherited deadline context: %v", contextErr)
	}
}
