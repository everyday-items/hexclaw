package router

import (
	"context"
	"errors"
	"testing"
)

// mustRegister registers an agent or fails the test.
func mustRegister(t *testing.T, d *Dispatcher, name string) {
	t.Helper()
	if err := d.Register(AgentConfig{Name: name}); err != nil {
		t.Fatalf("Register %q: %v", name, err)
	}
}

// TestDispatcher_FirstRegisterBecomesDefault verifies the first registered agent
// is auto-promoted to default, and later registrations do not change it.
func TestDispatcher_FirstRegisterBecomesDefault(t *testing.T) {
	d := New()
	mustRegister(t, d, "first")
	if d.DefaultAgent() != "first" {
		t.Fatalf("first agent should become default, got %q", d.DefaultAgent())
	}
	mustRegister(t, d, "second")
	if d.DefaultAgent() != "first" {
		t.Fatalf("default should remain 'first', got %q", d.DefaultAgent())
	}
}

// TestDispatcher_UpdateAgent covers the success, missing-name, and empty-name paths.
func TestDispatcher_UpdateAgent(t *testing.T) {
	d := New()
	mustRegister(t, d, "a1")

	t.Run("update_existing", func(t *testing.T) {
		err := d.UpdateAgent(AgentConfig{Name: "a1", DisplayName: "Renamed", Model: "m2"})
		if err != nil {
			t.Fatalf("UpdateAgent: %v", err)
		}
		got, ok := d.GetAgent("a1")
		if !ok {
			t.Fatal("agent a1 should still exist")
		}
		if got.DisplayName != "Renamed" || got.Model != "m2" {
			t.Fatalf("update not applied: %+v", got)
		}
	})

	t.Run("update_empty_name_errors", func(t *testing.T) {
		if err := d.UpdateAgent(AgentConfig{Name: ""}); err == nil {
			t.Fatal("UpdateAgent with empty name should error")
		}
	})

	t.Run("update_missing_errors", func(t *testing.T) {
		if err := d.UpdateAgent(AgentConfig{Name: "ghost"}); err == nil {
			t.Fatal("UpdateAgent on missing agent should error")
		}
	})
}

// TestDispatcher_RemoveRules deletes every rule for a given agent.
func TestDispatcher_RemoveRules(t *testing.T) {
	d := New()
	mustRegister(t, d, "a1")
	mustRegister(t, d, "a2")
	for _, r := range []Rule{
		{Platform: "feishu", AgentName: "a1"},
		{Platform: "discord", AgentName: "a1"},
		{Platform: "feishu", AgentName: "a2"},
	} {
		if err := d.AddRule(r); err != nil {
			t.Fatalf("AddRule: %v", err)
		}
	}

	d.RemoveRules("a1")
	rules := d.ListRules()
	if len(rules) != 1 || rules[0].AgentName != "a2" {
		t.Fatalf("expected only a2 rule to remain, got %+v", rules)
	}

	// Removing rules for an agent with none is a no-op.
	d.RemoveRules("nobody")
	if len(d.ListRules()) != 1 {
		t.Fatalf("no-op RemoveRules changed rule count: %d", len(d.ListRules()))
	}
}

// TestDispatcher_RemoveRule covers single-rule deletion by ID and not-found.
func TestDispatcher_RemoveRule(t *testing.T) {
	d := New()
	mustRegister(t, d, "a1")
	if err := d.AddRule(Rule{ID: 7, Platform: "feishu", AgentName: "a1"}); err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	if err := d.AddRule(Rule{ID: 8, Platform: "discord", AgentName: "a1"}); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	t.Run("remove_existing", func(t *testing.T) {
		if err := d.RemoveRule(7); err != nil {
			t.Fatalf("RemoveRule: %v", err)
		}
		rules := d.ListRules()
		if len(rules) != 1 || rules[0].ID != 8 {
			t.Fatalf("expected only rule ID=8 to remain, got %+v", rules)
		}
	})

	t.Run("remove_missing_errors", func(t *testing.T) {
		if err := d.RemoveRule(999); err == nil {
			t.Fatal("RemoveRule on missing ID should error")
		}
	})
}

// TestDispatcher_AddRule_Validation covers the empty-agent and unregistered-agent guards.
func TestDispatcher_AddRule_Validation(t *testing.T) {
	d := New()
	mustRegister(t, d, "a1")

	t.Run("empty_agent_name_errors", func(t *testing.T) {
		if err := d.AddRule(Rule{Platform: "feishu"}); err == nil {
			t.Fatal("AddRule without agent_name should error")
		}
	})
	t.Run("unregistered_agent_errors", func(t *testing.T) {
		if err := d.AddRule(Rule{Platform: "feishu", AgentName: "ghost"}); err == nil {
			t.Fatal("AddRule referencing unregistered agent should error")
		}
	})
}

