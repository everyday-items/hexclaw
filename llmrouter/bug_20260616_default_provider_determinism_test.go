package llmrouter

// BUG-20260616: picking the fallback default provider iterated the providers map
// directly. Go randomizes map iteration order, so when the configured default was
// missing the chosen provider varied across calls/restarts. pickFallbackDefault
// must deterministically prefer the smallest remote name (local providers last),
// and NewWithProviders must reuse that deterministic selection.

import (
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260616_PickFallbackDefaultDeterministic(t *testing.T) {
	providers := map[string]hexagon.Provider{
		"openai": nil,
		"gemini": nil,
		"zhipu":  nil,
	}
	seen := map[string]int{}
	for i := 0; i < 256; i++ {
		seen[pickFallbackDefault(providers)]++
	}
	if len(seen) != 1 {
		t.Fatalf("pickFallbackDefault non-deterministic across 256 calls: %v", seen)
	}
	if _, ok := seen["gemini"]; !ok {
		t.Fatalf("must pick smallest remote name 'gemini', got %v", seen)
	}
}

func TestBug20260616_PickFallbackPrefersRemote(t *testing.T) {
	// Local provider sorts first alphabetically but must still rank last.
	providers := map[string]hexagon.Provider{
		"aaa-ollama": nil,
		"zhipu":      nil,
	}
	if got := pickFallbackDefault(providers); got != "zhipu" {
		t.Fatalf("remote must beat local: want zhipu, got %q", got)
	}
}

func TestBug20260616_NewWithProvidersDefaultDeterministic(t *testing.T) {
	cfg := config.LLMConfig{Default: "missing"} // not in providers -> forces auto-pick
	providers := map[string]hexagon.Provider{
		"openai": nil,
		"gemini": nil,
		"zhipu":  nil,
	}
	seen := map[string]int{}
	for i := 0; i < 256; i++ {
		r := NewWithProviders(cfg, providers)
		seen[r.DefaultName()]++
	}
	if len(seen) != 1 {
		t.Fatalf("NewWithProviders default non-deterministic across 256 builds: %v", seen)
	}
	if _, ok := seen["gemini"]; !ok {
		t.Fatalf("default must be smallest remote name 'gemini', got %v", seen)
	}
}
