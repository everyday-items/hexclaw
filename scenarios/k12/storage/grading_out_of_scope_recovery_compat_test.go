package k12storage_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func TestOutOfScopeAssessmentWithoutProviderCallSurvivesStorageRestart(t *testing.T) {
	store, db := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "out-of-scope-recovery-compat")
	receipt := assessmentReceipt(job.RecordID, attempt, "", "")
	receipt.Status = k12.GradingAssessmentOutOfScope
	receipt.ResultJSON = `{"status":"out_of_scope","source":"curriculum_constraint"}`
	receipt.ResultDigest = "sha256:out-of-scope-curriculum-constraint"

	stored, created, err := store.CommitGradingAssessmentItem(
		context.Background(),
		receipt,
		k12storage.GradingAssessmentEffects{},
	)
	if err != nil || !created {
		t.Fatalf("commit deterministic zero-call out-of-scope receipt: created=%v err=%v", created, err)
	}
	if stored.SolveInvocationID != "" || stored.GradeInvocationID != "" {
		t.Fatalf("zero-call receipt fabricated invocation references: %+v", stored)
	}

	restarted := k12storage.NewStore(db, nil)
	reloaded, err := restarted.GetGradingAssessmentItem(
		context.Background(), receipt.AgentName, receipt.JobID, receipt.ProblemID,
	)
	if err != nil {
		t.Fatalf("restart read deterministic out-of-scope receipt: %v", err)
	}
	if reloaded.ResultDigest != receipt.ResultDigest ||
		reloaded.SolveInvocationID != "" || reloaded.GradeInvocationID != "" {
		t.Fatalf("restart read changed deterministic receipt: %+v", reloaded)
	}
	items, err := restarted.ListGradingAssessmentItems(
		context.Background(), receipt.AgentName, receipt.JobID,
	)
	if err != nil || len(items) != 1 || items[0].ResultDigest != receipt.ResultDigest {
		t.Fatalf("restart list deterministic receipt: items=%+v err=%v", items, err)
	}
}
