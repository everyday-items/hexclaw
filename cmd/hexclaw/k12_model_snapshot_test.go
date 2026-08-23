package main

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/api"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/storage"
)

// K12-PROJECTING-FROZEN-ROUTE-001：即使可变的路由默认值不同，页面摘要回调也会
// 继承已确认的 GradingJob 路由。
func TestResolveK12FrozenTextCompletionRouteUsesSnapshotOverDefault(t *testing.T) {
	defaultProvider := mockllm.NewLLMProvider("fallback")
	frozenProvider := mockllm.NewLLMProvider("hexclaw-gpt")
	frozenConfig := config.LLMProviderConfig{
		ProviderInstanceID: "pvd_v1_00112233445566778899aabbccddeeff",
		BaseURL:            "https://example.invalid/v1",
		APIKey:             "test-key",
		Model:              "gpt-5.6-sol",
		Models:             []string{"gpt-5.6-sol"},
		ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
		ModelSpecs: []config.LLMProviderModelSpec{{
			ID: "gpt-5.6-sol", Capabilities: []string{
				config.LLMModelCapabilityText,
				config.LLMModelCapabilityVision,
			},
		}},
	}
	router := llmrouter.NewWithProviders(config.LLMConfig{
		Default: "fallback",
		Providers: map[string]config.LLMProviderConfig{
			"fallback": {
				Model: "fallback-model", Models: []string{"fallback-model"},
			},
			"hexclaw-gpt": frozenConfig,
		},
	}, map[string]hexagon.Provider{
		"fallback":    defaultProvider,
		"hexclaw-gpt": frozenProvider,
	})

	fingerprint := api.ModelCapabilityProbeConfigFingerprint("hexclaw-gpt", frozenConfig, "gpt-5.6-sol")
	receipts := &k12CapabilityReceiptStoreStub{receipt: &storage.ModelCapabilityProbeReceipt{
		ProviderInstanceID: frozenConfig.ProviderInstanceID, ModelID: "gpt-5.6-sol", ProbeKind: "vision",
		ConfigFingerprint: fingerprint, ProbePolicyVersion: api.ModelCapabilityProbePolicyVersion, Outcome: "passed",
		TestedAt: 100, ProbeStartedAt: 99, LatencyMS: 12,
	}}
	frozenSnapshot, err := resolveK12GradingModelSnapshotWithCapabilityReceipt(
		context.Background(), router, receipts, k12.GradingModelSnapshot{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
		},
	)
	if err != nil {
		t.Fatalf("resolve frozen snapshot: %v", err)
	}
	frozenCtx := k12.WithGradingModelSnapshot(context.Background(), frozenSnapshot)
	provider, model, err := resolveK12FrozenTextCompletionRoute(
		frozenCtx, router, receipts, "k12 辅导要点",
	)
	if err != nil {
		t.Fatalf("resolve frozen route: %v", err)
	}
	if provider.Name() != "hexclaw-gpt" || model != "gpt-5.6-sol" {
		t.Fatalf("frozen page-summary route=%s/%s, want hexclaw-gpt/gpt-5.6-sol", provider.Name(), model)
	}

	provider, model, err = resolveK12FrozenTextCompletionRoute(
		context.Background(), router, receipts, "k12 辅导要点",
	)
	if err != nil {
		t.Fatalf("resolve default route: %v", err)
	}
	if provider.Name() != "fallback" || model != "" {
		t.Fatalf("non-grading default route=%s/%q, want fallback/empty model", provider.Name(), model)
	}
}

func TestResolveK12FrozenTextCompletionRouteFailsBeforeMissingProvider(t *testing.T) {
	router := llmrouter.NewWithProviders(config.LLMConfig{
		Default: "fallback",
		Providers: map[string]config.LLMProviderConfig{
			"fallback": {Model: "fallback-model", Models: []string{"fallback-model"}},
		},
	}, map[string]hexagon.Provider{
		"fallback": mockllm.NewLLMProvider("fallback"),
	})
	ctx := k12.WithGradingModelSnapshot(context.Background(), k12.GradingModelSnapshot{
		Provider: "missing", Model: "gpt-5.6-sol", Route: "missing/gpt-5.6-sol",
	})
	if _, _, err := resolveK12FrozenTextCompletionRoute(ctx, router, nil, "k12 辅导要点"); err == nil {
		t.Fatal("missing frozen provider must fail closed before any default fallback")
	}
}

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

func TestResolveK12PracticeModelSnapshotHonorsExplicitTextModel(t *testing.T) {
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

	got, err := resolveK12PracticeModelSnapshot(router, k12.GradingModelSnapshot{
		Provider: "hexclaw-gpt",
		Model:    "gpt-5.6-sol",
	})
	if err != nil {
		t.Fatalf("resolve explicit practice snapshot: %v", err)
	}
	if got.Provider != "hexclaw-gpt" || got.Model != "gpt-5.6-sol" ||
		got.Route != "hexclaw-gpt/gpt-5.6-sol" ||
		got.Capability != config.LLMModelCapabilityText {
		t.Fatalf("explicit practice route drifted: %+v", got)
	}
}

func TestResolveK12PracticeModelSnapshotAutomaticUsesConfiguredDefaultModel(t *testing.T) {
	cfg := config.LLMConfig{
		Default: "hexclaw-gpt",
		Providers: map[string]config.LLMProviderConfig{
			"hexclaw-gpt": {
				Model:          "gpt-5.6-sol",
				Models:         []string{"gpt-5.3-codex-spark", "gpt-5.6-sol"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{ID: "gpt-5.3-codex-spark", Capabilities: []string{config.LLMModelCapabilityText}},
					{ID: "gpt-5.6-sol", Capabilities: []string{config.LLMModelCapabilityText}},
				},
			},
		},
	}
	router := llmrouter.NewWithProviders(cfg, map[string]hexagon.Provider{
		"hexclaw-gpt": mockllm.NewLLMProvider("hexclaw-gpt"),
	})

	got, err := resolveK12PracticeModelSnapshot(router, k12.GradingModelSnapshot{})
	if err != nil {
		t.Fatalf("resolve automatic practice snapshot: %v", err)
	}
	if got.Provider != "hexclaw-gpt" || got.Model != "gpt-5.6-sol" {
		t.Fatalf("automatic practice route=%+v, want configured gpt-5.6-sol", got)
	}
}
