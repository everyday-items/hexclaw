package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	testOpenRouterNemotronEmbedID = "nvidia/nemotron-3-embed-1b:free"
	testOpenRouterVLLEmbedID      = "nvidia/llama-nemotron-embed-vl-1b-v2:free"
)

func TestNormalizeProviderModelSpecs_LegacyUsesOnlyExactEmbeddingMigration(t *testing.T) {
	provider := LLMProviderConfig{
		Models: []string{
			"gpt-4o-mini",
			testOpenRouterNemotronEmbedID,
			testOpenRouterVLLEmbedID,
			"acme-embed-model",
		},
	}

	mode, specs := NormalizeProviderModelSpecs(provider)
	if mode != LLMModelSpecsModeLegacy {
		t.Fatalf("mode=%q, want %q", mode, LLMModelSpecsModeLegacy)
	}
	if len(specs) != 4 {
		t.Fatalf("len(specs)=%d, want 4: %+v", len(specs), specs)
	}

	for _, id := range []string{testOpenRouterNemotronEmbedID, testOpenRouterVLLEmbedID} {
		spec := findProviderModelSpec(t, specs, id)
		if !modelSpecHasCapability(spec, LLMModelCapabilityEmbedding) {
			t.Fatalf("exact OpenRouter model %q capabilities=%v, want embedding", id, spec.Capabilities)
		}
		if modelSpecHasCapability(spec, LLMModelCapabilityText) {
			t.Fatalf("exact OpenRouter model %q must be embedding-only: %v", id, spec.Capabilities)
		}
		if spec.Embedding == nil || spec.Embedding.Dimension != 2048 || spec.Embedding.Protocol != LLMEmbeddingProtocolOpenAI {
			t.Fatalf("exact OpenRouter model %q embedding=%+v, want OpenAI/2048", id, spec.Embedding)
		}
	}

	for _, id := range []string{"gpt-4o-mini", "acme-embed-model"} {
		spec := findProviderModelSpec(t, specs, id)
		if !modelSpecHasCapability(spec, LLMModelCapabilityText) || modelSpecHasCapability(spec, LLMModelCapabilityEmbedding) {
			t.Fatalf("non-exact legacy model %q capabilities=%v, want text-only", id, spec.Capabilities)
		}
	}
}

func TestNormalizeProviderModelSpecs_ExactOpenRouterCatalogCannotBeReclassifiedAsChat(t *testing.T) {
	provider := LLMProviderConfig{
		Models:         []string{testOpenRouterNemotronEmbedID, testOpenRouterVLLEmbedID, "acme/embed:free"},
		ModelSpecsMode: LLMModelSpecsModeExplicit,
		ModelSpecs: []LLMProviderModelSpec{
			{ID: testOpenRouterNemotronEmbedID, Capabilities: []string{LLMModelCapabilityText}},
			{ID: testOpenRouterVLLEmbedID, Capabilities: []string{LLMModelCapabilityText, LLMModelCapabilityEmbedding}},
			{ID: "acme/embed:free", Capabilities: []string{LLMModelCapabilityText}},
		},
	}

	mode, specs := NormalizeProviderModelSpecs(provider)
	if mode != LLMModelSpecsModeExplicit {
		t.Fatalf("mode=%q, want explicit", mode)
	}
	for _, modelID := range []string{testOpenRouterNemotronEmbedID, testOpenRouterVLLEmbedID} {
		spec := findProviderModelSpec(t, specs, modelID)
		if len(spec.Capabilities) != 1 || spec.Capabilities[0] != LLMModelCapabilityEmbedding {
			t.Fatalf("exact catalog model %q capabilities=%v, want canonical embedding-only", modelID, spec.Capabilities)
		}
		if spec.Embedding == nil || spec.Embedding.Dimension != 2048 ||
			spec.Embedding.Protocol != LLMEmbeddingProtocolOpenAI || spec.Embedding.Normalization != "l2" {
			t.Fatalf("exact catalog model %q contract=%+v, want canonical OpenAI/2048/l2", modelID, spec.Embedding)
		}
	}
	unknown := findProviderModelSpec(t, specs, "acme/embed:free")
	if len(unknown.Capabilities) != 1 || unknown.Capabilities[0] != LLMModelCapabilityText {
		t.Fatalf("unknown similar ID was inferred: %+v", unknown)
	}
}

