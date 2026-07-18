package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260717_EmbeddingAutoConfigNeverGuessesCloudChatCapability(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = ""
	cfg.Knowledge.Embedding.Model = ""
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai-compatible-chat-only": {
			APIKey:  "sk-test",
			BaseURL: "https://chat.example.test/v1",
			Model:   "gpt-compatible",
		},
	}

	plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
	if plan.Provider != "" || plan.Model != "" {
		t.Fatalf("auto config guessed unsupported cloud embedding: %#v", plan)
	}
}

func TestBug20260717_EmbeddingAutoConfigKeepsUnavailableOllamaAsStandby(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = ""
	cfg.Knowledge.Embedding.Model = ""
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"Ollama (本地)": {
			BaseURL: "http://127.0.0.1:1/v1",
			Model:   "qwen3:8b",
		},
	}

	plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
	if plan.Provider != "Ollama (本地)" || plan.Model != "nomic-embed-text" {
		t.Fatalf("standby plan = %#v", plan)
	}
	if plan.Ready || plan.ServiceAvailable {
		t.Fatalf("unreachable Ollama must not be reported ready: %#v", plan)
	}
}

func TestBug20260717_EmbeddingAutoConfigPrefersInstalledModelDeterministically(t *testing.T) {
	installed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"bge-m3:latest"}]}`))
	}))
	defer installed.Close()

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer empty.Close()

	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = ""
	cfg.Knowledge.Embedding.Model = ""
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"a-ollama-empty":     {BaseURL: empty.URL + "/v1"},
		"z-ollama-installed": {BaseURL: installed.URL + "/v1"},
	}

	plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
	if plan.Provider != "z-ollama-installed" || plan.Model != "bge-m3:latest" || !plan.Ready {
		t.Fatalf("installed model plan = %#v", plan)
	}
}

func TestBug20260717_ExplicitCloudEmbeddingRequiresAnExplicitModel(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "cloud"
	cfg.Knowledge.Embedding.Model = ""
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud": {APIKey: "sk-test", BaseURL: "https://embedding.example.test/v1"},
	}

	plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
	if plan.Configured {
		t.Fatalf("model-less cloud embedding must remain disabled: %#v", plan)
	}
}
