package llmrouter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/llm/ollama"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"gopkg.in/yaml.v3"
)

type reasoningCapabilityCaptureProvider struct {
	calls int
	req   llm.CompletionRequest
}

func (p *reasoningCapabilityCaptureProvider) Name() string { return "capture" }

func (p *reasoningCapabilityCaptureProvider) Complete(
	_ context.Context,
	req llm.CompletionRequest,
) (*llm.CompletionResponse, error) {
	p.calls++
	p.req = req
	return &llm.CompletionResponse{Content: "ok"}, nil
}

func (p *reasoningCapabilityCaptureProvider) Stream(
	context.Context,
	llm.CompletionRequest,
) (*llm.Stream, error) {
	return nil, errors.New("not implemented")
}

func (p *reasoningCapabilityCaptureProvider) Models() []llm.ModelInfo { return nil }

func (p *reasoningCapabilityCaptureProvider) CountTokens([]llm.Message) (int, error) {
	return 0, nil
}

func TestReasoningConfigSnapshotsDeepCloneMutableControlState(t *testing.T) {
	t.Run("cloneLLMConfig isolates its input", func(t *testing.T) {
		input := reasoningSnapshotIsolationConfig()
		cloned := cloneLLMConfig(input)

		mutateReasoningSnapshot(t, cloned, "clone")
		assertReasoningSnapshotState(t, input, "low", "enabled", "1024", "disabled", "0")

		mutateReasoningSnapshot(t, input, "input")
		assertReasoningSnapshotState(t, cloned, "clone", "clone", "clone", "clone", "clone")
	})

	t.Run("buildSelectorState isolates active config from its input", func(t *testing.T) {
		input := reasoningSnapshotIsolationConfig()
		_, active, _ := buildSelectorState(input)

		mutateReasoningSnapshot(t, active, "active")
		assertReasoningSnapshotState(t, input, "low", "enabled", "1024", "disabled", "0")

		mutateReasoningSnapshot(t, input, "input")
		assertReasoningSnapshotState(t, active, "active", "active", "active", "active", "active")
	})

	t.Run("ActiveConfig returns an isolated snapshot", func(t *testing.T) {
		input := reasoningSnapshotIsolationConfig()
		selector := NewWithProviders(input, map[string]hexagon.Provider{
			"reasoning-provider": &reasoningCapabilityCaptureProvider{},
		})

		first := selector.ActiveConfig()
		mutateReasoningSnapshot(t, first, "returned")
		assertReasoningSnapshotState(t, selector.ActiveConfig(), "low", "enabled", "1024", "disabled", "0")

		mutateReasoningSnapshot(t, input, "input")
		assertReasoningSnapshotState(t, selector.ActiveConfig(), "low", "enabled", "1024", "disabled", "0")
	})
}

func reasoningSnapshotIsolationConfig() config.LLMConfig {
	return config.LLMConfig{
		Default: "reasoning-provider",
		Providers: map[string]config.LLMProviderConfig{
			"reasoning-provider": {
				APIKey:         "fake-router-key",
				BaseURL:        "https://api.example.com/v1",
				Model:          "effort-model",
				Models:         []string{"effort-model", "nested-model"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{
						ID:               "effort-model",
						Capabilities:     []string{config.LLMModelCapabilityText},
						ReasoningSupport: config.LLMReasoningSupportSupported,
						ReasoningControl: &config.LLMReasoningControlSpec{
							Dialect:        config.LLMReasoningDialectEffort,
							On:             "high",
							Off:            "none",
							AllowedEfforts: []string{"low", "medium", "high"},
						},
					},
					{
						ID:               "nested-model",
						Capabilities:     []string{config.LLMModelCapabilityText},
						ReasoningSupport: config.LLMReasoningSupportSupported,
						ReasoningControl: &config.LLMReasoningControlSpec{
							Dialect: config.LLMReasoningDialectThinking,
							On: map[string]any{
								"type": "enabled",
								"settings": []any{map[string]any{
									"budget": "1024",
								}},
							},
							Off: map[string]any{
								"type": "disabled",
								"settings": []any{map[string]any{
									"budget": "0",
								}},
							},
						},
					},
				},
			},
		},
	}
}

func mutateReasoningSnapshot(t *testing.T, cfg config.LLMConfig, marker string) {
	t.Helper()
	effort := reasoningSnapshotControl(t, cfg, "effort-model")
	effort.AllowedEfforts[0] = marker

	nested := reasoningSnapshotControl(t, cfg, "nested-model")
	for _, value := range []any{nested.On, nested.Off} {
		mapping, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("nested reasoning value=%T, want map[string]any", value)
		}
		mapping["type"] = marker
		settings, ok := mapping["settings"].([]any)
		if !ok || len(settings) != 1 {
			t.Fatalf("nested settings=%#v, want one entry", mapping["settings"])
		}
		entry, ok := settings[0].(map[string]any)
		if !ok {
			t.Fatalf("nested settings entry=%T, want map[string]any", settings[0])
		}
		entry["budget"] = marker
	}
}

