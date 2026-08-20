package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	testReasoningSupportSupported   = "supported"
	testReasoningSupportUnsupported = "unsupported"
	testReasoningSupportUnknown     = "unknown"
)

func TestProviderModelReasoningSupport_YAMLRoundTrip(t *testing.T) {
	for _, support := range []string{
		testReasoningSupportSupported,
		testReasoningSupportUnsupported,
		testReasoningSupportUnknown,
	} {
		t.Run(support, func(t *testing.T) {
			modelID := "model-" + support
			spec := LLMProviderModelSpec{
				ID:           modelID,
				Capabilities: []string{LLMModelCapabilityText},
			}
			setProviderModelReasoningSupport(t, &spec, support)
			if support == testReasoningSupportSupported {
				spec.ReasoningControl = testReasoningControl()
			}
			provider := LLMProviderConfig{
				Model:          modelID,
				Models:         []string{modelID},
				ModelSpecsMode: LLMModelSpecsModeExplicit,
				ModelSpecs:     []LLMProviderModelSpec{spec},
			}

			encoded, err := yaml.Marshal(provider)
			if err != nil {
				t.Fatalf("yaml.Marshal() error: %v", err)
			}
			if !strings.Contains(string(encoded), "reasoning_support: "+support) {
				t.Fatalf("YAML does not persist reasoning_support=%q:\n%s", support, encoded)
			}

			var decoded LLMProviderConfig
			if err := yaml.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("yaml.Unmarshal() error: %v", err)
			}
			if err := ValidateProviderModelSpecs(decoded); err != nil {
				t.Fatalf("round-tripped reasoning_support=%q rejected: %v", support, err)
			}
			_, specs := NormalizeProviderModelSpecs(decoded)
			got := providerModelReasoningSupport(t, reasoningModelSpecByID(t, specs, modelID))
			if got != support {
				t.Fatalf("reasoning_support after YAML round-trip=%q, want %q", got, support)
			}
		})
	}
}

func TestProviderModelReasoningSupport_CloneRoundTrip(t *testing.T) {
	for _, support := range []string{
		testReasoningSupportSupported,
		testReasoningSupportUnsupported,
		testReasoningSupportUnknown,
	} {
		t.Run(support, func(t *testing.T) {
			original := LLMProviderModelSpec{
				ID:           "exact-model",
				Capabilities: []string{LLMModelCapabilityText},
			}
			setProviderModelReasoningSupport(t, &original, support)
			if support == testReasoningSupportSupported {
				original.ReasoningControl = testReasoningControl()
			}

			cloned := cloneProviderModelSpec(original)
			if got := providerModelReasoningSupport(t, cloned); got != support {
				t.Fatalf("clone reasoning_support=%q, want %q", got, support)
			}
		})
	}
}

func TestProviderModelReasoningSupport_Validation(t *testing.T) {
	for _, support := range []string{
		testReasoningSupportSupported,
		testReasoningSupportUnsupported,
		testReasoningSupportUnknown,
	} {
		t.Run("accepts_"+support, func(t *testing.T) {
			provider := reasoningProviderWithSupport(t, support)
			if err := ValidateProviderModelSpecs(provider); err != nil {
				t.Fatalf("reasoning_support=%q rejected: %v", support, err)
			}
		})
	}

	for _, support := range []string{"auto", "SUPPORTED", " supported ", "enabled"} {
		t.Run("rejects_"+strings.TrimSpace(support), func(t *testing.T) {
			provider := reasoningProviderWithSupport(t, support)
			err := ValidateProviderModelSpecs(provider)
			if err == nil {
				t.Fatalf("invalid reasoning_support=%q was accepted", support)
			}
			if !strings.Contains(err.Error(), "reasoning_support") {
				t.Fatalf("validation error=%q, want reasoning_support field context", err)
			}
		})
	}
}

func TestProviderModelReasoningSupport_MissingDefaultsToUnknown(t *testing.T) {
	const raw = `
model: exact-model
models:
  - exact-model
model_specs_mode: explicit
model_specs:
  - id: exact-model
    capabilities:
      - text
`

	var provider LLMProviderConfig
	if err := yaml.Unmarshal([]byte(raw), &provider); err != nil {
		t.Fatalf("yaml.Unmarshal() error: %v", err)
	}
	if err := ValidateProviderModelSpecs(provider); err != nil {
		t.Fatalf("missing reasoning_support must remain valid: %v", err)
	}
	_, specs := NormalizeProviderModelSpecs(provider)
	got := providerModelReasoningSupport(t, reasoningModelSpecByID(t, specs, "exact-model"))
	if got != testReasoningSupportUnknown {
		t.Fatalf("missing reasoning_support normalized to %q, want %q", got, testReasoningSupportUnknown)
	}
}

