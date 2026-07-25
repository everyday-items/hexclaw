package k12storage_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func TestWrongAssessmentDurablyReferencesSeparateParentGuideInvocation(t *testing.T) {
	store, _ := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "assessment-parent-guide")
	solveID, gradeID := successfulAssessmentInvocations(t, store, job.RecordID, attempt)

	input := itemInvocation(
		job.RecordID,
		attempt,
		k12.GradingItemOperationParentGuide,
		1,
	)
	input.InvocationID = "item-inv-" + job.RecordID + "-parent-guide"
	invocation, _, err := store.PrepareGradingItemInvocation(context.Background(), input)
	if err != nil {
		t.Fatalf("prepare parent guide: %v", err)
	}
	if _, err := store.MarkGradingItemInvocationSent(
		context.Background(),
		"mingming",
		invocation.InvocationID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkGradingItemInvocationSucceeded(
		context.Background(),
		"mingming",
		invocation.InvocationID,
		"sha256:parent-guide-result",
		`{"answer":"2"}`,
	); err != nil {
		t.Fatal(err)
	}

	receipt := assessmentReceipt(job.RecordID, attempt, solveID, gradeID)
	receipt.ParentGuideInvocationID = invocation.InvocationID
	stored, created, err := store.CommitGradingAssessmentItem(
		context.Background(),
		receipt,
		k12storage.GradingAssessmentEffects{},
	)
	if err != nil || !created {
		t.Fatalf("commit parent guide receipt: created=%v err=%v", created, err)
	}
	if stored.ParentGuideInvocationID != invocation.InvocationID {
		t.Fatalf("parent guide invocation ref lost: %+v", stored)
	}
	replayed, err := store.GetGradingAssessmentItem(
		context.Background(),
		"mingming",
		job.RecordID,
		attempt.ProblemID,
	)
	if err != nil || replayed.ParentGuideInvocationID != invocation.InvocationID {
		t.Fatalf("replay parent guide ref: item=%+v err=%v", replayed, err)
	}
}

func TestHistoricalWrongAssessmentWithoutParentGuideReferenceRemainsValid(t *testing.T) {
	store, _ := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "assessment-parent-guide-legacy")
	solveID, gradeID := successfulAssessmentInvocations(t, store, job.RecordID, attempt)
	legacy := assessmentReceipt(job.RecordID, attempt, solveID, gradeID)

	stored, created, err := store.CommitGradingAssessmentItem(
		context.Background(),
		legacy,
		k12storage.GradingAssessmentEffects{},
	)
	if err != nil || !created || stored.ParentGuideInvocationID != "" {
		t.Fatalf("legacy receipt compatibility: item=%+v created=%v err=%v", stored, created, err)
	}
}
