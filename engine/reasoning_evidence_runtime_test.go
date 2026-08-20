package engine

import (
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestReplyChunkRuntimeSinkPublishesReasoningEvidenceObserver(t *testing.T) {
	chunks := make(chan *adapter.ReplyChunk, 1)
	sink := &replyChunkRuntimeSink{
		ch: chunks,
		route: adapter.FrozenReasoningRoute{
			Provider: "exact-provider",
			Model:    "exact-model",
		},
	}
	req := llm.CompletionRequest{
		Metadata: map[string]any{"thinking": "on"},
	}
	sink.bindReasoningEvidenceObserver(&req)

	observer, ok := req.Metadata[llm.ReasoningReceiptObserverMetadataKey].(func(llm.ReasoningReceipt))
	if !ok {
		t.Fatalf("reasoning evidence observer missing: %#v", req.Metadata)
	}
	observer(llm.ReasoningReceipt{
		Version:     1,
		Enabled:     true,
		Support:     llm.ReasoningSupported,
		Dialect:     llm.ReasoningDialectThink,
		Sent:        true,
		Accepted:    true,
		Observed:    true,
		Applied:     true,
		Application: llm.ReasoningApplicationApplied,
	})

	chunk := <-chunks
	if chunk.ReasoningEvidence == nil {
		t.Fatal("observer evidence was not forwarded to ReplyChunk")
	}
	got := *chunk.ReasoningEvidence
	if got.Request != adapter.ReasoningRequestOn ||
		got.Support != adapter.ReasoningSupportSupported ||
		!got.Applied || !got.Observed ||
		got.Provider != "exact-provider" || got.Model != "exact-model" {
		t.Fatalf("forwarded evidence=%+v", got)
	}
}
