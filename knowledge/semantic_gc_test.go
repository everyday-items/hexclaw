package knowledge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKnowledgeJobGCRemovesTombstonedDocumentResidue(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	h.runWorker(time.Unix(1_800_320_000, 0).UTC(), "worker-before-gc")
	if err := h.store.Delete(h.ctx, doc.ID); err != nil {
		t.Fatal(err)
	}
	var gcJobID, gcState string
	if err := h.db.QueryRowContext(h.ctx, `SELECT job_id,state FROM kb_knowledge_jobs
		WHERE corpus_uid=(SELECT corpus_uid FROM kb_semantic_corpora
		  WHERE owner_id='owner-1' AND corpus_alias='default') AND kind='gc'`).Scan(
		&gcJobID, &gcState,
	); err != nil {
		t.Fatalf("delete did not enqueue durable GC: %v", err)
	}
	if gcJobID == "" || gcState != string(KnowledgeJobQueued) {
		t.Fatalf("GC job=%q state=%q, want queued", gcJobID, gcState)
	}

	now := time.Unix(1_800_320_100, 0).UTC()
	worker := NewSemanticIndexWorker(h.repo, nil, workerConfig(&now, "worker-gc", 16))
	processed, err := worker.RunOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("run GC: processed=%v err=%v", processed, err)
	}

	for table, query := range map[string]string{
		"documents":          `SELECT COUNT(*) FROM kb_documents WHERE id='doc-1'`,
		"chunks":             `SELECT COUNT(*) FROM kb_chunks WHERE doc_id='doc-1'`,
		"fts":                `SELECT COUNT(*) FROM kb_chunks_fts WHERE chunk_id IN ('chunk-a','chunk-b')`,
		"cjk_fts_v2":         `SELECT COUNT(*) FROM kb_chunks_fts_v2 WHERE chunk_id IN ('chunk-a','chunk-b')`,
		"bindings":           `SELECT COUNT(*) FROM kb_semantic_document_bindings WHERE document_id='doc-1'`,
		"generations":        `SELECT COUNT(*) FROM kb_semantic_document_generations WHERE document_id='doc-1'`,
		"revision_documents": `SELECT COUNT(*) FROM kb_revision_documents WHERE document_id='doc-1'`,
		"vectors":            `SELECT COUNT(*) FROM kb_revision_vectors WHERE document_id='doc-1'`,
		"jobs":               `SELECT COUNT(*) FROM kb_knowledge_jobs WHERE document_id='doc-1' OR job_id='` + gcJobID + `'`,
		"job_checkpoints":    `SELECT COUNT(*) FROM kb_job_stage_checkpoints WHERE job_id='` + gcJobID + `'`,
		"batch_manifests":    `SELECT COUNT(*) FROM kb_embedding_batch_manifests`,
		"batch_chunks":       `SELECT COUNT(*) FROM kb_embedding_batch_chunks`,
		"page_checkpoints":   `SELECT COUNT(*) FROM kb_ingest_page_checkpoints`,
	} {
		var count int
		if err := h.db.QueryRowContext(h.ctx, query).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("GC left %s residue=%d", table, count)
		}
	}
	var cjkFTSVersion int
	if err := h.db.QueryRowContext(h.ctx, `SELECT version FROM kb_search_index_metadata
		WHERE index_name='chunks_cjk_fts'`).Scan(&cjkFTSVersion); err != nil {
		t.Fatalf("GC did not publish CJK FTS v2 version: %v", err)
	}
	if cjkFTSVersion != cjkFTSIndexVersion {
		t.Fatalf("GC CJK FTS version=%d want %d", cjkFTSVersion, cjkFTSIndexVersion)
	}

	// A fresh repository/store over the same durable DB must not resurrect any
	// of the physically collected rows.
	restarted := NewSQLiteStore(h.db, WithSQLiteSemanticMutations("owner-1", "default"))
	documents, err := restarted.List(h.ctx)
	if err != nil || len(documents) != 0 {
		t.Fatalf("restart list=%+v err=%v, want zero documents", documents, err)
	}
}

