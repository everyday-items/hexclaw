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

type k12CapabilityReceiptStoreStub struct {
	receipt *storage.ModelCapabilityProbeReceipt
}

func (s *k12CapabilityReceiptStoreStub) SaveModelCapabilityProbeReceipt(
	context.Context,
	*storage.ModelCapabilityProbeReceipt,
) (bool, error) {
	return true, nil
}

func (s *k12CapabilityReceiptStoreStub) GetModelCapabilityProbeReceipt(
	_ context.Context,
	providerInstanceID, modelID, probeKind string,
) (*storage.ModelCapabilityProbeReceipt, error) {
	if s.receipt == nil ||
		s.receipt.ProviderInstanceID != providerInstanceID ||
		s.receipt.ModelID != modelID ||
		s.receipt.ProbeKind != probeKind {
		return nil, nil
	}
	copy := *s.receipt
	return &copy, nil
}

func k12CapabilityReceiptTestRouter(provider config.LLMProviderConfig) *llmrouter.Selector {
	return llmrouter.NewWithProviders(config.LLMConfig{
		Default: "hexclaw-gpt",
		Providers: map[string]config.LLMProviderConfig{
			"hexclaw-gpt": provider,
		},
	}, map[string]hexagon.Provider{
		"hexclaw-gpt": mockllm.NewLLMProvider("hexclaw-gpt"),
	})
}

// K12-FROZEN-MODEL-ROUTE-001：新任务只有在当前执行配置下存在匹配的视觉
// 探测成功回执时才能冻结；不能把静态 text+vision 声明伪装成实测证据。
func TestResolveK12GradingModelSnapshotWithCapabilityReceiptFreezesVerifiedVision(t *testing.T) {
	const providerInstanceID = "pvd_v1_00112233445566778899aabbccddeeff"
	provider := config.LLMProviderConfig{
		ProviderInstanceID: providerInstanceID,
		BaseURL:            "https://example.invalid/v1",
		APIKey:             "test-key",
		Model:              "vision-v1",
		Models:             []string{"vision-v1"},
		ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
		ModelSpecs: []config.LLMProviderModelSpec{{
			ID: "vision-v1", Capabilities: []string{
				config.LLMModelCapabilityText,
				config.LLMModelCapabilityVision,
			},
		}},
	}
	fingerprint := api.ModelCapabilityProbeConfigFingerprint("hexclaw-gpt", provider, "vision-v1")
	receipts := &k12CapabilityReceiptStoreStub{receipt: &storage.ModelCapabilityProbeReceipt{
		ProviderInstanceID: providerInstanceID,
		ModelID:            "vision-v1",
		ProbeKind:          "vision",
		ConfigFingerprint:  fingerprint,
		ProbePolicyVersion: api.ModelCapabilityProbePolicyVersion,
		Outcome:            "passed",
		TestedAt:           100,
		ProbeStartedAt:     99,
		LatencyMS:          12,
	}}

	snapshot, err := resolveK12GradingModelSnapshotWithCapabilityReceipt(
		context.Background(), k12CapabilityReceiptTestRouter(provider), receipts, k12.GradingModelSnapshot{},
	)
	if err != nil {
		t.Fatalf("resolve verified K12 route: %v", err)
	}
	if !snapshot.HasFrozenCapabilityProbeEvidence() {
		t.Fatalf("frozen snapshot lacks capability evidence: %+v", snapshot)
	}
	if snapshot.ConfigFingerprint != fingerprint || snapshot.ProbePolicyVersion != api.ModelCapabilityProbePolicyVersion ||
		snapshot.CapabilityReceiptDigest == "" {
		t.Fatalf("frozen capability evidence drifted: %+v", snapshot)
	}
	if err := validateK12FrozenModelCapabilityReceipt(
		context.Background(), k12CapabilityReceiptTestRouter(provider), receipts, snapshot, "vision",
	); err != nil {
		t.Fatalf("validate matching frozen capability receipt: %v", err)
	}
	receipts.receipt.ProbePolicyVersion = "v0"
	if _, err := resolveK12GradingModelSnapshotWithCapabilityReceipt(
		context.Background(), k12CapabilityReceiptTestRouter(provider), receipts, k12.GradingModelSnapshot{},
	); err == nil {
		t.Fatal("stale probe policy receipt must fail before K12 route creation")
	}
}