// TestDispatcher_SetDefault covers clearing (empty string) and missing-agent error.
func TestDispatcher_SetDefault(t *testing.T) {
	d := New()
	mustRegister(t, d, "a1")

	t.Run("clear_default_with_empty", func(t *testing.T) {
		if err := d.SetDefault(""); err != nil {
			t.Fatalf("SetDefault(\"\") should clear without error: %v", err)
		}
		if d.DefaultAgent() != "" {
			t.Fatalf("expected default cleared, got %q", d.DefaultAgent())
		}
	})
	t.Run("set_missing_errors", func(t *testing.T) {
		if err := d.SetDefault("ghost"); err == nil {
			t.Fatal("SetDefault on missing agent should error")
		}
	})
}

// TestDispatcher_Unregister covers rule cleanup and default reselection.
func TestDispatcher_Unregister(t *testing.T) {
	t.Run("unregister_missing_errors", func(t *testing.T) {
		d := New()
		if err := d.Unregister("ghost"); err == nil {
			t.Fatal("Unregister on missing agent should error")
		}
	})

	t.Run("unregister_default_reselects", func(t *testing.T) {
		d := New()
		mustRegister(t, d, "a1") // becomes default
		mustRegister(t, d, "a2")
		if err := d.AddRule(Rule{Platform: "feishu", AgentName: "a1"}); err != nil {
			t.Fatalf("AddRule: %v", err)
		}
		if err := d.Unregister("a1"); err != nil {
			t.Fatalf("Unregister: %v", err)
		}
		// a1's rule must be purged.
		if len(d.ListRules()) != 0 {
			t.Fatalf("expected a1 rules purged, got %+v", d.ListRules())
		}
		// Default should have been reselected to the remaining agent.
		if d.DefaultAgent() != "a2" {
			t.Fatalf("expected default to reselect a2, got %q", d.DefaultAgent())
		}
	})
}

// TestDispatcher_LoadAll is table-driven over default-selection edge cases.
func TestDispatcher_LoadAll(t *testing.T) {
	agents := []AgentConfig{{Name: "a1"}, {Name: "a2"}}
	rules := []Rule{{Platform: "feishu", AgentName: "a2", Priority: 1}}

	tests := []struct {
		name        string
		defaultArg  string
		wantDefault string // exact, or "" meaning "any registered agent"
		anyDefault  bool
	}{
		{name: "explicit_valid_default", defaultArg: "a2", wantDefault: "a2"},
		{name: "explicit_invalid_default_falls_back", defaultArg: "ghost", anyDefault: true},
		{name: "empty_default_picks_some_agent", defaultArg: "", anyDefault: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := New()
			d.LoadAll(agents, tc.defaultArg, rules)

			if len(d.ListAgents()) != 2 {
				t.Fatalf("expected 2 agents loaded, got %d", len(d.ListAgents()))
			}
			if len(d.ListRules()) != 1 {
				t.Fatalf("expected 1 rule loaded, got %d", len(d.ListRules()))
			}
			def := d.DefaultAgent()
			if tc.anyDefault {
				if def != "a1" && def != "a2" {
					t.Fatalf("expected default to be one of the registered agents, got %q", def)
				}
			} else if def != tc.wantDefault {
				t.Fatalf("expected default %q, got %q", tc.wantDefault, def)
			}
			// Rule routing should work after LoadAll.
			if got := d.Route(RouteRequest{Platform: "feishu"}); got == nil || got.AgentName != "a2" {
				t.Fatalf("expected feishu to route to a2, got %+v", got)
			}
		})
	}
}

// TestDispatcher_LoadAll_EmptyAgents leaves no default and routes to nothing.
func TestDispatcher_LoadAll_EmptyAgents(t *testing.T) {
	d := New()
	d.LoadAll(nil, "", nil)
	if d.DefaultAgent() != "" {
		t.Fatalf("empty agent set should leave default empty, got %q", d.DefaultAgent())
	}
	if got := d.Route(RouteRequest{Platform: "feishu"}); got != nil {
		t.Fatalf("routing with no agents should return nil, got %+v", got)
	}
}

