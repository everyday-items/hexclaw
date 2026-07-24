package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	LLMModelSpecsModeLegacy   = "legacy"
	LLMModelSpecsModeExplicit = "explicit"

	LLMModelCapabilityText            = "text"
	LLMModelCapabilityVision          = "vision"
	LLMModelCapabilityVideo           = "video"
	LLMModelCapabilityAudio           = "audio"
	LLMModelCapabilityCode            = "code"
	LLMModelCapabilityImageGeneration = "image_generation"
	LLMModelCapabilityVideoGeneration = "video_generation"
	LLMModelCapabilityEmbedding       = "embedding"

	LLMEmbeddingProtocolOpenAI = "openai_embeddings"
	LLMEmbeddingProtocolOllama = "ollama_embeddings"

	OpenRouterNemotronEmbedFreeModelID = "nvidia/nemotron-3-embed-1b:free"
	OpenRouterVLLEmbedFreeModelID      = "nvidia/llama-nemotron-embed-vl-1b-v2:free"

	maxEmbeddingDimension          = 65536
	providerInstanceIDPrefix       = "pvd_v1_"
	legacyProviderInstanceIDPrefix = "pvd_legacy_v1_"
)

var validLLMModelCapabilities = map[string]struct{}{
	LLMModelCapabilityText:            {},
	LLMModelCapabilityVision:          {},
	LLMModelCapabilityVideo:           {},
	LLMModelCapabilityAudio:           {},
	LLMModelCapabilityCode:            {},
	LLMModelCapabilityImageGeneration: {},
	LLMModelCapabilityVideoGeneration: {},
	LLMModelCapabilityEmbedding:       {},
}

// LLMEmbeddingModelSpec is the immutable vector contract declared by one
// provider model. Runtime availability and credentials are deliberately not
// part of this persisted contract.
type LLMEmbeddingModelSpec struct {
	Protocol      string `yaml:"protocol" json:"protocol"`
	Dimension     int    `yaml:"dimension" json:"dimension"`
	Normalization string `yaml:"normalization,omitempty" json:"normalization,omitempty"`
}

