package main

import (
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
	return k12.GradingModelSnapshot{
		Provider:   route.ProviderName,
		Model:      route.Model,
		Route:      route.ProviderName + "/" + route.Model,
		Capability: config.LLMModelCapabilityVision,
	}, nil
}
