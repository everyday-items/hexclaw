package main

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260728QwenDefaultEmbeddingPlan(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = ""
	cfg.Knowledge.Embedding.Model = ""
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"HexClaw-GPT": {
			APIKey:  "test-key",
			BaseURL: "http://127.0.0.1:18080/v1",
			Model:   "gpt-5.6-sol",
		},
		"Ollama (本地)": {
			BaseURL: "http://127.0.0.1:1/v1",
			Model:   "qwen3.5:9b",
		},
	}

	plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
	if plan.Provider != "Ollama (本地)" {
		t.Fatalf("default provider = %q, want Ollama (本地)", plan.Provider)
	}
	if plan.Model != "qwen3-embedding:8b" {
		t.Fatalf("default embedding model = %q, want qwen3-embedding:8b", plan.Model)
	}
	if cfg.LLM.Providers["HexClaw-GPT"].Model != "gpt-5.6-sol" {
		t.Fatalf("resolving embedding changed chat model: %#v", cfg.LLM.Providers["HexClaw-GPT"])
	}
}

func TestBug20260728QwenDefaultEmbeddingDimension(t *testing.T) {
	for _, model := range []string{"qwen3-embedding:8b", "qwen3-embedding:latest"} {
		if got := knowledgeEmbeddingDimension(model); got != 4096 {
			t.Fatalf("knowledgeEmbeddingDimension(%q) = %d, want 4096", model, got)
		}
	}
}
