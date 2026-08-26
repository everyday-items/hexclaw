package knowledge

import (
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
		PageNumber: 1, PagesTotal: 1, SourceDigest: source.SHA256,
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
}
