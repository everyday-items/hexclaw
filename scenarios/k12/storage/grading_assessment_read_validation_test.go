package k12storage_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func TestGradingAssessmentReadPreservesSolveOnlyOutOfScopeAfterRestart(t *testing.T) {
	store, db := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "assessment-corrupt-restart")
	solveID, _ := successfulAssessmentInvocations(t, store, job.RecordID, attempt)
	receipt := assessmentReceipt(job.RecordID, attempt, solveID, "")
	receipt.Status = k12.GradingAssessmentOutOfScope
	receipt.ResultJSON = `{"status":"out_of_scope"}`
	receipt.ResultDigest = "sha256:out-of-scope"
	if _, created, err := store.CommitGradingAssessmentItem(
		context.Background(), receipt, k12storage.GradingAssessmentEffects{},
	); err != nil || !created {
		t.Fatalf("commit valid out-of-scope receipt: created=%v err=%v", created, err)
	}
	restarted := k12storage.NewStore(db, nil)
	if _, err := restarted.GetGradingAssessmentItem(
		context.Background(), receipt.AgentName, receipt.JobID, receipt.ProblemID,
	); err != nil {
		t.Fatalf("restarted single-item read rejected solve-only out-of-scope receipt: %v", err)
	}
	if _, err := restarted.ListGradingAssessmentItems(
		context.Background(), receipt.AgentName, receipt.JobID,
	); err != nil {
		t.Fatalf("restarted list read rejected solve-only out-of-scope receipt: %v", err)
	}
}
