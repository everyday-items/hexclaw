package k12storage_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestGenericPrintJobStorageFreezesArtifactAndScopesIdempotency(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	artifact := k12.PrintArtifact{
		ArtifactID: "part-a", AgentName: "mingming", SourceKind: k12.PrintSourceTutoringTips,
		SourceRef: "submission:s1", Title: "这份作业的辅导要点",
		CanonicalMarkdown: "# 辅导要点\n\n小数点对齐", SourceDigest: strings.Repeat("a", 64), CreatedAt: 100,
	}
	job := k12.GenericPrintJob{
		PrintJobID: "gprint-a", AgentName: "mingming", IdempotencyKey: "click-a",
		RequestDigest: strings.Repeat("b", 64), ArtifactID: artifact.ArtifactID, PreparedAt: 100,
	}
	first, replay, err := store.PrepareGenericPrintJob(ctx, artifact, job)
	if err != nil || replay || first.Status != k12.PrintJobPreparing || first.AttemptCount != 1 {
		t.Fatalf("first=%+v replay=%v err=%v", first, replay, err)
	}
	gotArtifact, err := store.GetPrintArtifact(ctx, "mingming", artifact.ArtifactID)
	if err != nil || gotArtifact.CanonicalMarkdown != artifact.CanonicalMarkdown {
		t.Fatalf("artifact=%+v err=%v", gotArtifact, err)
	}
	if _, err := store.GetPrintArtifact(ctx, "lele", artifact.ArtifactID); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("cross-owner artifact lookup must fail: %v", err)
	}
	second := job
	second.PrintJobID = "gprint-other"
	replayed, replay, err := store.PrepareGenericPrintJob(ctx, artifact, second)
	if err != nil || !replay || replayed.PrintJobID != first.PrintJobID {
		t.Fatalf("same key replay=%+v replayed=%v err=%v", replayed, replay, err)
	}
	refreshed := job
	refreshed.PrintJobID, refreshed.IdempotencyKey = "gprint-refresh", "fresh-key-after-reload"
	recovered, replay, err := store.PrepareGenericPrintJob(ctx, artifact, refreshed)
	if err != nil || !replay || recovered.PrintJobID != first.PrintJobID {
		t.Fatalf("new UI key must recover unresolved artifact job: recovered=%+v replay=%v err=%v", recovered, replay, err)
	}
	changedArtifact := artifact
	changedArtifact.CanonicalMarkdown = "# changed"
	changedArtifact.SourceDigest = strings.Repeat("c", 64)
	changedArtifact.ArtifactID = "part-b"
	changed := job
	changed.PrintJobID = "gprint-b"
	changed.RequestDigest = strings.Repeat("d", 64)
	changed.ArtifactID = changedArtifact.ArtifactID
	if _, _, err := store.PrepareGenericPrintJob(ctx, changedArtifact, changed); err == nil {
		t.Fatal("same owner/idempotency key with changed artifact must fail closed")
	}
}

