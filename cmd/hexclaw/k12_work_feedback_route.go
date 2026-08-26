package main

import (
	"fmt"
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
	return resolveK12WorkFeedbackRouteSelection(
		router,
		workType,
		k12.ImageTaskRouteSnapshot{SelectionSource: "auto"},
	)
}

func resolveK12RequestedWorkFeedbackRoute(
	router *llmrouter.Selector,
	workType string,
	requested k12.ImageTaskRouteSnapshot,
) (k12.ImageTaskRouteSnapshot, error) {
	requested = k12.NormalizeImageTaskRouteSnapshot(requested)
	if requested.SelectionSource == "" {
		requested.SelectionSource = "explicit"
	}
	if requested.SelectionSource != "explicit" ||
		requested.Provider == "" || strings.EqualFold(requested.Provider, "auto") ||
		requested.Model == "" || strings.EqualFold(requested.Model, "auto") {
		return k12.ImageTaskRouteSnapshot{}, fmt.Errorf(
			"work-feedback requested route requires an explicit provider and model",
		)
	}
	return resolveK12WorkFeedbackRouteSelection(router, workType, requested)
}

func resolveK12WorkFeedbackRouteSelection(
	router *llmrouter.Selector,
	workType string,
	requested k12.ImageTaskRouteSnapshot,
) (k12.ImageTaskRouteSnapshot, error) {
	capabilities := []string{config.LLMModelCapabilityText}
	promptVersion := "writing-feedback-v1"
	if workType == k12.WorkTypeArt {
		capabilities = append(capabilities, config.LLMModelCapabilityVision)
		promptVersion = "art-feedback-v1"
	}
	requested = k12.NormalizeImageTaskRouteSnapshot(requested)
	providerName := requested.Provider
	modelID := requested.Model
	selectionSource := strings.TrimSpace(requested.SelectionSource)
	if selectionSource == "" {
		if providerName == "" && modelID == "" {
			selectionSource = "auto"
		} else {
			selectionSource = "explicit"
		}
	}
	switch selectionSource {
	case "auto":
		if (providerName == "" || strings.EqualFold(providerName, "auto")) &&
			(modelID == "" || strings.EqualFold(modelID, "auto")) {
			providerName = ""
			modelID = ""
		} else {
			return k12.ImageTaskRouteSnapshot{}, fmt.Errorf(
				"work-feedback route auto selection cannot include an explicit provider or model",
			)
		}
	case "explicit":
		if providerName == "" || modelID == "" {
			return k12.ImageTaskRouteSnapshot{}, fmt.Errorf(
				"work-feedback route explicit selection requires provider and model",
			)
		}
	default:
		return k12.ImageTaskRouteSnapshot{}, fmt.Errorf(
			"work-feedback route selection source is invalid: %q", selectionSource,
		)
	}
	route, err := router.ResolveRouteForCapabilities(providerName, modelID, capabilities...)
	if err != nil {
		return k12.ImageTaskRouteSnapshot{}, err
	}
	providerInstanceID, err := k12ProviderInstanceID(router, route.ProviderName)
	if err != nil {
		return k12.ImageTaskRouteSnapshot{}, err
	}
	displayModelID := strings.TrimSpace(requested.ModelID)
	if displayModelID == "" {
		displayModelID = route.Model
	}
	return k12.ImageTaskRouteSnapshot{
		Provider:            route.ProviderName,
		ProviderDisplayName: requested.ProviderDisplayName,
		Model:               route.Model,
		ModelID:             displayModelID,
		Route:               route.ProviderName + "/" + route.Model,
		ProviderInstanceID:  providerInstanceID,
		Capability:          strings.Join(capabilities, "+"),
		SelectionSource:     selectionSource,
		PolicyVersion:       "work-feedback-routing-v1",
		PromptVersion:       promptVersion,
	}, nil
}
