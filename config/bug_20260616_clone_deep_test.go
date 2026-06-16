package config

// BUG-20260616: cloneConfig did `out := *c` (a shallow struct copy), so the
// clone shared the backing arrays of every map/slice field (LLM.Providers,
// Features, Platforms.*, Metadata, ...). The transaction snapshot was therefore
// not isolated: mutating a returned snapshot/Current() corrupted the live config
// and broke rollback. The clone must deep-copy.

import "testing"

func TestBug20260616_CloneConfigDeepCopiesMaps(t *testing.T) {
	orig := &Config{
		LLM:      LLMConfig{Providers: map[string]LLMProviderConfig{"a": {Model: "m1"}}},
		Features: map[string]bool{"f": true},
	}

	clone := cloneConfig(orig)
	clone.LLM.Providers["a"] = LLMProviderConfig{Model: "MUTATED"}
	clone.LLM.Providers["new"] = LLMProviderConfig{Model: "leak"}
	clone.Features["f"] = false

	if got := orig.LLM.Providers["a"].Model; got != "m1" {
		t.Errorf("clone shares LLM.Providers backing: orig mutated to %q", got)
	}
	if _, ok := orig.LLM.Providers["new"]; ok {
		t.Error("clone shares LLM.Providers backing: new key leaked into orig")
	}
	if !orig.Features["f"] {
		t.Error("clone shares Features backing: orig mutated")
	}
}
