package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexagon/testing/mock"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestBUG20260802017ProcessStreamPersistsCanonicalRuntimeSnapshot(t *testing.T) {
	provider := mock.NewLLMProvider("test").AddResponse("runtime snapshot answer")
	eng := newEngineWithProvider(t, provider)
	msg := &adapter.Message{
		ID:       "req-bug017-regression",
		Platform: adapter.PlatformWeb,
		UserID:   "bug017-user",
		Content:  "reply with one short marker",
		Metadata: map[string]string{"request_id": "req-bug017-regression"},
	}

	stream, err := eng.ProcessStream(context.Background(), msg)
	if err != nil {
		t.Fatalf("ProcessStream: %v", err)
	}
	var terminal *adapter.ReplyChunk
	for chunk := range stream {
		if chunk.Done {
			copy := *chunk
			terminal = &copy
		}
	}
	if terminal == nil {
		t.Fatal("missing terminal chunk")
	}
	if terminal.AssistantMessageID == "" || terminal.Sequence == 0 {
		t.Fatalf("terminal identity/sequence missing: id=%q sequence=%d", terminal.AssistantMessageID, terminal.Sequence)
	}

	record, err := eng.store.GetMessage(context.Background(), terminal.AssistantMessageID)
	if err != nil {
		t.Fatalf("canonical assistant message %q was not persisted: %v", terminal.AssistantMessageID, err)
	}
	if record.SessionID != msg.SessionID || record.Content == "" {
		t.Fatalf("persisted assistant record drifted: session=%q content=%q", record.SessionID, record.Content)
	}
	records, err := eng.store.ListMessages(context.Background(), msg.SessionID, 10, 0)
	if err != nil {
		t.Fatalf("list persisted messages: %v", err)
	}
	assistantCount := 0
	for _, item := range records {
		if item.Role == "assistant" {
			assistantCount++
			if item.ID != terminal.AssistantMessageID {
				t.Fatalf("second assistant identity persisted: canonical=%q stored=%q", terminal.AssistantMessageID, item.ID)
			}
		}
	}
	if assistantCount != 1 {
		t.Fatalf("assistant record count=%d, want exactly one", assistantCount)
	}
	var persisted struct {
		AssistantMessageID string                          `json:"assistant_message_id"`
		BackendMessageID   string                          `json:"backend_message_id"`
		MessageID          string                          `json:"message_id"`
		RuntimeEvents      []adapter.SequencedRuntimeEvent `json:"runtime_events"`
		LastSequence       uint64                          `json:"last_sequence"`
	}
	if err := json.Unmarshal([]byte(record.Metadata), &persisted); err != nil {
		t.Fatalf("decode assistant metadata: %v", err)
	}
	if persisted.AssistantMessageID != terminal.AssistantMessageID ||
		persisted.BackendMessageID != terminal.AssistantMessageID ||
		persisted.MessageID != terminal.AssistantMessageID {
		t.Fatalf("persisted aliases drifted: %+v", persisted)
	}
	if persisted.LastSequence != terminal.Sequence {
		t.Fatalf("persisted last_sequence=%d, terminal=%d", persisted.LastSequence, terminal.Sequence)
	}
	if len(persisted.RuntimeEvents) != 1 ||
		persisted.RuntimeEvents[0].Sequence != terminal.Sequence ||
		persisted.RuntimeEvents[0].Event.Kind != adapter.RuntimeEventTerminal {
		t.Fatalf("persisted terminal runtime event drifted: %+v", persisted.RuntimeEvents)
	}
}
