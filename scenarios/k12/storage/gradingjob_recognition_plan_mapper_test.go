package k12storage

import (
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestGradingJobBudgetMapperRecognitionPlanVersionCompatibility(t *testing.T) {
	legacyBudget := struct {
		PolicyVersion    int                                `json:"policy_version"`
		StageSeconds     k12.GradingStageBudgets            `json:"stage_seconds"`
		AssessingBuckets []k12.GradingAssessingBudgetBucket `json:"assessing_buckets"`
		ItemConcurrency  int                                `json:"item_concurrency"`
	}{
		PolicyVersion: 1,
		StageSeconds: k12.GradingStageBudgets{
			Queued: 11, Normalizing: 12, Recognizing: 13,
			Locating: 14, Rendering: 15, Projecting: 16,
		},
		AssessingBuckets: []k12.GradingAssessingBudgetBucket{
			{MaxProblems: 1, Seconds: 21}, {MaxProblems: 8, Seconds: 22},
			{MaxProblems: 16, Seconds: 23}, {MaxProblems: 32, Seconds: 24},
		},
		ItemConcurrency: 2,
	}
	legacyBudgetJSON, err := json.Marshal(legacyBudget)
	if err != nil {
		t.Fatal(err)
	}
	legacyEnvelopeJSON, err := json.Marshal(struct {
		Version                         int             `json:"grading_job_budget_envelope_version"`
		BudgetSnapshot                  json.RawMessage `json:"budget_snapshot"`
		ParentAutomaticAttemptID        string          `json:"parent_automatic_attempt_id"`
		ParentAutomaticDeadlineAt       int64           `json:"parent_automatic_deadline_at"`
		ParentAutomaticRemainingSeconds int64           `json:"parent_automatic_remaining_seconds"`
	}{
		Version: 1, BudgetSnapshot: legacyBudgetJSON,
		ParentAutomaticAttemptID: "attempt-old", ParentAutomaticDeadlineAt: 9_000,
		ParentAutomaticRemainingSeconds: 8_000,
	})
	if err != nil {
		t.Fatal(err)
	}

	mapper := gradingJobMapper{}
	dest, finish := mapper.newScan()
	*(dest[0].(*string)) = "submission-old"
	*(dest[1].(*string)) = "image_task"
	*(dest[2].(*string)) = "image_task|dispatch-old|v0"
	*(dest[3].(*int)) = 0
	*(dest[4].(*string)) = k12.GradingConfirmationPending
	*(dest[5].(*string)) = k12.GradingAnchorPending
	*(dest[6].(*int64)) = 0
	modelJSON, _ := json.Marshal(k12.GradingModelSnapshot{
		Provider: "hexclaw-gpt", Model: "gpt-5.6-sol", Route: "hexclaw-gpt/gpt-5.6-sol",
	})
	*(dest[7].(*string)) = string(modelJSON)
	*(dest[8].(*string)) = string(legacyEnvelopeJSON)
	fieldsJSON, err := finish()
	if err != nil {
		t.Fatalf("restore legacy mapper envelope: %v", err)
	}
	fields, err := k12.ParseGradingJobFields(fieldsJSON)
	if err != nil || fields.BudgetSnapshot.RecognitionPlanVersion != k12.RecognitionPlanVersionV1 {
		t.Fatalf("legacy missing plan must restore only as v1: plan=%d err=%v",
			fields.BudgetSnapshot.RecognitionPlanVersion, err)
	}

	v2 := fields
	v2.BudgetSnapshot.RecognitionPlanVersion = k12.RecognitionPlanVersionV2
	v2.BudgetSnapshot.RecognizingBuckets = k12.RecognitionLayoutBudgetBucketsV2{
		UpTo1ProblemMillis: 3_001, UpTo8ProblemsMillis: 6_001,
		UpTo16ProblemsMillis: 9_001, UpTo32ProblemsMillis: 13_000,
	}
	v2.BudgetSnapshot.PhysicalCallCapMillis = 120_000
	v2.BudgetSnapshot.WorkerHardCap = 2
	v2.BudgetSnapshot.EffectiveConcurrency = 1
	v2JSON, err := json.Marshal(v2)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := mapper.encode(string(v2JSON))
	if err != nil {
		t.Fatalf("encode v2 mapper envelope: %v", err)
	}
	var discriminator struct {
		Version int `json:"grading_job_budget_envelope_version"`
	}
	if err := json.Unmarshal([]byte(encoded[8].(string)), &discriminator); err != nil {
		t.Fatal(err)
	}
	if discriminator.Version != 2 {
		t.Fatalf("new recognition policy envelope version=%d, want 2", discriminator.Version)
	}
}
