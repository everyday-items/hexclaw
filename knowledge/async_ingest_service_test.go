package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func newAsyncIngestHarness(t *testing.T) (*sql.DB, *SemanticIndexService, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "knowledge.db")+
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, db, []migrate.Migration{
		migrate.KnowledgeIndexV23,
		migrate.KnowledgeIngestV24,
		migrate.KnowledgeIngestGenerationsV26,
		migrate.KnowledgeDocumentScopeV27,
		migrate.KnowledgeIngestCheckpointV28,
		migrate.KnowledgeIngestExecutionV46,
		migrate.KnowledgeUploadOperationsV71,
	}); err != nil {
		t.Fatal(err)
	}
	repository := NewSQLiteSemanticIndexRepository(db)
	if _, err := repository.BindLegacyDefaultCorpus(ctx, "desktop-user", "default"); err != nil {
		t.Fatal(err)
	}
	service := NewSemanticIndexService(repository, &staticEmbeddingResolver{})
	if err := service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}
	return db, service, ctx
}

type staticEmbeddingResolver struct{}

func (*staticEmbeddingResolver) Resolve(context.Context, string, string, EmbeddingSelection) (EmbeddingProfileSnapshot, error) {
	return EmbeddingProfileSnapshot{}, ErrProfileUnavailable
}

func (*staticEmbeddingResolver) Catalog(context.Context, string, string) (EmbeddingProfileCatalog, error) {
	return EmbeddingProfileCatalog{}, nil
}

