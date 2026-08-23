package main

import (
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
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
