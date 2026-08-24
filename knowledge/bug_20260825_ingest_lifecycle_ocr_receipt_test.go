package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

type readyIngestLifecycleObserver struct {
	calls          int
	textState      string
	documentStatus string
	chunkCount     int
}

func (o *readyIngestLifecycleObserver) ReconcileDocumentIngestLifecycle(
	ctx context.Context,
	tx *sql.Tx,
	event DocumentIngestLifecycleEvent,
) error {
	o.calls++
	return tx.QueryRowContext(ctx, `SELECT b.text_state,d.status,
		(SELECT COUNT(*) FROM kb_chunks WHERE doc_id=d.id)
		FROM kb_semantic_document_bindings b
		JOIN kb_documents d ON d.id=b.document_id AND d.corpus_uid=b.corpus_uid
		WHERE b.owner_id=? AND b.corpus_uid=? AND b.document_id=?
		  AND b.content_generation=?`, event.OwnerID, event.CorpusUID,
		event.DocumentID, event.DocumentGeneration).Scan(
		&o.textState, &o.documentStatus, &o.chunkCount,
	)
}

func TestCompleteIngestDocumentNotifiesObserverAfterTextReady(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	body := "durable textbook lifecycle content"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "textbook-lifecycle-success",
		Filename:       "textbook.md",
		MediaType:      "text/markdown",
		SizeBytes:      int64(len(body)),
		Body:           strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}

	observer := &readyIngestLifecycleObserver{}
	repository := NewSQLiteSemanticIndexRepository(db)
	repository.SetDocumentIngestLifecycleObserver(observer)
	clock := time.Now().UTC()
	worker := NewSemanticIndexWorker(repository, nil, SemanticIndexWorkerConfig{
		OwnerID: "desktop-user", CorpusID: "default", WorkerID: "lifecycle-worker",
		LeaseDuration: time.Minute, RetryDelay: time.Second, Now: func() time.Time { return clock },
	})
	worker.SetDocumentIngestProcessor(deterministicIngestProcessor{})
	worked, err := worker.RunOnce(ctx)
	if err != nil || !worked {
		t.Fatalf("RunOnce worked=%v err=%v", worked, err)
	}
	if observer.calls != 1 {
		t.Fatalf("successful ingest observer calls=%d, want 1 before any options read", observer.calls)
	}
	if observer.textState != string(TextIndexReady) || observer.documentStatus != "indexed" ||
		observer.chunkCount != 1 {
		t.Fatalf("observer transaction facts text/status/chunks=%q/%q/%d, want ready/indexed/1",
			observer.textState, observer.documentStatus, observer.chunkCount)
	}
	job, err := service.GetJob(ctx, "desktop-user", accepted.JobID)
	if err != nil || job.State != KnowledgeJobSucceeded {
		t.Fatalf("terminal job=%+v err=%v", job, err)
	}
}

