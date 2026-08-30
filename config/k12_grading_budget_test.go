package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func validK12GradingBudgetConfig() K12GradingBudgetConfig {
	return K12GradingBudgetConfig{
		PolicyVersion:          1,
		RecognitionPlanVersion: 1,
		QueuedSeconds:          60, NormalizingSeconds: 60, RecognizingSeconds: 120,
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

func TestDefaultK12GradingBudgetProvidesRunnableOperationalBaseline(t *testing.T) {
	want := validK12GradingBudgetConfig()
	got := DefaultK12GradingBudget()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default grading budget=%+v, want %+v", got, want)
	}

	cfg := DefaultConfig()
	if !cfg.K12.GradingBudget.IsZero() {
		t.Fatalf("persisted default must not claim release calibration: %+v", cfg.K12.GradingBudget)
	}
	cfg.K12.GradingBudget = got
	if err := cfg.Validate(); err != nil {
		t.Fatalf("operational grading baseline must validate: %v", err)
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

func TestValidateK12GradingBudgetReleaseKeepsV2EffectiveConcurrencyAtOne(t *testing.T) {
	cfg := DefaultConfig()
	budget := validK12GradingBudgetConfig()
	budget.RecognitionPlanVersion = 2
	budget.RecognizingBuckets = K12RecognizingBudgetBucketsConfig{
		UpTo1ProblemMillis: 30_001, UpTo8ProblemsMillis: 60_001,
		UpTo16ProblemsMillis: 90_001, UpTo32ProblemsMillis: 120_001,
	}
	budget.RecognizingSeconds = 121
	budget.PhysicalCallCapMillis = 120_000
	budget.WorkerHardCap = 2
	budget.EffectiveConcurrency = 1
	cfg.K12.GradingBudget = budget
	if err := cfg.Validate(); err != nil {
		t.Fatalf("complete release v2 policy should validate: %v", err)
	}
	cfg.K12.GradingBudget.EffectiveConcurrency = 2
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "effective_concurrency") {
		t.Fatalf("release effective=2 without approved calibration must fail exact field: %v", err)
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
		got.K12.GradingBudget.RecognitionPlanVersion != 1 ||
		len(got.K12.GradingBudget.AssessingBuckets) != 4 ||
		got.K12.GradingBudget.ItemConcurrency != 2 {
		t.Fatalf("budget policy lost on YAML round trip: %+v", got.K12.GradingBudget)
	}
}