func TestKnowledgeJobGCImmediatelyRemovesUnsharedManagedSourceObject(t *testing.T) {
	h := newSemanticMutationHarness(t)
	if err := h.service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}
	const body = "managed source bytes removed by durable gc"
	accepted, err := h.service.CreateDocument(h.ctx, "owner-1", "default", CreateDocumentInput{
		IdempotencyKey: "gc-removes-managed-source", Filename: "remove-me.txt",
		MediaType: "text/plain", SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	var sourcePath string
	if err := h.db.QueryRowContext(h.ctx, `SELECT b.storage_path
		FROM kb_ingest_document_sources s JOIN kb_ingest_blobs b
		  ON b.owner_id=s.owner_id AND b.corpus_uid=s.corpus_uid AND b.sha256=s.blob_sha256
		WHERE s.document_id=?`, accepted.DocumentID).Scan(&sourcePath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("managed source missing before delete: %v", err)
	}
	if err := h.store.Delete(h.ctx, accepted.DocumentID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(time.Minute)
	worker := NewSemanticIndexWorker(h.repo, nil, workerConfig(&now, "worker-gc-source", 16))
	processed, err := worker.RunOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("run source GC: processed=%v err=%v", processed, err)
	}
	if _, err := os.Stat(sourcePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GC returned success while managed source still exists: stat err=%v", err)
	}
	var blobRows int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_ingest_blobs
		WHERE storage_path=?`, sourcePath).Scan(&blobRows); err != nil {
		t.Fatal(err)
	}
	if blobRows != 0 {
		t.Fatalf("GC left unreferenced blob metadata rows=%d", blobRows)
	}
}

func TestKnowledgeJobGCWithoutConfiguredObjectStoreKeepsRelationalSourceForRetry(t *testing.T) {
	h := newSemanticMutationHarness(t)
	if err := h.service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}
	const body = "source must survive temporarily unavailable object storage"
	accepted, err := h.service.CreateDocument(h.ctx, "owner-1", "default", CreateDocumentInput{
		IdempotencyKey: "gc-store-unavailable", Filename: "store-unavailable.txt",
		MediaType: "text/plain", SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	var sourcePath string
	if err := h.db.QueryRowContext(h.ctx, `SELECT b.storage_path
		FROM kb_ingest_document_sources s JOIN kb_ingest_blobs b
		  ON b.owner_id=s.owner_id AND b.corpus_uid=s.corpus_uid AND b.sha256=s.blob_sha256
		WHERE s.document_id=?`, accepted.DocumentID).Scan(&sourcePath); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Delete(h.ctx, accepted.DocumentID); err != nil {
		t.Fatal(err)
	}
	// Model a worker reconstructed before ConfigureDocumentIngest has attached
	// the validated managed-object root.
	h.repo.ingestBlobStore = nil
	now := time.Now().UTC().Add(time.Minute)
	worker := NewSemanticIndexWorker(h.repo, nil, workerConfig(&now, "worker-gc-no-store", 16))
	processed, runErr := worker.RunOnce(h.ctx)
	if !processed || !errors.Is(runErr, ErrDocumentIngestUnavailable) {
		t.Fatalf("GC without object store processed=%v err=%v", processed, runErr)
	}
	var documents, sources int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_documents WHERE id=?`, accepted.DocumentID).
		Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_ingest_document_sources
		WHERE document_id=?`, accepted.DocumentID).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if documents != 1 || sources != 1 {
		t.Fatalf("unavailable object store committed relational cleanup documents=%d sources=%d", documents, sources)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("unavailable object store lost physical source: %v", err)
	}
}

func TestKnowledgeJobGCRefusesUnmanagedSourceBeforeRelationalCleanup(t *testing.T) {
	h := newSemanticMutationHarness(t)
	if err := h.service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "original-objects")); err != nil {
		t.Fatal(err)
	}
	const body = "source path must stay inside configured managed root"
	accepted, err := h.service.CreateDocument(h.ctx, "owner-1", "default", CreateDocumentInput{
		IdempotencyKey: "gc-unmanaged-source", Filename: "unmanaged.txt",
		MediaType: "text/plain", SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	var sourcePath string
	if err := h.db.QueryRowContext(h.ctx, `SELECT b.storage_path
		FROM kb_ingest_document_sources s JOIN kb_ingest_blobs b
		  ON b.owner_id=s.owner_id AND b.corpus_uid=s.corpus_uid AND b.sha256=s.blob_sha256
		WHERE s.document_id=?`, accepted.DocumentID).Scan(&sourcePath); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Delete(h.ctx, accepted.DocumentID); err != nil {
		t.Fatal(err)
	}
	otherStore, err := newLocalIngestBlobStore(filepath.Join(t.TempDir(), "other-objects"))
	if err != nil {
		t.Fatal(err)
	}
	h.repo.ingestBlobStore = otherStore
	now := time.Now().UTC().Add(time.Minute)
	worker := NewSemanticIndexWorker(h.repo, nil, workerConfig(&now, "worker-gc-unmanaged", 16))
	processed, runErr := worker.RunOnce(h.ctx)
	if !processed || runErr == nil {
		t.Fatalf("GC for unmanaged source processed=%v err=%v", processed, runErr)
	}
	var documents, sources int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_documents WHERE id=?`, accepted.DocumentID).
		Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_ingest_document_sources
		WHERE document_id=?`, accepted.DocumentID).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if documents != 1 || sources != 1 {
		t.Fatalf("unmanaged source committed relational cleanup documents=%d sources=%d", documents, sources)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("unmanaged source was removed: %v", err)
	}
}

func TestKnowledgeJobGCSourceRemovalFailureKeepsDurableRetry(t *testing.T) {
	h := newSemanticMutationHarness(t)
	if err := h.service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}
	const body = "retry physical source cleanup"
	accepted, err := h.service.CreateDocument(h.ctx, "owner-1", "default", CreateDocumentInput{
		IdempotencyKey: "gc-source-retry", Filename: "retry-source.txt",
		MediaType: "text/plain", SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	var sourcePath, gcJobID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT b.storage_path
		FROM kb_ingest_document_sources s JOIN kb_ingest_blobs b
		  ON b.owner_id=s.owner_id AND b.corpus_uid=s.corpus_uid AND b.sha256=s.blob_sha256
		WHERE s.document_id=?`, accepted.DocumentID).Scan(&sourcePath); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Delete(h.ctx, accepted.DocumentID); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT job_id FROM kb_knowledge_jobs
		WHERE kind='gc' AND state='queued'`).Scan(&gcJobID); err != nil {
		t.Fatal(err)
	}

	// A non-empty directory at the exact managed path deterministically makes
	// os.Remove fail without relying on platform-specific directory modes.
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "blocker"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(time.Minute)
	worker := NewSemanticIndexWorker(h.repo, nil, workerConfig(&now, "worker-gc-retry", 16))
	processed, firstErr := worker.RunOnce(h.ctx)
	if !processed || firstErr == nil {
		t.Fatalf("first GC removal processed=%v err=%v, want durable retry error", processed, firstErr)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("failed removal lost managed source: %v", err)
	}
	var state string
	if err := h.db.QueryRowContext(h.ctx, `SELECT state FROM kb_knowledge_jobs WHERE job_id=?`, gcJobID).
		Scan(&state); err != nil {
		t.Fatalf("failed removal lost GC job: %v", err)
	}
	if state != string(KnowledgeJobRetryWait) {
		t.Fatalf("GC state after removal failure=%q, want retry_wait", state)
	}

	if err := os.RemoveAll(sourcePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	processed, err = worker.RunOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("retry source GC: processed=%v err=%v", processed, err)
	}
	if _, err := os.Stat(sourcePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retried GC did not remove managed source: stat err=%v", err)
	}
	var jobs int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs WHERE job_id=?`, gcJobID).
		Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("successful retry left GC jobs=%d", jobs)
	}
}

