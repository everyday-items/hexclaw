package router

import (
	"context"
	"errors"
	"testing"
)

func TestUnregisterPersistedFailureKeepsWholeSnapshot(t *testing.T) {
	d := New()
	for _, name := range []string{"beta", "alpha"} {
		if err := d.Register(AgentConfig{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.AddRule(Rule{ID: 7, Platform: "web", AgentName: "beta"}); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("disk full")
	err := d.UnregisterPersisted("beta", func(name, nextDefault string, wasDefault bool) error {
		if name != "beta" || nextDefault != "alpha" || !wasDefault {
			t.Fatalf("callback snapshot=(%q,%q,%v), want (beta,alpha,true)", name, nextDefault, wasDefault)
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v, want %v", err, wantErr)
	}
	if _, ok := d.GetAgent("beta"); !ok {
		t.Fatal("failed persistence published agent deletion")
	}
	if got := d.DefaultAgent(); got != "beta" {
		t.Fatalf("failed persistence published default=%q, want beta", got)
	}
	if rules := d.ListRules(); len(rules) != 1 || rules[0].AgentName != "beta" {
		t.Fatalf("failed persistence published rule deletion: %+v", rules)
	}
}

func TestSetDefaultPersistedFailureKeepsPreviousDefault(t *testing.T) {
	d := New()
	for _, name := range []string{"alpha", "beta"} {
		if err := d.Register(AgentConfig{Name: name}); err != nil {
			t.Fatal(err)
		}
	}

	wantErr := errors.New("disk full")
	err := d.SetDefaultPersisted("beta", func(string) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v, want %v", err, wantErr)
	}
	if got := d.DefaultAgent(); got != "alpha" {
		t.Fatalf("failed persistence published default=%q, want alpha", got)
	}
}

func TestRemoveRulePersistedFailureKeepsRule(t *testing.T) {
	d := New()
	if err := d.Register(AgentConfig{Name: "assistant"}); err != nil {
		t.Fatal(err)
	}
	if err := d.AddRule(Rule{ID: 7, Platform: "web", AgentName: "assistant"}); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("disk full")
	err := d.RemoveRulePersisted(7, func(int) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v, want %v", err, wantErr)
	}
	if rules := d.ListRules(); len(rules) != 1 || rules[0].ID != 7 {
		t.Fatalf("failed persistence published rule deletion: %+v", rules)
	}
}

func TestRouteWithFallbackReturnsDetachedAgentSnapshot(t *testing.T) {
	d := New()
	for _, cfg := range []AgentConfig{
		{Name: "alpha"},
		{Name: "beta", Metadata: map[string]string{"scope": "original"}},
	} {
		if err := d.Register(cfg); err != nil {
			t.Fatal(err)
		}
	}
	d.SetClassifier(NewLLMClassifier(func(context.Context, string, string) (string, error) {
		return `{"agent":"beta","confidence":1}`, nil
	}))

	result, source := d.RouteWithFallback(context.Background(), RouteRequest{}, "route me")
	if source != RouteSourceLLM || result == nil || result.AgentConfig == nil {
		t.Fatalf("route result=%+v source=%q", result, source)
	}
	result.AgentConfig.Metadata["scope"] = "mutated"
	again, _ := d.GetAgent("beta")
	if again.Metadata["scope"] != "original" {
		t.Fatalf("RouteWithFallback leaked mutable dispatcher state: %+v", again)
	}
}