func TestProviderModelReasoningSupport_DoesNotInferFromModelName(t *testing.T) {
	modelIDs := []string{
		"gpt-5.6-luna",
		"o3-reasoning",
		"claude-thinking",
		"qwen3-thinking",
		"deepseek-r1",
	}
	provider := LLMProviderConfig{Models: modelIDs}

	_, specs := NormalizeProviderModelSpecs(provider)
	for _, modelID := range modelIDs {
		got := providerModelReasoningSupport(t, reasoningModelSpecByID(t, specs, modelID))
		if got != testReasoningSupportUnknown {
			t.Fatalf("model %q inferred reasoning_support=%q, want %q", modelID, got, testReasoningSupportUnknown)
		}
	}
}

func TestProviderModelReasoningControl_YAMLRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		dialect string
		on      string
		off     string
	}{
		{name: "openai", dialect: "reasoning_effort", on: "high", off: "none"},
		{name: "qwen", dialect: "enable_thinking", on: "true", off: "false"},
		{name: "ollama", dialect: "think", on: "true", off: "false"},
		{name: "anthropic", dialect: "thinking", on: "{type: enabled, budget_tokens: 1024}", off: "{type: disabled}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := "model: exact-model\n" +
				"models: [exact-model]\n" +
				"model_specs_mode: explicit\n" +
				"model_specs:\n" +
				"  - id: exact-model\n" +
				"    capabilities: [text]\n" +
				"    reasoning_support: supported\n" +
				"    reasoning_control:\n" +
				"      dialect: " + tt.dialect + "\n" +
				"      on: " + tt.on + "\n" +
				"      off: " + tt.off + "\n"

			var provider LLMProviderConfig
			if err := yaml.Unmarshal([]byte(raw), &provider); err != nil {
				t.Fatalf("yaml.Unmarshal() error: %v", err)
			}
			if err := ValidateProviderModelSpecs(provider); err != nil {
				t.Fatalf("valid reasoning control rejected: %v", err)
			}
			encoded, err := yaml.Marshal(provider)
			if err != nil {
				t.Fatalf("yaml.Marshal() error: %v", err)
			}
			if !strings.Contains(string(encoded), "reasoning_control:") ||
				!strings.Contains(string(encoded), "dialect: "+tt.dialect) {
				t.Fatalf("reasoning control was not preserved:\n%s", encoded)
			}
		})
	}
}

func TestProviderModelReasoningControl_AllowedEffortsYAMLRoundTrip(t *testing.T) {
	const raw = `
model: exact-model
models: [exact-model]
model_specs_mode: explicit
model_specs:
  - id: exact-model
    capabilities: [text]
    reasoning_support: supported
    reasoning_control:
      dialect: reasoning_effort
      on: high
      off: none
      allowed_efforts: [low, medium, high, xhigh, max]
`

	var provider LLMProviderConfig
	if err := yaml.Unmarshal([]byte(raw), &provider); err != nil {
		t.Fatalf("yaml.Unmarshal() error: %v", err)
	}
	if err := ValidateProviderModelSpecs(provider); err != nil {
		t.Fatalf("valid allowed_efforts declaration rejected: %v", err)
	}

	control := provider.ModelSpecs[0].ReasoningControl
	got := reasoningControlAllowedEfforts(t, control)
	want := []string{"low", "medium", "high", "xhigh", "max"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed_efforts after YAML decode=%v, want %v", got, want)
	}

	encoded, err := yaml.Marshal(provider)
	if err != nil {
		t.Fatalf("yaml.Marshal() error: %v", err)
	}
	if !strings.Contains(string(encoded), "allowed_efforts:") {
		t.Fatalf("YAML does not persist allowed_efforts:\n%s", encoded)
	}

	var decoded LLMProviderConfig
	if err := yaml.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("yaml.Unmarshal(round-trip) error: %v", err)
	}
	if err := ValidateProviderModelSpecs(decoded); err != nil {
		t.Fatalf("round-tripped allowed_efforts rejected: %v", err)
	}
	if got := reasoningControlAllowedEfforts(t, decoded.ModelSpecs[0].ReasoningControl); !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed_efforts after YAML round-trip=%v, want %v", got, want)
	}
	_, resolved := ModelReasoningControl(decoded, "exact-model")
	if got := reasoningControlAllowedEfforts(t, resolved); !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed_efforts after exact-model resolution=%v, want %v", got, want)
	}
}

func TestProviderModelReasoningControl_RejectsAllowedEffortsForNonEffortDialect(t *testing.T) {
	const raw = `
model: exact-model
models: [exact-model]
model_specs_mode: explicit
model_specs:
  - id: exact-model
    capabilities: [text]
    reasoning_support: supported
    reasoning_control:
      dialect: think
      on: true
      off: false
      allowed_efforts: [low, medium]
`

	var provider LLMProviderConfig
	if err := yaml.Unmarshal([]byte(raw), &provider); err != nil {
		t.Fatalf("yaml.Unmarshal() error: %v", err)
	}
	err := ValidateProviderModelSpecs(provider)
	if err == nil {
		t.Fatal("non-effort dialect accepted allowed_efforts")
	}
	if !strings.Contains(err.Error(), "allowed_efforts") {
		t.Fatalf("validation error=%q, want allowed_efforts context", err)
	}
}

