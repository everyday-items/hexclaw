package k12storage_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestPracticePrintCommitRequiresDialogBoundaryAndMatchesUnknownNativeJob(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	fields := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceManual,
		Title:      "手工卷",
		Items: []k12.PracticeItem{{
			ItemID: "q1", Subject: "数学", QuestionMarkdown: "1+1=?", ExpectedAnswerMarkdown: "2",
			VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算",
		}},
	}
	set, err := k12.NewPracticeSetRecord("mingming", "print-boundary", fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, set); err != nil {
		t.Fatal(err)
	}
	job := k12.PracticePrintJob{
		PrintJobID: "print-boundary", AgentName: "mingming", IdempotencyKey: "boundary",
		RequestDigest: strings.Repeat("a", 64), PracticeSetID: set.RecordID, BaseSetVersion: set.Version,
		ArtifactKind: k12.PaperKindQuestion, ArtifactID: "question-boundary",
		QuestionArtifactID: "question-boundary", AnswerArtifactID: "answer-boundary", PreparedAt: 100,
	}
	if _, _, err := store.PreparePracticePrintJob(ctx, job, fields); err != nil {
		t.Fatal(err)
	}
	snapshot := `{"printer":"Office","paper":"A4"}`
	if _, err := store.CommitPracticePrintJob(ctx, "mingming", job.PrintJobID, "native-known", "receipt-known", snapshot, 101); err == nil {
		t.Fatal("preparing must not commit without a proven dialog_open boundary")
	}
	if _, err := store.AdvancePracticePrintJob(ctx, "mingming", job.PrintJobID, k12.PrintJobDialogOpen, "", "", "", 102); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdvancePracticePrintJob(ctx, "mingming", job.PrintJobID, k12.PrintJobOutcomeUnknown, "native-known", "driver_result_ambiguous", "redacted", 103); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitPracticePrintJob(ctx, "mingming", job.PrintJobID, "native-other", "receipt-other", snapshot, 104); err == nil {
		t.Fatal("outcome_unknown must reject a receipt for a different native job")
	}
	committed, err := store.CommitPracticePrintJob(ctx, "mingming", job.PrintJobID, "native-known", "receipt-known", snapshot, 105)
	if err != nil || committed.Status != k12.PrintJobPrinted {
		t.Fatalf("matching reconciliation receipt should settle outcome_unknown: job=%+v err=%v", committed, err)
	}
	if _, err := store.CommitPracticePrintJob(ctx, "mingming", job.PrintJobID, "native-known", "receipt-known", `{"paper":"A4","printer":"Office"}`, 106); err != nil {
		t.Fatalf("snapshot key order must not change receipt identity: %v", err)
	}
	if _, err := store.CommitPracticePrintJob(ctx, "mingming", job.PrintJobID, "native-known", "receipt-known", `{"printer":"Office","paper":"Letter"}`, 107); err == nil {
		t.Fatal("matching receipt IDs with a different snapshot must conflict")
	}
}
