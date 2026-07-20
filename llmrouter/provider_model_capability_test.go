package llmrouter

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
)

type capabilityGuardCountingProvider struct {
	completeCalls atomic.Int32
	streamCalls   atomic.Int32
}

func (*capabilityGuardCountingProvider) Name() string { return "mixed" }

func (p *capabilityGuardCountingProvider) Complete(
	context.Context,
	llm.CompletionRequest,
) (*llm.CompletionResponse, error) {
	p.completeCalls.Add(1)
	return &llm.CompletionResponse{}, nil
}

func (p *capabilityGuardCountingProvider) Stream(
	context.Context,
	llm.CompletionRequest,
) (*llm.Stream, error) {
	p.streamCalls.Add(1)
	return nil, fmt.Errorf("test stream")
}

func (*capabilityGuardCountingProvider) Models() []llm.ModelInfo { return nil }

func (*capabilityGuardCountingProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

var _ hexagon.Provider = (*capabilityGuardCountingProvider)(nil)

func TestSelectorProviderRejectsNonTextModelsBeforeCompletionTransport(t *testing.T) {
	const (
		chatModel   = "chat-model"
		vectorModel = "vector-model"
	)
	cfg := config.LLMConfig{
		Default: "mixed",
		Providers: map[string]config.LLMProviderConfig{
			"mixed": {
				Model: chatModel, Models: []string{chatModel, vectorModel},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{ID: chatModel, Capabilities: []string{config.LLMModelCapabilityText}},
					{ID: vectorModel, Capabilities: []string{config.LLMModelCapabilityEmbedding}},
				},
			},
		},
	}
	inner := &capabilityGuardCountingProvider{}
	selector := NewWithProviders(cfg, map[string]hexagon.Provider{"mixed": inner})
	provider, ok := selector.Get("mixed")
	if !ok {
		t.Fatal("configured provider missing")
	}

	if _, err := provider.Complete(context.Background(), llm.CompletionRequest{Model: vectorModel}); err == nil {
		t.Fatal("embedding-only model reached completion guard without error")
	}
	if _, err := provider.Stream(context.Background(), llm.CompletionRequest{Model: vectorModel}); err == nil {
		t.Fatal("embedding-only model reached stream guard without error")
	}
	if _, err := provider.Complete(context.Background(), llm.CompletionRequest{Model: "unknown-model"}); err == nil {
		t.Fatal("unclassified model reached completion guard without error")
	}
	if got := inner.completeCalls.Load(); got != 0 {
		t.Fatalf("non-text completions reached transport %d times", got)
	}
	if got := inner.streamCalls.Load(); got != 0 {
		t.Fatalf("non-text streams reached transport %d times", got)
	}

	if _, err := provider.Complete(context.Background(), llm.CompletionRequest{Model: chatModel}); err != nil {
		t.Fatalf("text model rejected: %v", err)
	}
	if got := inner.completeCalls.Load(); got != 1 {
		t.Fatalf("text completion calls=%d, want 1", got)
	}
}