func TestResolveK12GradingModelSnapshotWithCapabilityReceiptRejectsMissingVisionReceipt(t *testing.T) {
	provider := config.LLMProviderConfig{
		ProviderInstanceID: "pvd_v1_00112233445566778899aabbccddeeff",
		Model:              "vision-v1",
		Models:             []string{"vision-v1"},
		ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
		ModelSpecs: []config.LLMProviderModelSpec{{
			ID: "vision-v1", Capabilities: []string{
				config.LLMModelCapabilityText,
				config.LLMModelCapabilityVision,
			},
		}},
	}
	if _, err := resolveK12GradingModelSnapshotWithCapabilityReceipt(
		context.Background(), k12CapabilityReceiptTestRouter(provider), &k12CapabilityReceiptStoreStub{}, k12.GradingModelSnapshot{},
	); err == nil {
		t.Fatal("missing vision receipt must fail before K12 route creation")
	}
}

func TestResolveK12FrozenTextCompletionRouteRejectsLegacySnapshotBeforeProviderCall(t *testing.T) {
	provider := config.LLMProviderConfig{
		ProviderInstanceID: "pvd_v1_00112233445566778899aabbccddeeff",
		Model:              "vision-v1",
		Models:             []string{"vision-v1"},
		ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
		ModelSpecs: []config.LLMProviderModelSpec{{
			ID: "vision-v1", Capabilities: []string{
				config.LLMModelCapabilityText,
				config.LLMModelCapabilityVision,
			},
		}},
	}
	ctx := k12.WithGradingModelSnapshot(context.Background(), k12.GradingModelSnapshot{
		Provider: "hexclaw-gpt", Model: "vision-v1", Route: "hexclaw-gpt/vision-v1",
		Capability: "text+vision",
	})
	if _, _, err := resolveK12FrozenTextCompletionRoute(
		ctx, k12CapabilityReceiptTestRouter(provider), &k12CapabilityReceiptStoreStub{}, "K12 test",
	); err == nil {
		t.Fatal("legacy snapshot without receipt evidence must stop before provider selection")
	}
}

func TestValidateK12FrozenModelCapabilityReceiptRejectsChangedExecutionConfig(t *testing.T) {
	const providerInstanceID = "pvd_v1_00112233445566778899aabbccddeeff"
	provider := config.LLMProviderConfig{
		ProviderInstanceID: providerInstanceID,
		BaseURL:            "https://old.example.invalid/v1",
		APIKey:             "test-key",
		Model:              "vision-v1",
		Models:             []string{"vision-v1"},
		ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
		ModelSpecs: []config.LLMProviderModelSpec{{
			ID: "vision-v1", Capabilities: []string{
				config.LLMModelCapabilityText,
				config.LLMModelCapabilityVision,
			},
		}},
	}
	fingerprint := api.ModelCapabilityProbeConfigFingerprint("hexclaw-gpt", provider, "vision-v1")
	receipts := &k12CapabilityReceiptStoreStub{receipt: &storage.ModelCapabilityProbeReceipt{
		ProviderInstanceID: providerInstanceID, ModelID: "vision-v1", ProbeKind: "vision",
		ConfigFingerprint: fingerprint, ProbePolicyVersion: api.ModelCapabilityProbePolicyVersion, Outcome: "passed",
		TestedAt: 100, ProbeStartedAt: 99, LatencyMS: 12,
	}}
	snapshot, err := resolveK12GradingModelSnapshotWithCapabilityReceipt(
		context.Background(), k12CapabilityReceiptTestRouter(provider), receipts, k12.GradingModelSnapshot{},
	)
	if err != nil {
		t.Fatalf("resolve snapshot: %v", err)
	}
	changed := provider
	changed.BaseURL = "https://changed.example.invalid/v1"
	if err := validateK12FrozenModelCapabilityReceipt(
		context.Background(), k12CapabilityReceiptTestRouter(changed), receipts, snapshot, "vision",
	); err == nil {
		t.Fatal("changed execution config must fail before a provider call")
	}
}
