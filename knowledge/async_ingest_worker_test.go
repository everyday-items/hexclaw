package knowledge

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type deterministicIngestProcessor struct{}

func (deterministicIngestProcessor) Prepare(
	_ context.Context,
	source PersistedIngestDocument,
) (PreparedIngestDocument, error) {
	document := &Document{
		ID: source.DocumentID, Title: source.Filename,
		Content: "restart durable algebra lesson", Source: "upload:" + source.Filename,
		Status: "indexed", SourceType: "upload", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	return PreparedIngestDocument{
		Document: document,
		Chunks: []*Chunk{{
			ID: document.ID + "-chunk-0", DocID: document.ID, DocTitle: document.Title,
			Source: document.Source, SourceType: "upload", ChunkCount: 1,
			Content: document.Content, Index: 0, CreatedAt: document.CreatedAt,
			PageStart: 1, PageEnd: 1, SourceDigest: source.SHA256,
			SourceOffsetStart: 0, SourceOffsetEnd: int64(len(document.Content)),
		}},
		PageCount: 1,
		Warnings:  []string{},
	}, nil
}

type delayedIngestProcessor struct{ delay time.Duration }

func (p delayedIngestProcessor) Prepare(ctx context.Context, source PersistedIngestDocument) (PreparedIngestDocument, error) {
	timer := time.NewTimer(p.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return PreparedIngestDocument{}, ctx.Err()
	case <-timer.C:
		return deterministicIngestProcessor{}.Prepare(ctx, source)
	}
}

type blockingPageCaptionerProcessor struct {
	entered   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
	calls     atomic.Int32
}

func (p *blockingPageCaptionerProcessor) Prepare(
	ctx context.Context,
	source PersistedIngestDocument,
) (PreparedIngestDocument, error) {
	return p.PrepareResumable(ctx, source, nil)
}

func (p *blockingPageCaptionerProcessor) PrepareResumable(
	ctx context.Context,
	_ PersistedIngestDocument,
	_ IngestPageProgress,
) (PreparedIngestDocument, error) {
	for page := int32(1); page <= 2; page++ {
		p.calls.Add(1)
		if page != 1 {
			continue
		}
		close(p.entered)
		select {
		case <-ctx.Done():
			close(p.cancelled)
			return PreparedIngestDocument{}, ctx.Err()
		case <-p.release:
			return PreparedIngestDocument{}, errors.New("test: release blocked captioner")
		}
	}
	return PreparedIngestDocument{}, errors.New("test: captioner unexpectedly reached page two")
}

func TestCancelRunningIngestInterruptsCurrentProviderBeforeNextPage(t *testing.T) {
	db, _, ctx := newAsyncIngestHarness(t)
	repository := NewSQLiteSemanticIndexRepository(db)
	service := NewSemanticIndexService(repository, &staticEmbeddingResolver{})
	if err := service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}
	body := "%PDF-1.4\ntwo scanned pages"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "cancel-running-captioner", Filename: "scanned.pdf", MediaType: "application/pdf",
		SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}

	processor := &blockingPageCaptionerProcessor{
		entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{}),
	}
	worker := NewSemanticIndexWorker(repository, nil, SemanticIndexWorkerConfig{
		OwnerID: "desktop-user", CorpusID: "default", WorkerID: "cancel-running-worker",
		LeaseDuration: time.Minute,
	})
	worker.SetDocumentIngestProcessor(processor)
	type workerResult struct {
		worked bool
		err    error
	}
	workerDone := make(chan workerResult, 1)
	go func() {
		worked, runErr := worker.RunOnce(ctx)
		workerDone <- workerResult{worked: worked, err: runErr}
	}()

	select {
	case <-processor.entered:
	case <-time.After(2 * time.Second):
		close(processor.release)
		t.Fatal("captioner did not enter the first page")
	}
	cancelledJob, err := service.CancelJob(ctx, "desktop-user", accepted.JobID)
	if err != nil || cancelledJob.State != KnowledgeJobCancelled {
		close(processor.release)
		t.Fatalf("CancelJob job=%+v err=%v", cancelledJob, err)
	}

	cancelObserved := false
	select {
	case <-processor.cancelled:
		cancelObserved = true
	case <-time.After(500 * time.Millisecond):
	}
	close(processor.release)
	var result workerResult
	select {
	case result = <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
	if !cancelObserved {
		t.Fatal("running captioner context was not cancelled promptly")
	}
	if got := processor.calls.Load(); got != 1 {
		t.Fatalf("captioner calls=%d, want only the in-flight first page", got)
	}
	if !result.worked || !errors.Is(result.err, ErrJobFenced) {
		t.Fatalf("RunOnce after CancelJob worked=%v err=%v, want worked with ErrJobFenced", result.worked, result.err)
	}
}