func assertReasoningSnapshotState(
	t *testing.T,
	cfg config.LLMConfig,
	allowedFirst string,
	onType string,
	onBudget string,
	offType string,
	offBudget string,
) {
	t.Helper()
	effort := reasoningSnapshotControl(t, cfg, "effort-model")
	if got := effort.AllowedEfforts; !reflect.DeepEqual(got, []string{allowedFirst, "medium", "high"}) {
		t.Fatalf("allowed_efforts=%v, want first value %q", got, allowedFirst)
	}

	nested := reasoningSnapshotControl(t, cfg, "nested-model")
	for _, testCase := range []struct {
		name       string
		value      any
		wantType   string
		wantBudget string
	}{
		{name: "on", value: nested.On, wantType: onType, wantBudget: onBudget},
		{name: "off", value: nested.Off, wantType: offType, wantBudget: offBudget},
	} {
		mapping, ok := testCase.value.(map[string]any)
		if !ok {
			t.Fatalf("%s value=%T, want map[string]any", testCase.name, testCase.value)
		}
		settings, ok := mapping["settings"].([]any)
		if !ok || len(settings) != 1 {
			t.Fatalf("%s settings=%#v, want one entry", testCase.name, mapping["settings"])
		}
		entry, ok := settings[0].(map[string]any)
		if !ok {
			t.Fatalf("%s settings entry=%T, want map[string]any", testCase.name, settings[0])
		}
		if mapping["type"] != testCase.wantType || entry["budget"] != testCase.wantBudget {
			t.Fatalf(
				"%s nested value=(type=%v budget=%v), want (type=%q budget=%q)",
				testCase.name,
				mapping["type"],
				entry["budget"],
				testCase.wantType,
				testCase.wantBudget,
			)
		}
	}
}

func reasoningSnapshotControl(
	t *testing.T,
	cfg config.LLMConfig,
	modelID string,
) *config.LLMReasoningControlSpec {
	t.Helper()
	provider, ok := cfg.Providers["reasoning-provider"]
	if !ok {
		t.Fatal("reasoning-provider is missing")
	}
	for _, spec := range provider.ModelSpecs {
		if spec.ID == modelID {
			if spec.ReasoningControl == nil {
				t.Fatalf("model %q reasoning control is nil", modelID)
			}
			return spec.ReasoningControl
		}
	}
	t.Fatalf("model %q is missing", modelID)
	return nil
}

func TestReasoningCapabilityBoundaryUsesExactRequestedModel(t *testing.T) {
	capture := &reasoningCapabilityCaptureProvider{}
	provider := &completionCapabilityProvider{
		next:         capture,
		providerName: "exact-provider",
		providerConfig: config.LLMProviderConfig{
			Model:          "model-a",
			Models:         []string{"model-a", "model-b"},
			ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{
				{
					ID:               "model-a",
					Capabilities:     []string{config.LLMModelCapabilityText},
					ReasoningSupport: config.LLMReasoningSupportSupported,
					ReasoningControl: &config.LLMReasoningControlSpec{
						Dialect: config.LLMReasoningDialectEffort,
						On:      "high",
						Off:     "none",
					},
				},
				{
					ID:               "model-b",
					Capabilities:     []string{config.LLMModelCapabilityText},
					ReasoningSupport: config.LLMReasoningSupportSupported,
					ReasoningControl: &config.LLMReasoningControlSpec{
						Dialect: config.LLMReasoningDialectEnableThinking,
						On:      true,
						Off:     false,
					},
				},
			},
		},
	}

	_, err := provider.Complete(context.Background(), llm.CompletionRequest{
		Model:    "model-b",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		Metadata: map[string]any{"thinking": "on"},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	capability, ok := capture.req.Metadata[llm.ReasoningCapabilityMetadataKey].(llm.ReasoningCapability)
	if !ok {
		t.Fatalf("exact reasoning capability missing: %#v", capture.req.Metadata)
	}
	if capability.Dialect != llm.ReasoningDialectEnableThinking || capability.OnValue != true {
		t.Fatalf("model-b capability=%+v, must not inherit model-a", capability)
	}
}

func TestReasoningCapabilityBoundaryRejectsUnsupportedBeforeProvider(t *testing.T) {
	capture := &reasoningCapabilityCaptureProvider{}
	provider := &completionCapabilityProvider{
		next:         capture,
		providerName: "exact-provider",
		providerConfig: config.LLMProviderConfig{
			Model:          "plain-model",
			Models:         []string{"plain-model"},
			ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID:               "plain-model",
				Capabilities:     []string{config.LLMModelCapabilityText},
				ReasoningSupport: config.LLMReasoningSupportUnsupported,
			}},
		},
	}
	var observed llm.ReasoningReceipt

	_, err := provider.Complete(context.Background(), llm.CompletionRequest{
		Model:    "plain-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		Metadata: map[string]any{
			"thinking": "on",
			llm.ReasoningReceiptObserverMetadataKey: func(receipt llm.ReasoningReceipt) {
				observed = receipt
			},
		},
	})
	var unsupported *llm.ReasoningUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Complete() error=%v, want ReasoningUnsupportedError", err)
	}
	if capture.calls != 0 {
		t.Fatalf("provider calls=%d, want 0", capture.calls)
	}
	if observed.Support != llm.ReasoningUnsupported ||
		observed.Application != llm.ReasoningApplicationRejected {
		t.Fatalf("observed receipt=%+v, want unsupported rejected", observed)
	}
}

