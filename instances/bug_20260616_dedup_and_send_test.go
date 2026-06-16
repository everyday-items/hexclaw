package instances

// BUG-20260616: two instance-manager defects.
//   I1 wrapHandler recorded the dedup event BEFORE invoking the handler, so a
//      transient handler failure left the event_id permanently marked "seen" and
//      the platform's redelivery was silently dropped -> permanent message loss.
//   I2 Send's provider fallback iterated m.running (random map order), so with
//      two instances of the same provider the proactive message went to a random
//      instance each call.

import (
	"context"
	"fmt"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type recordingAdapter struct {
	stubAdapter
	called bool
}

func (a *recordingAdapter) Send(context.Context, string, *adapter.Reply) error {
	a.called = true
	return nil
}

func TestBug20260616_TransientFailureAllowsRedelivery(t *testing.T) {
	m, cleanup := newTestManager(t)
	defer cleanup()

	inst := &Instance{Name: "feishu-1", Provider: "feishu"}
	attempts := 0
	h := m.wrapHandler(inst, func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		attempts++
		if attempts == 1 {
			return nil, fmt.Errorf("transient upstream error")
		}
		return &adapter.Reply{Content: "ok"}, nil
	})

	msg := &adapter.Message{ID: "evt-1"}
	if _, err := h(context.Background(), msg); err == nil {
		t.Fatal("first attempt should surface the transient error")
	}

	// Platform redelivers the same event_id: must be retried, not dropped.
	reply, err := h(context.Background(), msg)
	if err != nil {
		t.Fatalf("redelivery errored: %v", err)
	}
	if reply == nil || reply.Content != "ok" {
		t.Fatalf("redelivery silently dropped (dedup not rolled back on failure); reply=%v attempts=%d", reply, attempts)
	}
}

func TestBug20260616_SendProviderFallbackDeterministic(t *testing.T) {
	m, cleanup := newTestManager(t)
	defer cleanup()

	a := &recordingAdapter{}
	b := &recordingAdapter{}
	m.mu.Lock()
	m.running["feishu-a"] = a
	m.running["feishu-b"] = b
	m.metadata["feishu-a"] = &Instance{Name: "feishu-a", Provider: "feishu"}
	m.metadata["feishu-b"] = &Instance{Name: "feishu-b", Provider: "feishu"}
	m.mu.Unlock()

	seen := map[string]int{}
	for i := 0; i < 256; i++ {
		a.called, b.called = false, false
		if err := m.Send(context.Background(), "feishu", "chat", &adapter.Reply{}); err != nil {
			t.Fatalf("send: %v", err)
		}
		switch {
		case a.called:
			seen["feishu-a"]++
		case b.called:
			seen["feishu-b"]++
		}
	}
	if len(seen) != 1 {
		t.Fatalf("Send provider fallback non-deterministic: %v", seen)
	}
	if _, ok := seen["feishu-a"]; !ok {
		t.Fatalf("must deterministically pick smallest instance name 'feishu-a', got %v", seen)
	}
}
