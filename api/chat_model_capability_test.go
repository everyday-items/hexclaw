package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

func TestHandleChatRejectsNonTextModelBeforeEngine(t *testing.T) {
	const vectorModel = "custom-vector-model"
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			Model: "chat", Models: []string{"chat", vectorModel},
			ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{
				{ID: "chat", Capabilities: []string{config.LLMModelCapabilityText}},
				{ID: vectorModel, Capabilities: []string{config.LLMModelCapabilityEmbedding}},
			},
		},
	}

	for _, tt := range []struct {
		name string
		body string
	}{
		{
			name: "top-level provider and model",
			body: `{"message":"hello","provider":"custom","model":"custom-vector-model"}`,
		},
		{
			name: "legacy metadata provider and model",
			body: `{"message":"hello","metadata":{"provider":"custom","model":"custom-vector-model"}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			engine := &mockEngine{reply: &adapter.Reply{Content: "unexpected"}}
			srv := NewServer(cfg, engine, nil, nil)
			w := httptest.NewRecorder()

			srv.handleChat(w, httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(tt.body)))

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want non-text rejection", w.Code, w.Body.String())
			}
			if engine.calls != 0 {
				t.Fatalf("non-text model reached engine %d times", engine.calls)
			}
		})
	}
}

func TestHandleChatModelValidationIsProviderScoped(t *testing.T) {
	const sharedModel = "shared-model"
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"vector-provider": {
			Models: []string{sharedModel}, ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID: sharedModel, Capabilities: []string{config.LLMModelCapabilityEmbedding},
			}},
		},
		"chat-provider": {
			Model: sharedModel, Models: []string{sharedModel}, ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID: sharedModel, Capabilities: []string{config.LLMModelCapabilityText},
			}},
		},
	}
	engine := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, engine, nil, nil)
	w := httptest.NewRecorder()

	srv.handleChat(w, httptest.NewRequest(
		http.MethodPost,
		"/api/v1/chat",
		strings.NewReader(`{"message":"hello","provider":"chat-provider","model":"shared-model"}`),
	))

	if w.Code != http.StatusOK || engine.calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s, provider-scoped text model should pass", w.Code, engine.calls, w.Body.String())
	}
}
