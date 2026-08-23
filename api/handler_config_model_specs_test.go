package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

const apiTestOpenRouterEmbedID = "nvidia/nemotron-3-embed-1b:free"

func TestHandleGetLLMConfig_ReturnsNormalizedModelSpecsAndMasksKey(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openrouter": {
			APIKey: "sk-openrouter-secret", Model: "gpt-4o-mini",
			Models: []string{"gpt-4o-mini", apiTestOpenRouterEmbedID},
		},
	}
	srv := NewServer(cfg, &mockEngine{}, nil, nil)
	w := httptest.NewRecorder()

	srv.handleGetLLMConfig(w, httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var response struct {
		Providers map[string]struct {
			ProviderInstanceID string                        `json:"provider_instance_id"`
			APIKey             string                        `json:"api_key"`
			Models             []string                      `json:"models"`
			ModelSpecs         []config.LLMProviderModelSpec `json:"model_specs"`
			ModelSpecsMode     string                        `json:"model_specs_mode"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	provider := response.Providers["openrouter"]
	wantProviderID := config.EffectiveProviderInstanceID("openrouter", cfg.LLM.Providers["openrouter"])
	if provider.ProviderInstanceID != wantProviderID {
		t.Fatalf("provider_instance_id=%q, want shared legacy helper result %q", provider.ProviderInstanceID, wantProviderID)
	}
	if provider.APIKey != config.MaskAPIKey("sk-openrouter-secret") || strings.Contains(w.Body.String(), "sk-openrouter-secret") {
		t.Fatalf("API key masking drifted: %s", w.Body.String())
	}
	if len(provider.Models) != 2 || provider.Models[0] != "gpt-4o-mini" || provider.Models[1] != apiTestOpenRouterEmbedID {
		t.Fatalf("legacy models=%v, want original order", provider.Models)
	}
	if provider.ModelSpecsMode != config.LLMModelSpecsModeLegacy || len(provider.ModelSpecs) != 2 {
		t.Fatalf("mode=%q specs=%+v, want normalized legacy specs", provider.ModelSpecsMode, provider.ModelSpecs)
	}
	if !config.ModelHasCapability(cfg.LLM.Providers["openrouter"], apiTestOpenRouterEmbedID, config.LLMModelCapabilityEmbedding) {
		t.Fatal("exact OpenRouter model must normalize to embedding capability")
	}
}

func TestHandleUpdateLLMConfig_ProviderInstanceIDPreservedOrGenerated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const existingID = "pvd_v1_00112233445566778899aabbccddeeff"
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"existing": {
			ProviderInstanceID: existingID,
			APIKey:             "sk-existing",
			BaseURL:            "https://old.example.test/v1",
			Model:              "chat",
			Models:             []string{"chat"},
		},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	body := `{"providers":{` +
		`"existing":{"api_key":"****ting","base_url":"https://new.example.test/v1","model":"chat","models":["chat"]},` +
		`"new-provider":{"api_key":"sk-new","base_url":"https://new-provider.example.test/v1","model":"chat","models":["chat"]}` +
		`}}`
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := srv.cfg.LLM.Providers["existing"].ProviderInstanceID; got != existingID {
		t.Fatalf("existing provider_instance_id changed to %q", got)
	}
	generated := srv.cfg.LLM.Providers["new-provider"].ProviderInstanceID
	if err := config.ValidateProviderInstanceID(generated); err != nil {
		t.Fatalf("generated provider_instance_id=%q invalid: %v", generated, err)
	}
	if generated == existingID {
		t.Fatalf("new provider reused existing ID %q", generated)
	}
}

func TestHandleUpdateLLMConfig_RejectsProviderInstanceIDMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			ProviderInstanceID: "pvd_v1_00112233445566778899aabbccddeeff",
			APIKey:             "sk-old",
			Model:              "chat",
			Models:             []string{"chat"},
		},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	body := `{"providers":{"custom":{"provider_instance_id":"pvd_v1_ffeeddccbbaa99887766554433221100","api_key":"****-old","model":"chat","models":["chat"]}}}`
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "provider_instance_id") {
		t.Fatalf("status=%d body=%s, want immutable provider_instance_id error", w.Code, w.Body.String())
	}
	if got := srv.cfg.LLM.Providers["custom"].ProviderInstanceID; got != "pvd_v1_00112233445566778899aabbccddeeff" {
		t.Fatalf("invalid mutation changed provider_instance_id to %q", got)
	}
}

func TestHandleUpdateLLMConfig_ModelSpecsOmittedPreservesModeSpecsAndMaskedKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	enabled := true
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "custom"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			APIKey: "sk-original-secret", Model: "chat", Models: []string{"chat", apiTestOpenRouterEmbedID, "removed"}, Enabled: &enabled,
			ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{
				{ID: "chat", DisplayName: "Chat", Capabilities: []string{config.LLMModelCapabilityText}},
				{ID: apiTestOpenRouterEmbedID, DisplayName: "Embed", Capabilities: []string{config.LLMModelCapabilityEmbedding}, Embedding: &config.LLMEmbeddingModelSpec{Protocol: config.LLMEmbeddingProtocolOpenAI, Dimension: 2048}},
				{ID: "removed", DisplayName: "Removed", Capabilities: []string{config.LLMModelCapabilityText}},
			},
		},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	body := `{"default":"custom","providers":{"custom":{"api_key":"` + config.MaskAPIKey("sk-original-secret") + `","base_url":"https://openrouter.ai/api/v1","model":"chat","models":["chat","` + apiTestOpenRouterEmbedID + `","new-chat"],"compatible":"openai","locality":"cloud","enabled":true}}}`
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got := srv.cfg.LLM.Providers["custom"]
	if got.APIKey != "sk-original-secret" {
		t.Fatalf("masked key overwrote secret: %q", got.APIKey)
	}
	if got.ModelSpecsMode != config.LLMModelSpecsModeExplicit || len(got.ModelSpecs) != 3 {
		t.Fatalf("mode=%q specs=%+v, want preserved explicit specs plus new legacy text model", got.ModelSpecsMode, got.ModelSpecs)
	}
	if !config.ModelHasCapability(got, "new-chat", config.LLMModelCapabilityText) {
		t.Fatalf("legacy omitted model_specs did not synthesize text for new model: %+v", got.ModelSpecs)
	}
	for _, spec := range got.ModelSpecs {
		if spec.ID == "removed" {
			t.Fatalf("removed model spec was not pruned: %+v", got.ModelSpecs)
		}
	}
}

func TestHandleUpdateLLMConfig_RejectsEmbeddingOnlyDefaultProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	body := `{"default":"embed-only","providers":{"embed-only":{"api_key":"sk-test","model":"","models":["vector-model"],"model_specs":[{"id":"vector-model","display_name":"Vector","capabilities":["embedding"]}]}}}`
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest || !strings.Contains(strings.ToLower(w.Body.String()), "default") {
		t.Fatalf("status=%d body=%s, want embedding-only default rejection", w.Code, w.Body.String())
	}
}

func TestHandleTestLLMConfig_RejectsEmbeddingOnlyBeforeCompletionProbe(t *testing.T) {
	oldFactory := llmTestProviderFactory
	probeCalls := 0
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider {
		probeCalls++
		return &mockCompletionProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	tests := []struct {
		name  string
		cfg   *config.Config
		model string
	}{
		{
			name:  "exact OpenRouter migration model",
			cfg:   config.DefaultConfig(),
			model: apiTestOpenRouterEmbedID,
		},
		{
			name: "configured custom embedding-only model",
			cfg: func() *config.Config {
				cfg := config.DefaultConfig()
				cfg.LLM.Providers = map[string]config.LLMProviderConfig{
					"custom": {
						Models: []string{"vector-model"}, ModelSpecsMode: config.LLMModelSpecsModeExplicit,
						ModelSpecs: []config.LLMProviderModelSpec{{ID: "vector-model", Capabilities: []string{config.LLMModelCapabilityEmbedding}}},
					},
				}
				return cfg
			}(),
			model: "vector-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServer(tt.cfg, &mockEngine{}, nil, nil)
			body := `{"provider":{"type":"custom","api_key":"sk-test","model":"` + tt.model + `"}}`
			w := httptest.NewRecorder()
			srv.handleTestLLMConfig(w, httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/test", strings.NewReader(body)))
			if w.Code != http.StatusBadRequest || !strings.Contains(strings.ToLower(w.Body.String()), "embedding") {
				t.Fatalf("status=%d body=%s, want embedding-only rejection", w.Code, w.Body.String())
			}
		})
	}
	if probeCalls != 0 {
		t.Fatalf("embedding-only models reached completion probe %d times", probeCalls)
	}
}

func TestHandleTestLLMConfig_AllowsSameModelIDOnMatchedTextProvider(t *testing.T) {
	oldFactory := llmTestProviderFactory
	probeCalls := 0
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider {
		probeCalls++
		return &mockCompletionProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	const sharedModelID = "shared-model"
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"embed-provider": {
			BaseURL: "https://embed.example.test/v1", Models: []string{sharedModelID},
			ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID: sharedModelID, Capabilities: []string{config.LLMModelCapabilityEmbedding},
			}},
		},
		"text-provider": {
			BaseURL: "https://text.example.test/v1", Model: sharedModelID, Models: []string{sharedModelID},
			ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID: sharedModelID, Capabilities: []string{config.LLMModelCapabilityText},
			}},
		},
	}
	srv := NewServer(cfg, &mockEngine{}, nil, nil)
	body := `{"provider":{"type":"custom","base_url":"https://text.example.test/v1/","api_key":"sk-test","model":"` + sharedModelID + `"}}`
	w := httptest.NewRecorder()

	srv.handleTestLLMConfig(w, httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/test", strings.NewReader(body)))
	if w.Code != http.StatusOK || probeCalls != 1 {
		t.Fatalf("status=%d calls=%d body=%s, want matched text provider completion probe", w.Code, probeCalls, w.Body.String())
	}
}

func TestHandleUpdateLLMConfig_ExplicitEmptyClearsCapabilities(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			APIKey: "sk-original-secret", Model: "chat", Models: []string{"chat"},
			ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs:     []config.LLMProviderModelSpec{{ID: "chat", Capabilities: []string{config.LLMModelCapabilityText}}},
		},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	body := `{"providers":{"custom":{"api_key":"` + config.MaskAPIKey("sk-original-secret") + `","model":"","models":["chat"],"model_specs":[]}}}`
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got := srv.cfg.LLM.Providers["custom"]
	if got.ModelSpecsMode != config.LLMModelSpecsModeExplicit || got.ModelSpecs == nil || len(got.ModelSpecs) != 0 {
		t.Fatalf("explicit [] lost: mode=%q specs=%#v", got.ModelSpecsMode, got.ModelSpecs)
	}
	if config.ModelHasCapability(got, "chat", config.LLMModelCapabilityText) {
		t.Fatal("explicit [] must not regain text capability")
	}
}

func TestHandleUpdateLLMConfig_RejectsEmbeddingOnlySelectedModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {APIKey: "sk-old", Model: "chat", Models: []string{"chat"}},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	body := `{"providers":{"custom":{"api_key":"****-old","model":"` + apiTestOpenRouterEmbedID + `","models":["` + apiTestOpenRouterEmbedID + `"],"model_specs":[{"id":"` + apiTestOpenRouterEmbedID + `","display_name":"Embed","capabilities":["embedding"],"embedding":{"protocol":"openai_embeddings","dimension":2048}}]}}}`
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest || !strings.Contains(strings.ToLower(w.Body.String()), "text") {
		t.Fatalf("status=%d body=%s, want 400 text-capability error", w.Code, w.Body.String())
	}
	if got := srv.cfg.LLM.Providers["custom"].Model; got != "chat" {
		t.Fatalf("invalid request mutated config model=%q", got)
	}
}

func TestBUG20260723_023_CustomModelProvenanceSurvivesConfigRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const providerID = "pvd_v1_00112233445566778899aabbccddeeff"
	const modelID = "team-custom-chat"
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "custom"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			ProviderInstanceID: providerID,
			APIKey:             "sk-round-trip-secret",
			BaseURL:            "https://models.example.test/v1",
			Model:              modelID,
			Models:             []string{modelID},
			ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID:           modelID,
				DisplayName:  "Team custom chat",
				IsCustom:     true,
				Capabilities: []string{config.LLMModelCapabilityText},
			}},
		},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)

	getBefore := httptest.NewRecorder()
	srv.handleGetLLMConfig(getBefore, httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil))
	if getBefore.Code != http.StatusOK {
		t.Fatalf("initial GET status=%d body=%s", getBefore.Code, getBefore.Body.String())
	}
	var before LLMConfigResponse
	if err := json.Unmarshal(getBefore.Body.Bytes(), &before); err != nil {
		t.Fatalf("decode initial GET: %v", err)
	}
	if len(before.Providers["custom"].ModelSpecs) != 1 || !before.Providers["custom"].ModelSpecs[0].IsCustom {
		t.Fatalf("initial custom provenance=%+v", before.Providers["custom"].ModelSpecs)
	}
	if strings.Contains(getBefore.Body.String(), "sk-round-trip-secret") {
		t.Fatalf("GET leaked API key: %s", getBefore.Body.String())
	}

	body, err := json.Marshal(before)
	if err != nil {
		t.Fatalf("encode GET response for round trip: %v", err)
	}
	put := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(put, httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(string(body))))
	if put.Code != http.StatusOK {
		t.Fatalf("round-trip PUT status=%d body=%s", put.Code, put.Body.String())
	}

	getAfter := httptest.NewRecorder()
	srv.handleGetLLMConfig(getAfter, httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil))
	if getAfter.Code != http.StatusOK {
		t.Fatalf("round-trip GET status=%d body=%s", getAfter.Code, getAfter.Body.String())
	}
	var after LLMConfigResponse
	if err := json.Unmarshal(getAfter.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode round-trip GET: %v", err)
	}
	modelSpecs := after.Providers["custom"].ModelSpecs
	if len(modelSpecs) != 1 || modelSpecs[0].ID != modelID || !modelSpecs[0].IsCustom {
		t.Fatalf("custom provenance lost after round trip: %+v", modelSpecs)
	}
}