// LLMProviderModelSpec declares capabilities for one exact model ID.
// Capabilities intentionally has no omitempty tag: explicit [] means
// unclassified and must survive YAML/JSON round-trips.
type LLMProviderModelSpec struct {
	ID           string                 `yaml:"id" json:"id"`
	DisplayName  string                 `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	IsCustom     bool                   `yaml:"is_custom,omitempty" json:"is_custom,omitempty"`
	Capabilities []string               `yaml:"capabilities" json:"capabilities"`
	Embedding    *LLMEmbeddingModelSpec `yaml:"embedding,omitempty" json:"embedding,omitempty"`
}

// NewProviderInstanceID creates a stable opaque provider identity. It contains
// no endpoint, credential, model, map key, or editable display-name material.
func NewProviderInstanceID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("生成 provider_instance_id: %w", err)
	}
	return providerInstanceIDPrefix + hex.EncodeToString(random), nil
}

// ValidateProviderInstanceID accepts only the canonical server-generated
// representation so provider identity cannot be confused with a credential or
// an editable provider map key.
func ValidateProviderInstanceID(id string) error {
	prefix := providerInstanceIDPrefix
	hexLength := 32
	if strings.HasPrefix(id, legacyProviderInstanceIDPrefix) {
		prefix = legacyProviderInstanceIDPrefix
		hexLength = 64
	}
	if len(id) != len(prefix)+hexLength || !strings.HasPrefix(id, prefix) {
		return fmt.Errorf("provider_instance_id 必须为 canonical v1 格式")
	}
	hexPart := strings.TrimPrefix(id, prefix)
	if hexPart != strings.ToLower(hexPart) {
		return fmt.Errorf("provider_instance_id 必须使用小写十六进制")
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return fmt.Errorf("provider_instance_id 非法: %w", err)
	}
	return nil
}

// EffectiveProviderInstanceID returns the persisted identity when available.
// Legacy configs receive a deterministic, versioned SHA-256 fallback derived
// only from the normalized provider map key. GET and runtime must use this same
// helper until the next PUT persists the value.
func EffectiveProviderInstanceID(providerKey string, provider LLMProviderConfig) string {
	if provider.ProviderInstanceID != "" {
		return provider.ProviderInstanceID
	}
	normalizedKey := strings.ToLower(strings.TrimSpace(providerKey))
	digest := sha256.Sum256([]byte(normalizedKey))
	return legacyProviderInstanceIDPrefix + hex.EncodeToString(digest[:])
}

// MigrateOpenRouterEmbeddingModelSpec returns an embedding-only declaration
// for the two approved exact OpenRouter IDs. It intentionally performs no
// substring, prefix, suffix, or provider-name inference.
func MigrateOpenRouterEmbeddingModelSpec(modelID string) (LLMProviderModelSpec, bool) {
	switch modelID {
	case OpenRouterNemotronEmbedFreeModelID, OpenRouterVLLEmbedFreeModelID:
		return LLMProviderModelSpec{
			ID:           modelID,
			DisplayName:  modelID,
			Capabilities: []string{LLMModelCapabilityEmbedding},
			Embedding: &LLMEmbeddingModelSpec{
				Protocol:      LLMEmbeddingProtocolOpenAI,
				Dimension:     2048,
				Normalization: "l2",
			},
		}, true
	default:
		return LLMProviderModelSpec{}, false
	}
}

// NormalizeProviderModelSpecs returns the effective, non-secret capability
// catalog for a provider. Legacy providers synthesize text specs, with only
// the exact approved OpenRouter IDs migrated to embedding-only. Explicit mode
// never fills absent model rows; within an existing row, nil capabilities is
// the legacy form while a non-nil empty slice remains explicitly empty.
func NormalizeProviderModelSpecs(provider LLMProviderConfig) (string, []LLMProviderModelSpec) {
	mode := providerModelSpecsMode(provider)
	if mode == LLMModelSpecsModeExplicit {
		specs := make([]LLMProviderModelSpec, len(provider.ModelSpecs))
		for i, spec := range provider.ModelSpecs {
			// These two exact catalog IDs are intrinsic embedding-only models.
			// Canonicalize even an explicit stale/malicious text declaration so
			// old clients cannot route them to completion or tool probes. Keep
			// presentation/ownership metadata because it does not affect routing.
			if catalogSpec, ok := MigrateOpenRouterEmbeddingModelSpec(spec.ID); ok {
				if spec.DisplayName != "" {
					catalogSpec.DisplayName = spec.DisplayName
				}
				catalogSpec.IsCustom = spec.IsCustom
				specs[i] = catalogSpec
				continue
			}
			specs[i] = cloneProviderModelSpec(spec)
			if spec.Capabilities == nil {
				specs[i].Capabilities = []string{LLMModelCapabilityText}
			}
		}
		return mode, specs
	}

	modelIDs := make([]string, 0, len(provider.Models)+1)
	seen := make(map[string]struct{}, len(provider.Models)+1)
	for _, modelID := range provider.Models {
		if _, exists := seen[modelID]; exists {
			continue
		}
		seen[modelID] = struct{}{}
		modelIDs = append(modelIDs, modelID)
	}
	if provider.Model != "" {
		if _, exists := seen[provider.Model]; !exists {
			modelIDs = append(modelIDs, provider.Model)
		}
	}

	specs := make([]LLMProviderModelSpec, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		if migrated, ok := MigrateOpenRouterEmbeddingModelSpec(modelID); ok {
			specs = append(specs, migrated)
			continue
		}
		specs = append(specs, LLMProviderModelSpec{
			ID:           modelID,
			DisplayName:  modelID,
			Capabilities: []string{LLMModelCapabilityText},
		})
	}
	return mode, specs
}

// ModelHasCapability checks only normalized explicit metadata. It never
// infers from the provider name or arbitrary model-ID substrings.
func ModelHasCapability(provider LLMProviderConfig, modelID, capability string) bool {
	return ModelHasCapabilities(provider, modelID, capability)
}

// ModelHasCapabilities checks one exact model against all required normalized
// capability declarations. Provider names and model-ID substrings are never
// treated as capability evidence.
func ModelHasCapabilities(provider LLMProviderConfig, modelID string, required ...string) bool {
	if strings.TrimSpace(modelID) == "" || len(required) == 0 {
		return false
	}
	_, specs := NormalizeProviderModelSpecs(provider)
	for _, spec := range specs {
		if spec.ID != modelID {
			continue
		}
		return modelSpecHasCapabilities(spec, required)
	}
	return false
}

// PreferredModelWithCapabilities selects within one provider only. A capable
// current model wins; otherwise the normalized model-spec catalog order is the
// stable fallback. Legacy configurations synthesize text only, so media
// capabilities remain fail-closed until explicitly declared.
func PreferredModelWithCapabilities(provider LLMProviderConfig, required ...string) (string, bool) {
	current := strings.TrimSpace(provider.Model)
	_, specs := NormalizeProviderModelSpecs(provider)
	if current != "" {
		for _, spec := range specs {
			if spec.ID == current && modelSpecHasCapabilities(spec, required) {
				return current, true
			}
		}
	}
	for _, spec := range specs {
		if spec.ID == current {
			continue
		}
		if modelSpecHasCapabilities(spec, required) {
			return spec.ID, true
		}
	}
	return "", false
}

func modelSpecHasCapabilities(spec LLMProviderModelSpec, required []string) bool {
	if len(required) == 0 {
		return false
	}
	for _, capability := range required {
		found := false
		for _, candidate := range spec.Capabilities {
			if candidate == capability {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ValidateProviderModelSpecs rejects ambiguous or internally inconsistent
// capability declarations before config persistence or runtime hot reload.
func ValidateProviderModelSpecs(provider LLMProviderConfig) error {
	switch provider.ModelSpecsMode {
	case "", LLMModelSpecsModeLegacy, LLMModelSpecsModeExplicit:
	default:
		return fmt.Errorf("model_specs_mode %q 非法：应为 %q 或 %q", provider.ModelSpecsMode, LLMModelSpecsModeLegacy, LLMModelSpecsModeExplicit)
	}

	modelIDs := make(map[string]struct{}, len(provider.Models))
	for _, modelID := range provider.Models {
		if strings.TrimSpace(modelID) == "" {
			return fmt.Errorf("models 包含空 model id")
		}
		modelIDs[modelID] = struct{}{}
	}

	mode, specs := NormalizeProviderModelSpecs(provider)
	seenSpecs := make(map[string]struct{}, len(specs))
	for i, spec := range specs {
		if strings.TrimSpace(spec.ID) == "" || spec.ID != strings.TrimSpace(spec.ID) {
			return fmt.Errorf("model_specs[%d].id 必须是非空且无首尾空白的精确 model id", i)
		}
		if _, duplicate := seenSpecs[spec.ID]; duplicate {
			return fmt.Errorf("model_specs[%d].id=%q 重复", i, spec.ID)
		}
		seenSpecs[spec.ID] = struct{}{}
		if mode == LLMModelSpecsModeExplicit {
			if _, exists := modelIDs[spec.ID]; !exists {
				return fmt.Errorf("model_specs[%d].id=%q 不在 models 中", i, spec.ID)
			}
		}

		seenCapabilities := make(map[string]struct{}, len(spec.Capabilities))
		for j, capability := range spec.Capabilities {
			if _, valid := validLLMModelCapabilities[capability]; !valid {
				return fmt.Errorf("model_specs[%d].capabilities[%d]=%q 非法", i, j, capability)
			}
			if _, duplicate := seenCapabilities[capability]; duplicate {
				return fmt.Errorf("model_specs[%d].capabilities 包含重复值 %q", i, capability)
			}
			seenCapabilities[capability] = struct{}{}
		}

		_, hasEmbedding := seenCapabilities[LLMModelCapabilityEmbedding]
		if !hasEmbedding && spec.Embedding != nil {
			return fmt.Errorf("model_specs[%d].embedding 仅允许在 capabilities 包含 embedding 时声明", i)
		}
		if hasEmbedding && spec.Embedding != nil {
			if spec.Embedding.Dimension < 0 || spec.Embedding.Dimension > maxEmbeddingDimension {
				return fmt.Errorf("model_specs[%d].embedding.dimension=%d 非法：应为 0-%d，0 表示等待精确目录或预检解析", i, spec.Embedding.Dimension, maxEmbeddingDimension)
			}
			switch spec.Embedding.Protocol {
			case LLMEmbeddingProtocolOpenAI, LLMEmbeddingProtocolOllama:
			default:
				return fmt.Errorf("model_specs[%d].embedding.protocol=%q 非法", i, spec.Embedding.Protocol)
			}
			switch spec.Embedding.Normalization {
			case "", "l2", "none":
			default:
				return fmt.Errorf("model_specs[%d].embedding.normalization=%q 非法", i, spec.Embedding.Normalization)
			}
		}
	}

	if provider.Model != "" && !ModelHasCapability(provider, provider.Model, LLMModelCapabilityText) {
		return fmt.Errorf("model %q 必须指向包含 text capability 的模型", provider.Model)
	}
	return nil
}

func providerModelSpecsMode(provider LLMProviderConfig) string {
	if provider.ModelSpecsMode == LLMModelSpecsModeExplicit {
		return LLMModelSpecsModeExplicit
	}
	if provider.ModelSpecsMode == "" && provider.ModelSpecs != nil {
		return LLMModelSpecsModeExplicit
	}
	return LLMModelSpecsModeLegacy
}

func cloneProviderModelSpec(spec LLMProviderModelSpec) LLMProviderModelSpec {
	copy := spec
	if spec.Capabilities != nil {
		copy.Capabilities = append([]string(nil), spec.Capabilities...)
		if len(spec.Capabilities) == 0 {
			copy.Capabilities = make([]string, 0)
		}
	}
	if spec.Embedding != nil {
		embedding := *spec.Embedding
		copy.Embedding = &embedding
	}
	return copy
}
