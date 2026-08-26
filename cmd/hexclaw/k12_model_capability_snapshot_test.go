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

// K12-FROZEN-MODEL-ROUTE-001：生产控制面解析必须冻结稳定实例身份，展示名或
// provider map key 之后变化时，后续回执校验才能判定是否仍是同一配置实体。
func TestResolveK12GradingModelSnapshotFreezesProviderInstanceID(t *testing.T) {
	const providerInstanceID = "pvd_v1_00112233445566778899aabbccddeeff"
	router := llmrouter.NewWithProviders(config.LLMConfig{
		Default: "hexclaw-gpt",
		Providers: map[string]config.LLMProviderConfig{
			"hexclaw-gpt": {
				ProviderInstanceID: providerInstanceID,
				Model:              "gpt-5.6-sol",
				Models:             []string{"gpt-5.6-sol"},
				ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{{
					ID: "gpt-5.6-sol", Capabilities: []string{
						config.LLMModelCapabilityText,
						config.LLMModelCapabilityVision,
					},
				}},
			},
		},
	}, map[string]hexagon.Provider{
		"hexclaw-gpt": mockllm.NewLLMProvider("hexclaw-gpt"),
	})

	got, err := resolveK12GradingModelSnapshot(router, k12.GradingModelSnapshot{})
	if err != nil {
		t.Fatalf("resolve K12 grading snapshot: %v", err)
	}
	if got.ProviderInstanceID != providerInstanceID {
		t.Fatalf("provider instance identity=%q, want %q", got.ProviderInstanceID, providerInstanceID)
	}
}

func TestResolveK12WorkFeedbackRouteFreezesProviderInstanceID(t *testing.T) {
	const providerInstanceID = "pvd_v1_ffeeddccbbaa99887766554433221100"
	router := llmrouter.NewWithProviders(config.LLMConfig{
		Default: "hexclaw-gpt",
		Providers: map[string]config.LLMProviderConfig{
			"hexclaw-gpt": {
				ProviderInstanceID: providerInstanceID,
				Model:              "gpt-5.6-sol",
				Models:             []string{"gpt-5.6-sol"},
				ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{{
					ID: "gpt-5.6-sol", Capabilities: []string{
						config.LLMModelCapabilityText,
						config.LLMModelCapabilityVision,
					},
				}},
			},
		},
	}, map[string]hexagon.Provider{
		"hexclaw-gpt": mockllm.NewLLMProvider("hexclaw-gpt"),
	})

	got, err := resolveK12WorkFeedbackRoute(router, k12.WorkTypeArt)
	if err != nil {
		t.Fatalf("resolve K12 work-feedback route: %v", err)
	}
	if got.ProviderInstanceID != providerInstanceID {
		t.Fatalf("provider instance identity=%q, want %q", got.ProviderInstanceID, providerInstanceID)
	}
}

