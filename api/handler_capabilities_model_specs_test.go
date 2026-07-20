package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
)

type capabilityProbeCountingProvider struct {
	calls atomic.Int32
}

func (*capabilityProbeCountingProvider) Name() string { return "custom" }

func (p *capabilityProbeCountingProvider) Complete(
	context.Context,
	llm.CompletionRequest,
) (*llm.CompletionResponse, error) {
	p.calls.Add(1)
	return &llm.CompletionResponse{}, nil
}

func (*capabilityProbeCountingProvider) Stream(
	context.Context,
	llm.CompletionRequest,
) (*llm.Stream, error) {
	return nil, fmt.Errorf("not implemented")
}

func (*capabilityProbeCountingProvider) Models() []llm.ModelInfo { return nil }

func (*capabilityProbeCountingProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

var _ hexagon.Provider = (*capabilityProbeCountingProvider)(nil)

func TestHandleProbeCapabilityRejectsEmbeddingOnlyBeforeCompletion(t *testing.T) {
	const modelID = "custom-vector-model"
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			Model:          "",
			Models:         []string{modelID},
			ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID: modelID, Capabilities: []string{config.LLMModelCapabilityEmbedding},
			}},
		},
	}
	provider := &capabilityProbeCountingProvider{}
	selector := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"custom": provider})
	srv := NewServer(cfg, &mockEngine{}, nil, nil)
	srv.SetCapabilityService(llmrouter.NewCapabilityService(selector, nil))
	w := httptest.NewRecorder()

	srv.handleProbeCapability(w, httptest.NewRequest(
		http.MethodPost,
		"/api/v1/llm/capabilities/probe?provider=custom&model="+modelID,
		nil,
	))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want embedding-only rejection", w.Code, w.Body.String())
	}
	if got := provider.calls.Load(); got != 0 {
		t.Fatalf("embedding-only model reached tool/completion probe %d times", got)
	}
}
