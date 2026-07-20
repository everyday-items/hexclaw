package knowledge

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func failNextKnowledgeJob(t *testing.T, repository *SQLiteSemanticIndexRepository, ownerID, corpusID, workerID string) KnowledgeJob {
	t.Helper()
	now := time.Now().UTC()
	job, claimed, err := repository.ClaimNextJobForCorpus(
		context.Background(), ownerID, corpusID, workerID, now, time.Minute,
	)
	if err != nil || !claimed {
		t.Fatalf("claim next job: claimed=%v err=%v", claimed, err)
	}
	failed, err := repository.FailJob(context.Background(), job.Lease(), now.Add(time.Second), "fixture failure")
	if err != nil {
		t.Fatalf("fail claimed job: %v", err)
	}
	return failed
}

type countingIngestProcessor struct{ calls atomic.Int32 }

func (p *countingIngestProcessor) Prepare(ctx context.Context, source PersistedIngestDocument) (PreparedIngestDocument, error) {
	p.calls.Add(1)
	return deterministicIngestProcessor{}.Prepare(ctx, source)
}

func TestRetryFailedIngestCreatesIndependentRootAndPublishesExactlyOnce(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	repository := NewSQLiteSemanticIndexRepository(db)
	body := "retry the immutable source"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "original-upload", Filename: "retry.txt", MediaType: "text/plain",
		SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	failed := failNextKnowledgeJob(t, repository, "desktop-user", "default", "first-worker")
	if failed.JobID != accepted.JobID || failed.Kind != KnowledgeJobIngest || failed.State != KnowledgeJobFailed {
		t.Fatalf("failed original job=%+v", failed)
	}

	retry, err := service.RetryDocument(ctx, "desktop-user", "default", accepted.DocumentID, "retry-click-1")
	if err != nil {
		t.Fatal(err)
	}
	if retry.DocumentID != accepted.DocumentID || retry.JobID == accepted.JobID ||
		retry.TextIndexState != TextIndexPending {
		t.Fatalf("retry result=%+v original=%+v", retry, accepted)
	}
	replayed, err := service.RetryDocument(ctx, "desktop-user", "default", accepted.DocumentID, "retry-click-1")
	if err != nil || replayed != retry {
		t.Fatalf("retry replay=%+v err=%v want=%+v", replayed, err, retry)
	}
	if _, err := service.RetryDocument(ctx, "desktop-user", "default", accepted.DocumentID, "retry-click-2"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("parallel retry error=%v, want ErrIdempotencyConflict", err)
	}

	var oldState, newState, documentStatus, textState string
	var oldGeneration, newGeneration, sourceRows int64
	if err := db.QueryRowContext(ctx, `SELECT state,document_generation FROM kb_knowledge_jobs WHERE job_id=?`, accepted.JobID).
		Scan(&oldState, &oldGeneration); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT state,document_generation FROM kb_knowledge_jobs WHERE job_id=?`, retry.JobID).
		Scan(&newState, &newGeneration); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM kb_documents WHERE id=?`, accepted.DocumentID).
		Scan(&documentStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT text_state FROM kb_semantic_document_bindings WHERE document_id=?`, accepted.DocumentID).
		Scan(&textState); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_ingest_document_sources
		WHERE document_id=? AND content_generation=1`, accepted.DocumentID).Scan(&sourceRows); err != nil {
		t.Fatal(err)
	}
	if oldState != string(KnowledgeJobFailed) || newState != string(KnowledgeJobQueued) ||
		oldGeneration != 1 || newGeneration != 1 || sourceRows != 1 ||
		documentStatus != "processing" || textState != string(TextIndexPending) {
		t.Fatalf("old=%s/%d new=%s/%d sources=%d document=%s text=%s",
			oldState, oldGeneration, newState, newGeneration, sourceRows, documentStatus, textState)
	}

	processor := &countingIngestProcessor{}
	worker := NewSemanticIndexWorker(repository, nil, SemanticIndexWorkerConfig{
		OwnerID: "desktop-user", CorpusID: "default", WorkerID: "retry-worker", LeaseDuration: time.Minute,
	})
	worker.SetDocumentIngestProcessor(processor)
	if worked, err := worker.RunOnce(ctx); err != nil || !worked {
		t.Fatalf("retry worker worked=%v err=%v", worked, err)
	}
	afterSuccess, err := service.RetryDocument(ctx, "desktop-user", "default", accepted.DocumentID, "retry-click-1")
	if err != nil || afterSuccess.JobID != retry.JobID || afterSuccess.TextIndexState != TextIndexReady {
		t.Fatalf("successful retry replay=%+v err=%v", afterSuccess, err)
	}
	var ingestJobs, chunks int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE document_id=? AND kind='ingest'`, accepted.DocumentID).Scan(&ingestJobs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_chunks WHERE doc_id=?`, accepted.DocumentID).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if processor.calls.Load() != 1 || ingestJobs != 2 || chunks != 1 {
		t.Fatalf("processor calls=%d ingest jobs=%d chunks=%d", processor.calls.Load(), ingestJobs, chunks)
	}
}

