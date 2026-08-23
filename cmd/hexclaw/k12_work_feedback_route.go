package main

import (
	"strings"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// resolveK12WorkFeedbackRoute 是生产 K12 runtime 和显式启用的真实 Provider 探针
// 共用的唯一路由快照编译器。
func resolveK12WorkFeedbackRoute(
	router *llmrouter.Selector,
	workType string,
) (k12.ImageTaskRouteSnapshot, error) {
	capabilities := []string{config.LLMModelCapabilityText}
	promptVersion := "writing-feedback-v1"
	if workType == k12.WorkTypeArt {
		capabilities = append(capabilities, config.LLMModelCapabilityVision)
		promptVersion = "art-feedback-v1"
	}
	route, err := router.ResolveRouteForCapabilities("", "", capabilities...)
	if err != nil {
		return k12.ImageTaskRouteSnapshot{}, err
	}
	providerInstanceID, err := k12ProviderInstanceID(router, route.ProviderName)
	if err != nil {
		return k12.ImageTaskRouteSnapshot{}, err
	}
	return k12.ImageTaskRouteSnapshot{
		Provider:           route.ProviderName,
		Model:              route.Model,
		Route:              route.ProviderName + "/" + route.Model,
		ProviderInstanceID: providerInstanceID,
		Capability:         strings.Join(capabilities, "+"),
		SelectionSource:    "auto",
		PolicyVersion:      "work-feedback-routing-v1",
		PromptVersion:      promptVersion,
	}, nil
}
