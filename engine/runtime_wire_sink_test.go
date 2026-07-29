package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/streamx"
	hruntime "github.com/hexagon-codes/hexagon/runtime"
	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestReplyChunkRuntimeSinkProjectsOnlyRealAllowlistedToolLifecycle(t *testing.T) {
	ch := make(chan *adapter.ReplyChunk, 8)
	sink := &replyChunkRuntimeSink{
		ch: ch,
		allowedToolNames: map[string]struct{}{
			"web_search": {},
		},
	}
	call := llm.ToolCall{ID: "call-1", Name: "web_search", Arguments: `{"secret":"must-not-leak"}`}
	for _, event := range []hruntime.Event{
		{Type: hruntime.EventToolCallStarted, ToolCall: &call},
		{
			Type:       hruntime.EventToolCallFailed,
			ToolCall:   &call,
			ToolResult: &hruntime.ToolResult{Content: "raw result must not leak"},
			Error:      errors.New("raw error must not leak"),
		},
		// Hexagon v0.5.9 emits completed after failed; public lifecycle must not
		// contradict the already terminal failed receipt.
		{Type: hruntime.EventToolCallCompleted, ToolCall: &call},
		// Not in this run's host allowlist: zero projection.
		{Type: hruntime.EventToolCallStarted, ToolCall: &llm.ToolCall{ID: "fake", Name: "search"}},
	} {
		if err := sink.Emit(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}

	first := <-ch
	second := <-ch
	if first.RuntimeEvent == nil || first.RuntimeEvent.Kind != adapter.RuntimeEventToolStarted {
		t.Fatalf("first event = %+v", first.RuntimeEvent)
	}
	if second.RuntimeEvent == nil || second.RuntimeEvent.Kind != adapter.RuntimeEventToolFailed {
		t.Fatalf("second event = %+v", second.RuntimeEvent)
	}
	select {
	case extra := <-ch:
		t.Fatalf("unexpected invented/contradictory event: %+v", extra)
	default:
	}
}

func TestReplyChunkRuntimeSinkUsesTrustedRuntimeSequenceForUniqueEventIDs(t *testing.T) {
	ch := make(chan *adapter.ReplyChunk, 2)
	sink := &replyChunkRuntimeSink{
		ch:               ch,
		allowedToolNames: map[string]struct{}{"web_search": {}},
	}
	call := llm.ToolCall{ID: "provider-reused-call-id", Name: "web_search"}
	for _, sequence := range []int64{4, 12} {
		if err := sink.Emit(context.Background(), hruntime.Event{
			Type:     hruntime.EventToolCallStarted,
			Sequence: sequence,
			Turn:     int(sequence),
			ToolCall: &call,
		}); err != nil {
			t.Fatal(err)
		}
	}
	first := <-ch
	second := <-ch
	if first.RuntimeEvent == nil || second.RuntimeEvent == nil {
		t.Fatalf("missing runtime events: first=%+v second=%+v", first, second)
	}
	if first.RuntimeEvent.EventID == second.RuntimeEvent.EventID {
		t.Fatalf("reused provider call ID collided across turns: %q", first.RuntimeEvent.EventID)
	}
}

func TestReplyChunkRuntimeSinkEmitsReasoningOnlyWithTrustedVisibleDisclosure(t *testing.T) {
	ch := make(chan *adapter.ReplyChunk, 2)
	sink := &replyChunkRuntimeSink{
		ch: ch,
		route: adapter.FrozenReasoningRoute{
			Provider: "ollama",
			Model:    "trusted-model",
		},
	}
	notExposed := &streamx.ReasoningDisclosure{
		Visibility: streamx.ReasoningNotExposed,
		Source:     "ollama",
		Dialect:    "message.thinking",
		Provider:   "ollama",
		Model:      "trusted-model",
	}
	visible := *notExposed
	visible.Visibility = streamx.ReasoningVisible
	for _, disclosure := range []*streamx.ReasoningDisclosure{notExposed, &visible} {
		if err := sink.Emit(context.Background(), hruntime.Event{
			Type: hruntime.EventLLMChunk,
			Chunk: &llm.StreamChunk{
				Reasoning:           "provider reasoning",
				ReasoningDisclosure: disclosure,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	privateChunk := <-ch
	publicChunk := <-ch
	if privateChunk.Reasoning != "" {
		t.Fatalf("sink leaked not_exposed reasoning: %+v", privateChunk)
	}
	if publicChunk.Reasoning != "provider reasoning" ||
		publicChunk.ReasoningDisclosure.Visibility != adapter.ReasoningVisible {
		t.Fatalf("sink lost trusted visible reasoning: %+v", publicChunk)
	}
}