func TestRetryCancelledIngestRequiresReupload(t *testing.T) {
	_, service, ctx := newAsyncIngestHarness(t)
	body := "cancelled retry source"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "cancel-before-retry", Filename: "cancelled.txt", MediaType: "text/plain",
		SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CancelJob(ctx, "desktop-user", accepted.JobID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RetryDocument(ctx, "desktop-user", "default", accepted.DocumentID, "retry-cancelled"); !errors.Is(err, ErrDocumentRetryRequiresReupload) {
		t.Fatalf("cancelled retry error=%v, want ErrDocumentRetryRequiresReupload", err)
	}
}

func TestConcurrentRetryReplayCreatesOneRootJob(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	repository := NewSQLiteSemanticIndexRepository(db)
	body := "concurrent durable retry"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "concurrent-original", Filename: "concurrent.txt", MediaType: "text/plain",
		SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	failNextKnowledgeJob(t, repository, "desktop-user", "default", "failure-worker")

	start := make(chan struct{})
	results := make([]CreateDocumentResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			results[index], errs[index] = service.RetryDocument(
				ctx, "desktop-user", "default", accepted.DocumentID, "same-double-click",
			)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent retry %d: %v", i, err)
		}
	}
	if results[0] != results[1] {
		t.Fatalf("concurrent replay diverged: %+v / %+v", results[0], results[1])
	}
	var retryRoots int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE document_id=? AND kind='ingest' AND idempotency_key LIKE 'document-retry|%'`,
		accepted.DocumentID).Scan(&retryRoots); err != nil {
		t.Fatal(err)
	}
	if retryRoots != 1 {
		t.Fatalf("concurrent retry roots=%d", retryRoots)
	}
}

func TestRetryFailedEmbedChildQueuesOnlyEmbeddingWithoutOCR(t *testing.T) {
	h := newSemanticMutationHarness(t)
	if err := h.service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}
	body := "text succeeds before embedding fails"
	accepted, err := h.service.CreateDocument(h.ctx, "owner-1", "default", CreateDocumentInput{
		IdempotencyKey: "embed-original", Filename: "embed.txt", MediaType: "text/plain",
		SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	ingestWorker := NewSemanticIndexWorker(h.repo, nil, SemanticIndexWorkerConfig{
		OwnerID: "owner-1", CorpusID: "default", WorkerID: "ingest-worker", LeaseDuration: time.Minute,
	})
	ingestWorker.SetDocumentIngestProcessor(deterministicIngestProcessor{})
	if worked, err := ingestWorker.RunOnce(h.ctx); err != nil || !worked {
		t.Fatalf("complete ingest worked=%v err=%v", worked, err)
	}
	failedChild := failNextKnowledgeJob(t, h.repo, "owner-1", "default", "embed-worker")
	if failedChild.Kind != KnowledgeJobEmbedDocument || failedChild.State != KnowledgeJobFailed {
		t.Fatalf("failed child=%+v", failedChild)
	}

	retry, err := h.service.RetryDocument(h.ctx, "owner-1", "default", accepted.DocumentID, "retry-embed-1")
	if err != nil {
		t.Fatal(err)
	}
	if retry.DocumentID != accepted.DocumentID || retry.JobID == accepted.JobID || retry.JobID == failedChild.JobID ||
		retry.TextIndexState != TextIndexReady || retry.VectorIndexState != VectorIndexPending {
		t.Fatalf("embed retry=%+v", retry)
	}
	replayed, err := h.service.RetryDocument(h.ctx, "owner-1", "default", accepted.DocumentID, "retry-embed-1")
	if err != nil || replayed != retry {
		t.Fatalf("embed replay=%+v err=%v", replayed, err)
	}
	if _, err := h.service.RetryDocument(h.ctx, "owner-1", "default", accepted.DocumentID, "retry-embed-2"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("parallel embed retry error=%v, want conflict", err)
	}

	var kind, state, parentID, targetRevision, documentStatus, textState, vectorState string
	if err := h.db.QueryRowContext(h.ctx, `SELECT kind,state,parent_job_id,target_revision_id
		FROM kb_knowledge_jobs WHERE job_id=?`, retry.JobID).
		Scan(&kind, &state, &parentID, &targetRevision); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT status FROM kb_documents WHERE id=?`, accepted.DocumentID).
		Scan(&documentStatus); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT text_state FROM kb_semantic_document_bindings WHERE document_id=?`, accepted.DocumentID).
		Scan(&textState); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT vector_state FROM kb_revision_documents
		WHERE revision_id=? AND document_id=? AND content_generation=1`, h.active, accepted.DocumentID).
		Scan(&vectorState); err != nil {
		t.Fatal(err)
	}
	var ingestJobs, embedJobs int
	_ = h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs WHERE document_id=? AND kind='ingest'`, accepted.DocumentID).Scan(&ingestJobs)
	_ = h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs WHERE document_id=? AND kind='embed_document'`, accepted.DocumentID).Scan(&embedJobs)
	if kind != string(KnowledgeJobEmbedDocument) || state != string(KnowledgeJobQueued) ||
		parentID != accepted.JobID || targetRevision != h.active || documentStatus != "indexed" ||
		textState != string(TextIndexReady) || vectorState != string(VectorIndexPending) ||
		ingestJobs != 1 || embedJobs != 2 {
		t.Fatalf("kind=%s state=%s parent=%s target=%s doc=%s text=%s vector=%s ingest=%d embed=%d",
			kind, state, parentID, targetRevision, documentStatus, textState, vectorState, ingestJobs, embedJobs)
	}
	processor := &countingIngestProcessor{}
	embedWorker := NewSemanticIndexWorker(h.repo, nil, SemanticIndexWorkerConfig{
		OwnerID: "owner-1", CorpusID: "default", WorkerID: "embed-retry-worker", LeaseDuration: time.Minute,
	})
	embedWorker.SetDocumentIngestProcessor(processor)
	worked, err := embedWorker.RunOnce(h.ctx)
	if !worked || err == nil {
		t.Fatalf("embedding retry without registry worked=%v err=%v", worked, err)
	}
	if processor.calls.Load() != 0 {
		t.Fatalf("embedding-only retry invoked extraction/OCR processor %d time(s)", processor.calls.Load())
	}
}