func TestEffectiveProviderInstanceID_LegacyIsDeterministicAndNonSecret(t *testing.T) {
	provider := LLMProviderConfig{APIKey: "sk-never-in-identity", BaseURL: "https://example.test/v1"}
	first := EffectiveProviderInstanceID(" Custom Provider ", provider)
	second := EffectiveProviderInstanceID("custom provider", LLMProviderConfig{APIKey: "rotated", BaseURL: "https://changed.example.test/v1"})
	if first != second {
		t.Fatalf("legacy effective ID drifted across key/endpoint rotation: %q != %q", first, second)
	}
	if strings.Contains(first, "never") || strings.Contains(first, "example") {
		t.Fatalf("effective ID leaked config material: %q", first)
	}
	if err := ValidateProviderInstanceID(first); err != nil {
		t.Fatalf("effective legacy ID %q invalid: %v", first, err)
	}
	if changedName := EffectiveProviderInstanceID("renamed", provider); changedName == first {
		t.Fatalf("different unmigrated provider keys unexpectedly share %q", first)
	}

	const persisted = "pvd_v1_00112233445566778899aabbccddeeff"
	if got := EffectiveProviderInstanceID("any-name", LLMProviderConfig{ProviderInstanceID: persisted}); got != persisted {
		t.Fatalf("persisted ID lost: got %q want %q", got, persisted)
	}
}

func TestNormalizeProviderModelSpecs_PreservesExplicitEmptyAndCapabilityPresence(t *testing.T) {
	t.Run("provider explicit empty", func(t *testing.T) {
		provider := LLMProviderConfig{
			Models:         []string{"gpt-4o-mini"},
			ModelSpecsMode: LLMModelSpecsModeExplicit,
			ModelSpecs:     []LLMProviderModelSpec{},
		}

		mode, specs := NormalizeProviderModelSpecs(provider)
		if mode != LLMModelSpecsModeExplicit || len(specs) != 0 {
			t.Fatalf("mode=%q specs=%+v, want explicit empty", mode, specs)
		}
		if ModelHasCapability(provider, "gpt-4o-mini", LLMModelCapabilityText) {
			t.Fatal("explicit empty provider must not regain legacy text capability")
		}
	})

	t.Run("missing capabilities is legacy text but explicit empty remains empty", func(t *testing.T) {
		provider := LLMProviderConfig{
			Models:         []string{"missing", "empty"},
			ModelSpecsMode: LLMModelSpecsModeExplicit,
			ModelSpecs: []LLMProviderModelSpec{
				{ID: "missing", Capabilities: nil},
				{ID: "empty", Capabilities: []string{}},
			},
		}

		_, specs := NormalizeProviderModelSpecs(provider)
		if got := findProviderModelSpec(t, specs, "missing").Capabilities; len(got) != 1 || got[0] != LLMModelCapabilityText {
			t.Fatalf("missing capabilities normalized to %v, want [text]", got)
		}
		if got := findProviderModelSpec(t, specs, "empty").Capabilities; got == nil || len(got) != 0 {
			t.Fatalf("explicit empty capabilities normalized to %#v, want non-nil empty", got)
		}
	})
}

