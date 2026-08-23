package main

import (
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
)

func TestREGBUGK12RealProbeFixtureRoute002ExplicitModelIgnoresDefaultModelOrder(t *testing.T) {
	cfg := config.LLMConfig{
		Default: "hexclaw-gpt",
		Providers: map[string]config.LLMProviderConfig{
			"hexclaw-gpt": {
				Model:          "gpt-5.6-luna",
				Models:         []string{"gpt-5.6-luna", "gpt-5.6-terra", "gpt-5.6-sol"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{ID: "gpt-5.6-luna", Capabilities: []string{config.LLMModelCapabilityText, config.LLMModelCapabilityVision}},
					{ID: "gpt-5.6-terra", Capabilities: []string{config.LLMModelCapabilityText, config.LLMModelCapabilityVision}},
					{ID: "gpt-5.6-sol", Capabilities: []string{config.LLMModelCapabilityText, config.LLMModelCapabilityVision}},
				},
			},
		},
	}
	router := llmrouter.NewWithProviders(cfg, map[string]hexagon.Provider{
		"hexclaw-gpt": mockllm.NewLLMProvider("hexclaw-gpt"),
	})

	snapshot, err := k12SelfInventorySnapshot(router, "hexclaw-gpt", "gpt-5.6-sol")
	if err != nil {
		t.Fatalf("freeze explicit probe route: %v", err)
	}
	if snapshot.Provider != "hexclaw-gpt" || snapshot.Model != "gpt-5.6-sol" {
		t.Fatalf("explicit probe route drifted to %s/%s", snapshot.Provider, snapshot.Model)
	}
}

func TestREGBUGK12RealProbeFixtureRoute002ManifestIsThreeDistinctFrozenFixtures(t *testing.T) {
	want := map[string]string{
		"clear": "0c4b1a972319203b1483ffbce43e8835b1367be53edceea23c89368a2f2bc861",
		"messy": "78cf3a1b5c52e12ca17ca13aa71c7a9439baed244e88b438aa2f1f70cd782fb5",
		"blank": "76c3bbab79486619d680114b8c182c0e23d15ce305239dc762819a5f0407eed7",
	}
	if len(k12SelfInventoryFixtures) != len(want) {
		t.Fatalf("fixture manifest entries=%d want=%d", len(k12SelfInventoryFixtures), len(want))
	}
	seen := make(map[string]string, len(want))
	for id, sha := range want {
		fixture, ok := k12SelfInventoryFixtures[id]
		if !ok {
			t.Fatalf("fixture manifest missing %q", id)
		}
		if fixture.sha256 != sha {
			t.Fatalf("fixture %q sha=%q want=%q", id, fixture.sha256, sha)
		}
		if fixture.assertQuestions == nil {
			t.Fatalf("fixture %q has no semantic assertion", id)
		}
		if fixture.diagnosticQuestion == "" {
			t.Fatalf("fixture %q has no safe diagnostic anchor", id)
		}
		if prior, duplicate := seen[sha]; duplicate {
			t.Fatalf("fixture %q shares SHA with %q", id, prior)
		}
		seen[sha] = id
	}
}

func TestREGBUGK12RealProbeFixtureRoute002UnknownOrDriftedFixtureFailsBeforeProviderPreparation(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		raw  []byte
	}{
		{name: "unknown", id: "unknown", raw: []byte("not-a-known-fixture")},
		{name: "digest drift", id: "clear", raw: []byte("wrong-bytes")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := k12SelfInventoryFixtureFor(tc.id, tc.raw); err == nil {
				t.Fatal("fixture preflight unexpectedly accepted untrusted bytes")
			}
		})
	}
}
