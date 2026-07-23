package main

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestK12GradingBudgetSnapshotFromConfigPreservesReleaseEvidence(t *testing.T) {
	cfg := config.K12GradingBudgetConfig{
		PolicyVersion: 3, QueuedSeconds: 11, NormalizingSeconds: 12,
		RecognizingSeconds: 13, LocatingSeconds: 14, RenderingSeconds: 15,
		ProjectingSeconds: 16, ItemConcurrency: 2,
		AssessingBuckets: []config.K12AssessingBudgetBucketConfig{
			{MaxProblems: 1, Seconds: 21}, {MaxProblems: 8, Seconds: 22},
			{MaxProblems: 16, Seconds: 23}, {MaxProblems: 32, Seconds: 24},
		},
	}
	got := k12GradingBudgetSnapshotFromConfig(cfg)
	if err := got.Validate(); err != nil {
		t.Fatalf("converted snapshot invalid: %v", err)
	}
	if got.PolicyVersion != 3 || got.StageSeconds.Recognizing != 13 ||
		got.AssessingBuckets[2].Seconds != 23 || got.ItemConcurrency != 2 {
		t.Fatalf("release evidence changed during composition: %+v", got)
	}
}

func TestK12GradingBudgetSnapshotFromConfigKeepsZeroPolicyUnfrozen(t *testing.T) {
	got := k12GradingBudgetSnapshotFromConfig(config.K12GradingBudgetConfig{})
	if got.IsFrozen() || got.Validate() != nil {
		t.Fatalf("zero config must remain strict legacy gate: %+v", got)
	}
}
