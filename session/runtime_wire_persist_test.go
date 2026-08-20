package session

import (
	"context"
	"encoding/json"
	"strings"
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

func TestPersistAssistantRuntimeSnapshotUpdatesExistingMessageInPlace(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()
	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{
		Platform: adapter.PlatformWeb,
		UserID:   "runtime-wire-update-user",
		Content:  "seed",
	})
	if err != nil {
		t.Fatal(err)
	}
	const messageID = "msg-runtime-update"
	if _, saveErr := mgr.SaveAssistantReply(ctx, sess.ID, "answer", AssistantMeta{
		MessageID: messageID,
		Provider:  "hexclaw-gpt",
		Model:     "gpt-5.6-sol",
	}); saveErr != nil {
		t.Fatal(saveErr)
	}
	snapshot := adapter.RuntimeSnapshot{
		AssistantMessageID:  messageID,
		BackendMessageID:    messageID,
		MessageID:           messageID,
		ReasoningDisclosure: adapter.ReasoningDisclosure{Visibility: adapter.ReasoningNotExposed},
		RuntimeEvents: []adapter.SequencedRuntimeEvent{{
			Sequence: 2,
			Event: adapter.RuntimeEvent{
				Version:        1,
				EventID:        "terminal:completed",
				Kind:           adapter.RuntimeEventTerminal,
				TerminalStatus: adapter.RuntimeTerminalCompleted,
			},
		}},
		LastSequence: 2,
	}
	if persistErr := mgr.PersistAssistantRuntimeSnapshot(ctx, messageID, snapshot); persistErr != nil {
		t.Fatalf("persist runtime snapshot: %v", persistErr)
	}

	record, err := store.GetMessage(ctx, messageID)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(record.Metadata), &meta); err != nil {
		t.Fatal(err)
	}
	if meta["provider"] != "hexclaw-gpt" || meta["model"] != "gpt-5.6-sol" {
		t.Fatalf("existing metadata was not preserved: %s", record.Metadata)
	}
	if got, _ := meta["last_sequence"].(float64); got != 2 {
		t.Fatalf("last_sequence=%v, want 2", meta["last_sequence"])
	}
	if count, err := store.CountMessages(ctx, sess.ID); err != nil || count != 1 {
		t.Fatalf("message count=%d err=%v, want one in-place record", count, err)
	}
}

