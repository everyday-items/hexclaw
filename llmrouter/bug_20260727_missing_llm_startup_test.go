package llmrouter

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260727_ZeroProviderSelectorIsOperationallyEmpty(t *testing.T) {
	assertUnavailable := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, ErrNoProvider) {
			t.Errorf("error = %v, want errors.Is(error, ErrNoProvider)", err)
		}
	}

	selector, newErr := New(config.LLMConfig{})
	if selector == nil {
		t.Error("zero-provider New must return a non-nil empty selector")
	}
	assertUnavailable(t, newErr)
	if newErr == nil {
		t.Error("zero-provider New must report provider unavailability")
	}

	assertNoPanic := func(t *testing.T, name string, call func()) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("%s panicked for zero-provider selector: %v", name, recovered)
				}
			}()
			call()
		})
	}

	assertNoPanic(t, "Default", func() {
		if got := selector.Default(); got != nil {
			t.Errorf("Default() = %v, want nil", got)
		}
	})
	assertNoPanic(t, "DefaultName", func() {
		if got := selector.DefaultName(); got != "" {
			t.Errorf("DefaultName() = %q, want empty", got)
		}
	})
	assertNoPanic(t, "Get", func() {
		if got, ok := selector.Get("missing"); got != nil || ok {
			t.Errorf("Get(missing) = (%v, %v), want (nil, false)", got, ok)
		}
	})
	assertNoPanic(t, "IsLocalProviderName", func() {
		if selector.IsLocalProviderName("missing") {
			t.Error("IsLocalProviderName(missing) = true, want false")
		}
	})
	assertNoPanic(t, "Providers", func() {
		if got := selector.Providers(); len(got) != 0 {
			t.Errorf("Providers() = %v, want empty", got)
		}
	})
	assertNoPanic(t, "ActiveConfig", func() {
		if got := selector.ActiveConfig(); len(got.Providers) != 0 || got.Default != "" {
			t.Errorf("ActiveConfig() = %#v, want empty LLM config", got)
		}
	})
	assertNoPanic(t, "ProviderModel", func() {
		if got := selector.ProviderModel("missing"); got != "" {
			t.Errorf("ProviderModel(missing) = %q, want empty", got)
		}
	})
	assertNoPanic(t, "ProviderConfig", func() {
		if got, ok := selector.ProviderConfig("missing"); ok {
			t.Errorf("ProviderConfig(missing) = (%#v, true), want false", got)
		}
	})
	assertNoPanic(t, "DefaultRouteForCapabilities", func() {
		_, err := selector.DefaultRouteForCapabilities(config.LLMModelCapabilityText)
		assertUnavailable(t, err)
		if err == nil {
			t.Error("DefaultRouteForCapabilities() error = nil, want unavailable")
		}
	})
	assertNoPanic(t, "ResolveRouteForCapabilities", func() {
		_, err := selector.ResolveRouteForCapabilities("", "", config.LLMModelCapabilityText)
		assertUnavailable(t, err)
		if err == nil {
			t.Error("ResolveRouteForCapabilities() error = nil, want unavailable")
		}
	})
	assertNoPanic(t, "Route", func() {
		_, _, err := selector.Route(context.Background())
		assertUnavailable(t, err)
		if err == nil {
			t.Error("Route() error = nil, want unavailable")
		}
	})
	assertNoPanic(t, "RouteModel", func() {
		_, _, err := selector.RouteModel(context.Background())
		assertUnavailable(t, err)
		if err == nil {
			t.Error("RouteModel() error = nil, want unavailable")
		}
	})
	assertNoPanic(t, "Fallback", func() {
		_, _, err := selector.Fallback()
		assertUnavailable(t, err)
		if err == nil {
			t.Error("Fallback() error = nil, want unavailable")
		}
	})
}