func TestOCRPageCheckpointRequiresDurableRouteReceipt(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	body := "%PDF-1.7\nscanned textbook page"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "ocr-route-receipt-required",
		Filename:       "scanned-textbook.pdf",
		MediaType:      "application/pdf",
		SizeBytes:      int64(len(body)),
		Body:           strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteSemanticIndexRepository(db)
	now := time.Now().UTC()
	job, claimed, err := repository.ClaimNextJobForCorpus(
		ctx, "desktop-user", "default", "ocr-receipt-worker", now, time.Minute,
	)
	if err != nil || !claimed || job.JobID != accepted.JobID {
		t.Fatalf("claim=%v job=%+v err=%v", claimed, job, err)
	}
	source, err := repository.GetIngestDocument(ctx, "desktop-user", accepted.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetIngestPageTotal(ctx, job.Lease(), now, source.SHA256, 1); err != nil {
		t.Fatal(err)
	}
	err = repository.SaveIngestPageCheckpoint(ctx, job.Lease(), now, IngestPageCheckpoint{
		PageNumber: 1, PagesTotal: 1, SourceDigest: source.SHA256,
		ExtractionMode: "ocr_vlm", Content: "第 1 页教材转写",
	})
	if !errors.Is(err, ErrInvalidDocumentUpload) {
		t.Fatalf("OCR checkpoint without route receipt err=%v, want ErrInvalidDocumentUpload", err)
	}
	var checkpoints int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_ingest_page_checkpoints
		WHERE job_id=?`, job.JobID).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if checkpoints != 0 {
		t.Fatalf("receipt-less OCR page persisted checkpoints=%d, want 0", checkpoints)
	}
}

func TestTextPageCheckpointRejectsOCRRouteReceipt(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	repository, job, source := createClaimedOCRJob(t, db, service, ctx, "text-page-receipt-forbidden")
	now := time.Now().UTC()
	if err := repository.SetIngestPageTotal(ctx, job.Lease(), now, source.SHA256, 1); err != nil {
		t.Fatal(err)
	}
	err := repository.SaveIngestPageCheckpoint(ctx, job.Lease(), now, IngestPageCheckpoint{
		PageNumber: 1, PagesTotal: 1, SourceDigest: source.SHA256,
		ExtractionMode: "text", Content: "real PDF text layer",
		OCRRouteReceipt: testOCRRouteReceipt(),
	})
	if !errors.Is(err, ErrInvalidDocumentUpload) {
		t.Fatalf("text page accepted OCR route receipt err=%v", err)
	}
}

func TestOCRPageRouteReceiptMustMatchFrozenExecutionRoute(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	route := testOCRVisionRoute()
	service.ConfigureVisionRouteResolver(VisionRouteSnapshotResolverFunc(
		func(context.Context) (VisionRouteSnapshot, error) { return route, nil },
	))
	repository, job, source := createClaimedOCRJob(t, db, service, ctx, "ocr-route-mismatch")
	now := time.Now().UTC()
	if err := repository.SetIngestPageTotal(ctx, job.Lease(), now, source.SHA256, 1); err != nil {
		t.Fatal(err)
	}
	err := repository.SaveIngestPageCheckpoint(ctx, job.Lease(), now, IngestPageCheckpoint{
		PageNumber: 1, PagesTotal: 1, SourceDigest: source.SHA256,
		ExtractionMode: "ocr_vlm", Content: "第 1 页教材转写",
		OCRRouteReceipt: &OCRRouteReceipt{
			Provider: "different-provider", Model: route.Model,
			Operation: "knowledge_pdf_page_ocr", Status: "succeeded", Fake: false,
		},
	})
	if !errors.Is(err, ErrInvalidDocumentUpload) {
		t.Fatalf("mismatched frozen OCR route err=%v, want ErrInvalidDocumentUpload", err)
	}
	var checkpoints int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_ingest_page_checkpoints
		WHERE job_id=?`, job.JobID).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if checkpoints != 0 {
		t.Fatalf("mismatched route persisted checkpoints=%d, want 0", checkpoints)
	}
}

