package llmrouter

// New() returns (nil, err) when no LLM provider is configured (empty
// config.LLMConfig{} / providers:{}). A nil *Selector is a valid "no LLM" state,
// so its read accessors must behave as an empty selector instead of
// dereferencing nil — calling Default() on a nil router must return nil, not panic.

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260620_NilRouterDefaultNoPanic(t *testing.T) {
	// Reproduce the exact production path: empty LLM config -> New returns nil.
	router, err := New(config.LLMConfig{})
	if err == nil {
		t.Fatalf("expected error when no provider configured, got nil")
	}
	if router != nil {
		t.Fatalf("expected nil *Selector when no provider, got %#v", router)
	}

	// Reading Default() on the returned nil router must not panic with a nil
	// pointer dereference in (*Selector).Default.
	if got := router.Default(); got != nil {
		t.Fatalf("nil router Default() must return nil, got %v", got)
	}
}