func TestProviderModelSpecs_YAMLRoundTripPreservesExplicitEmpty(t *testing.T) {
	providers := []LLMProviderConfig{
		{
			Models:         []string{"unclassified"},
			ModelSpecsMode: LLMModelSpecsModeExplicit,
			ModelSpecs: []LLMProviderModelSpec{{
				ID: "unclassified", Capabilities: []string{},
			}},
		},
		{
			Models:         []string{"unclassified"},
			ModelSpecsMode: LLMModelSpecsModeExplicit,
			ModelSpecs:     []LLMProviderModelSpec{},
		},
	}

	for i, provider := range providers {
		encoded, err := yaml.Marshal(provider)
		if err != nil {
			t.Fatalf("case %d marshal: %v", i, err)
		}
		var decoded LLMProviderConfig
		if err := yaml.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("case %d unmarshal: %v", i, err)
		}
		mode, specs := NormalizeProviderModelSpecs(decoded)
		if mode != LLMModelSpecsModeExplicit {
			t.Fatalf("case %d mode=%q after round-trip", i, mode)
		}
		if i == 0 {
			if len(specs) != 1 || specs[0].Capabilities == nil || len(specs[0].Capabilities) != 0 {
				t.Fatalf("nested explicit [] lost after round-trip: %+v", specs)
			}
		} else if specs == nil || len(specs) != 0 {
			t.Fatalf("provider explicit [] lost after round-trip: %#v", specs)
		}
	}
}

func TestPreferredModelWithCapabilitiesUsesCurrentThenStableCatalogOrder(t *testing.T) {
	provider := LLMProviderConfig{
		Model:          "text-default",
		Models:         []string{"text-default", "vision-first", "vision-second"},
		ModelSpecsMode: LLMModelSpecsModeExplicit,
		ModelSpecs: []LLMProviderModelSpec{
			{ID: "text-default", Capabilities: []string{LLMModelCapabilityText}},
			{ID: "vision-first", Capabilities: []string{LLMModelCapabilityText, LLMModelCapabilityVision}},
			{ID: "vision-second", Capabilities: []string{LLMModelCapabilityText, LLMModelCapabilityVision}},
		},
	}

	if ModelHasCapabilities(provider, "text-default", LLMModelCapabilityText, LLMModelCapabilityVision) {
		t.Fatal("text-only default unexpectedly satisfies text+vision")
	}
	model, ok := PreferredModelWithCapabilities(
		provider,
		LLMModelCapabilityText,
		LLMModelCapabilityVision,
	)
	if !ok || model != "vision-first" {
		t.Fatalf("stable same-provider vision selection=(%q,%v), want (vision-first,true)", model, ok)
	}

	provider.Model = "vision-second"
	model, ok = PreferredModelWithCapabilities(
		provider,
		LLMModelCapabilityText,
		LLMModelCapabilityVision,
	)
	if !ok || model != "vision-second" {
		t.Fatalf("capable current model must win, got (%q,%v)", model, ok)
	}
}

func TestPreferredModelWithCapabilitiesDoesNotInferVisionFromModelName(t *testing.T) {
	provider := LLMProviderConfig{
		Model:  "gpt-super-vision-vl",
		Models: []string{"gpt-super-vision-vl"},
	}
	if model, ok := PreferredModelWithCapabilities(
		provider,
		LLMModelCapabilityText,
		LLMModelCapabilityVision,
	); ok || model != "" {
		t.Fatalf("legacy/model-name heuristic selected undeclared vision model (%q,%v)", model, ok)
	}
}

