package llmrouter

// BUG-20260616: Fallback classified local-vs-remote via isLocalProvider(
// r.cfg.Providers[name]). For providers injected through NewWithProviders
// without a matching cfg entry, that lookup returned a zero-value config (empty
// BaseURL) and the provider was misjudged as remote — so an injected local
// Ollama could win fallback over a real remote provider, the exact regression
// BUG-20260612 was meant to prevent. Local classification must also honor the
// provider name (consistent with pickFallbackDefault).

import (
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260616_FallbackInjectedOllamaRanksLast(t *testing.T) {
	r := NewWithProviders(config.LLMConfig{}, map[string]hexagon.Provider{
		"ollama": nil, // injected without a matching cfg.Providers entry
		"zhipu":  nil,
	})

	_, name, err := r.Fallback()
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if name != "zhipu" {
		t.Fatalf("injected ollama must rank last; expected remote 'zhipu', got %q", name)
	}
}