func TestSuccessfulOCRPageReceiptIsDurableAndPublic(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	route := testOCRVisionRoute()
	service.ConfigureVisionRouteResolver(VisionRouteSnapshotResolverFunc(
		func(context.Context) (VisionRouteSnapshot, error) { return route, nil },
	))
	repository, job, source := createClaimedOCRJob(t, db, service, ctx, "ocr-route-public")
	now := time.Now().UTC()
	if err := repository.SetIngestPageTotal(ctx, job.Lease(), now, source.SHA256, 1); err != nil {
		t.Fatal(err)
	}
	content := "第 1 页教材文字与公式 a ÷ b = a/b"
	if err := repository.SaveIngestPageCheckpoint(ctx, job.Lease(), now, IngestPageCheckpoint{
		PageNumber: 1, PagesTotal: 1, SourceDigest: source.SHA256,
		ExtractionMode: "ocr_vlm", Content: content,
		OCRRouteReceipt: &OCRRouteReceipt{
			Provider: route.ProviderName, Model: route.Model,
			Operation: "knowledge_pdf_page_ocr", Status: "succeeded", Fake: false,
		},
	}); err != nil {
		t.Fatal(err)
	}
	prepared := preparedOnePageOCRDocument(job.DocumentID, source.SHA256, content)
	if err := repository.CompleteIngestDocument(ctx, job.Lease(), now, prepared); err != nil {
		t.Fatal(err)
	}
	projection, err := service.GetIngestDocumentProjectionForCorpus(
		ctx, "desktop-user", "default", job.DocumentID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.OCRPageReceipts) != 1 {
		t.Fatalf("public OCR receipts=%+v, want one page", projection.OCRPageReceipts)
	}
	receipt := projection.OCRPageReceipts[0]
	if receipt.PageNumber != 1 || receipt.PagesTotal != 1 ||
		receipt.Provider != route.ProviderName || receipt.Model != route.Model ||
		receipt.Operation != "knowledge_pdf_page_ocr" || receipt.Status != "succeeded" ||
		receipt.Fake || receipt.SourceDigest != source.SHA256 ||
		receipt.ContentDigest != ingestPageContentDigest(content) {
		t.Fatalf("public OCR receipt=%+v", receipt)
	}
	publicJob, err := service.GetJobForCorpus(
		ctx, "desktop-user", "default", job.JobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(publicJob.OCRPageReceipts) != 1 || publicJob.OCRPageReceipts[0] != receipt {
		t.Fatalf("public job OCR receipts=%+v want=%+v", publicJob.OCRPageReceipts, receipt)
	}
}

func TestFakeOCRPageReceiptCannotPublishReadyDocument(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	route := testOCRVisionRoute()
	service.ConfigureVisionRouteResolver(VisionRouteSnapshotResolverFunc(
		func(context.Context) (VisionRouteSnapshot, error) { return route, nil },
	))
	repository, job, source := createClaimedOCRJob(t, db, service, ctx, "ocr-route-fake")
	now := time.Now().UTC()
	if err := repository.SetIngestPageTotal(ctx, job.Lease(), now, source.SHA256, 1); err != nil {
		t.Fatal(err)
	}
	content := "synthetic OCR output"
	if err := repository.SaveIngestPageCheckpoint(ctx, job.Lease(), now, IngestPageCheckpoint{
		PageNumber: 1, PagesTotal: 1, SourceDigest: source.SHA256,
		ExtractionMode: "ocr_vlm", Content: content,
		OCRRouteReceipt: &OCRRouteReceipt{
			Provider: route.ProviderName, Model: route.Model,
			Operation: "knowledge_pdf_page_ocr", Status: "succeeded", Fake: true,
		},
	}); err != nil {
		t.Fatal(err)
	}
	err := repository.CompleteIngestDocument(
		ctx, job.Lease(), now, preparedOnePageOCRDocument(job.DocumentID, source.SHA256, content),
	)
	if !errors.Is(err, ErrInvalidDocumentUpload) {
		t.Fatalf("fake OCR completion err=%v, want ErrInvalidDocumentUpload", err)
	}
	projection, err := service.GetIngestDocumentProjectionForCorpus(
		ctx, "desktop-user", "default", job.DocumentID,
	)
	if err != nil || projection.TextIndexState == TextIndexReady {
		t.Fatalf("fake OCR leaked ready projection=%+v err=%v", projection, err)
	}
}

func TestRestartRejectsOCRReceiptThatDoesNotMatchFrozenRoute(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	repository, job, source := createClaimedOCRJob(t, db, service, ctx, "ocr-route-tampered-restart")
	now := time.Now().UTC()
	insertTamperedOCRCheckpoint(t, db, repository, ctx, job, source, now)
	_, err := repository.LoadIngestPageCheckpoints(
		ctx, job.Lease(), now, source.SHA256, 1,
	)
	if !errors.Is(err, ErrInvalidDocumentUpload) {
		t.Fatalf("restart loaded mismatched durable OCR receipt err=%v", err)
	}
}

func TestCompletionRejectsOCRReceiptThatDoesNotMatchFrozenRoute(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	repository, job, source := createClaimedOCRJob(t, db, service, ctx, "ocr-route-tampered-complete")
	now := time.Now().UTC()
	content := insertTamperedOCRCheckpoint(t, db, repository, ctx, job, source, now)
	err := repository.CompleteIngestDocument(
		ctx, job.Lease(), now, preparedOnePageOCRDocument(job.DocumentID, source.SHA256, content),
	)
	if !errors.Is(err, ErrInvalidDocumentUpload) {
		t.Fatalf("completion accepted mismatched durable OCR receipt err=%v", err)
	}
}

func insertTamperedOCRCheckpoint(
	t *testing.T,
	db *sql.DB,
	repository *SQLiteSemanticIndexRepository,
	ctx context.Context,
	job KnowledgeJob,
	source PersistedIngestDocument,
	now time.Time,
) string {
	t.Helper()
	if err := repository.SetIngestPageTotal(ctx, job.Lease(), now, source.SHA256, 1); err != nil {
		t.Fatal(err)
	}
	content := "tampered durable OCR"
	contentDigest := ingestPageContentDigest(content)
	nowMillis := now.UnixMilli()
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_ingest_page_checkpoints
		(job_id,page_number,pages_total,source_digest,extraction_mode,content,content_digest,
		 source_offset_start,source_offset_end,lease_epoch,created_at,updated_at)
		VALUES(?,1,1,?,'ocr_vlm',?,?,0,0,?,?,?)`, job.JobID, source.SHA256,
		content, contentDigest, job.LeaseEpoch, nowMillis, nowMillis); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_ingest_page_route_receipts
		(job_id,page_number,pages_total,provider,model,operation,status,source_digest,
		 content_digest,fake,created_at)
		VALUES(?,1,1,'different-provider','gpt-5.6-sol',?,? ,?,?,0,?)`, job.JobID,
		OCRRouteOperationPDFPage, OCRRouteStatusSucceeded, source.SHA256,
		contentDigest, nowMillis); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE kb_knowledge_jobs SET pages_done=1
		WHERE job_id=?`, job.JobID); err != nil {
		t.Fatal(err)
	}
	return content
}

func createClaimedOCRJob(
	t *testing.T,
	db *sql.DB,
	service *SemanticIndexService,
	ctx context.Context,
	idempotencyKey string,
) (*SQLiteSemanticIndexRepository, KnowledgeJob, PersistedIngestDocument) {
	t.Helper()
	body := "%PDF-1.7\nscanned textbook"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: idempotencyKey, Filename: "scanned.pdf",
		MediaType: "application/pdf", SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteSemanticIndexRepository(db)
	job, claimed, err := repository.ClaimNextJobForCorpus(
		ctx, "desktop-user", "default", idempotencyKey+"-worker", time.Now().UTC(), time.Minute,
	)
	if err != nil || !claimed || job.JobID != accepted.JobID {
		t.Fatalf("claim=%v job=%+v err=%v", claimed, job, err)
	}
	source, err := repository.GetIngestDocument(ctx, "desktop-user", accepted.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	return repository, job, source
}

func testOCRVisionRoute() VisionRouteSnapshot {
	return VisionRouteSnapshot{
		ProviderInstanceID: "provider-instance-1", ProviderName: "hexclaw-gpt",
		ProviderDisplayName: "HexClaw-GPT", Model: "gpt-5.6-sol",
		Capabilities: []string{"text", "vision"},
	}.Canonical()
}

func testOCRRouteReceipt() *OCRRouteReceipt {
	route := testOCRVisionRoute()
	return &OCRRouteReceipt{
		Provider: route.ProviderName, Model: route.Model,
		Operation: OCRRouteOperationPDFPage, Status: OCRRouteStatusSucceeded, Fake: false,
	}
}

func preparedOnePageOCRDocument(documentID, sourceDigest, content string) PreparedIngestDocument {
	now := time.Now().UTC()
	return PreparedIngestDocument{
		Document: &Document{ID: documentID, Content: content, Status: "indexed", CreatedAt: now, UpdatedAt: now},
		Chunks: []*Chunk{{
			ID: documentID + "-chunk-0", DocID: documentID, Content: content,
			PageStart: 1, PageEnd: 1, SourceDigest: sourceDigest,
			SourceOffsetStart: 0, SourceOffsetEnd: int64(len(content)),
		}},
		PageCount: 1, Warnings: []string{},
	}
}

func TestCaptionImageWithRouteReceiptRejectsUnreceiptedCaptioner(t *testing.T) {
	manager := NewManager(stubRepo{}, stubSearcher{}, nil, WithCaptioner(CaptionerFunc(
		func(context.Context, []byte, string) (string, error) {
			return "legacy caption", nil
		},
	)))
	_, err := manager.CaptionImageWithRouteReceipt(
		context.Background(), []byte("page"), "image/png",
	)
	if !errors.Is(err, ErrInvalidDocumentUpload) {
		t.Fatalf("unreceipted OCR adapter err=%v, want ErrInvalidDocumentUpload", err)
	}
}

func TestCaptionImageWithRouteReceiptPreservesExecutionFacts(t *testing.T) {
	want := OCRRouteReceipt{
		Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
		Operation: OCRRouteOperationPDFPage, Status: OCRRouteStatusSucceeded, Fake: false,
	}
	manager := NewManager(stubRepo{}, stubSearcher{}, nil, WithCaptioner(CaptionerWithReceiptFunc(
		func(context.Context, []byte, string) (CaptionResult, error) {
			return CaptionResult{Content: "  公式 a ÷ b = a/b  ", RouteReceipt: want}, nil
		},
	)))
	result, err := manager.CaptionImageWithRouteReceipt(
		context.Background(), []byte("page"), "image/png",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "公式 a ÷ b = a/b" || result.RouteReceipt != want {
		t.Fatalf("caption result=%+v want content+receipt=%+v", result, want)
	}
}