func TestReasoningCapabilityBoundaryUsesAllowedThinkingEffortOnlyForExactEffortDialect(t *testing.T) {
	const rawConfig = `
model: effort-model
models: [effort-model, bool-model]
model_specs_mode: explicit
model_specs:
  - id: effort-model
    capabilities: [text]
    reasoning_support: supported
    reasoning_control:
      dialect: reasoning_effort
      on: high
      off: none
      allowed_efforts: [low, medium, high, xhigh, max]
  - id: bool-model
    capabilities: [text]
    reasoning_support: supported
    reasoning_control:
      dialect: think
      on: true
      off: false
`
	var providerConfig config.LLMProviderConfig
	if err := yaml.Unmarshal([]byte(rawConfig), &providerConfig); err != nil {
		t.Fatalf("yaml.Unmarshal() error: %v", err)
	}

	capture := &reasoningCapabilityCaptureProvider{}
	provider := &completionCapabilityProvider{
		next:           capture,
		providerName:   "exact-provider",
		providerConfig: providerConfig,
	}

	_, err := provider.Complete(context.Background(), llm.CompletionRequest{
		Model:    "effort-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		Metadata: map[string]any{
			"thinking":        "on",
			"thinking_effort": "xhigh",
		},
	})
	if err != nil {
		t.Fatalf("Complete(effort-model) error: %v", err)
	}
	capability, ok := capture.req.Metadata[llm.ReasoningCapabilityMetadataKey].(llm.ReasoningCapability)
	if !ok {
		t.Fatalf("effort capability missing: %#v", capture.req.Metadata)
	}
	if capability.Dialect != llm.ReasoningDialectEffort || capability.OnValue != "xhigh" {
		t.Fatalf("effort capability=%+v, want exact allowed effort xhigh", capability)
	}

	_, err = provider.Complete(context.Background(), llm.CompletionRequest{
		Model:    "bool-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		Metadata: map[string]any{
			"thinking":        "on",
			"thinking_effort": "xhigh",
		},
	})
	if err != nil {
		t.Fatalf("Complete(bool-model) error: %v", err)
	}
	capability, ok = capture.req.Metadata[llm.ReasoningCapabilityMetadataKey].(llm.ReasoningCapability)
	if !ok {
		t.Fatalf("bool capability missing: %#v", capture.req.Metadata)
	}
	if capability.Dialect != llm.ReasoningDialectThink || capability.OnValue != true {
		t.Fatalf("bool-model capability=%+v must not consume thinking_effort", capability)
	}
}

func TestReasoningCapabilityBoundaryRejectsThinkingEffortOutsideExactAllowedSet(t *testing.T) {
	const rawConfig = `
model: effort-model
models: [effort-model]
model_specs_mode: explicit
model_specs:
  - id: effort-model
    capabilities: [text]
    reasoning_support: supported
    reasoning_control:
      dialect: reasoning_effort
      on: high
      off: none
      allowed_efforts: [low, medium, high, xhigh, max]
`
	var providerConfig config.LLMProviderConfig
	if err := yaml.Unmarshal([]byte(rawConfig), &providerConfig); err != nil {
		t.Fatalf("yaml.Unmarshal() error: %v", err)
	}
	capture := &reasoningCapabilityCaptureProvider{}
	provider := &completionCapabilityProvider{
		next:           capture,
		providerName:   "exact-provider",
		providerConfig: providerConfig,
	}

	_, err := provider.Complete(context.Background(), llm.CompletionRequest{
		Model:    "effort-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		Metadata: map[string]any{
			"thinking":        "on",
			"thinking_effort": "unsupported-effort",
		},
	})
	if err == nil {
		t.Fatal("Complete() accepted a thinking_effort outside the exact allowed set")
	}
	if capture.calls != 0 {
		t.Fatalf("provider calls=%d, want 0", capture.calls)
	}
}

func TestReasoningCapabilityBoundaryKeepsUnknownOllamaThinkingOnAsNativeBoolean(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"model":"qwen3.5:9b","message":{"role":"assistant","content":"ok"},"done":true}`))
	}))
	defer server.Close()

	provider := &completionCapabilityProvider{
		next:         ollama.New(ollama.WithBaseURL(server.URL)),
		providerName: "local-ollama",
		providerConfig: config.LLMProviderConfig{
			Model:          "qwen3.5:9b",
			Models:         []string{"qwen3.5:9b"},
			ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID:               "qwen3.5:9b",
				Capabilities:     []string{config.LLMModelCapabilityText},
				ReasoningSupport: config.LLMReasoningSupportUnknown,
			}},
		},
	}

	_, err := provider.Complete(context.Background(), llm.CompletionRequest{
		Model:    "qwen3.5:9b",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
		Metadata: map[string]any{"thinking": "on"},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if think, ok := payload["think"].(bool); !ok || !think {
		t.Fatalf("unknown capability short-circuited native think=true: payload=%v", payload)
	}
}