// TestDispatcher_Explain_RuleSource verifies the rule-hit explanation, ordering of
// candidate matches by score, and that the winning rule is surfaced.
func TestDispatcher_Explain_RuleSource(t *testing.T) {
	d := New()
	mustRegister(t, d, "a1")
	mustRegister(t, d, "a2")
	// Platform-only rule (score 10) and a more specific user rule (score 110).
	if err := d.AddRule(Rule{Platform: "feishu", AgentName: "a1"}); err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	if err := d.AddRule(Rule{Platform: "feishu", UserID: "boss", AgentName: "a2"}); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	exp := d.Explain(context.Background(), RouteRequest{Platform: "feishu", UserID: "boss"}, "")
	if !exp.Matched {
		t.Fatal("expected a match")
	}
	if exp.Source != RouteSourceRule {
		t.Fatalf("expected rule source, got %q", exp.Source)
	}
	if exp.AgentName != "a2" {
		t.Fatalf("expected most specific rule (a2) to win, got %q", exp.AgentName)
	}
	if len(exp.Matches) != 2 {
		t.Fatalf("expected 2 candidate matches, got %d", len(exp.Matches))
	}
	if exp.Matches[0].Score < exp.Matches[1].Score {
		t.Fatalf("matches must be sorted by score desc: %+v", exp.Matches)
	}
	if exp.Score != exp.Matches[0].Score {
		t.Fatalf("explanation score should equal top match score: %d vs %d", exp.Score, exp.Matches[0].Score)
	}
	if exp.Rule == nil || exp.Rule.AgentName != "a2" {
		t.Fatalf("explanation should surface winning rule for a2, got %+v", exp.Rule)
	}
}

// TestDispatcher_Explain_DefaultSource verifies explanation when no rule matches
// but a default agent exists.
func TestDispatcher_Explain_DefaultSource(t *testing.T) {
	d := New()
	mustRegister(t, d, "a1") // default
	exp := d.Explain(context.Background(), RouteRequest{Platform: "unknown"}, "")
	if !exp.Matched {
		t.Fatal("expected match via default")
	}
	if exp.Source != RouteSourceDefault {
		t.Fatalf("expected default source, got %q", exp.Source)
	}
	if exp.AgentName != "a1" {
		t.Fatalf("expected default a1, got %q", exp.AgentName)
	}
	if len(exp.Matches) != 0 {
		t.Fatalf("expected no rule matches, got %d", len(exp.Matches))
	}
}

// TestDispatcher_Explain_NoMatch verifies the none-source path: no rules, no default.
func TestDispatcher_Explain_NoMatch(t *testing.T) {
	d := New()
	mustRegister(t, d, "a1")
	if err := d.SetDefault(""); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	exp := d.Explain(context.Background(), RouteRequest{Platform: "feishu"}, "")
	if exp.Matched {
		t.Fatalf("expected no match, got %+v", exp)
	}
	if exp.Source != RouteSourceNone {
		t.Fatalf("expected none source, got %q", exp.Source)
	}
}

// TestDispatcher_Explain_LLMSource verifies that with two agents, no matching rule,
// and a classifier returning high confidence, Explain reports the LLM source.
func TestDispatcher_Explain_LLMSource(t *testing.T) {
	d := New()
	mustRegister(t, d, "a1")
	mustRegister(t, d, "a2")
	if err := d.SetDefault(""); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	clf := NewLLMClassifier(func(_ context.Context, _, _ string) (string, error) {
		return `{"agent":"a2","confidence":0.95}`, nil
	})
	d.SetClassifier(clf)

	exp := d.Explain(context.Background(), RouteRequest{Platform: "no-rule"}, "route me")
	if !exp.Matched || exp.Source != RouteSourceLLM || exp.AgentName != "a2" {
		t.Fatalf("expected LLM route to a2, got %+v", exp)
	}
}

// TestRouteWithFallback_LowConfidenceFallsBackToDefault verifies sub-threshold
// classifier confidence yields the default agent with default source.
func TestRouteWithFallback_LowConfidenceFallsBackToDefault(t *testing.T) {
	d := New()
	mustRegister(t, d, "a1") // default
	mustRegister(t, d, "a2")
	clf := NewLLMClassifier(func(_ context.Context, _, _ string) (string, error) {
		return `{"agent":"a2","confidence":0.2}`, nil
	})
	clf.SetConfidenceThreshold(0.6)
	d.SetClassifier(clf)

	res, src := d.RouteWithFallback(context.Background(), RouteRequest{Platform: "no-rule"}, "hi")
	if src != RouteSourceDefault {
		t.Fatalf("low confidence should fall back to default, got source %q", src)
	}
	if res == nil || res.AgentName != "a1" {
		t.Fatalf("expected default a1, got %+v", res)
	}
}

// TestRouteWithFallback_ClassifierError falls back to default when the classifier errors.
func TestRouteWithFallback_ClassifierError(t *testing.T) {
	d := New()
	mustRegister(t, d, "a1") // default
	mustRegister(t, d, "a2")
	clf := NewLLMClassifier(func(_ context.Context, _, _ string) (string, error) {
		return "", errors.New("upstream down")
	})
	d.SetClassifier(clf)

	res, src := d.RouteWithFallback(context.Background(), RouteRequest{Platform: "no-rule"}, "hi")
	if src != RouteSourceDefault || res == nil || res.AgentName != "a1" {
		t.Fatalf("classifier error should fall back to default a1, got src=%q res=%+v", src, res)
	}
}

