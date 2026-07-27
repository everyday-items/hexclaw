package llmrouter

// New() returns a non-nil empty selector plus ErrNoProvider when no LLM provider
// is configured. The selector remains safe for read access while LLM work is
// unavailable.

import (
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260620_NilRouterDefaultNoPanic(t *testing.T) {
	// Reproduce the exact production path: empty LLM config -> operationally empty selector.
	router, err := New(config.LLMConfig{})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("error = %v, want errors.Is(error, ErrNoProvider)", err)
	}
	if router == nil {
		t.Fatal("expected non-nil empty selector when no provider")
	}

	// Reading Default() on the empty selector must not panic.
	if got := router.Default(); got != nil {
		t.Fatalf("empty selector Default() must return nil, got %v", got)
	}
}