func TestRetryOutcomeUnknownEmbedChildRequiresReconciliation(t *testing.T) {
	h := newSemanticMutationHarness(t)
	if err := h.service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}
	body := "provider may have accepted this embedding request"
	accepted, err := h.service.CreateDocument(h.ctx, "owner-1", "default", CreateDocumentInput{
		IdempotencyKey: "outcome-unknown-original", Filename: "unknown.txt", MediaType: "text/plain",
		SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	ingestWorker := NewSemanticIndexWorker(h.repo, nil, SemanticIndexWorkerConfig{
		OwnerID: "owner-1", CorpusID: "default", WorkerID: "outcome-unknown-ingest", LeaseDuration: time.Minute,
	})
	ingestWorker.SetDocumentIngestProcessor(deterministicIngestProcessor{})
	if worked, err := ingestWorker.RunOnce(h.ctx); err != nil || !worked {
		t.Fatalf("complete ingest worked=%v err=%v", worked, err)
	}
	firstFailed := failNextKnowledgeJob(t, h.repo, "owner-1", "default", "first-embed-failure")
	if firstFailed.Kind != KnowledgeJobEmbedDocument {
		t.Fatalf("first failed child=%+v", firstFailed)
	}
	firstRetry, err := h.service.RetryDocument(
		h.ctx, "owner-1", "default", accepted.DocumentID, "retry-before-outcome-unknown",
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_320_000, 0).UTC()
	job, claimed, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "outcome-unknown-worker", now, time.Minute,
	)
	if err != nil || !claimed || job.Kind != KnowledgeJobEmbedDocument || job.JobID != firstRetry.JobID {
		t.Fatalf("claim embed child: job=%+v claimed=%v err=%v", job, claimed, err)
	}
	plan, err := h.repo.LoadJobExecutionPlan(h.ctx, job.Lease(), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := h.repo.ListRevisionChunkInputs(h.ctx, job.Lease(), now.Add(2*time.Second), nil, 10)
	if err != nil || len(inputs) == 0 {
		t.Fatalf("list embed inputs: len=%d err=%v", len(inputs), err)
	}
	texts := make([]string, len(inputs))
	for i := range inputs {
		texts[i] = inputs[i].Content
	}
	manifest, err := h.repo.CreateEmbeddingBatchManifest(
		h.ctx,
		job.Lease(),
		now.Add(3*time.Second),
		makeEmbeddingBatchManifest(
			job.JobID,
			plan,
			inputs,
			texts,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.repo.BeginEmbeddingBatch(
		h.ctx, job.Lease(), now.Add(4*time.Second), manifest.BatchID,
	); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.MarkEmbeddingBatchOutcomeUnknown(
		h.ctx, job.Lease(), now.Add(5*time.Second), manifest.BatchID, "provider response unknown",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.FailJob(
		h.ctx, job.Lease(), now.Add(6*time.Second), "embedding outcome unknown",
	); err != nil {
		t.Fatal(err)
	}

	if _, err := h.service.RetryDocument(
		h.ctx, "owner-1", "default", accepted.DocumentID, "retry-outcome-unknown",
	); !errors.Is(err, ErrDocumentRetryNotAllowed) {
		t.Fatalf("outcome-unknown retry error=%v, want ErrDocumentRetryNotAllowed", err)
	}
	var embedJobs int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE document_id=? AND kind='embed_document'`, accepted.DocumentID).Scan(&embedJobs); err != nil {
		t.Fatal(err)
	}
	if embedJobs != 2 {
		t.Fatalf("outcome-unknown retry created %d embed jobs, want original and first retry only", embedJobs)
	}
	cancelled, err := h.service.CancelJob(h.ctx, "owner-1", job.JobID)
	if err != nil || cancelled.State != KnowledgeJobFailed || !cancelled.CancelRequested {
		t.Fatalf("reconcile outcome-unknown job by cancellation: job=%+v err=%v", cancelled, err)
	}
	var vectorState, batchState string
	if err := h.db.QueryRowContext(h.ctx, `SELECT vector_state FROM kb_revision_documents
		WHERE corpus_uid=? AND revision_id=? AND document_id=? AND content_generation=?`,
		job.CorpusUID, job.TargetRevisionID, job.DocumentID, job.DocumentGeneration).Scan(&vectorState); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT state FROM kb_embedding_batch_manifests
		WHERE job_id=? AND batch_id=?`, job.JobID, manifest.BatchID).Scan(&batchState); err != nil {
		t.Fatal(err)
	}
	if vectorState != string(VectorIndexCancelled) || batchState != string(EmbeddingBatchCancelled) {
		t.Fatalf("cancel reconciliation vector=%q batch=%q, want cancelled/cancelled", vectorState, batchState)
	}
}
