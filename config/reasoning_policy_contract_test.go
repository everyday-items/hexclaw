package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReasoningPolicyJSONContractExactSet(t *testing.T) {
	tests := []struct {
		name         string
		policy       ReasoningPolicy
		wire         string
		allowInherit bool
	}{
		{name: "auto", policy: ReasoningPolicy{Mode: ReasoningPolicyModeAuto}, wire: `{"mode":"auto"}`},
		{name: "inherit", policy: ReasoningPolicy{Mode: ReasoningPolicyModeInherit}, wire: `{"mode":"inherit"}`, allowInherit: true},
		{name: "on", policy: ReasoningPolicy{Mode: ReasoningPolicyModeOn}, wire: `{"mode":"on"}`},
		{name: "off", policy: ReasoningPolicy{Mode: ReasoningPolicyModeOff}, wire: `{"mode":"off"}`},
		{name: "low", policy: ReasoningPolicy{Mode: ReasoningPolicyModeEffort, Effort: ReasoningEffortLow}, wire: `{"mode":"effort","effort":"low"}`},
		{name: "medium", policy: ReasoningPolicy{Mode: ReasoningPolicyModeEffort, Effort: ReasoningEffortMedium}, wire: `{"mode":"effort","effort":"medium"}`},
		{name: "high", policy: ReasoningPolicy{Mode: ReasoningPolicyModeEffort, Effort: ReasoningEffortHigh}, wire: `{"mode":"effort","effort":"high"}`},
		{name: "xhigh", policy: ReasoningPolicy{Mode: ReasoningPolicyModeEffort, Effort: ReasoningEffortXHigh}, wire: `{"mode":"effort","effort":"xhigh"}`},
		{name: "max", policy: ReasoningPolicy{Mode: ReasoningPolicyModeEffort, Effort: ReasoningEffortMax}, wire: `{"mode":"effort","effort":"max"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.policy.Validate(tt.allowInherit); err != nil {
				t.Fatalf("valid policy rejected: %v", err)
			}
			encoded, err := json.Marshal(tt.policy)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(encoded) != tt.wire {
				t.Fatalf("wire = %s, want %s", encoded, tt.wire)
			}
			var decoded ReasoningPolicy
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if decoded != tt.policy {
				t.Fatalf("round-trip = %+v, want %+v", decoded, tt.policy)
			}
		})
	}
}

func TestDefaultReasoningPolicyDefaultsToAuto(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.LLM.DefaultReasoningPolicy; got != (ReasoningPolicy{Mode: ReasoningPolicyModeAuto}) {
		t.Fatalf("default reasoning policy = %+v, want mode=auto", got)
	}
}

func TestDefaultReasoningPolicyYAMLRoundTrip(t *testing.T) {
	want := ReasoningPolicy{Mode: ReasoningPolicyModeEffort, Effort: ReasoningEffortXHigh}
	cfg := DefaultConfig()
	cfg.LLM.DefaultReasoningPolicy = want

	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if !strings.Contains(string(encoded), "default_reasoning_policy:") ||
		!strings.Contains(string(encoded), "mode: effort") ||
		!strings.Contains(string(encoded), "effort: xhigh") {
		t.Fatalf("reasoning policy missing from YAML:\n%s", encoded)
	}

	var decoded Config
	if err := yaml.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if decoded.LLM.DefaultReasoningPolicy != want {
		t.Fatalf("round-trip policy = %+v, want %+v", decoded.LLM.DefaultReasoningPolicy, want)
	}
}

func TestDefaultReasoningPolicyYAMLRejectsInvalidGlobalPolicies(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{name: "inherit is agent only", yaml: "llm:\n  default_reasoning_policy:\n    mode: inherit\n"},
		{name: "effort value missing", yaml: "llm:\n  default_reasoning_policy:\n    mode: effort\n"},
		{name: "effort attached to boolean mode", yaml: "llm:\n  default_reasoning_policy:\n    mode: on\n    effort: high\n"},
		{name: "unknown effort", yaml: "llm:\n  default_reasoning_policy:\n    mode: effort\n    effort: extreme\n"},
		{name: "unknown mode", yaml: "llm:\n  default_reasoning_policy:\n    mode: sometimes\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tt.yaml), &cfg); err == nil {
				t.Fatalf("invalid policy unexpectedly decoded: %+v", cfg.LLM.DefaultReasoningPolicy)
			}
		})
	}
}

func TestDefaultReasoningPolicyYAMLMarshalRejectsInvalidGlobalPolicies(t *testing.T) {
	tests := []ReasoningPolicy{
		{Mode: ReasoningPolicyModeInherit},
		{Mode: ReasoningPolicyModeEffort},
		{Mode: ReasoningPolicyModeOn, Effort: ReasoningEffortHigh},
		{Mode: ReasoningPolicyModeEffort, Effort: ReasoningEffort("extreme")},
		{Mode: ReasoningPolicyMode("sometimes")},
	}
	for _, policy := range tests {
		cfg := DefaultConfig()
		cfg.LLM.DefaultReasoningPolicy = policy
		if _, err := yaml.Marshal(cfg); err == nil {
			t.Fatalf("invalid global policy unexpectedly marshaled: %+v", policy)
		}
	}
}