func TestKnowledgeJobGCRetainsManagedSourceSharedByAnotherDocument(t *testing.T) {
	h := newSemanticMutationHarness(t)
	if err := h.service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}
	const body = "shared content addressed source"
	create := func(key, filename string) CreateDocumentResult {
		t.Helper()
		result, err := h.service.CreateDocument(h.ctx, "owner-1", "default", CreateDocumentInput{
			IdempotencyKey: key, Filename: filename, MediaType: "text/plain",
			SizeBytes: int64(len(body)), Body: strings.NewReader(body),
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := create("gc-shared-first", "shared-first.txt")
	second := create("gc-shared-second", "shared-second.txt")
	var sourcePath string
	if err := h.db.QueryRowContext(h.ctx, `SELECT storage_path FROM kb_ingest_blobs LIMIT 1`).
		Scan(&sourcePath); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Delete(h.ctx, first.DocumentID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(time.Minute)
	worker := NewSemanticIndexWorker(h.repo, nil, workerConfig(&now, "worker-gc-shared", 16))
	worker.SetDocumentIngestProcessor(deterministicIngestProcessor{})
	if processed, err := worker.RunOnce(h.ctx); err != nil || !processed {
		t.Fatalf("process surviving ingest: processed=%v err=%v", processed, err)
	}
	now = now.Add(time.Second)
	if processed, err := worker.RunOnce(h.ctx); err != nil || !processed {
		t.Fatalf("run shared source GC: processed=%v err=%v", processed, err)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("GC removed source still shared by document %s: %v", second.DocumentID, err)
	}
	var sources, blobs int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_ingest_document_sources
		WHERE document_id=?`, second.DocumentID).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_ingest_blobs
		WHERE storage_path=?`, sourcePath).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if sources != 1 || blobs != 1 {
		t.Fatalf("shared source durable rows sources=%d blobs=%d, want 1/1", sources, blobs)
	}
}

func TestQueuedKnowledgeJobGCIsRetiredBeforeImmediateIngestRevive(t *testing.T) {
	h := newSemanticMutationHarness(t)
	if err := h.service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}
	const body = "delete then immediately upload again"
	create := func(key string) CreateDocumentResult {
		t.Helper()
		result, err := h.service.CreateDocument(h.ctx, "owner-1", "default", CreateDocumentInput{
			IdempotencyKey: key, Filename: "revive.txt", MediaType: "text/plain",
			SizeBytes: int64(len(body)), Body: strings.NewReader(body),
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := create("gc-revive-first")
	if err := h.store.Delete(h.ctx, first.DocumentID); err != nil {
		t.Fatal(err)
	}
	var gcJobID, gcState string
	if err := h.db.QueryRowContext(h.ctx, `SELECT job_id,state FROM kb_knowledge_jobs
		WHERE kind='gc'`).Scan(&gcJobID, &gcState); err != nil {
		t.Fatal(err)
	}
	if gcState != string(KnowledgeJobQueued) {
		t.Fatalf("pre-revive GC state=%q, want queued", gcState)
	}

	revived := create("gc-revive-second")
	if revived.DocumentID != first.DocumentID || revived.JobID == first.JobID {
		t.Fatalf("revive identity first=%+v revived=%+v", first, revived)
	}
	var oldGCJobs int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs WHERE job_id=?`, gcJobID).
		Scan(&oldGCJobs); err != nil {
		t.Fatal(err)
	}
	if oldGCJobs != 0 {
		t.Fatalf("revive left obsolete GC jobs=%d", oldGCJobs)
	}

	now := time.Now().UTC().Add(time.Minute)
	worker := NewSemanticIndexWorker(h.repo, nil, workerConfig(&now, "worker-after-revive", 16))
	worker.SetDocumentIngestProcessor(deterministicIngestProcessor{})
	processed, err := worker.RunOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("post-revive worker: processed=%v err=%v", processed, err)
	}
	job, err := h.service.GetJob(h.ctx, "owner-1", revived.JobID)
	if err != nil || job.State != KnowledgeJobSucceeded {
		t.Fatalf("revived ingest job=%+v err=%v", job, err)
	}
}
