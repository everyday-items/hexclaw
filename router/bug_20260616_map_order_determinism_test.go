package router

// BUG-20260616: several routing decisions iterated a map directly. Go randomizes
// map iteration order, so identical inputs produced different outcomes across
// calls/restarts: the fuzzy fallback returned a random agent when names overlap,
// the classifier prompt listed agents in a random order (defeating its cache),
// and the auto-selected default agent (the handler for unmatched messages) was
// whichever name the runtime yielded first. These tests pin determinism by
// running each path 256 times and asserting a single stable outcome.

import "testing"

func TestBug20260616_FuzzyFallbackDeterministic(t *testing.T) {
	// "research" is a substring of "research-bot": both Contains the raw text,
	// so unsorted iteration returned whichever matched first.
	agents := map[string]*AgentConfig{
		"research":     {Name: "research"},
		"research-bot": {Name: "research-bot"},
	}
	raw := "please route this to research-bot"

	seen := map[string]int{}
	for i := 0; i < 256; i++ {
		a, _ := parseClassifyResult(raw, agents)
		seen[a]++
	}
	if len(seen) != 1 {
		t.Fatalf("fuzzy fallback non-deterministic across 256 calls: %v", seen)
	}
	if _, ok := seen["research-bot"]; !ok {
		t.Fatalf("most-specific match must win, got %v", seen)
	}
}

func TestBug20260616_ClassifierPromptDeterministic(t *testing.T) {
	agents := map[string]*AgentConfig{
		"alpha": {Name: "alpha", Description: "a"},
		"beta":  {Name: "beta", Description: "b"},
		"gamma": {Name: "gamma", Description: "c"},
	}
	seen := map[string]bool{}
	for i := 0; i < 256; i++ {
		seen[buildClassifierPrompt(agents)] = true
	}
	if len(seen) != 1 {
		t.Fatalf("classifier prompt non-deterministic across 256 builds: %d distinct variants", len(seen))
	}
}

func TestBug20260616_LoadAllDefaultDeterministic(t *testing.T) {
	agentList := []AgentConfig{{Name: "gamma"}, {Name: "alpha"}, {Name: "beta"}}

	seen := map[string]int{}
	for i := 0; i < 256; i++ {
		d := New()
		d.LoadAll(agentList, "", nil) // no explicit default -> auto-pick
		seen[d.DefaultAgent()]++
	}
	if len(seen) != 1 {
		t.Fatalf("auto default agent non-deterministic across 256 loads: %v", seen)
	}
	if _, ok := seen["alpha"]; !ok {
		t.Fatalf("auto default must be smallest name 'alpha', got %v", seen)
	}
}

func TestBug20260616_UnregisterDefaultRepickDeterministic(t *testing.T) {
	seen := map[string]int{}
	for i := 0; i < 256; i++ {
		d := New()
		for _, n := range []string{"gamma", "alpha", "beta"} {
			if err := d.Register(AgentConfig{Name: n}); err != nil {
				t.Fatalf("register %q: %v", n, err)
			}
		}
		if err := d.SetDefault("gamma"); err != nil {
			t.Fatalf("SetDefault: %v", err)
		}
		if err := d.Unregister("gamma"); err != nil { // removing the default forces a re-pick
			t.Fatalf("Unregister: %v", err)
		}
		seen[d.DefaultAgent()]++
	}
	if len(seen) != 1 {
		t.Fatalf("default re-pick after Unregister non-deterministic: %v", seen)
	}
	if _, ok := seen["alpha"]; !ok {
		t.Fatalf("re-picked default must be smallest remaining name 'alpha', got %v", seen)
	}
}
