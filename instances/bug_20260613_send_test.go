package instances

// BUG-20260613 (audit L2): cron jobs' IM deliver targets (feishu/discord/...)
// only logged "kept in history" and never sent. Manager.Send is the proactive
// delivery path the scheduler's Deliverer routes through. These tests pin its
// branch behavior: resolve by instance name, fall back to provider/platform,
// and error when nothing matches.

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type captureAdapter struct {
	stubAdapter
	chatID  string
	content string
	sent    bool
}

func (a *captureAdapter) Send(_ context.Context, chatID string, reply *adapter.Reply) error {
	a.chatID = chatID
	a.content = reply.Content
	a.sent = true
	return nil
}

func TestBug20260613_SendResolvesByInstanceName(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()
	cap := &captureAdapter{}
	mgr.running["feishu-main"] = cap
	mgr.metadata["feishu-main"] = &Instance{Name: "feishu-main", Provider: "feishu"}

	if err := mgr.Send(context.Background(), "feishu-main", "chat-1", &adapter.Reply{Content: "结果"}); err != nil {
		t.Fatalf("Send by name: %v", err)
	}
	if !cap.sent || cap.chatID != "chat-1" || cap.content != "结果" {
		t.Errorf("must forward chatID+content to the named adapter, got %+v", cap)
	}
}

func TestBug20260613_SendFallsBackToProvider(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()
	cap := &captureAdapter{}
	// Running adapter is keyed by a custom name; the cron target is the platform.
	mgr.running["my-feishu-bot"] = cap
	mgr.metadata["my-feishu-bot"] = &Instance{Name: "my-feishu-bot", Provider: "feishu"}

	if err := mgr.Send(context.Background(), "feishu", "chat-2", &adapter.Reply{Content: "日报"}); err != nil {
		t.Fatalf("Send by provider: %v", err)
	}
	if !cap.sent || cap.chatID != "chat-2" {
		t.Errorf("must resolve target by provider when no name matches, got %+v", cap)
	}
}

func TestBug20260613_SendNoMatchErrors(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()
	// No running adapter for the target.
	err := mgr.Send(context.Background(), "discord", "chat-3", &adapter.Reply{Content: "x"})
	if err == nil {
		t.Error("Send must error when no running adapter matches the target")
	}
}
