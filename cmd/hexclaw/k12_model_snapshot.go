package main

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/ai-core/llm"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/storage"
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
	providerInstanceID, err := k12ProviderInstanceID(router, route.ProviderName)
	if err != nil {
		return k12.GradingModelSnapshot{}, err
	}
	snapshot := k12.GradingModelSnapshot{
		Provider:           route.ProviderName,
		Model:              route.Model,
		Route:              route.ProviderName + "/" + route.Model,
		ProviderInstanceID: providerInstanceID,
		Capability:         config.LLMModelCapabilityVision,
		TimeoutMS:          int(k12.GradingStageBudgetSeconds(k12.GradingStageRecognizing) * 1000),
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
	providerInstanceID, err := k12ProviderInstanceID(router, route.ProviderName)
	if err != nil {
		return k12.GradingModelSnapshot{}, err
	}
	return k12.GradingModelSnapshot{
		Provider:           route.ProviderName,
		Model:              route.Model,
		Route:              route.ProviderName + "/" + route.Model,
		ProviderInstanceID: providerInstanceID,
		Capability:         config.LLMModelCapabilityText,
		TimeoutMS:          60_000,
	}, nil
}

// k12ProviderInstanceID 将路由名绑定到稳定的配置实体，而不使用可编辑的展示名。
func k12ProviderInstanceID(router *llmrouter.Selector, providerName string) (string, error) {
	providerConfig, configured := router.ProviderConfig(providerName)
	if !configured {
		return "", fmt.Errorf("K12 frozen provider %q has no active configuration", providerName)
	}
	return config.EffectiveProviderInstanceID(providerName, providerConfig), nil
}

// k12ProviderLogIdentity 分开记录路由注册名与底层兼容适配器名，避免把
// hexclaw-gpt 使用 OpenAI-compatible adapter 误读成跨 Provider 路由。
func k12ProviderLogIdentity(ctx context.Context, provider llm.Provider) (routeProvider, adapterProvider string) {
	if provider == nil {
		return "", ""
	}
	adapterProvider = provider.Name()
	routeProvider = adapterProvider
	if snapshot, pinned := k12.GradingModelSnapshotFromContext(ctx); pinned {
		routeProvider = snapshot.Provider
	}
	return routeProvider, adapterProvider
}

// resolveK12FrozenTextCompletionRoute 是 K12 文本回调的数据平面唯一解析器，
// 这些回调可能从已确认的 GradingJob 中运行。冻结的 Job 路由具有权威性；
// 只有不带该上下文的调用方才保留原有的配置默认行为。
func resolveK12FrozenTextCompletionRoute(
	ctx context.Context,
	router *llmrouter.Selector,
	receipts storage.ModelCapabilityProbeReceiptStore,
	operation string,
) (llm.Provider, string, error) {
	if router == nil {
		return nil, "", fmt.Errorf("%s: LLM router is not initialized", operation)
	}
	if snapshot, pinned := k12.GradingModelSnapshotFromContext(ctx); pinned {
		provider, found := router.Get(snapshot.Provider)
		if !found || provider == nil {
			return nil, "", fmt.Errorf(
				"K12 GradingJob frozen provider %q is unavailable; cross-route fallback is refused",
				snapshot.Provider,
			)
		}
		if err := k12.ValidateGradingModelRoute(ctx, snapshot.Provider, snapshot.Model); err != nil {
			return nil, "", err
		}
		if err := validateK12FrozenModelCapabilityReceipt(
			ctx, router, receipts, snapshot, k12ProbeKindForSnapshot(snapshot),
		); err != nil {
			return nil, "", err
		}
		return provider, snapshot.Model, nil
	}

	provider := router.Default()
	if provider == nil {
		return nil, "", fmt.Errorf("%s: no default LLM provider is available", operation)
	}
	return provider, "", nil
}

// k12VisionRequestMetadata translates the typed, stage-scoped semantic policy
// into ai-core metadata plus the typed adapter inference scope. The Provider
// adapter, not HexClaw, owns the final wire dialect
// (`reasoning_effort=none` for gpt-5.6-sol).
func k12VisionRequestMetadata(
	ctx context.Context,
) (map[string]any, llm.ReasoningPolicyScope, error) {
	policy, marked := k12.GradingModelRequestPolicyFromContext(ctx)
	if !marked {
		return nil, "", nil
	}
	snapshot, frozen := k12.GradingModelSnapshotFromContext(ctx)
	if !frozen {
		return nil, "", fmt.Errorf("K12 recognizing request policy has no frozen route")
	}
	if err := k12.ValidateModelInvocationRequestPolicy(
		k12.GradingStageRecognizing,
		snapshot,
		policy,
	); err != nil {
		return nil, "", err
	}
	if !policy.IsApprovedRecognizing() {
		return nil, "", fmt.Errorf("K12 recognizing request policy is not approved")
	}
	return map[string]any{"thinking": policy.Thinking},
		llm.ReasoningPolicyScopeStructuredVisionRecognition,
		nil
}
