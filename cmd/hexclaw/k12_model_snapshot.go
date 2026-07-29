package main

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// resolveK12GradingModelSnapshot is the single control-plane selector shared
// by all K12 GradingJob entry points. An explicit session provider/model is
// validated and preserved exactly. Only an empty request may select a stable
// text+vision model within the configured default provider.
func resolveK12GradingModelSnapshot(
	router *llmrouter.Selector,
	requested k12.GradingModelSnapshot,
) (k12.GradingModelSnapshot, error) {
	if router == nil {
		return k12.GradingModelSnapshot{}, fmt.Errorf("LLM router 未初始化")
	}
	requested = k12.NormalizeGradingModelSnapshot(requested)
	route, err := router.ResolveRouteForCapabilities(
		requested.Provider,
		requested.Model,
		config.LLMModelCapabilityText,
		config.LLMModelCapabilityVision,
	)
	if err != nil {
		return k12.GradingModelSnapshot{}, err
	}
	snapshot := k12.GradingModelSnapshot{
		Provider:   route.ProviderName,
		Model:      route.Model,
		Route:      route.ProviderName + "/" + route.Model,
		Capability: config.LLMModelCapabilityVision,
		TimeoutMS:  int(k12.GradingStageBudgetSeconds(k12.GradingStageRecognizing) * 1000),
	}
	if snapshot.Model == k12.RecognizingPolicyModel {
		snapshot.RecognizingRequestPolicy = k12.ApprovedRecognizingRequestPolicy()
	}
	return k12.NormalizeGradingModelSnapshot(snapshot), nil
}

// resolveK12PracticeModelSnapshot freezes the exact text-capable route used by
// one asynchronous practice-generation job. Explicit session selections are
// preserved exactly; only an empty request may resolve from current defaults.
func resolveK12PracticeModelSnapshot(
	router *llmrouter.Selector,
	requested k12.GradingModelSnapshot,
) (k12.GradingModelSnapshot, error) {
	if router == nil {
		return k12.GradingModelSnapshot{}, fmt.Errorf("LLM router 未初始化")
	}
	requested = k12.NormalizeGradingModelSnapshot(requested)
	route, err := router.ResolveRouteForCapabilities(
		requested.Provider,
		requested.Model,
		config.LLMModelCapabilityText,
	)
	if err != nil {
		return k12.GradingModelSnapshot{}, err
	}
	return k12.GradingModelSnapshot{
		Provider:   route.ProviderName,
		Model:      route.Model,
		Route:      route.ProviderName + "/" + route.Model,
		Capability: config.LLMModelCapabilityText,
		TimeoutMS:  60_000,
	}, nil
}

// k12VisionRequestMetadata translates the typed, stage-scoped semantic policy
// into ai-core metadata. The Provider adapter, not HexClaw, owns the final wire
// dialect (`reasoning_effort=none` for gpt-5.6-sol).
func k12VisionRequestMetadata(ctx context.Context) (map[string]any, error) {
	policy, marked := k12.GradingModelRequestPolicyFromContext(ctx)
	if !marked {
		return nil, nil
	}
	snapshot, frozen := k12.GradingModelSnapshotFromContext(ctx)
	if !frozen {
		return nil, fmt.Errorf("K12 recognizing request policy has no frozen route")
	}
	if err := k12.ValidateModelInvocationRequestPolicy(
		k12.GradingStageRecognizing,
		snapshot,
		policy,
	); err != nil {
		return nil, err
	}
	if !policy.IsApprovedRecognizing() {
		return nil, fmt.Errorf("K12 recognizing request policy is not approved")
	}
	return map[string]any{"thinking": policy.Thinking}, nil
}