func TestIngestJobSurvivesWorkerReconstructionAndPublishesTextAtomically(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	body := "durable source bytes"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "restart-job", Filename: "lesson.txt", MediaType: "text/plain",
		SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The worker is constructed only after durable acceptance, modelling a
	// process restart between 202 and the first extraction attempt.
	repository := NewSQLiteSemanticIndexRepository(db)
	worker := NewSemanticIndexWorker(repository, nil, SemanticIndexWorkerConfig{
		OwnerID: "desktop-user", CorpusID: "default", WorkerID: "restart-worker",
		LeaseDuration: time.Minute,
	})
	worker.SetDocumentIngestProcessor(deterministicIngestProcessor{})
	worked, err := worker.RunOnce(ctx)
	if err != nil || !worked {
		t.Fatalf("RunOnce worked=%v err=%v", worked, err)
	}

	job, err := service.GetJob(ctx, "desktop-user", accepted.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != KnowledgeJobSucceeded || job.Stage != JobStageTextIndexing {
		t.Fatalf("job after ingest = %+v", job)
	}
	projection, err := service.GetIngestDocumentProjection(ctx, "desktop-user", accepted.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.TextIndexState != TextIndexReady || projection.PageCount == nil || *projection.PageCount != 1 {
		t.Fatalf("projection after ingest = %+v", projection)
	}
	if len(projection.SourceSpans) != 1 || projection.SourceSpans[0].PageStart != 1 ||
		projection.SourceSpans[0].SourceDigest == "" {
		t.Fatalf("projection source spans=%+v", projection.SourceSpans)
	}
	var content, status string
	var chunks, ftsRows int
	if err := db.QueryRowContext(ctx, `SELECT content,status,chunk_count FROM kb_documents WHERE id=?`, accepted.DocumentID).
		Scan(&content, &status, &chunks); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_chunks_fts WHERE kb_chunks_fts MATCH 'algebra'`).Scan(&ftsRows); err != nil {
		t.Fatal(err)
	}
	if content != "restart durable algebra lesson" || status != "indexed" || chunks != 1 || ftsRows != 1 {
		t.Fatalf("published content=%q status=%q chunks=%d fts=%d", content, status, chunks, ftsRows)
	}
	for _, stage := range []JobStage{JobStageExtracting, JobStageChunking, JobStageTextIndexing} {
		var checkpoints int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_job_stage_checkpoints
			WHERE job_id=? AND stage=? AND state='succeeded'`, accepted.JobID, stage).Scan(&checkpoints); err != nil {
			t.Fatal(err)
		}
		if checkpoints != 1 {
			t.Fatalf("stage %s checkpoints=%d", stage, checkpoints)
		}
	}
}

