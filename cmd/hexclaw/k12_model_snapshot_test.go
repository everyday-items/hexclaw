package main

import (
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestResolveK12GradingModelSnapshotHonorsExplicitSessionModel(t *testing.T) {
	cfg := config.LLMConfig{
		Default: "hexclaw-gpt",
		Providers: map[string]config.LLMProviderConfig{
			"hexclaw-gpt": {
				Model:          "gpt-5.3-codex-spark",
				Models:         []string{"gpt-5.3-codex-spark", "gpt-5.6-sol"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{ID: "gpt-5.3-codex-spark", Capabilities: []string{config.LLMModelCapabilityText}},
					{ID: "gpt-5.6-sol", Capabilities: []string{config.LLMModelCapabilityText, config.LLMModelCapabilityVision}},
				},
			},
		},
	}
	router := llmrouter.NewWithProviders(cfg, map[string]hexagon.Provider{
		"hexclaw-gpt": mockllm.NewLLMProvider("hexclaw-gpt"),
	})

	got, err := resolveK12GradingModelSnapshot(router, k12.GradingModelSnapshot{
		Provider: "hexclaw-gpt",
		Model:    "gpt-5.6-sol",
	})
	if err != nil {
		t.Fatalf("resolve explicit snapshot: %v", err)
	}
	if got.Provider != "hexclaw-gpt" || got.Model != "gpt-5.6-sol" ||
		got.Route != "hexclaw-gpt/gpt-5.6-sol" ||
		got.Capability != config.LLMModelCapabilityVision {
		t.Fatalf("explicit session route drifted: %+v", got)
	}
}

func TestResolveK12GradingModelSnapshotRejectsExplicitTextOnlyWithoutSwitching(t *testing.T) {
	cfg := config.LLMConfig{
		Default: "hexclaw-gpt",
		Providers: map[string]config.LLMProviderConfig{
			"hexclaw-gpt": {
				Model:          "gpt-5.3-codex-spark",
				Models:         []string{"gpt-5.3-codex-spark", "gpt-5.6-sol"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{ID: "gpt-5.3-codex-spark", Capabilities: []string{config.LLMModelCapabilityText}},
					{ID: "gpt-5.6-sol", Capabilities: []string{config.LLMModelCapabilityText, config.LLMModelCapabilityVision}},
				},
			},
		},
	}
	router := llmrouter.NewWithProviders(cfg, map[string]hexagon.Provider{
		"hexclaw-gpt": mockllm.NewLLMProvider("hexclaw-gpt"),
	})

	if _, err := resolveK12GradingModelSnapshot(router, k12.GradingModelSnapshot{
		Provider: "hexclaw-gpt",
		Model:    "gpt-5.3-codex-spark",
	}); err == nil {
		t.Fatal("explicit text-only model must fail closed instead of switching to gpt-5.6-sol")
	}
}

func TestResolveK12GradingModelSnapshotAutoUsesStableDefaultProviderVisionModel(t *testing.T) {
	cfg := config.LLMConfig{
		Default: "hexclaw-gpt",
		Providers: map[string]config.LLMProviderConfig{
			"hexclaw-gpt": {
				Model: "text-default", Models: []string{"text-default", "vision-first", "vision-second"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{ID: "text-default", Capabilities: []string{config.LLMModelCapabilityText}},
					{ID: "vision-first", Capabilities: []string{config.LLMModelCapabilityText, config.LLMModelCapabilityVision}},
					{ID: "vision-second", Capabilities: []string{config.LLMModelCapabilityText, config.LLMModelCapabilityVision}},
				},
			},
			"other": {
				Model: "other-vision", Models: []string{"other-vision"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{{
					ID: "other-vision", Capabilities: []string{config.LLMModelCapabilityText, config.LLMModelCapabilityVision},
				}},
			},
		},
	}
	router := llmrouter.NewWithProviders(cfg, map[string]hexagon.Provider{
		"hexclaw-gpt": mockllm.NewLLMProvider("hexclaw-gpt"),
		"other":       mockllm.NewLLMProvider("other"),
	})

	got, err := resolveK12GradingModelSnapshot(router, k12.GradingModelSnapshot{})
	if err != nil {
		t.Fatalf("resolve automatic snapshot: %v", err)
	}
	if got.Provider != "hexclaw-gpt" || got.Model != "vision-first" {
		t.Fatalf("automatic route=%+v, want stable default-provider vision-first", got)
	}
}
