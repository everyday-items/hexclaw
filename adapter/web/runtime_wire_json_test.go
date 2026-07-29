package web

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/streamstate"
)

func TestWSMessageRuntimeWireFieldsAreAdditive(t *testing.T) {
	event := &adapter.RuntimeEvent{
		Version:        1,
		EventID:        "terminal:completed",
		Kind:           adapter.RuntimeEventTerminal,
		TerminalStatus: adapter.RuntimeTerminalCompleted,
	}
	raw, err := json.Marshal(wsMessage{
		Type:               "chunk",
		Content:            "ok",
		AssistantMessageID: "msg-assistant-007",
		BackendMessageID:   "msg-assistant-007",
		MessageID:          "msg-assistant-007",
		Sequence:           4,
		ReasoningDisclosure: adapter.ReasoningDisclosure{
			Visibility: adapter.ReasoningNotExposed,
		},
		RuntimeEvent: event,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"type", "content", "assistant_message_id", "backend_message_id",
		"message_id", "sequence", "reasoning_disclosure", "runtime_event",
	} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing additive WS field %q in %s", key, raw)
		}
	}
}

func TestWSResumeSnapshotCarriesRuntimeEventsAndLastSequence(t *testing.T) {
	event := adapter.SequencedRuntimeEvent{
		Sequence: 2,
		Event: adapter.RuntimeEvent{
			Version:    1,
			EventID:    "tool:call-1:tool_completed:9",
			Kind:       adapter.RuntimeEventToolCompleted,
			ToolCallID: "call-1",
			ToolName:   "web_search",
		},
	}
	msg := snapshotToMessage(&streamstate.Snapshot{
		RequestID:          "req-1",
		AssistantMessageID: "msg-1",
		BackendMessageID:   "msg-1",
		MessageID:          "msg-1",
		RuntimeEvents:      []adapter.SequencedRuntimeEvent{event},
		LastSequence:       3,
	})
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		RuntimeEvents []adapter.SequencedRuntimeEvent `json:"runtime_events"`
		LastSequence  uint64                          `json:"last_sequence"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.LastSequence != 3 || len(got.RuntimeEvents) != 1 ||
		got.RuntimeEvents[0].Event.EventID != event.Event.EventID {
		t.Fatalf("resume runtime snapshot = %+v, want events + last_sequence", got)
	}
}

func TestWebErrorTerminalRuntimeEventIsRetainedBeforeFail(t *testing.T) {
	a := New()
	a.streams.StartMessage("user", "session", "request", "msg-1")
	wire := adapter.NewRuntimeWire(
		"msg-1",
		adapter.ReasoningDisclosure{Visibility: adapter.ReasoningNotExposed},
	)
	chunks := make(chan *adapter.ReplyChunk, 1)
	chunks <- wire.Decorate(&adapter.ReplyChunk{
		Done:  true,
		Error: errors.New("private upstream failure"),
	})
	close(chunks)

	if err := a.sendStreamWithIDs(context.Background(), "missing-chat", "session", "request", chunks); err == nil {
		t.Fatal("expected stream error")
	}
	snapshot, ok := a.streams.GetStreamSnapshot("user", "request")
	if !ok {
		t.Fatal("failed stream snapshot missing")
	}
	if len(snapshot.RuntimeEvents) != 1 ||
		snapshot.RuntimeEvents[0].Event.Kind != adapter.RuntimeEventTerminal ||
		snapshot.RuntimeEvents[0].Event.TerminalStatus != adapter.RuntimeTerminalFailed ||
		snapshot.LastSequence != 1 {
		t.Fatalf("failed terminal snapshot = %+v", snapshot)
	}
	if snapshot.UpdatedAt.IsZero() || time.Since(snapshot.UpdatedAt) > time.Minute {
		t.Fatalf("failed snapshot timestamp invalid: %v", snapshot.UpdatedAt)
	}
}

func TestWSJSONNeverCarriesNotExposedReasoning(t *testing.T) {
	wire := adapter.NewRuntimeWire(
		"msg-private",
		adapter.ReasoningDisclosure{Visibility: adapter.ReasoningNotExposed},
	)
	chunk := wire.Decorate(&adapter.ReplyChunk{
		Reasoning: "private chain of thought",
		ReasoningDisclosure: adapter.ReasoningDisclosure{
			Visibility: adapter.ReasoningNotExposed,
		},
	})
	raw, err := json.Marshal(wsMessage{
		Type:                "chunk",
		Reasoning:           chunk.Reasoning,
		ReasoningDisclosure: chunk.ReasoningDisclosure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private chain of thought") {
		t.Fatalf("WS payload leaked not_exposed reasoning: %s", raw)
	}
}