// TestRouteWithFallback_SingleAgentSkipsClassifier verifies the classifier is not
// consulted when fewer than 2 agents exist.
func TestRouteWithFallback_SingleAgentSkipsClassifier(t *testing.T) {
	d := New()
	mustRegister(t, d, "a1") // sole agent, also default
	called := false
	clf := NewLLMClassifier(func(_ context.Context, _, _ string) (string, error) {
		called = true
		return `{"agent":"a1","confidence":0.99}`, nil
	})
	d.SetClassifier(clf)

	res, src := d.RouteWithFallback(context.Background(), RouteRequest{Platform: "no-rule"}, "hi")
	if called {
		t.Fatal("classifier must not be called with a single agent")
	}
	if src != RouteSourceDefault || res == nil || res.AgentName != "a1" {
		t.Fatalf("expected default a1, got src=%q res=%+v", src, res)
	}
}

// TestRouteWithFallback_NoAgentsReturnsNone verifies the terminal none path.
func TestRouteWithFallback_NoAgentsReturnsNone(t *testing.T) {
	d := New()
	res, src := d.RouteWithFallback(context.Background(), RouteRequest{Platform: "x"}, "hi")
	if res != nil || src != RouteSourceNone {
		t.Fatalf("empty dispatcher should yield (nil, none), got res=%+v src=%q", res, src)
	}
}

// TestLLMClassifier_Cache verifies the classify function is invoked once for repeated
// identical inputs (result is cached).
func TestLLMClassifier_Cache(t *testing.T) {
	d := New()
	mustRegister(t, d, "a1")
	mustRegister(t, d, "a2")
	if err := d.SetDefault(""); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	calls := 0
	clf := NewLLMClassifier(func(_ context.Context, _, _ string) (string, error) {
		calls++
		return `{"agent":"a2","confidence":0.9}`, nil
	})
	d.SetClassifier(clf)

	for i := 0; i < 3; i++ {
		res, src := d.RouteWithFallback(context.Background(), RouteRequest{Platform: "no-rule"}, "same message")
		if src != RouteSourceLLM || res == nil || res.AgentName != "a2" {
			t.Fatalf("iter %d: expected LLM route to a2, got src=%q res=%+v", i, src, res)
		}
	}
	if calls != 1 {
		t.Fatalf("classify should be cached and called once, got %d calls", calls)
	}
}

// TestParseClassifyResult is table-driven over the parser's decision branches.
func TestParseClassifyResult(t *testing.T) {
	agents := map[string]*AgentConfig{
		"research": {Name: "research"},
		"coder":    {Name: "coder"},
	}
	tests := []struct {
		name      string
		raw       string
		wantAgent string
		wantConf  float64
	}{
		{name: "valid_json", raw: `{"agent":"research","confidence":0.85}`, wantAgent: "research", wantConf: 0.85},
		{name: "json_zero_confidence_defaults", raw: `{"agent":"coder","confidence":0}`, wantAgent: "coder", wantConf: 0.8},
		{name: "plain_text_exact_case_insensitive", raw: "RESEARCH", wantAgent: "research", wantConf: 0.8},
		{name: "fuzzy_contains_match", raw: "I think the coder agent fits", wantAgent: "coder", wantConf: 0.6},
		{name: "unknown_falls_through", raw: "totally unrelated", wantAgent: "totally unrelated", wantConf: 0.3},
		{name: "json_unknown_agent_falls_through", raw: `{"agent":"ghost","confidence":0.9}`, wantAgent: `{"agent":"ghost","confidence":0.9}`, wantConf: 0.3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent, conf := parseClassifyResult(tc.raw, agents)
			if agent != tc.wantAgent {
				t.Fatalf("agent: want %q got %q", tc.wantAgent, agent)
			}
			if conf != tc.wantConf {
				t.Fatalf("confidence: want %v got %v", tc.wantConf, conf)
			}
		})
	}
}

// TestBuildClassifierPrompt confirms the prompt lists agents with descriptions.
func TestBuildClassifierPrompt(t *testing.T) {
	agents := map[string]*AgentConfig{
		"research": {Name: "research", Description: "does research", SystemPrompt: "be thorough"},
	}
	prompt := buildClassifierPrompt(agents)
	if prompt == "" {
		t.Fatal("prompt should not be empty")
	}
	for _, want := range []string{"research", "does research"} {
		if !containsStr(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func containsStr(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
