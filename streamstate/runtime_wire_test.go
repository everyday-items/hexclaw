package streamstate

import (
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestRegistryRuntimeSnapshotKeepsIdentityOrderedEventsAndLastSequence(t *testing.T) {
	r := NewRegistry(time.Minute)
	r.StartMessage("user", "session", "request", "msg-assistant-007")

	completed, _ := adapter.NewToolRuntimeEvent(
		adapter.RuntimeEventToolCompleted,
		"call-1",
		"web_search",
		map[string]struct{}{"web_search": {}},
	)
	started, _ := adapter.NewToolRuntimeEvent(
		adapter.RuntimeEventToolStarted,
		"call-1",
		"web_search",
		map[string]struct{}{"web_search": {}},
	)
	r.Append("request", &adapter.ReplyChunk{
		AssistantMessageID: "msg-assistant-007",
		BackendMessageID:   "msg-assistant-007",
		MessageID:          "msg-assistant-007",
		Sequence:           2,
		RuntimeEvent:       completed,
	})
	r.Append("request", &adapter.ReplyChunk{
		AssistantMessageID: "msg-assistant-007",
		BackendMessageID:   "msg-assistant-007",
		MessageID:          "msg-assistant-007",
		Sequence:           1,
		RuntimeEvent:       started,
	})
	// Exact replay must be idempotent.
	r.Append("request", &adapter.ReplyChunk{
		AssistantMessageID: "msg-assistant-007",
		BackendMessageID:   "msg-assistant-007",
		MessageID:          "msg-assistant-007",
		Sequence:           1,
		RuntimeEvent:       started,
	})

	snapshot, ok := r.GetStreamSnapshot("user", "request")
	if !ok {
		t.Fatal("snapshot not found")
	}
	if snapshot.AssistantMessageID != "msg-assistant-007" ||
		snapshot.BackendMessageID != snapshot.AssistantMessageID ||
		snapshot.MessageID != snapshot.AssistantMessageID {
		t.Fatalf("snapshot aliases diverged: %+v", snapshot)
	}
	if snapshot.LastSequence != 2 {
		t.Fatalf("last_sequence = %d, want 2", snapshot.LastSequence)
	}
	if len(snapshot.RuntimeEvents) != 2 ||
		snapshot.RuntimeEvents[0].Sequence != 1 ||
		snapshot.RuntimeEvents[1].Sequence != 2 {
		t.Fatalf("runtime_events = %+v, want ordered deduplicated events", snapshot.RuntimeEvents)
	}
	if snapshot.ReasoningDisclosure.Visibility != adapter.ReasoningNotExposed {
		t.Fatalf("legacy/unknown disclosure = %+v", snapshot.ReasoningDisclosure)
	}

	// Conflicting identity must not mutate the authoritative snapshot.
	r.Append("request", &adapter.ReplyChunk{
		AssistantMessageID: "spoofed",
		BackendMessageID:   "spoofed",
		MessageID:          "spoofed",
		Sequence:           3,
		Content:            "must-not-append",
	})
	after, _ := r.GetStreamSnapshot("user", "request")
	if after.LastSequence != 2 || after.Content != "" {
		t.Fatalf("identity conflict mutated snapshot: %+v", after)
	}
}

func TestRegistryNeverStoresNotExposedReasoning(t *testing.T) {
	r := NewRegistry(time.Minute)
	r.StartMessage("user", "session", "request-private", "msg-private")
	snapshot := r.Append("request-private", &adapter.ReplyChunk{
		AssistantMessageID: "msg-private",
		BackendMessageID:   "msg-private",
		MessageID:          "msg-private",
		Sequence:           1,
		Reasoning:          "private chain of thought",
		ReasoningDisclosure: adapter.ReasoningDisclosure{
			Visibility: adapter.ReasoningNotExposed,
		},
	})
	if snapshot == nil || snapshot.Reasoning != "" {
		t.Fatalf("not_exposed snapshot leaked reasoning: %+v", snapshot)
	}

	r.StartMessage("user", "session", "request-public", "msg-public")
	public := r.Append("request-public", &adapter.ReplyChunk{
		AssistantMessageID: "msg-public",
		BackendMessageID:   "msg-public",
		MessageID:          "msg-public",
		Sequence:           1,
		Reasoning:          "public provider summary",
		ReasoningDisclosure: adapter.ReasoningDisclosure{
			Visibility: adapter.ReasoningVisible,
			Source:     "ollama",
			Dialect:    "message.thinking",
			Provider:   "ollama",
			Model:      "trusted-model",
		},
	})
	if public.Reasoning != "public provider summary" {
		t.Fatalf("visible snapshot lost public reasoning: %+v", public)
	}
}