func TestGenericPrintJobStorageRequiresReceiptAndBoundsRetry(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	artifact := k12.PrintArtifact{ArtifactID: "part-a", AgentName: "mingming", SourceKind: k12.PrintSourceCreativeObservation,
		SourceRef: "tutoring-tips:tips-1", Title: "辅导要点", CanonicalMarkdown: "# 辅导要点", SourceDigest: strings.Repeat("a", 64), CreatedAt: 100}
	job := k12.GenericPrintJob{PrintJobID: "gprint-a", AgentName: "mingming", IdempotencyKey: "click",
		RequestDigest: strings.Repeat("b", 64), ArtifactID: artifact.ArtifactID, PreparedAt: 100}
	if _, _, err := store.PrepareGenericPrintJob(ctx, artifact, job); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitGenericPrintJob(ctx, "mingming", job.PrintJobID, "", "", `{}`, 101); err == nil {
		t.Fatal("printed without native job/receipt must fail")
	}
	for attempt := 1; attempt < k12.MaxPrintAttempts; attempt++ {
		if _, err := store.AdvanceGenericPrintJob(ctx, "mingming", job.PrintJobID, k12.PrintJobFailed, "", "native_error", "redacted", 100+int64(attempt)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RetryGenericPrintJob(ctx, "mingming", job.PrintJobID, 110+int64(attempt)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AdvanceGenericPrintJob(ctx, "mingming", job.PrintJobID, k12.PrintJobFailed, "", "native_error", "redacted", 120); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RetryGenericPrintJob(ctx, "mingming", job.PrintJobID, 121); err == nil {
		t.Fatal("retry beyond bounded attempt count must fail")
	}
	unknownJob := job
	unknownJob.PrintJobID, unknownJob.IdempotencyKey = "gprint-unknown", "unknown"
	if _, _, err := store.PrepareGenericPrintJob(ctx, artifact, unknownJob); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdvanceGenericPrintJob(ctx, "mingming", unknownJob.PrintJobID, k12.PrintJobOutcomeUnknown, "", "receipt_lost", "redacted", 130); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RetryGenericPrintJob(ctx, "mingming", unknownJob.PrintJobID, 131); err == nil {
		t.Fatal("outcome_unknown must not permit blind retry")
	}
}

func TestGenericPrintCommitRequiresDialogBoundaryAndMatchesUnknownNativeJob(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	artifact := k12.PrintArtifact{ArtifactID: "part-boundary", AgentName: "mingming", SourceKind: k12.PrintSourceTutoringTips,
		SourceRef: "submission:boundary", Title: "辅导要点", CanonicalMarkdown: "# 卡片", SourceDigest: strings.Repeat("a", 64), CreatedAt: 100}
	job := k12.GenericPrintJob{PrintJobID: "gprint-boundary", AgentName: "mingming", IdempotencyKey: "boundary",
		RequestDigest: strings.Repeat("b", 64), ArtifactID: artifact.ArtifactID, PreparedAt: 100}
	if _, _, err := store.PrepareGenericPrintJob(ctx, artifact, job); err != nil {
		t.Fatal(err)
	}
	snapshot := `{"printer":"Office","paper":"A4"}`
	if _, err := store.CommitGenericPrintJob(ctx, "mingming", job.PrintJobID, "native-1", "receipt-1", snapshot, 101); err == nil {
		t.Fatal("preparing must not commit without a proven dialog_open boundary")
	}
	if _, err := store.AdvanceGenericPrintJob(ctx, "mingming", job.PrintJobID, k12.PrintJobDialogOpen, "", "", "", 102); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitGenericPrintJob(ctx, "mingming", job.PrintJobID, "native-1", "receipt-1", snapshot, 103); err != nil {
		t.Fatalf("dialog_open receipt should commit: %v", err)
	}
	if _, err := store.CommitGenericPrintJob(ctx, "mingming", job.PrintJobID, "native-1", "receipt-1", `{"paper":"A4","printer":"Office"}`, 104); err != nil {
		t.Fatalf("snapshot key order must not change receipt identity: %v", err)
	}
	if _, err := store.CommitGenericPrintJob(ctx, "mingming", job.PrintJobID, "native-1", "receipt-1", `{"printer":"Office","paper":"Letter"}`, 105); err == nil {
		t.Fatal("matching receipt IDs with a different snapshot must conflict")
	}

	unknownArtifact := artifact
	unknownArtifact.ArtifactID, unknownArtifact.SourceRef = "part-unknown-boundary", "submission:unknown-boundary"
	unknownJob := job
	unknownJob.PrintJobID, unknownJob.IdempotencyKey, unknownJob.ArtifactID = "gprint-unknown-boundary", "unknown-boundary", unknownArtifact.ArtifactID
	if _, _, err := store.PrepareGenericPrintJob(ctx, unknownArtifact, unknownJob); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdvanceGenericPrintJob(ctx, "mingming", unknownJob.PrintJobID, k12.PrintJobDialogOpen, "", "", "", 106); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdvanceGenericPrintJob(ctx, "mingming", unknownJob.PrintJobID, k12.PrintJobOutcomeUnknown, "native-known", "driver_result_ambiguous", "redacted", 107); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitGenericPrintJob(ctx, "mingming", unknownJob.PrintJobID, "native-other", "receipt-other", snapshot, 108); err == nil {
		t.Fatal("outcome_unknown must reject a receipt for a different native job")
	}
	if _, err := store.CommitGenericPrintJob(ctx, "mingming", unknownJob.PrintJobID, "native-known", "receipt-known", snapshot, 109); err != nil {
		t.Fatalf("matching reconciliation receipt should settle outcome_unknown: %v", err)
	}
}