func TestResolveK12WorkFeedbackRouteWithCapabilityReceiptKeepsAutomaticDefault(t *testing.T) {
	const providerInstanceID = "pvd_v1_ffeeddccbbaa99887766554433221100"
	provider := config.LLMProviderConfig{
		ProviderInstanceID: providerInstanceID,
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
	fingerprint := api.ModelCapabilityProbeConfigFingerprint(
		"hexclaw-gpt", provider, "gpt-5.6-sol",
	)
	receipts := &k12CapabilityReceiptStoreStub{receipt: &storage.ModelCapabilityProbeReceipt{
		ProviderInstanceID: providerInstanceID,
		ModelID:            "gpt-5.6-sol",
		ProbeKind:          config.LLMModelCapabilityVision,
		ConfigFingerprint:  fingerprint,
		ProbePolicyVersion: api.ModelCapabilityProbePolicyVersion,
		Outcome:            "passed",
		TestedAt:           100,
		ProbeStartedAt:     99,
		LatencyMS:          12,
	}}
	router := llmrouter.NewWithProviders(config.LLMConfig{
		Default: "hexclaw-gpt",
		Providers: map[string]config.LLMProviderConfig{
			"hexclaw-gpt": provider,
		},
	}, map[string]hexagon.Provider{
		"hexclaw-gpt": mockllm.NewLLMProvider("hexclaw-gpt"),
	})

	got, err := resolveK12WorkFeedbackRouteWithCapabilityReceipt(
		context.Background(), router, receipts, k12.WorkTypeArt,
	)
	if err != nil {
		t.Fatalf("resolve default work-feedback route: %v", err)
	}
	if got.Provider != "hexclaw-gpt" || got.Model != "gpt-5.6-sol" ||
		got.SelectionSource != "auto" || got.ProviderInstanceID != providerInstanceID ||
		got.ConfigFingerprint != fingerprint || got.CapabilityReceiptDigest == "" {
		t.Fatalf("default work-feedback route or receipt drifted: %+v", got)
	}
}

func TestResolveK12RequestedWorkFeedbackRouteKeepsParentSelectionAndCapabilityReceipt(t *testing.T) {
	const providerInstanceID = "pvd_v1_ffeeddccbbaa99887766554433221100"
	provider := config.LLMProviderConfig{
		ProviderInstanceID: providerInstanceID,
		BaseURL:            "https://example.invalid/v1",
		APIKey:             "test-key",
		Model:              "gpt-5.6-luna",
		Models:             []string{"gpt-5.6-luna", "gpt-5.6-sol"},
		ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
		ModelSpecs: []config.LLMProviderModelSpec{
			{
				ID: "gpt-5.6-luna", Capabilities: []string{
					config.LLMModelCapabilityText,
					config.LLMModelCapabilityVision,
				},
			},
			{
				ID: "gpt-5.6-sol", Capabilities: []string{
					config.LLMModelCapabilityText,
					config.LLMModelCapabilityVision,
				},
			},
		},
	}
	router := llmrouter.NewWithProviders(config.LLMConfig{
		Default: "hexclaw-gpt",
		Providers: map[string]config.LLMProviderConfig{
			"hexclaw-gpt": provider,
		},
	}, map[string]hexagon.Provider{
		"hexclaw-gpt": mockllm.NewLLMProvider("hexclaw-gpt"),
	})

	for _, tt := range []struct {
		name, workType, probeKind, capability, promptVersion string
	}{
		{
			name: "art", workType: k12.WorkTypeArt,
			probeKind:  config.LLMModelCapabilityVision,
			capability: "text+vision", promptVersion: "art-feedback-v1",
		},
		{
			name: "writing", workType: k12.WorkTypeWriting,
			probeKind:  config.LLMModelCapabilityText,
			capability: "text", promptVersion: "writing-feedback-v1",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fingerprint := api.ModelCapabilityProbeConfigFingerprint(
				"hexclaw-gpt", provider, "gpt-5.6-sol",
			)
			receipts := &k12CapabilityReceiptStoreStub{receipt: &storage.ModelCapabilityProbeReceipt{
				ProviderInstanceID: providerInstanceID,
				ModelID:            "gpt-5.6-sol",
				ProbeKind:          tt.probeKind,
				ConfigFingerprint:  fingerprint,
				ProbePolicyVersion: api.ModelCapabilityProbePolicyVersion,
				Outcome:            "passed",
				TestedAt:           100,
				ProbeStartedAt:     99,
				LatencyMS:          12,
			}}
			got, err := resolveK12RequestedWorkFeedbackRouteWithCapabilityReceipt(
				context.Background(), router, receipts, tt.workType,
				k12.ImageTaskRouteSnapshot{
					Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
					SelectionSource: "explicit",
				},
			)
			if err != nil {
				t.Fatalf("resolve requested work-feedback route: %v", err)
			}
			if got.Provider != "hexclaw-gpt" || got.Model != "gpt-5.6-sol" ||
				got.Route != "hexclaw-gpt/gpt-5.6-sol" ||
				got.SelectionSource != "explicit" || got.Capability != tt.capability ||
				got.PromptVersion != tt.promptVersion ||
				got.ProviderInstanceID != providerInstanceID ||
				got.ConfigFingerprint != fingerprint ||
				got.CapabilityReceiptDigest == "" ||
				got.ProbePolicyVersion != api.ModelCapabilityProbePolicyVersion {
				t.Fatalf("requested work-feedback route or receipt drifted: %+v", got)
			}
		})
	}
}

func TestResolveK12RequestedWorkFeedbackRouteRejectsMissingOrAutomaticSelection(t *testing.T) {
	router := llmrouter.NewWithProviders(config.LLMConfig{
		Default: "hexclaw-gpt",
		Providers: map[string]config.LLMProviderConfig{
			"hexclaw-gpt": {
				Model: "gpt-5.6-luna", Models: []string{"gpt-5.6-luna", "gpt-5.6-sol"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{ID: "gpt-5.6-luna", Capabilities: []string{config.LLMModelCapabilityText, config.LLMModelCapabilityVision}},
					{ID: "gpt-5.6-sol", Capabilities: []string{config.LLMModelCapabilityText, config.LLMModelCapabilityVision}},
				},
			},
		},
	}, map[string]hexagon.Provider{
		"hexclaw-gpt": mockllm.NewLLMProvider("hexclaw-gpt"),
	})

	for _, requested := range []k12.ImageTaskRouteSnapshot{
		{},
		{SelectionSource: "auto"},
		{Provider: "hexclaw-gpt", Model: "auto", SelectionSource: "explicit"},
		{Provider: "auto", Model: "gpt-5.6-sol", SelectionSource: "explicit"},
	} {
		if _, err := resolveK12RequestedWorkFeedbackRoute(
			router, k12.WorkTypeArt, requested,
		); err == nil {
			t.Fatalf("accepted non-explicit requested route: %+v", requested)
		}
	}
}
