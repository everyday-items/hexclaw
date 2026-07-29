package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestSaveAssistantReply_RuntimeWireSurvivesReloadWithPreallocatedID(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := WithAssistantMessageID(context.Background(), "msg-assistant-007")
	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{
		Platform: adapter.PlatformWeb,
		UserID:   "runtime-wire-user",
		Content:  "seed",
	})
	if err != nil {
		t.Fatal(err)
	}
	event, ok := adapter.NewToolRuntimeEvent(
		adapter.RuntimeEventToolCompleted,
		"call-1",
		"web_search",
		map[string]struct{}{"web_search": {}},
	)
	if !ok {
		t.Fatal("expected allowlisted event")
	}
	if _, err := mgr.SaveAssistantReply(ctx, sess.ID, "answer", AssistantMeta{
		ReasoningDisclosure: adapter.ReasoningDisclosure{Visibility: adapter.ReasoningNotExposed},
		RuntimeEvents: []adapter.SequencedRuntimeEvent{{
			Sequence: 2,
			Event:    *event,
		}},
		LastSequence: 3,
	}); err != nil {
		t.Fatal(err)
	}

	messages, err := store.ListMessages(ctx, sess.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Role != "assistant" {
			continue
		}
		if message.ID != "msg-assistant-007" {
			t.Fatalf("persisted ID = %q, want preallocated ID", message.ID)
		}
		var meta struct {
			AssistantMessageID string                          `json:"assistant_message_id"`
			BackendMessageID   string                          `json:"backend_message_id"`
			MessageID          string                          `json:"message_id"`
			Disclosure         adapter.ReasoningDisclosure     `json:"reasoning_disclosure"`
			RuntimeEvents      []adapter.SequencedRuntimeEvent `json:"runtime_events"`
			LastSequence       uint64                          `json:"last_sequence"`
		}
		if err := json.Unmarshal([]byte(message.Metadata), &meta); err != nil {
			t.Fatal(err)
		}
		if meta.AssistantMessageID != message.ID ||
			meta.BackendMessageID != message.ID ||
			meta.MessageID != message.ID {
			t.Fatalf("reloaded aliases diverged: %+v", meta)
		}
		if meta.Disclosure.Visibility != adapter.ReasoningNotExposed ||
			meta.LastSequence != 3 ||
			len(meta.RuntimeEvents) != 1 ||
			meta.RuntimeEvents[0].Sequence != 2 {
			t.Fatalf("reloaded runtime snapshot = %+v", meta)
		}
		return
	}
	t.Fatal("assistant message not found")
}

func TestSaveAssistantReplyPersistsReasoningOnlyWhenExplicitlyVisible(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()
	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{
		Platform: adapter.PlatformWeb,
		UserID:   "reasoning-privacy-user",
		Content:  "seed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.SaveAssistantReply(ctx, sess.ID, "private answer", AssistantMeta{
		MessageID: "msg-private",
		Reasoning: "private chain of thought",
		ReasoningDisclosure: adapter.ReasoningDisclosure{
			Visibility: adapter.ReasoningNotExposed,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.SaveAssistantReply(ctx, sess.ID, "public answer", AssistantMeta{
		MessageID: "msg-public",
		Reasoning: "public provider summary",
		ReasoningDisclosure: adapter.ReasoningDisclosure{
			Visibility: adapter.ReasoningVisible,
			Source:     "ollama",
			Dialect:    "message.thinking",
			Provider:   "ollama",
			Model:      "trusted-model",
		},
	}); err != nil {
		t.Fatal(err)
	}
	messages, err := store.ListMessages(ctx, sess.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		var meta map[string]any
		if message.Role != "assistant" || json.Unmarshal([]byte(message.Metadata), &meta) != nil {
			continue
		}
		switch message.ID {
		case "msg-private":
			if _, leaked := meta["reasoning"]; leaked {
				t.Fatalf("not_exposed persisted reasoning: %s", message.Metadata)
			}
		case "msg-public":
			if meta["reasoning"] != "public provider summary" {
				t.Fatalf("visible reasoning not persisted: %s", message.Metadata)
			}
		}
	}
}