func TestProviderModelReasoningControl_RejectsInconsistentDeclarations(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "supported_without_control",
			raw:  "reasoning_support: supported\n",
		},
		{
			name: "unsupported_with_control",
			raw: "reasoning_support: unsupported\n" +
				"    reasoning_control: {dialect: think, on: true, off: false}\n",
		},
		{
			name: "unknown_with_control",
			raw: "reasoning_support: unknown\n" +
				"    reasoning_control: {dialect: think, on: true, off: false}\n",
		},
		{
			name: "invalid_dialect",
			raw: "reasoning_support: supported\n" +
				"    reasoning_control: {dialect: auto, on: true, off: false}\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := "model: exact-model\n" +
				"models: [exact-model]\n" +
				"model_specs_mode: explicit\n" +
				"model_specs:\n" +
				"  - id: exact-model\n" +
				"    capabilities: [text]\n" +
				"    " + tt.raw
			var provider LLMProviderConfig
			if err := yaml.Unmarshal([]byte(raw), &provider); err != nil {
				t.Fatalf("yaml.Unmarshal() error: %v", err)
			}
			if err := ValidateProviderModelSpecs(provider); err == nil {
				t.Fatalf("inconsistent reasoning declaration accepted:\n%s", raw)
			}
		})
	}
}

func reasoningProviderWithSupport(t *testing.T, support string) LLMProviderConfig {
	t.Helper()
	spec := LLMProviderModelSpec{
		ID:           "exact-model",
		Capabilities: []string{LLMModelCapabilityText},
	}
	setProviderModelReasoningSupport(t, &spec, support)
	if support == testReasoningSupportSupported {
		spec.ReasoningControl = testReasoningControl()
	}
	return LLMProviderConfig{
		Model:          spec.ID,
		Models:         []string{spec.ID},
		ModelSpecsMode: LLMModelSpecsModeExplicit,
		ModelSpecs:     []LLMProviderModelSpec{spec},
	}
}

func testReasoningControl() *LLMReasoningControlSpec {
	return &LLMReasoningControlSpec{
		Dialect: LLMReasoningDialectEffort,
		On:      "high",
		Off:     "none",
	}
}

func reasoningControlAllowedEfforts(t *testing.T, control *LLMReasoningControlSpec) []string {
	t.Helper()
	if control == nil {
		t.Fatal("reasoning control is nil")
	}
	typeField, ok := reflect.TypeOf(LLMReasoningControlSpec{}).FieldByName("AllowedEfforts")
	if !ok {
		t.Fatal("LLMReasoningControlSpec must declare AllowedEfforts")
	}
	if typeField.Type != reflect.TypeOf([]string(nil)) {
		t.Fatalf("LLMReasoningControlSpec.AllowedEfforts type=%s, want []string", typeField.Type)
	}
	if tagName := strings.Split(typeField.Tag.Get("yaml"), ",")[0]; tagName != "allowed_efforts" {
		t.Fatalf("LLMReasoningControlSpec.AllowedEfforts yaml tag=%q, want allowed_efforts", typeField.Tag.Get("yaml"))
	}
	field := reflect.ValueOf(control).Elem().FieldByIndex(typeField.Index)
	if field.IsNil() {
		return nil
	}
	return append([]string(nil), field.Interface().([]string)...)
}

func setProviderModelReasoningSupport(t *testing.T, spec *LLMProviderModelSpec, support string) {
	t.Helper()
	field := requireProviderModelReasoningSupportField(t, reflect.ValueOf(spec).Elem())
	if !field.CanSet() {
		t.Fatal("LLMProviderModelSpec.ReasoningSupport is not settable")
	}
	field.SetString(support)
}

func providerModelReasoningSupport(t *testing.T, spec LLMProviderModelSpec) string {
	t.Helper()
	return requireProviderModelReasoningSupportField(t, reflect.ValueOf(spec)).String()
}

func requireProviderModelReasoningSupportField(t *testing.T, value reflect.Value) reflect.Value {
	t.Helper()
	typeField, ok := reflect.TypeOf(LLMProviderModelSpec{}).FieldByName("ReasoningSupport")
	if !ok {
		t.Fatal("LLMProviderModelSpec must declare ReasoningSupport")
	}
	if typeField.Type.Kind() != reflect.String {
		t.Fatalf("LLMProviderModelSpec.ReasoningSupport kind=%s, want string", typeField.Type.Kind())
	}
	if tagName := strings.Split(typeField.Tag.Get("yaml"), ",")[0]; tagName != "reasoning_support" {
		t.Fatalf("LLMProviderModelSpec.ReasoningSupport yaml tag=%q, want reasoning_support", typeField.Tag.Get("yaml"))
	}
	return value.FieldByIndex(typeField.Index)
}

func reasoningModelSpecByID(t *testing.T, specs []LLMProviderModelSpec, modelID string) LLMProviderModelSpec {
	t.Helper()
	for _, spec := range specs {
		if spec.ID == modelID {
			return spec
		}
	}
	t.Fatalf("model spec %q not found in %+v", modelID, specs)
	return LLMProviderModelSpec{}
}
