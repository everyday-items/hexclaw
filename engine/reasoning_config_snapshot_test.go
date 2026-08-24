package engine

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/skill"
)

const reasoningSnapshotProvider = "snapshot-provider"

type reasoningSnapshotState struct {
	dialect     string
	firstEffort string
	onMode      string
	onBudget    string
	offMode     string
}

func TestCloneLLMConfig_ReasoningSnapshotIsolation(t *testing.T) {
	input := reasoningSnapshotConfig()
	cloned := cloneLLMConfig(input)

	mutateReasoningSnapshot(t, cloned, "clone")
	assertReasoningSnapshotState(t, input, initialReasoningSnapshotState())

	mutateReasoningSnapshot(t, input, "input")
	assertReasoningSnapshotState(t, cloned, mutatedReasoningSnapshotState("clone"))
}

func TestReActEngine_ActiveLLMConfigReasoningSnapshotIsolation(t *testing.T) {
	eng, owner := newReasoningSnapshotEngine(t, reasoningSnapshotConfig())

	returned := eng.ActiveLLMConfig()
	mutateReasoningSnapshot(t, returned, "returned")

	assertReasoningSnapshotState(t, eng.ActiveLLMConfig(), initialReasoningSnapshotState())
	assertReasoningSnapshotState(t, owner.LLM, initialReasoningSnapshotState())
}

func TestReActEngine_ReloadLLMConfigReasoningSnapshotIsolation(t *testing.T) {
	eng, owner := newReasoningSnapshotEngine(t, reasoningSnapshotConfig())
	next := reasoningSnapshotConfig()

	if err := eng.ReloadLLMConfig(context.Background(), next); err != nil {
		t.Fatalf("ReloadLLMConfig 失败: %v", err)
	}
	mutateReasoningSnapshot(t, next, "caller")

	assertReasoningSnapshotState(t, owner.LLM, initialReasoningSnapshotState())
	assertReasoningSnapshotState(t, eng.ActiveLLMConfig(), initialReasoningSnapshotState())
}

func newReasoningSnapshotEngine(
	t *testing.T,
	llmCfg config.LLMConfig,
) (*ReActEngine, *config.Config) {
	t.Helper()
	owner := config.DefaultConfig()
	owner.LLM = llmCfg
	return NewReActEngine(owner, nil, nil, skill.NewRegistry()), owner
}

func reasoningSnapshotConfig() config.LLMConfig {
	return config.LLMConfig{
		Default: reasoningSnapshotProvider,
		Providers: map[string]config.LLMProviderConfig{
			reasoningSnapshotProvider: {
				APIKey:         "test-key",
				Model:          "reasoning-model",
				Models:         []string{"reasoning-model"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{
					{
						ID:               "reasoning-model",
						Capabilities:     []string{config.LLMModelCapabilityText},
						ReasoningSupport: config.LLMReasoningSupportSupported,
						ReasoningControl: &config.LLMReasoningControlSpec{
							Dialect: "snapshot-dialect",
							On: map[string]any{
								"mode": "high",
								"nested": []any{
									map[string]any{"budget": "large"},
								},
							},
							Off: []any{
								map[string]any{"mode": "none"},
							},
							AllowedEfforts: []string{"low", "high"},
						},
					},
				},
			},
		},
	}
}

func initialReasoningSnapshotState() reasoningSnapshotState {
	return reasoningSnapshotState{
		dialect:     "snapshot-dialect",
		firstEffort: "low",
		onMode:      "high",
		onBudget:    "large",
		offMode:     "none",
	}
}

func mutatedReasoningSnapshotState(label string) reasoningSnapshotState {
	return reasoningSnapshotState{
		dialect:     label,
		firstEffort: label,
		onMode:      label,
		onBudget:    label,
		offMode:     label,
	}
}

func mutateReasoningSnapshot(t *testing.T, cfg config.LLMConfig, label string) {
	t.Helper()
	control, on, onNested, off := reasoningSnapshotParts(t, cfg)
	control.Dialect = label
	control.AllowedEfforts[0] = label
	on["mode"] = label
	onNested["budget"] = label
	off["mode"] = label
}

func assertReasoningSnapshotState(
	t *testing.T,
	cfg config.LLMConfig,
	want reasoningSnapshotState,
) {
	t.Helper()
	control, on, onNested, off := reasoningSnapshotParts(t, cfg)
	got := reasoningSnapshotState{
		dialect:     control.Dialect,
		firstEffort: control.AllowedEfforts[0],
		onMode:      stringValue(t, on["mode"], "reasoning_control.on.mode"),
		onBudget:    stringValue(t, onNested["budget"], "reasoning_control.on.nested.budget"),
		offMode:     stringValue(t, off["mode"], "reasoning_control.off.mode"),
	}
	if got != want {
		t.Fatalf("reasoning 快照发生共享变异: got=%+v want=%+v", got, want)
	}
}

func reasoningSnapshotParts(
	t *testing.T,
	cfg config.LLMConfig,
) (*config.LLMReasoningControlSpec, map[string]any, map[string]any, map[string]any) {
	t.Helper()
	provider, ok := cfg.Providers[reasoningSnapshotProvider]
	if !ok {
		t.Fatalf("缺少 provider %q", reasoningSnapshotProvider)
	}
	if len(provider.ModelSpecs) != 1 || provider.ModelSpecs[0].ReasoningControl == nil {
		t.Fatalf("reasoning model spec 非预期: %+v", provider.ModelSpecs)
	}
	control := provider.ModelSpecs[0].ReasoningControl
	if len(control.AllowedEfforts) != 2 {
		t.Fatalf("allowed_efforts 非预期: %#v", control.AllowedEfforts)
	}
	on, ok := control.On.(map[string]any)
	if !ok {
		t.Fatalf("reasoning_control.on 类型=%T，期望 map[string]any", control.On)
	}
	onValues, ok := on["nested"].([]any)
	if !ok || len(onValues) != 1 {
		t.Fatalf("reasoning_control.on.nested 非预期: %#v", on["nested"])
	}
	onNested, ok := onValues[0].(map[string]any)
	if !ok {
		t.Fatalf("reasoning_control.on.nested[0] 类型=%T，期望 map[string]any", onValues[0])
	}
	offValues, ok := control.Off.([]any)
	if !ok || len(offValues) != 1 {
		t.Fatalf("reasoning_control.off 非预期: %#v", control.Off)
	}
	off, ok := offValues[0].(map[string]any)
	if !ok {
		t.Fatalf("reasoning_control.off[0] 类型=%T，期望 map[string]any", offValues[0])
	}
	return control, on, onNested, off
}

func stringValue(t *testing.T, value any, field string) string {
	t.Helper()
	result, ok := value.(string)
	if !ok {
		t.Fatalf("%s 类型=%T，期望 string", field, value)
	}
	return result
}