func TestValidateProviderModelSpecs_RejectsInvalidContractsAndNonTextSelection(t *testing.T) {
	validEmbedding := LLMProviderModelSpec{
		ID:           testOpenRouterNemotronEmbedID,
		Capabilities: []string{LLMModelCapabilityEmbedding},
		Embedding: &LLMEmbeddingModelSpec{
			Protocol:  LLMEmbeddingProtocolOpenAI,
			Dimension: 2048,
		},
	}

	tests := []struct {
		name     string
		provider LLMProviderConfig
		wantPart string
	}{
		{
			name: "duplicate id",
			provider: LLMProviderConfig{Models: []string{"m"}, ModelSpecsMode: LLMModelSpecsModeExplicit, ModelSpecs: []LLMProviderModelSpec{
				{ID: "m", Capabilities: []string{LLMModelCapabilityText}},
				{ID: "m", Capabilities: []string{LLMModelCapabilityText}},
			}},
			wantPart: "重复",
		},
		{
			name: "spec not in models",
			provider: LLMProviderConfig{Models: []string{"m"}, ModelSpecsMode: LLMModelSpecsModeExplicit, ModelSpecs: []LLMProviderModelSpec{
				{ID: "other", Capabilities: []string{LLMModelCapabilityText}},
			}},
			wantPart: "models",
		},
		{
			name: "unknown capability",
			provider: LLMProviderConfig{Models: []string{"m"}, ModelSpecsMode: LLMModelSpecsModeExplicit, ModelSpecs: []LLMProviderModelSpec{
				{ID: "m", Capabilities: []string{"embed"}},
			}},
			wantPart: "capabilities",
		},
		{
			name: "embedding dimension negative",
			provider: LLMProviderConfig{Models: []string{"m"}, ModelSpecsMode: LLMModelSpecsModeExplicit, ModelSpecs: []LLMProviderModelSpec{
				{ID: "m", Capabilities: []string{LLMModelCapabilityEmbedding}, Embedding: &LLMEmbeddingModelSpec{Protocol: LLMEmbeddingProtocolOpenAI, Dimension: -1}},
			}},
			wantPart: "dimension",
		},
		{
			name: "embedding contract without capability",
			provider: LLMProviderConfig{Models: []string{"m"}, ModelSpecsMode: LLMModelSpecsModeExplicit, ModelSpecs: []LLMProviderModelSpec{
				{ID: "m", Capabilities: []string{LLMModelCapabilityText}, Embedding: &LLMEmbeddingModelSpec{Protocol: LLMEmbeddingProtocolOpenAI, Dimension: 2048}},
			}},
			wantPart: "embedding",
		},
		{
			name: "selected model is embedding only",
			provider: LLMProviderConfig{
				Model: testOpenRouterNemotronEmbedID, Models: []string{testOpenRouterNemotronEmbedID},
				ModelSpecsMode: LLMModelSpecsModeExplicit, ModelSpecs: []LLMProviderModelSpec{validEmbedding},
			},
			wantPart: "text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateProviderModelSpecs(tt.provider)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantPart)) {
				t.Fatalf("ValidateProviderModelSpecs() error=%v, want containing %q", err, tt.wantPart)
			}
		})
	}

	if err := ValidateProviderModelSpecs(LLMProviderConfig{
		Models:         []string{"chat", testOpenRouterNemotronEmbedID},
		Model:          "chat",
		ModelSpecsMode: LLMModelSpecsModeExplicit,
		ModelSpecs: []LLMProviderModelSpec{
			{ID: "chat", Capabilities: []string{LLMModelCapabilityText}},
			validEmbedding,
		},
	}); err != nil {
		t.Fatalf("valid provider rejected: %v", err)
	}

	if err := ValidateProviderModelSpecs(LLMProviderConfig{Model: "legacy-chat"}); err != nil {
		t.Fatalf("legacy selected model must remain text-capable: %v", err)
	}

	for _, embedding := range []*LLMEmbeddingModelSpec{
		nil,
		{Protocol: LLMEmbeddingProtocolOpenAI, Dimension: 0},
	} {
		provider := LLMProviderConfig{
			Models:         []string{"probe-resolved"},
			ModelSpecsMode: LLMModelSpecsModeExplicit,
			ModelSpecs: []LLMProviderModelSpec{{
				ID: "probe-resolved", Capabilities: []string{LLMModelCapabilityEmbedding}, Embedding: embedding,
			}},
		}
		if err := ValidateProviderModelSpecs(provider); err != nil {
			t.Fatalf("embedding contract %+v should allow exact-catalog/preflight dimension resolution: %v", embedding, err)
		}
	}
}

func findProviderModelSpec(t *testing.T, specs []LLMProviderModelSpec, id string) LLMProviderModelSpec {
	t.Helper()
	for _, spec := range specs {
		if spec.ID == id {
			return spec
		}
	}
	t.Fatalf("model spec %q not found in %+v", id, specs)
	return LLMProviderModelSpec{}
}

func modelSpecHasCapability(spec LLMProviderModelSpec, capability string) bool {
	for _, candidate := range spec.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}