func TestPersistAssistantRuntimeSnapshotPreservesReasoningReceiptForReload(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()
	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{
		Platform: adapter.PlatformWeb,
		UserID:   "runtime-wire-receipt-user",
		Content:  "seed",
	})
	if err != nil {
		t.Fatal(err)
	}
	const messageID = "msg-runtime-receipt"
	if _, saveErr := mgr.SaveAssistantReply(ctx, sess.ID, "answer", AssistantMeta{
		MessageID: messageID,
		Provider:  "hexclaw-gpt",
		Model:     "gpt-5.6-luna",
	}); saveErr != nil {
		t.Fatalf("save assistant reply: %v", saveErr)
	}
	want := adapter.ReasoningReceipt{
		Version:            adapter.ReasoningReceiptVersion,
		ReasoningRequest:   adapter.ReasoningRequestOn,
		ReasoningSupport:   adapter.ReasoningSupportSupported,
		ReasoningExecution: adapter.ReasoningExecutionApplied,
	}
	if persistErr := mgr.PersistAssistantRuntimeSnapshot(ctx, messageID, adapter.RuntimeSnapshot{
		AssistantMessageID:  messageID,
		BackendMessageID:    messageID,
		MessageID:           messageID,
		ReasoningReceipt:    want,
		ReasoningDisclosure: adapter.ReasoningDisclosure{Visibility: adapter.ReasoningNotExposed},
	}); persistErr != nil {
		t.Fatalf("persist runtime snapshot: %v", persistErr)
	}

	record, err := store.GetMessage(ctx, messageID)
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		ReasoningReceipt adapter.ReasoningReceipt `json:"reasoning_receipt"`
	}
	if err := json.Unmarshal([]byte(record.Metadata), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.ReasoningReceipt != want {
		t.Fatalf("reloaded reasoning receipt = %+v, want %+v", meta.ReasoningReceipt, want)
	}
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

func TestPersistAssistantRuntimeSnapshotAccumulatesOnlyFrozenVisibleReasoning(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()
	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{
		Platform: adapter.PlatformWeb,
		UserID:   "runtime-public-reasoning-user",
		Content:  "seed",
	})
	if err != nil {
		t.Fatal(err)
	}
	const messageID = "msg-runtime-public-reasoning"
	const provider = "ollama-instance-a"
	const model = "qwen3.5:9b"
	if _, err := mgr.SaveAssistantReply(ctx, sess.ID, "answer", AssistantMeta{
		MessageID: messageID,
		Provider:  provider,
		Model:     model,
	}); err != nil {
		t.Fatal(err)
	}

	wire := adapter.NewRuntimeWire(messageID, adapter.ReasoningDisclosure{
		Visibility: adapter.ReasoningNotExposed,
		Provider:   provider,
		Model:      model,
	})
	for _, reasoning := range []string{"**公开", "摘要**"} {
		chunk := wire.Decorate(&adapter.ReplyChunk{
			Reasoning: reasoning,
			ReasoningDisclosure: adapter.ReasoningDisclosure{
				Visibility: adapter.ReasoningVisible,
				Source:     "ollama.message.thinking",
				Dialect:    "ollama_chat_think",
				Provider:   provider,
				Model:      model,
			},
		})
		if chunk.Reasoning != reasoning {
			t.Fatalf("trusted reasoning chunk = %q, want %q", chunk.Reasoning, reasoning)
		}
	}
	if err := mgr.PersistAssistantRuntimeSnapshot(ctx, messageID, wire.Snapshot()); err != nil {
		t.Fatalf("persist trusted runtime snapshot: %v", err)
	}
	record, err := store.GetMessage(ctx, messageID)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(record.Metadata), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["reasoning"] != "**公开摘要**" {
		t.Fatalf("reloaded public reasoning = %#v, want accumulated public summary", metadata["reasoning"])
	}

	private := wire.Decorate(&adapter.ReplyChunk{
		Reasoning: "PRIVATE_CHAIN_OF_THOUGHT_MUST_NOT_PERSIST",
		ReasoningDisclosure: adapter.ReasoningDisclosure{
			Visibility: adapter.ReasoningVisible,
			Source:     "ollama.message.thinking",
			Dialect:    "ollama_chat_think",
			Provider:   "ollama-instance-b",
			Model:      model,
		},
	})
	if private.Reasoning != "" {
		t.Fatalf("mismatched reasoning leaked: %q", private.Reasoning)
	}
	if snapshot := wire.Snapshot(); snapshot.Reasoning != "" ||
		snapshot.ReasoningDisclosure.Visibility != adapter.ReasoningNotExposed {
		t.Fatalf("mismatch did not clear runtime reasoning: %+v", snapshot)
	}

	if err := mgr.PersistAssistantRuntimeSnapshot(ctx, messageID, wire.Snapshot()); err != nil {
		t.Fatalf("persist runtime snapshot: %v", err)
	}
	record, err = store.GetMessage(ctx, messageID)
	if err != nil {
		t.Fatal(err)
	}
	metadata = map[string]any{}
	if err := json.Unmarshal([]byte(record.Metadata), &metadata); err != nil {
		t.Fatal(err)
	}
	if _, persisted := metadata["reasoning"]; persisted {
		t.Fatalf("mismatched reasoning left a persisted public summary: %s", record.Metadata)
	}
	if got := string(record.Metadata); strings.Contains(got, "PRIVATE_CHAIN_OF_THOUGHT_MUST_NOT_PERSIST") {
		t.Fatalf("private reasoning persisted: %s", record.Metadata)
	}
}