func TestCreateDocumentStreamsFrozen57313616BytePDFBeforeAcceptingJob(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	const size = int64(57_313_616)
	prefix := "%PDF-1.6\n"
	body := io.MultiReader(strings.NewReader(prefix), io.LimitReader(zeroReader{}, size-int64(len(prefix))))

	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "fixture-six-upper-v1",
		Filename:       "人教版·小学六年级上册.pdf",
		MediaType:      "application/pdf",
		SizeBytes:      size,
		Body:           body,
		Subject:        "数学",
		Grade:          "六年级上",
	})
	if err != nil {
		t.Fatal(err)
	}
	if accepted.DocumentID == "" || accepted.JobID == "" {
		t.Fatalf("accepted identifiers are empty: %+v", accepted)
	}
	if accepted.TextIndexState != TextIndexPending || accepted.VectorIndexState != VectorIndexDisabled {
		t.Fatalf("accepted states = %+v", accepted)
	}

	var storedPath, digest string
	var storedSize int64
	if err := db.QueryRowContext(ctx, `SELECT b.storage_path,b.sha256,b.size_bytes
		FROM kb_ingest_blobs b JOIN kb_ingest_document_sources s
		  ON s.owner_id=b.owner_id AND s.corpus_uid=b.corpus_uid AND s.blob_sha256=b.sha256
		WHERE s.document_id=?`, accepted.DocumentID).Scan(&storedPath, &digest, &storedSize); err != nil {
		t.Fatal(err)
	}
	if storedSize != size {
		t.Fatalf("stored bytes=%d want=%d", storedSize, size)
	}
	info, err := os.Stat(storedPath)
	if err != nil || info.Size() != size {
		t.Fatalf("persisted blob size=%v err=%v", func() int64 {
			if info == nil {
				return -1
			}
			return info.Size()
		}(), err)
	}
	if len(digest) != 64 {
		t.Fatalf("sha256=%q", digest)
	}

	var documents, jobs, checkpoints int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_documents WHERE id=? AND status='processing'`, accepted.DocumentID).Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs WHERE job_id=? AND kind='ingest' AND state='queued'`, accepted.JobID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_job_stage_checkpoints WHERE job_id=? AND stage='extracting' AND state='prepared'`, accepted.JobID).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if documents != 1 || jobs != 1 || checkpoints != 1 {
		t.Fatalf("durable acceptance window documents=%d jobs=%d checkpoints=%d", documents, jobs, checkpoints)
	}
}

func TestCreateDocumentIdempotencyIsOwnerCorpusScopedAndPayloadBound(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	input := func(body string) CreateDocumentInput {
		return CreateDocumentInput{
			IdempotencyKey: "same-click", Filename: "lesson.txt", MediaType: "text/plain",
			SizeBytes: int64(len(body)), Body: strings.NewReader(body),
		}
	}
	first, err := service.CreateDocument(ctx, "desktop-user", "default", input("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateDocument(ctx, "desktop-user", "default", input("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if first.DocumentID != second.DocumentID || first.JobID != second.JobID {
		t.Fatalf("idempotent replay diverged: first=%+v second=%+v", first, second)
	}
	var documents, jobs int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_documents`).Scan(&documents)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs WHERE kind='ingest'`).Scan(&jobs)
	if documents != 1 || jobs != 1 {
		t.Fatalf("idempotent replay duplicated rows documents=%d jobs=%d", documents, jobs)
	}
	if _, err := service.CreateDocument(ctx, "desktop-user", "default", input("different")); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same key with different bytes error=%v, want ErrIdempotencyConflict", err)
	}
	if _, err := service.GetJob(ctx, "another-owner", first.JobID); !errors.Is(err, ErrSemanticIndexNotFound) {
		t.Fatalf("cross-owner job lookup error=%v", err)
	}
}

func TestCreateDocumentIdempotencyReplayBindsK12MetadataAndAllowsCorpusScopedKeys(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	repository := NewSQLiteSemanticIndexRepository(db)
	if _, err := repository.BindLegacyDefaultCorpus(ctx, "desktop-user", "second"); err != nil {
		t.Fatal(err)
	}
	body := "same immutable source"
	base := CreateDocumentInput{
		IdempotencyKey: "metadata-bound-intent", Filename: "lesson.txt", MediaType: "text/plain",
		SizeBytes: int64(len(body)), AgentID: "tutor-a", LearnerID: "learner-a",
		Subject: "数学", Grade: "六年级上",
	}
	create := func(corpus string, mutate func(*CreateDocumentInput)) (CreateDocumentResult, error) {
		input := base
		input.Body = strings.NewReader(body)
		if mutate != nil {
			mutate(&input)
		}
		return service.CreateDocument(ctx, "desktop-user", corpus, input)
	}
	first, err := create("default", nil)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := create("default", nil)
	if err != nil {
		t.Fatalf("exact replay failed: %v", err)
	}
	if replayed.DocumentID != first.DocumentID || replayed.JobID != first.JobID {
		t.Fatalf("same-corpus replay diverged: first=%+v replayed=%+v", first, replayed)
	}
	for name, mutate := range map[string]func(*CreateDocumentInput){
		"agent":   func(input *CreateDocumentInput) { input.AgentID = "tutor-b" },
		"learner": func(input *CreateDocumentInput) { input.LearnerID = "learner-b" },
		"subject": func(input *CreateDocumentInput) { input.Subject = "语文" },
		"grade":   func(input *CreateDocumentInput) { input.Grade = "五年级下" },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := create("default", mutate); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("metadata mismatch error=%v, want ErrIdempotencyConflict", err)
			}
		})
	}
	second, err := create("second", nil)
	if err != nil {
		t.Fatalf("same key in a different corpus must be independent: %v", err)
	}
	if second.DocumentID == first.DocumentID || second.JobID == first.JobID {
		t.Fatalf("cross-corpus scoped key reused physical work: first=%+v second=%+v", first, second)
	}
	var referencedPath string
	var sources, jobs int
	if err := db.QueryRowContext(ctx, `SELECT bl.storage_path
		FROM kb_ingest_document_sources s JOIN kb_ingest_blobs bl
		  ON bl.owner_id=s.owner_id AND bl.corpus_uid=s.corpus_uid AND bl.sha256=s.blob_sha256
		LIMIT 1`).Scan(&referencedPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(referencedPath); err != nil {
		t.Fatalf("idempotency conflict removed referenced content-addressed object: %v", err)
	}
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_ingest_document_sources`).Scan(&sources)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs WHERE kind='ingest'`).Scan(&jobs)
	if sources != 2 || jobs != 2 {
		t.Fatalf("corpus-scoped keys must create one durable item per corpus: sources=%d jobs=%d", sources, jobs)
	}
}

func TestCreateDocumentAfterCancelRevivesSameLogicalDocumentAsNewGeneration(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	body := "durable cancellation source"
	create := func(key string) (CreateDocumentResult, error) {
		return service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
			IdempotencyKey: key, Filename: "cancelled.txt", MediaType: "text/plain",
			SizeBytes: int64(len(body)), Body: strings.NewReader(body),
			AgentID: "tutor-a", LearnerID: "learner-a", Subject: "数学", Grade: "六年级上",
		})
	}
	first, err := create("cancelled-generation-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CancelJob(ctx, "desktop-user", first.JobID); err != nil {
		t.Fatal(err)
	}
	replayedCancelled, err := create("cancelled-generation-1")
	if err != nil {
		t.Fatalf("replay cancelled intent: %v", err)
	}
	if replayedCancelled.DocumentID != first.DocumentID || replayedCancelled.JobID != first.JobID {
		t.Fatalf("cancelled idempotency replay changed identity: first=%+v replay=%+v", first, replayedCancelled)
	}
	second, err := create("cancelled-generation-2")
	if err != nil {
		t.Fatalf("create explicit generation after cancellation: %v", err)
	}
	if second.DocumentID != first.DocumentID || second.JobID == first.JobID {
		t.Fatalf("generation did not revive stable document: first=%+v second=%+v", first, second)
	}
	var generation int64
	var lifecycle, textState string
	if err := db.QueryRowContext(ctx, `SELECT content_generation,lifecycle_state,text_state
		FROM kb_semantic_document_bindings WHERE document_id=?`, first.DocumentID).
		Scan(&generation, &lifecycle, &textState); err != nil {
		t.Fatal(err)
	}
	var sourceGenerations, activeDocuments int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_ingest_document_sources
		WHERE document_id=?`, first.DocumentID).Scan(&sourceGenerations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_documents
		WHERE source='upload:cancelled.txt' AND title='cancelled.txt' AND deleted=0`).Scan(&activeDocuments); err != nil {
		t.Fatal(err)
	}
	if generation != 2 || lifecycle != "active" || textState != "pending" ||
		sourceGenerations != 2 || activeDocuments != 1 {
		t.Fatalf("revived generation=%d lifecycle=%s text=%s sources=%d active=%d",
			generation, lifecycle, textState, sourceGenerations, activeDocuments)
	}
	if _, err := create("cancelled-generation-3"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("parallel active generation error=%v, want ErrIdempotencyConflict", err)
	}
}

func TestCreateDocumentImmediatelyPrunesOnlyZeroReferenceObjectAfterRepositoryRejects(t *testing.T) {
	_, service, ctx := newAsyncIngestHarness(t)
	managedCount := func() int {
		count := 0
		if err := filepath.WalkDir(service.blobStore.root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && isManagedIngestObjectPath(service.blobStore.root, path) {
				count++
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return count
	}

	firstBody := "referenced source bytes"
	if _, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "orphan-guard", Filename: "orphan.txt", MediaType: "text/plain",
		SizeBytes: int64(len(firstBody)), Body: strings.NewReader(firstBody),
	}); err != nil {
		t.Fatal(err)
	}
	if got := managedCount(); got != 1 {
		t.Fatalf("managed objects after accepted create=%d, want 1", got)
	}

	differentBody := "different rejected source bytes"
	if _, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "orphan-guard", Filename: "orphan.txt", MediaType: "text/plain",
		SizeBytes: int64(len(differentBody)), Body: strings.NewReader(differentBody),
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different payload error=%v", err)
	}
	if got := managedCount(); got != 1 {
		t.Fatalf("rejected payload leaked object count=%d, want referenced object only", got)
	}

	missingBody := "missing corpus orphan"
	if _, err := service.CreateDocument(ctx, "desktop-user", "missing", CreateDocumentInput{
		IdempotencyKey: "missing-corpus", Filename: "missing.txt", MediaType: "text/plain",
		SizeBytes: int64(len(missingBody)), Body: strings.NewReader(missingBody),
	}); err == nil {
		t.Fatal("missing corpus create unexpectedly succeeded")
	}
	if got := managedCount(); got != 1 {
		t.Fatalf("missing corpus leaked object count=%d, want referenced object only", got)
	}
}

func TestCreateDocumentDoesNotReuseBlobAcrossCorpora(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	repository := NewSQLiteSemanticIndexRepository(db)
	if _, err := repository.BindLegacyDefaultCorpus(ctx, "desktop-user", "second"); err != nil {
		t.Fatal(err)
	}
	body := "same bytes, separate corpus blob"
	create := func(corpusID, key string) CreateDocumentResult {
		result, err := service.CreateDocument(ctx, "desktop-user", corpusID, CreateDocumentInput{
			IdempotencyKey: key, Filename: "same.txt", MediaType: "text/plain",
			SizeBytes: int64(len(body)), Body: strings.NewReader(body),
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := create("default", "same-default")
	second := create("second", "same-second")
	if first.DocumentID == second.DocumentID {
		t.Fatal("cross-corpus upload reused document")
	}
	var firstCorpus, secondCorpus string
	if err := db.QueryRowContext(ctx, `SELECT corpus_uid FROM kb_documents WHERE id=?`, first.DocumentID).Scan(&firstCorpus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT corpus_uid FROM kb_documents WHERE id=?`, second.DocumentID).Scan(&secondCorpus); err != nil {
		t.Fatal(err)
	}
	if firstCorpus == "" || secondCorpus == "" || firstCorpus == secondCorpus {
		t.Fatalf("document corpus ownership=%q/%q", firstCorpus, secondCorpus)
	}
	rows, err := db.QueryContext(ctx, `SELECT b.corpus_uid,b.storage_path
		FROM kb_ingest_blobs b ORDER BY b.corpus_uid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var corpusUID, path string
		if err := rows.Scan(&corpusUID, &path); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] == paths[1] {
		t.Fatalf("cross-corpus blob rows/paths=%v", paths)
	}
}

func TestConfigureDocumentIngestPrunesOnlyUnreferencedManagedObjects(t *testing.T) {
	_, service, ctx := newAsyncIngestHarness(t)
	root := filepath.Join(t.TempDir(), "objects")
	if err := service.ConfigureDocumentIngest(root); err != nil {
		t.Fatal(err)
	}
	body := "referenced durable bytes"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "keep-referenced", Filename: "keep.txt", MediaType: "text/plain",
		SizeBytes: int64(len(body)), Body: strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	referenced, err := service.GetIngestDocument(ctx, "desktop-user", accepted.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	orphanDigest := strings.Repeat("a", 64)
	orphanPath := filepath.Join(root, strings.Repeat("b", 24), "aa", orphanDigest)
	if err := os.MkdirAll(filepath.Dir(orphanPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	staleIncoming := filepath.Join(root, ".incoming", "upload-stale")
	if err := os.MkdirAll(filepath.Dir(staleIncoming), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleIncoming, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigureDocumentIngest(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(referenced.StoragePath); err != nil {
		t.Fatalf("referenced object was removed: %v", err)
	}
	for _, path := range []string{orphanPath, staleIncoming} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan %s still exists: %v", path, err)
		}
	}
}