func TestDefaultKnowledgeWorkerNeverConsumesAnotherCorpusQueue(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	repository := NewSQLiteSemanticIndexRepository(db)
	if _, err := repository.BindLegacyDefaultCorpus(ctx, "desktop-user", "second"); err != nil {
		t.Fatal(err)
	}
	create := func(corpusID, key, filename string) CreateDocumentResult {
		body := "durable " + corpusID + " bytes"
		result, err := service.CreateDocument(ctx, "desktop-user", corpusID, CreateDocumentInput{
			IdempotencyKey: key, Filename: filename, MediaType: "text/plain",
			SizeBytes: int64(len(body)), Body: strings.NewReader(body),
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	other := create("second", "second-worker-scope", "second.txt")
	defaultJob := create("default", "default-worker-scope", "default.txt")

	worker := NewSemanticIndexWorker(repository, nil, SemanticIndexWorkerConfig{
		OwnerID: "desktop-user", CorpusID: "default", WorkerID: "default-only-worker",
		LeaseDuration: time.Minute,
	})
	worker.SetDocumentIngestProcessor(deterministicIngestProcessor{})
	if worked, err := worker.RunOnce(ctx); err != nil || !worked {
		t.Fatalf("default worker worked=%v err=%v", worked, err)
	}
	defaultState, err := service.GetJobForCorpus(ctx, "desktop-user", "default", defaultJob.JobID)
	if err != nil || defaultState.State != KnowledgeJobSucceeded {
		t.Fatalf("default job=%+v err=%v", defaultState, err)
	}
	otherState, err := service.GetJobForCorpus(ctx, "desktop-user", "second", other.JobID)
	if err != nil || otherState.State != KnowledgeJobQueued {
		t.Fatalf("other corpus job=%+v err=%v", otherState, err)
	}
	if worked, err := worker.RunOnce(ctx); err != nil || worked {
		t.Fatalf("default worker consumed another corpus: worked=%v err=%v", worked, err)
	}
}

func TestCancellingQueuedIngestFencesWorkerAndHidesPlaceholder(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	body := "cancel before extraction"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "cancel-job", Filename: "cancel.txt", MediaType: "text/plain",
		SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.CancelJob(ctx, "desktop-user", accepted.JobID)
	if err != nil || job.State != KnowledgeJobCancelled {
		t.Fatalf("cancelled job=%+v err=%v", job, err)
	}
	if _, err := service.GetIngestDocumentProjection(ctx, "desktop-user", accepted.DocumentID); !errors.Is(err, ErrSemanticIndexNotFound) {
		t.Fatalf("cancelled document projection error=%v", err)
	}
	worker := NewSemanticIndexWorker(NewSQLiteSemanticIndexRepository(db), nil, SemanticIndexWorkerConfig{
		OwnerID: "desktop-user", CorpusID: "default", WorkerID: "late-worker",
	})
	worker.SetDocumentIngestProcessor(deterministicIngestProcessor{})
	worked, err := worker.RunOnce(ctx)
	if err != nil || worked {
		t.Fatalf("cancelled job was claimed: worked=%v err=%v", worked, err)
	}
	var deleted int
	if err := db.QueryRowContext(ctx, `SELECT deleted FROM kb_documents WHERE id=?`, accepted.DocumentID).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("cancelled placeholder deleted=%d", deleted)
	}
}

func TestCompletedIngestQueuesRevisionScopedEmbeddingAsChildJob(t *testing.T) {
	h := newSemanticMutationHarness(t)
	if err := h.service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}
	body := "active revision source"
	accepted, err := h.service.CreateDocument(h.ctx, "owner-1", "default", CreateDocumentInput{
		IdempotencyKey: "active-child", Filename: "active.txt", MediaType: "text/plain",
		SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewSemanticIndexWorker(h.repo, nil, SemanticIndexWorkerConfig{
		OwnerID: "owner-1", CorpusID: "default", WorkerID: "ingest-worker", LeaseDuration: time.Minute,
	})
	worker.SetDocumentIngestProcessor(deterministicIngestProcessor{})
	if worked, err := worker.RunOnce(h.ctx); err != nil || !worked {
		t.Fatalf("ingest worked=%v err=%v", worked, err)
	}
	var parentID, kind, state, revision string
	if err := h.db.QueryRowContext(h.ctx, `SELECT parent_job_id,kind,state,target_revision_id
		FROM kb_knowledge_jobs WHERE document_id=? AND kind='embed_document'`, accepted.DocumentID).
		Scan(&parentID, &kind, &state, &revision); err != nil {
		t.Fatal(err)
	}
	if parentID != accepted.JobID || kind != string(KnowledgeJobEmbedDocument) ||
		state != string(KnowledgeJobQueued) || revision != h.active {
		t.Fatalf("child parent=%q kind=%q state=%q revision=%q", parentID, kind, state, revision)
	}
	var vectorState string
	if err := h.db.QueryRowContext(h.ctx, `SELECT vector_state FROM kb_revision_documents
		WHERE revision_id=? AND document_id=? AND content_generation=1`, h.active, accepted.DocumentID).
		Scan(&vectorState); err != nil {
		t.Fatal(err)
	}
	if vectorState != string(VectorIndexPending) {
		t.Fatalf("revision document state=%q", vectorState)
	}
	cancelledRoot, err := h.service.CancelJob(h.ctx, "owner-1", accepted.JobID)
	if err != nil || cancelledRoot.State != KnowledgeJobSucceeded || !cancelledRoot.CancelRequested {
		t.Fatalf("cancel text-ready root=%+v err=%v", cancelledRoot, err)
	}
	var childState string
	if err := h.db.QueryRowContext(h.ctx, `SELECT state FROM kb_knowledge_jobs
		WHERE parent_job_id=? AND kind='embed_document'`, accepted.JobID).Scan(&childState); err != nil {
		t.Fatal(err)
	}
	if childState != string(KnowledgeJobCancelled) {
		t.Fatalf("child state after root cancellation=%q", childState)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT vector_state FROM kb_revision_documents
		WHERE revision_id=? AND document_id=? AND content_generation=1`, h.active, accepted.DocumentID).
		Scan(&vectorState); err != nil {
		t.Fatal(err)
	}
	if vectorState != string(VectorIndexCancelled) {
		t.Fatalf("revision document after root cancellation=%q", vectorState)
	}
	projection, err := h.service.GetIngestDocumentProjection(h.ctx, "owner-1", accepted.DocumentID)
	if err != nil || projection.TextIndexState != TextIndexReady {
		t.Fatalf("text-ready document disappeared on embedding cancel: projection=%+v err=%v", projection, err)
	}
}

func TestIngestWorkerRenewsLeaseDuringSlowExtraction(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	body := "slow extraction source"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "slow-heartbeat", Filename: "slow.txt", MediaType: "text/plain",
		SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewSemanticIndexWorker(NewSQLiteSemanticIndexRepository(db), nil, SemanticIndexWorkerConfig{
		OwnerID: "desktop-user", CorpusID: "default", WorkerID: "heartbeat-worker",
		LeaseDuration: 150 * time.Millisecond,
	})
	worker.SetDocumentIngestProcessor(delayedIngestProcessor{delay: 350 * time.Millisecond})
	if worked, err := worker.RunOnce(ctx); err != nil || !worked {
		t.Fatalf("slow ingest worked=%v err=%v", worked, err)
	}
	job, err := service.GetJob(ctx, "desktop-user", accepted.JobID)
	if err != nil || job.State != KnowledgeJobSucceeded || job.LeaseEpoch < 2 {
		t.Fatalf("slow ingest job=%+v err=%v", job, err)
	}
}
