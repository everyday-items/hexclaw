package api

import (
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/config"
)

func resolveProviderInstanceID(
	providerKey string,
	old config.LLMProviderConfig,
	oldExists bool,
	requested string,
) (string, error) {
	if oldExists {
		effectiveOld := config.EffectiveProviderInstanceID(providerKey, old)
		if requested == "" {
			return effectiveOld, nil
		}
		if err := config.ValidateProviderInstanceID(requested); err != nil {
			return "", err
		}
		if requested != effectiveOld {
			return "", fmt.Errorf("provider_instance_id 不可变：已有 %q，收到 %q", effectiveOld, requested)
		}
		return requested, nil
	}

	if requested != "" {
		if err := config.ValidateProviderInstanceID(requested); err != nil {
			return "", err
		}
		return requested, nil
	}
	return config.NewProviderInstanceID()
}

func resolveProviderModelSpecs(
	old config.LLMProviderConfig,
	oldExists bool,
	requested LLMProviderConfigUpdateItem,
) (string, []config.LLMProviderModelSpec) {
	if requested.ModelSpecs != nil {
		explicit := make([]config.LLMProviderModelSpec, len(*requested.ModelSpecs))
		copy(explicit, *requested.ModelSpecs)
		candidate := config.LLMProviderConfig{
			Models:         requested.Models,
			ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs:     explicit,
		}
		_, normalized := config.NormalizeProviderModelSpecs(candidate)
		return config.LLMModelSpecsModeExplicit, normalized
	}

	if !oldExists {
		return config.LLMModelSpecsModeLegacy, nil
	}
	oldMode, oldSpecs := config.NormalizeProviderModelSpecs(old)
	if oldMode != config.LLMModelSpecsModeExplicit {
		return config.LLMModelSpecsModeLegacy, nil
	}

	allowed := make(map[string]struct{}, len(requested.Models))
	for _, modelID := range requested.Models {
		allowed[modelID] = struct{}{}
	}
	filtered := make([]config.LLMProviderModelSpec, 0, len(oldSpecs))
	for _, spec := range oldSpecs {
		if _, keep := allowed[spec.ID]; keep {
			filtered = append(filtered, spec)
		}
	}
	oldModelIDs := make(map[string]struct{}, len(old.Models))
	for _, modelID := range old.Models {
		oldModelIDs[modelID] = struct{}{}
	}
	for _, modelID := range requested.Models {
		if _, existed := oldModelIDs[modelID]; existed {
			continue
		}
		filtered = append(filtered, config.LLMProviderModelSpec{
			ID:           modelID,
			DisplayName:  modelID,
			Capabilities: []string{config.LLMModelCapabilityText},
		})
	}
	return config.LLMModelSpecsModeExplicit, filtered
}

func isEmbeddingOnlyCompletionModel(llmCfg config.LLMConfig, providerType, baseURL, modelID string) bool {
	if _, ok := config.MigrateOpenRouterEmbeddingModelSpec(modelID); ok {
		return true
	}

	normalizedBaseURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for providerKey, provider := range llmCfg.Providers {
		if normalizedBaseURL != "" {
			if strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/") != normalizedBaseURL {
				continue
			}
		} else if !strings.EqualFold(strings.TrimSpace(providerKey), strings.TrimSpace(providerType)) {
			continue
		}
		if config.ModelHasCapability(provider, modelID, config.LLMModelCapabilityEmbedding) &&
			!config.ModelHasCapability(provider, modelID, config.LLMModelCapabilityText) {
			return true
		}
	}
	return false
}

// validateConfiguredTextModel is the shared API-boundary guard for operations
// that necessarily invoke a completion model (chat, agent binding and tool
// capability probes). A configured provider/model must be explicitly eligible
// for text routing; unknown and embedding-only rows fail closed.
func validateConfiguredTextModel(llmCfg config.LLMConfig, providerName, modelID string) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return fmt.Errorf("model 不能为空")
	}
	if _, intrinsicEmbeddingOnly := config.MigrateOpenRouterEmbeddingModelSpec(modelID); intrinsicEmbeddingOnly {
		return fmt.Errorf("model %q 是 embedding-only，不能执行 completion 或工具探测", modelID)
	}
	providerKey, ok := findLLMProviderKey(llmCfg, providerName)
	if !ok {
		return fmt.Errorf("指定的 provider %q 不存在", strings.TrimSpace(providerName))
	}
	provider := llmCfg.Providers[providerKey]
	if !config.ModelHasCapability(provider, modelID, config.LLMModelCapabilityText) {
		return fmt.Errorf("provider %q 的 model %q 不具备 text capability", providerKey, modelID)
	}
	return nil
}

func validateRequestedCompletionModel(llmCfg config.LLMConfig, providerName, modelID string) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" || strings.EqualFold(modelID, "auto") {
		return nil
	}
	if _, embeddingOnly := config.MigrateOpenRouterEmbeddingModelSpec(modelID); embeddingOnly {
		return fmt.Errorf("embedding-only 模型不能执行 completion")
	}
	providerName = strings.TrimSpace(providerName)
	if providerName == "" || strings.EqualFold(providerName, "auto") {
		// Dynamic/provider-less routing is checked again by the selector's
		// data-plane capability facade once the actual provider is known.
		return nil
	}
	return validateConfiguredTextModel(llmCfg, providerName, modelID)
}

func validateUniqueProviderInstanceIDs(providers map[string]config.LLMProviderConfig) error {
	seen := make(map[string]string, len(providers))
	for name, provider := range providers {
		id := provider.ProviderInstanceID
		if previous, exists := seen[id]; exists {
			return fmt.Errorf("provider %q 与 %q 复用了 provider_instance_id %q", name, previous, id)
		}
		seen[id] = name
	}
	return nil
}
