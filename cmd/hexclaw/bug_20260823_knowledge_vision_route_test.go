package main

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260823DefaultVisionRouteUsesEffectiveProviderIdentity(t *testing.T) {
	provider := config.LLMProviderConfig{
		APIKey:  "local",
		BaseURL: "http://127.0.0.1:11434",
		Model:   "qwen3.5:9b",
	}

	got := effectiveVisionProviderInstanceID("ollama", provider)
	want := config.EffectiveProviderInstanceID("ollama", provider)
	if got == "" || got != want {
		t.Fatalf("vision provider identity=%q, want effective identity %q", got, want)
	}
}
