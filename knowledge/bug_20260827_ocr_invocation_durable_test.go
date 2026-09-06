package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

func TestOCRPageInvocationClaimAndSuccessSurviveWorkerRestartWithoutDuplicate(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	if err := migrate.Run(ctx, db, []migrate.Migration{migrate.K12KnowledgeInvocationLedgersV91}); err != nil {
		t.Fatal(err)
	}
	body := "%PDF-1.7\ndurable invocation ledger"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "ocr-invocation-ledger",
		Filename:       "scan.pdf",
		MediaType:      "application/pdf",
		SizeBytes:      int64(len(body)),
		Body:           strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewSQLiteSemanticIndexRepository(db)
	now := time.Now().UTC()
	job, claimed, err := repo.ClaimNextJobForCorpus(ctx, "desktop-user", "default", "ocr-ledger-worker", now, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	source, err := repo.GetIngestDocument(ctx, "desktop-user", accepted.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	claim := OCRPageInvocationClaim{
		PageNumber: 1, PagesTotal: 2, SourceDigest: source.SHA256,
		RequestDigest: strings.Repeat("a", 64), Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
	}
	first, err := repo.ClaimOCRPageInvocation(ctx, job.Lease(), now, claim)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Fresh || first.Status != OCRPageInvocationStatusRunning || first.InvocationID == "" {
		t.Fatalf("first claim=%+v", first)
	}
	second, err := NewSQLiteSemanticIndexRepository(db).ClaimOCRPageInvocation(ctx, job.Lease(), now, claim)
	if err != nil {
		t.Fatal(err)
	}
	if second.Fresh || second.InvocationID != first.InvocationID {
		t.Fatalf("duplicate claim created a new invocation: first=%+v second=%+v", first, second)
	}
	if !errors.Is(ErrOCRPageInvocationOutcomeUnknown, ErrOCRPageInvocationOutcomeUnknown) {
		t.Fatal("sentinel must be available for parked invocation recovery")
	}
	result := OCRPageInvocationResult{
		Content: "第 1 页教材内容",
		RouteReceipt: OCRRouteReceipt{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
			Operation: OCRRouteOperationPDFPage, Status: OCRRouteStatusSucceeded,
		},
	}
	if err := repo.SaveOCRPageInvocation(ctx, job.Lease(), now, first, result); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteSemanticIndexRepository(db).ClaimOCRPageInvocation(ctx, job.Lease(), now, claim)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Fresh || restarted.Status != OCRPageInvocationStatusSucceeded ||
		restarted.Content != result.Content || restarted.RouteReceipt != result.RouteReceipt {
		t.Fatalf("restart did not reuse succeeded invocation: %+v", restarted)
	}
	claim.PageNumber = 2
	claim.RequestDigest = strings.Repeat("b", 64)
	unknown, err := repo.ClaimOCRPageInvocation(ctx, job.Lease(), now, claim)
	if err != nil {
		t.Fatal(err)
	}
	failedMarker, ok := any(repo).(interface {
		MarkOCRPageInvocationFailed(context.Context, JobLease, time.Time, OCRPageInvocation, string) error
	})
	if !ok {
		t.Fatal("OCR outcome marker cannot persist a definite failure")
	}
	if err := failedMarker.MarkOCRPageInvocationFailed(ctx, job.Lease(), now, unknown, "provider rejected: 429"); err != nil {
		t.Fatal(err)
	}
	retried, err := repo.ClaimOCRPageInvocation(ctx, job.Lease(), now, claim)
	if err != nil || !retried.Fresh || retried.InvocationID != unknown.InvocationID ||
		retried.Status != OCRPageInvocationStatusRunning || retried.JobID != job.JobID ||
		retried.SourceDigest != claim.SourceDigest || retried.RequestDigest != claim.RequestDigest {
		t.Fatalf("definite failed page did not resume its original invocation: %+v err=%v", retried, err)
	}
	const cause = "provider response body: unexpected EOF"
	if err := repo.MarkOCRPageInvocationOutcomeUnknown(ctx, job.Lease(), now, unknown, cause); err != nil {
		t.Fatal(err)
	}
	current, err := repo.GetJob(ctx, "desktop-user", job.JobID)
	if err != nil || !strings.Contains(current.LastError, cause) ||
		!strings.Contains(current.LastError, unknown.InvocationID) {
		t.Errorf("unknown OCR cause was lost: job=%+v err=%v", current, err)
	}
	if err := failedMarker.MarkOCRPageInvocationFailed(ctx, job.Lease(), now, unknown, "late failure"); !errors.Is(err, ErrJobFenced) {
		t.Fatalf("unknown invocation was changed into a retryable failure: err=%v", err)
	}
	if _, err := repo.FailJob(ctx, job.Lease(), now, current.LastError); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RetryDocument(ctx, "desktop-user", "default", accepted.DocumentID, "retry-unknown-page"); !errors.Is(err, ErrDocumentRetryNotAllowed) {
		t.Fatalf("replacement root bypassed unknown OCR invocation: err=%v", err)
	}
}
