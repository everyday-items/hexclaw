package llmrouter

// BUG-20260612: Fallback picked the alphabetically-first provider, so a local
// Ollama provider (which structurally cannot serve full engine prompts before
// ResponseHeaderTimeout) won over healthy remote providers whenever its name
// sorted first. Observed live: a cron agent run degraded to localhost:11434
// and died with "timeout awaiting response headers". Remote providers must be
// preferred; local ones are last-resort only.

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260612_FallbackPrefersRemoteOverLocal(t *testing.T) {
	cfg := config.LLMConfig{
		Default: "primary",
		Providers: map[string]config.LLMProviderConfig{
			"primary": {APIKey: "sk-1", Model: "model-1"},
			// Alphabetically first on purpose: old code would pick it.
			"aaa-ollama": {BaseURL: "http://localhost:11434/v1", Model: "qwen"},
			"zhipu":      {APIKey: "sk-2", Model: "glm-4-flash"},
		},
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("router init: %v", err)
	}

	_, name, err := r.Fallback("primary")
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if name != "zhipu" {
		t.Errorf("fallback must prefer remote provider zhipu, got %q (local provider must be last resort)", name)
	}
}

func TestBug20260612_FallbackUsesLocalWhenOnlyOption(t *testing.T) {
	cfg := config.LLMConfig{
		Default: "primary",
		Providers: map[string]config.LLMProviderConfig{
			"primary":    {APIKey: "sk-1", Model: "model-1"},
			"aaa-ollama": {BaseURL: "http://127.0.0.1:11434/v1", Model: "qwen"},
		},
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("router init: %v", err)
	}

	_, name, err := r.Fallback("primary")
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if name != "aaa-ollama" {
		t.Errorf("local provider should still serve as last resort, got %q", name)
	}
}
