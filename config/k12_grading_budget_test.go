package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func validK12GradingBudgetConfig() K12GradingBudgetConfig {
	return K12GradingBudgetConfig{
		PolicyVersion: 1,
		QueuedSeconds: 60, NormalizingSeconds: 60, RecognizingSeconds: 120,
		LocatingSeconds: 60, RenderingSeconds: 60, ProjectingSeconds: 60,
		AssessingBuckets: []K12AssessingBudgetBucketConfig{
			{MaxProblems: 1, Seconds: 90},
			{MaxProblems: 8, Seconds: 180},
			{MaxProblems: 16, Seconds: 300},
			{MaxProblems: 32, Seconds: 540},
		},
		ItemConcurrency: 2,
	}
}

func TestDefaultK12GradingBudgetRemainsUnfrozenUntilRealBenchmarkCompletes(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.K12.GradingBudget.PolicyVersion != 0 {
		t.Fatalf("default policy_version=%d, want strict unfrozen 0", cfg.K12.GradingBudget.PolicyVersion)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unfrozen shipping default must validate: %v", err)
	}
}

func TestValidateK12GradingBudgetRequiresCompleteMeasuredPolicy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.K12.GradingBudget = validK12GradingBudgetConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("complete measured policy should validate: %v", err)
	}

	cfg.K12.GradingBudget.AssessingBuckets = cfg.K12.GradingBudget.AssessingBuckets[:3]
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "k12.grading_budget.assessing_buckets") {
		t.Fatalf("missing 32 bucket must report exact field, got %v", err)
	}
}

func TestK12GradingBudgetYAMLRoundTripPreservesFrozenEvidence(t *testing.T) {
	want := Config{K12: K12Config{GradingBudget: validK12GradingBudgetConfig()}}
	raw, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.K12.GradingBudget.PolicyVersion != 1 ||
		len(got.K12.GradingBudget.AssessingBuckets) != 4 ||
		got.K12.GradingBudget.ItemConcurrency != 2 {
		t.Fatalf("budget policy lost on YAML round trip: %+v", got.K12.GradingBudget)
	}
}
