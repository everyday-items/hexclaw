package knowledge

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

func TestSQLiteStoreInitIncludesSoftDeleteColumnForScopedRetrieval(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`PRAGMA table_info(kb_documents)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "deleted" {
			found = true
		}
	}
	if !found {
		t.Fatal("kb_documents.deleted column missing after SQLiteStore.Init")
	}
}

type semanticMutationHarness struct {
	t       *testing.T
	ctx     context.Context
	db      *sql.DB
	store   *SQLiteStore
	repo    *SQLiteSemanticIndexRepository
	service *SemanticIndexService
	active  string
}

func newSemanticMutationHarness(t *testing.T) *semanticMutationHarness {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "semantic-mutations.db") +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	baseStore := NewSQLiteStore(db)
	if err := baseStore.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, db, semanticIndexTestMigrations()); err != nil {
		t.Fatal(err)
	}
	repo := NewSQLiteSemanticIndexRepository(db)
	resolver := &workerTestResolver{profiles: map[string]EmbeddingProfileSnapshot{
		"profile-a": revisionSearchProfile("profile-a", "ollama", "bge-m3",
			ProviderLocationLocal, ProfileAvailabilityInstalled, 3, "hash-a"),
		"profile-b": revisionSearchProfile("profile-b", "openai", "text-embedding-3-small",
			ProviderLocationCloud, ProfileAvailabilityConnected, 3, "hash-b"),
	}}
	service := NewSemanticIndexService(repo, resolver)
	if _, err := repo.BindLegacyDefaultCorpus(ctx, "owner-1", "default"); err != nil {
		t.Fatal(err)
	}
	boot, err := service.EnsureDefaultPolicy(ctx, "owner-1", "default")
	if err != nil || boot.ActiveRevisionID == nil {
		t.Fatalf("bootstrap active revision: result=%+v err=%v", boot, err)
	}
	return &semanticMutationHarness{
		t: t, ctx: ctx, db: db,
		store: NewSQLiteStore(db, WithSQLiteSemanticMutations("owner-1", "default")),
		repo:  repo, service: service, active: *boot.ActiveRevisionID,
	}
}

func semanticMutationDocument() (*Document, []*Chunk) {
	now := time.Unix(1_800_300_000, 0).UTC()
	doc := &Document{
		ID: "doc-1", Title: "Scoped", Content: "alpha beta", Source: "manual",
		SourceType: "manual", ChunkCount: 2, Status: "indexed",
		CreatedAt: now, UpdatedAt: now,
	}
	chunks := []*Chunk{
		{ID: "chunk-a", DocID: doc.ID, Content: "alpha", Index: 0, CreatedAt: now},
		{ID: "chunk-b", DocID: doc.ID, Content: "beta", Index: 1, CreatedAt: now},
	}
	return doc, chunks
}

func TestSQLiteStoreSemanticAddAtomicallyBindsAndQueuesActiveRevisionJob(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	var generation, contentVersion, expectedChunks int64
	var lifecycle, textState, kind, state, targetRevision string
	if err := h.db.QueryRowContext(h.ctx, `SELECT b.content_generation,b.lifecycle_state,b.text_state,c.content_version
		FROM kb_semantic_document_bindings b JOIN kb_semantic_corpora c ON c.corpus_uid=b.corpus_uid
		WHERE b.document_id='doc-1' AND c.owner_id='owner-1' AND c.corpus_alias='default'`).Scan(
		&generation, &lifecycle, &textState, &contentVersion,
	); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT kind,state,target_revision_id
		FROM kb_knowledge_jobs WHERE document_id='doc-1'`).Scan(&kind, &state, &targetRevision); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT expected_chunks FROM kb_revision_documents
		WHERE revision_id=? AND document_id='doc-1' AND content_generation=1`, h.active).Scan(&expectedChunks); err != nil {
		t.Fatal(err)
	}
	if generation != 1 || lifecycle != "active" || textState != "ready" || contentVersion != 1 ||
		kind != string(KnowledgeJobEmbedDocument) || state != string(KnowledgeJobQueued) ||
		targetRevision != h.active || expectedChunks != 2 {
		t.Fatalf("semantic add state: gen=%d lifecycle=%s text=%s version=%d job=%s/%s target=%s chunks=%d",
			generation, lifecycle, textState, contentVersion, kind, state, targetRevision, expectedChunks)
	}
}

func TestSQLiteStoreSemanticAddRollsBackDocumentWhenCorpusMutationFails(t *testing.T) {
	h := newSemanticMutationHarness(t)
	store := NewSQLiteStore(h.db, WithSQLiteSemanticMutations("missing-owner", "default"))
	doc, chunks := semanticMutationDocument()
	if err := store.Add(h.ctx, doc, chunks); err == nil {
		t.Fatal("semantic Add succeeded without an owner-scoped corpus")
	}
	var documents, chunkRows, ftsRows int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_documents WHERE id='doc-1'`).Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_chunks WHERE doc_id='doc-1'`).Scan(&chunkRows); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_chunks_fts WHERE chunk_id IN ('chunk-a','chunk-b')`).Scan(&ftsRows); err != nil {
		t.Fatal(err)
	}
	if documents != 0 || chunkRows != 0 || ftsRows != 0 {
		t.Fatalf("failed semantic Add leaked rows: documents=%d chunks=%d fts=%d", documents, chunkRows, ftsRows)
	}
}

func (h *semanticMutationHarness) runWorker(now time.Time, workerID string) {
	h.t.Helper()
	executor := &scriptedWorkerExecutor{dimension: 3}
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": executor},
	}, workerConfig(&now, workerID, 16))
	processed, err := worker.RunOnce(h.ctx)
	if err != nil || !processed {
		h.t.Fatalf("run semantic mutation worker: processed=%v err=%v", processed, err)
	}
}

func TestSQLiteStoreSemanticReplaceAdvancesGenerationAndQueuesOnlyNewContent(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	h.runWorker(time.Unix(1_800_300_100, 0).UTC(), "worker-add")

	doc.Content = "gamma replacement"
	doc.ChunkCount = 1
	doc.UpdatedAt = time.Unix(1_800_300_200, 0).UTC()
	replacement := []*Chunk{{
		ID: "chunk-c", DocID: doc.ID, Content: "gamma replacement", Index: 0, CreatedAt: doc.UpdatedAt,
	}}
	if err := h.store.Replace(h.ctx, doc, replacement); err != nil {
		t.Fatal(err)
	}
	var generation, contentVersion int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT b.content_generation,c.content_version
		FROM kb_semantic_document_bindings b JOIN kb_semantic_corpora c ON c.corpus_uid=b.corpus_uid
		WHERE b.document_id='doc-1'`).Scan(&generation, &contentVersion); err != nil {
		t.Fatal(err)
	}
	var queuedGeneration, expected int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT document_generation,chunks_total
		FROM kb_knowledge_jobs WHERE document_id='doc-1' AND state='queued'`).Scan(
		&queuedGeneration, &expected,
	); err != nil {
		t.Fatal(err)
	}
	if generation != 2 || contentVersion != 2 || queuedGeneration != 2 || expected != 1 {
		t.Fatalf("replace state: generation=%d version=%d queuedGeneration=%d expected=%d",
			generation, contentVersion, queuedGeneration, expected)
	}
	var currentVisible int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*)
		FROM kb_revision_vectors v JOIN kb_semantic_document_bindings b
		ON b.corpus_uid=v.corpus_uid AND b.document_id=v.document_id
		AND b.content_generation=v.content_generation
		WHERE v.document_id='doc-1' AND b.lifecycle_state='active'`).Scan(&currentVisible); err != nil {
		t.Fatal(err)
	}
	if currentVisible != 0 {
		t.Fatalf("old generation remained visible before replacement embedding: %d", currentVisible)
	}

	h.runWorker(time.Unix(1_800_300_300, 0).UTC(), "worker-replace")
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*)
		FROM kb_revision_vectors v JOIN kb_semantic_document_bindings b
		ON b.corpus_uid=v.corpus_uid AND b.document_id=v.document_id
		AND b.content_generation=v.content_generation
		WHERE v.document_id='doc-1' AND b.lifecycle_state='active'`).Scan(&currentVisible); err != nil {
		t.Fatal(err)
	}
	if currentVisible != 1 {
		t.Fatalf("replacement current-generation vectors=%d, want 1", currentVisible)
	}
	var expectedChunks, embeddedChunks, failedChunks int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT expected_chunks,embedded_chunks,failed_chunks
		FROM kb_index_revisions WHERE revision_id=?`, h.active).Scan(
		&expectedChunks, &embeddedChunks, &failedChunks,
	); err != nil {
		t.Fatal(err)
	}
	if expectedChunks != 1 || embeddedChunks != 1 || failedChunks != 0 {
		t.Fatalf("active current-generation aggregate=%d/%d failed=%d, want 1/1/0",
			embeddedChunks, expectedChunks, failedChunks)
	}
}

func TestSQLiteStoreSemanticDeleteTombstonesAndFencesLateVectorCommit(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_301_000, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-before-delete", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim document job: job=%+v ok=%v err=%v", job, ok, err)
	}
	inputs, err := h.repo.ListRevisionChunkInputs(h.ctx, job.Lease(), now.Add(time.Second), nil, 10)
	if err != nil || len(inputs) != 2 {
		t.Fatalf("list document inputs: len=%d err=%v", len(inputs), err)
	}
	manifestInput := makeEmbeddingBatchManifest(job.JobID, JobExecutionPlan{
		RevisionID: job.TargetRevisionID,
		Snapshot: revisionSearchProfile("profile-a", "ollama", "bge-m3",
			ProviderLocationLocal, ProfileAvailabilityInstalled, 3, "hash-a"),
	}, inputs, []string{"alpha", "beta"})
	manifest, err := h.repo.CreateEmbeddingBatchManifest(h.ctx, job.Lease(), now.Add(2*time.Second), manifestInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.repo.BeginEmbeddingBatch(h.ctx, job.Lease(), now.Add(2500*time.Millisecond), manifest.BatchID); err != nil {
		t.Fatal(err)
	}

	if err := h.store.Delete(h.ctx, "doc-1"); err != nil {
		t.Fatal(err)
	}
	lateVectors := []RevisionVector{
		vectorForInput(inputs[0], []float32{1, 0, 0}),
		vectorForInput(inputs[1], []float32{0, 1, 0}),
	}
	if err := h.repo.CommitEmbeddingBatch(h.ctx, job.Lease(), now.Add(3*time.Second), EmbeddingBatchCommit{
		BatchID: manifest.BatchID, Vectors: lateVectors, ChunksDone: 2, ChunksTotal: 2,
	}); !errors.Is(err, ErrJobFenced) {
		t.Fatalf("late deleted-document commit error=%v, want ErrJobFenced", err)
	}
	var deleted, generation, generationFacts, ftsRows, vectors, contentVersion int64
	var lifecycle string
	if err := h.db.QueryRowContext(h.ctx, `SELECT deleted FROM kb_documents WHERE id='doc-1'`).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT lifecycle_state,content_generation
		FROM kb_semantic_document_bindings WHERE document_id='doc-1'`).Scan(
		&lifecycle, &generation,
	); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_semantic_document_generations
		WHERE document_id='doc-1'`).Scan(&generationFacts); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_chunks_fts
		WHERE chunk_id IN ('chunk-a','chunk-b')`).Scan(&ftsRows); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_vectors`).Scan(&vectors); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT content_version FROM kb_semantic_corpora
		WHERE owner_id='owner-1' AND corpus_alias='default'`).Scan(&contentVersion); err != nil {
		t.Fatal(err)
	}
	if deleted != 1 || lifecycle != "tombstoned" || generation != 2 || generationFacts != 2 ||
		ftsRows != 0 || vectors != 0 || contentVersion != 2 {
		t.Fatalf("delete state: deleted=%d lifecycle=%s generation=%d facts=%d fts=%d vectors=%d version=%d",
			deleted, lifecycle, generation, generationFacts, ftsRows, vectors, contentVersion)
	}
	var expectedChunks, embeddedChunks, failedChunks int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT expected_chunks,embedded_chunks,failed_chunks
		FROM kb_index_revisions WHERE revision_id=?`, h.active).Scan(
		&expectedChunks, &embeddedChunks, &failedChunks,
	); err != nil {
		t.Fatal(err)
	}
	if expectedChunks != 0 || embeddedChunks != 0 || failedChunks != 0 {
		t.Fatalf("deleted active aggregate=%d/%d failed=%d, want 0/0/0",
			embeddedChunks, expectedChunks, failedChunks)
	}
	listed, err := h.store.List(h.ctx)
	if err != nil || len(listed) != 0 {
		t.Fatalf("List after tombstone=%+v err=%v", listed, err)
	}
	if got, err := h.store.Get(h.ctx, "doc-1"); err == nil || got != nil {
		t.Fatalf("Get tombstoned doc=%+v err=%v, want not found", got, err)
	}
}

func TestSQLiteStoreSemanticActiveWatermarkWaitsForEveryCurrentDocument(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_302_000, 0).UTC()
	docTwo := &Document{
		ID: "doc-2", Title: "Second", Content: "delta", Source: "manual-2",
		SourceType: "manual", ChunkCount: 1, Status: "indexed",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := h.store.Add(h.ctx, docTwo, []*Chunk{{
		ID: "chunk-d", DocID: docTwo.ID, Content: "delta", Index: 0, CreatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}

	h.runWorker(now.Add(time.Minute), "worker-first-document")
	var indexedThrough int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT indexed_through_version
		FROM kb_index_revisions WHERE revision_id=?`, h.active).Scan(&indexedThrough); err != nil {
		t.Fatal(err)
	}
	if indexedThrough >= 2 {
		t.Fatalf("first document advanced corpus watermark to %d while another current document was pending", indexedThrough)
	}

	h.runWorker(now.Add(2*time.Minute), "worker-second-document")
	if err := h.db.QueryRowContext(h.ctx, `SELECT indexed_through_version
		FROM kb_index_revisions WHERE revision_id=?`, h.active).Scan(&indexedThrough); err != nil {
		t.Fatal(err)
	}
	if indexedThrough != 2 {
		t.Fatalf("completed active corpus watermark=%d, want 2", indexedThrough)
	}
}

func TestSQLiteStoreSemanticStagedReplaceDropsSupersededGenerationProgress(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	h.runWorker(time.Unix(1_800_303_000, 0).UTC(), "worker-active")
	policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", policy.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	if err != nil || staged.DesiredRevisionID == nil {
		t.Fatalf("stage profile-b: result=%+v err=%v", staged, err)
	}

	now := time.Unix(1_800_303_100, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-partial", now, time.Minute)
	if err != nil || !ok || job.Kind != KnowledgeJobRebuildRevision {
		t.Fatalf("claim staged rebuild: job=%+v ok=%v err=%v", job, ok, err)
	}
	plan, err := h.repo.LoadJobExecutionPlan(h.ctx, job.Lease(), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := h.repo.ListRevisionChunkInputs(h.ctx, job.Lease(), now.Add(2*time.Second), nil, 1)
	if err != nil || len(inputs) != 1 {
		t.Fatalf("list first staged chunk: len=%d err=%v", len(inputs), err)
	}
	manifest, err := h.repo.CreateEmbeddingBatchManifest(h.ctx, job.Lease(), now.Add(3*time.Second),
		makeEmbeddingBatchManifest(job.JobID, plan, inputs, []string{inputs[0].Content}))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.repo.BeginEmbeddingBatch(h.ctx, job.Lease(), now.Add(3500*time.Millisecond), manifest.BatchID); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CommitEmbeddingBatch(h.ctx, job.Lease(), now.Add(4*time.Second), EmbeddingBatchCommit{
		BatchID: manifest.BatchID, ChunksDone: 1, ChunksTotal: 2,
		Vectors: []RevisionVector{vectorForInput(inputs[0], []float32{1, 0, 0})},
	}); err != nil {
		t.Fatal(err)
	}

	// Deliberately reuse the exact committed chunk ID and content. Generation
	// must still produce a distinct batch identity instead of replaying the old
	// succeeded manifest as a no-op.
	doc.Content = inputs[0].Content
	doc.ChunkCount = 1
	doc.UpdatedAt = now.Add(5 * time.Second)
	if err := h.store.Replace(h.ctx, doc, []*Chunk{{
		ID: inputs[0].ChunkID, DocID: doc.ID, Content: doc.Content, Index: 0, CreatedAt: doc.UpdatedAt,
	}}); err != nil {
		t.Fatal(err)
	}
	var staleVectors, staleDocuments, revisionEmbedded int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_vectors
		WHERE revision_id=? AND document_id='doc-1' AND content_generation=1`,
		*staged.DesiredRevisionID).Scan(&staleVectors); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_documents
		WHERE revision_id=? AND document_id='doc-1' AND content_generation=1`,
		*staged.DesiredRevisionID).Scan(&staleDocuments); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT embedded_chunks FROM kb_index_revisions
		WHERE revision_id=?`, *staged.DesiredRevisionID).Scan(&revisionEmbedded); err != nil {
		t.Fatal(err)
	}
	if staleVectors != 0 || staleDocuments != 0 || revisionEmbedded != 0 {
		t.Fatalf("staged superseded generation leaked: vectors=%d documents=%d aggregate=%d",
			staleVectors, staleDocuments, revisionEmbedded)
	}

	workerNow := now.Add(2 * time.Minute)
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{executors: map[string]ProfileEmbeddingExecutor{
		"profile-b": &scriptedWorkerExecutor{dimension: 3},
	}}, workerConfig(&workerNow, "worker-restaged", 16))
	processed, err := worker.RunOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("finish restaged revision: processed=%v err=%v", processed, err)
	}
	after, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil || after.ActiveRevision == nil || after.ActiveRevision.RevisionID != *staged.DesiredRevisionID {
		t.Fatalf("restaged policy=%+v err=%v", after, err)
	}
	var currentVectors int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_vectors
		WHERE revision_id=? AND document_id='doc-1' AND content_generation=2`,
		*staged.DesiredRevisionID).Scan(&currentVectors); err != nil {
		t.Fatal(err)
	}
	if currentVectors != 1 {
		t.Fatalf("current staged generation vectors=%d, want 1", currentVectors)
	}
}

func TestSQLiteStoreSemanticStagedDeleteAfterPartialCommitPublishesCurrentEmptySet(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	h.runWorker(time.Unix(1_800_303_500, 0).UTC(), "worker-active")
	policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", policy.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	if err != nil || staged.DesiredRevisionID == nil {
		t.Fatalf("stage profile-b: result=%+v err=%v", staged, err)
	}
	now := time.Unix(1_800_303_600, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(h.ctx, "owner-1", "default", "worker-delete-partial", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim staged rebuild: job=%+v ok=%v err=%v", job, ok, err)
	}
	plan, err := h.repo.LoadJobExecutionPlan(h.ctx, job.Lease(), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := h.repo.ListRevisionChunkInputs(h.ctx, job.Lease(), now.Add(2*time.Second), nil, 1)
	if err != nil || len(inputs) != 1 {
		t.Fatalf("list partial input: len=%d err=%v", len(inputs), err)
	}
	manifest, err := h.repo.CreateEmbeddingBatchManifest(h.ctx, job.Lease(), now.Add(3*time.Second),
		makeEmbeddingBatchManifest(job.JobID, plan, inputs, []string{inputs[0].Content}))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.repo.BeginEmbeddingBatch(h.ctx, job.Lease(), now.Add(3500*time.Millisecond), manifest.BatchID); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CommitEmbeddingBatch(h.ctx, job.Lease(), now.Add(4*time.Second), EmbeddingBatchCommit{
		BatchID: manifest.BatchID, ChunksDone: 1, ChunksTotal: 2,
		Vectors: []RevisionVector{vectorForInput(inputs[0], []float32{1, 0, 0})},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Delete(h.ctx, doc.ID); err != nil {
		t.Fatal(err)
	}
	workerNow := now.Add(2 * time.Minute)
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{executors: map[string]ProfileEmbeddingExecutor{
		"profile-b": &scriptedWorkerExecutor{dimension: 3},
	}}, workerConfig(&workerNow, "worker-delete-restaged", 16))
	processed, err := worker.RunOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("publish empty current set: processed=%v err=%v", processed, err)
	}
	after, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil || after.ActiveRevision == nil || after.ActiveRevision.RevisionID != *staged.DesiredRevisionID {
		t.Fatalf("empty-set publish policy=%+v err=%v", after, err)
	}
	var expected, embedded, vectors int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT expected_chunks,embedded_chunks
		FROM kb_index_revisions WHERE revision_id=?`, *staged.DesiredRevisionID).Scan(&expected, &embedded); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_vectors
		WHERE revision_id=?`, *staged.DesiredRevisionID).Scan(&vectors); err != nil {
		t.Fatal(err)
	}
	if expected != 0 || embedded != 0 || vectors != 0 {
		t.Fatalf("published deleted current set: expected=%d embedded=%d vectors=%d", expected, embedded, vectors)
	}
}

func TestSQLiteStoreSemanticCancelDesiredQueuesCurrentGenerationForOldActive(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	h.runWorker(time.Unix(1_800_304_000, 0).UTC(), "worker-active")
	policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", policy.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	if err != nil || staged.JobID == nil {
		t.Fatalf("stage profile-b: result=%+v err=%v", staged, err)
	}
	doc.Content = "replacement while desired"
	doc.ChunkCount = 1
	doc.UpdatedAt = time.Unix(1_800_304_100, 0).UTC()
	if err := h.store.Replace(h.ctx, doc, []*Chunk{{
		ID: "chunk-current", DocID: doc.ID, Content: doc.Content, Index: 0, CreatedAt: doc.UpdatedAt,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CancelJob(h.ctx, "owner-1", *staged.JobID); err != nil {
		t.Fatal(err)
	}
	var catchupJob, targetRevision string
	var generation int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT job_id,target_revision_id,document_generation
		FROM kb_knowledge_jobs WHERE kind='embed_document' AND document_id='doc-1'
		  AND state='queued' ORDER BY created_at DESC LIMIT 1`).Scan(
		&catchupJob, &targetRevision, &generation,
	); err != nil {
		t.Fatalf("load active catch-up job: %v", err)
	}
	if catchupJob == "" || targetRevision != h.active || generation != 2 {
		t.Fatalf("catch-up job=%q target=%q generation=%d, want old active/gen2",
			catchupJob, targetRevision, generation)
	}
	workerNow := time.Unix(1_800_304_200, 0).UTC()
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{executors: map[string]ProfileEmbeddingExecutor{
		"profile-a": &scriptedWorkerExecutor{dimension: 3},
	}}, workerConfig(&workerNow, "worker-catchup", 16))
	processed, err := worker.RunOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("run active catch-up: processed=%v err=%v", processed, err)
	}
	var currentVectors int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_vectors
		WHERE revision_id=? AND document_id='doc-1' AND content_generation=2`, h.active).Scan(&currentVectors); err != nil {
		t.Fatal(err)
	}
	if currentVectors != 1 {
		t.Fatalf("old active current-generation vectors=%d, want 1", currentVectors)
	}
}

func TestSQLiteStoreSemanticCancelDesiredQueuesAddedDocumentForOldActive(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	h.runWorker(time.Unix(1_800_305_000, 0).UTC(), "worker-active")
	policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", policy.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	if err != nil || staged.JobID == nil {
		t.Fatalf("stage profile-b: result=%+v err=%v", staged, err)
	}
	now := time.Unix(1_800_305_100, 0).UTC()
	added := &Document{
		ID: "doc-added", Title: "Added", Content: "new while desired", Source: "manual-added",
		SourceType: "manual", ChunkCount: 1, Status: "indexed", CreatedAt: now, UpdatedAt: now,
	}
	if err := h.store.Add(h.ctx, added, []*Chunk{{
		ID: "chunk-added", DocID: added.ID, Content: added.Content, Index: 0, CreatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.CancelJob(h.ctx, "owner-1", *staged.JobID); err != nil {
		t.Fatal(err)
	}
	var target string
	var generation int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT target_revision_id,document_generation
		FROM kb_knowledge_jobs WHERE kind='embed_document' AND document_id='doc-added' AND state='queued'`).Scan(
		&target, &generation,
	); err != nil {
		t.Fatal(err)
	}
	if target != h.active || generation != 1 {
		t.Fatalf("added catch-up target=%q generation=%d, want active/gen1", target, generation)
	}
}

func TestSQLiteStoreSemanticActiveCompatibleIntentReconcilesDesiredOnlyAdd(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	h.runWorker(time.Unix(1_800_306_000, 0).UTC(), "worker-active")
	policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", policy.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	if err != nil || staged.DesiredRevisionID == nil {
		t.Fatalf("stage profile-b: result=%+v err=%v", staged, err)
	}
	now := time.Unix(1_800_306_100, 0).UTC()
	added := &Document{
		ID: "doc-intent", Title: "Intent", Content: "intent add", Source: "manual-intent",
		SourceType: "manual", ChunkCount: 1, Status: "indexed", CreatedAt: now, UpdatedAt: now,
	}
	if err := h.store.Add(h.ctx, added, []*Chunk{{
		ID: "chunk-intent", DocID: added.ID, Content: added.Content, Index: 0, CreatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	current, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", current.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionAuto})
	if err != nil || restored.Branch != ApplyPolicyIntentOnly || restored.DesiredRevisionID != nil {
		t.Fatalf("restore active-compatible intent: result=%+v err=%v", restored, err)
	}
	var jobs int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE kind='embed_document' AND document_id='doc-intent' AND target_revision_id=? AND state='queued'`,
		h.active).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("active-compatible intent catch-up jobs=%d, want 1", jobs)
	}
}

func TestSQLiteStoreSemanticOtherDocumentMutationDoesNotStrandCompletedJob(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks[:1]); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_307_000, 0).UTC()
	executor := &scriptedWorkerExecutor{dimension: 3}
	executor.afterCall = func() {
		other := &Document{
			ID: "doc-concurrent", Title: "Concurrent", Content: "arrived during provider",
			Source: "manual-concurrent", SourceType: "manual", ChunkCount: 1, Status: "indexed",
			CreatedAt: now, UpdatedAt: now,
		}
		if err := h.store.Add(h.ctx, other, []*Chunk{{
			ID: "chunk-concurrent", DocID: other.ID, Content: other.Content, Index: 0, CreatedAt: now,
		}}); err != nil {
			t.Errorf("add other document during provider call: %v", err)
		}
	}
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{executors: map[string]ProfileEmbeddingExecutor{
		"profile-a": executor,
	}}, workerConfig(&now, "worker-concurrent-mutation", 16))
	processed, err := worker.RunOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("complete first document across other mutation: processed=%v err=%v", processed, err)
	}
	var succeeded, queued int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE kind='embed_document' AND document_id='doc-1' AND state='succeeded'`).Scan(&succeeded); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE kind='embed_document' AND document_id='doc-concurrent' AND state='queued'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || queued != 1 {
		t.Fatalf("interleaved jobs: first succeeded=%d other queued=%d", succeeded, queued)
	}
}

func TestSQLiteStoreSemanticDeleteThenSameSourceTitleReuploadRevivesNewGeneration(t *testing.T) {
	h := newSemanticMutationHarness(t)
	if _, err := h.db.ExecContext(h.ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_kb_documents_unique
		ON kb_documents(source,title) WHERE source!=''`); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultHybridConfig()
	cfg.ContextualEnabled = false
	manager := NewManager(h.store, h.store, nil, WithSplitter(testSplitter()), WithHybridConfig(cfg))
	first, err := manager.AddDocument(h.ctx, "Stable title", "first body", "stable-source")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteDocument(h.ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	reuploaded, err := manager.AddDocument(h.ctx, "Stable title", "second body", "stable-source")
	if err != nil {
		t.Fatalf("reupload tombstoned source/title: %v", err)
	}
	if reuploaded.ID != first.ID {
		t.Fatalf("reupload document id=%q, want revived %q", reuploaded.ID, first.ID)
	}
	var generation, deleted int64
	var lifecycle string
	if err := h.db.QueryRowContext(h.ctx, `SELECT b.content_generation,b.lifecycle_state,d.deleted
		FROM kb_semantic_document_bindings b JOIN kb_documents d ON d.id=b.document_id
		WHERE b.document_id=?`, first.ID).Scan(&generation, &lifecycle, &deleted); err != nil {
		t.Fatal(err)
	}
	var ftsRows, jobs, gcJobs int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_chunks_fts f
		JOIN kb_chunks c ON c.id=f.chunk_id WHERE c.doc_id=?`, first.ID).Scan(&ftsRows); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE kind='embed_document' AND document_id=? AND document_generation=3 AND state='queued'`,
		first.ID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE kind='gc' AND idempotency_key=?`,
		documentGCIdempotencyPrefix+hex.EncodeToString([]byte(first.ID))).Scan(&gcJobs); err != nil {
		t.Fatal(err)
	}
	if generation != 3 || lifecycle != "active" || deleted != 0 || ftsRows == 0 || jobs != 1 || gcJobs != 0 {
		t.Fatalf("revived state: generation=%d lifecycle=%s deleted=%d fts=%d jobs=%d gc_jobs=%d",
			generation, lifecycle, deleted, ftsRows, jobs, gcJobs)
	}
}

func TestSQLiteStoreSemanticFailedDesiredKeepsCRUDTextFirstAndRoutesToActive(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	h.runWorker(time.Unix(1_800_308_000, 0).UTC(), "worker-active-before-failure")
	policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", policy.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	if err != nil || staged.JobID == nil || staged.DesiredRevisionID == nil {
		t.Fatalf("stage desired revision: result=%+v err=%v", staged, err)
	}
	now := time.Unix(1_800_308_100, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "worker-fail-desired", now, time.Minute,
	)
	if err != nil || !ok || job.JobID != *staged.JobID {
		t.Fatalf("claim desired rebuild: job=%+v ok=%v err=%v", job, ok, err)
	}
	if _, err := h.repo.FailJob(h.ctx, job.Lease(), now.Add(time.Second), "provider rejected"); err != nil {
		t.Fatal(err)
	}

	added := &Document{
		ID: "doc-after-failure", Title: "Added after failure", Content: "failedaddtoken",
		Source: "manual-failed-add", SourceType: "manual", ChunkCount: 1, Status: "indexed",
		CreatedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(2 * time.Second),
	}
	if err := h.store.Add(h.ctx, added, []*Chunk{{
		ID: "chunk-after-failure", DocID: added.ID, Content: added.Content, Index: 0,
		CreatedAt: added.CreatedAt,
	}}); err != nil {
		t.Fatalf("add while desired is terminal failed: %v", err)
	}
	doc.Content = "failedreplacetoken"
	doc.ChunkCount = 1
	doc.UpdatedAt = now.Add(3 * time.Second)
	if err := h.store.Replace(h.ctx, doc, []*Chunk{{
		ID: "chunk-replaced-after-failure", DocID: doc.ID, Content: doc.Content, Index: 0,
		CreatedAt: doc.UpdatedAt,
	}}); err != nil {
		t.Fatalf("replace while desired is terminal failed: %v", err)
	}
	for _, query := range []string{"failedaddtoken", "failedreplacetoken"} {
		results, searchErr := h.store.TextSearch(h.ctx, query, 10, Filter{})
		if searchErr != nil || len(results) != 1 {
			t.Fatalf("text-first search %q results=%+v err=%v", query, results, searchErr)
		}
	}
	if err := h.store.Delete(h.ctx, added.ID); err != nil {
		t.Fatalf("delete while desired is terminal failed: %v", err)
	}
	var deletedFTS int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_chunks_fts
		WHERE chunk_id='chunk-after-failure'`).Scan(&deletedFTS); err != nil {
		t.Fatal(err)
	}
	if deletedFTS != 0 {
		t.Fatalf("deleted document FTS rows=%d, want 0", deletedFTS)
	}
	deletedResults, err := h.store.TextSearch(h.ctx, "failedaddtoken", 10, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range deletedResults {
		if result.Chunk != nil && result.Chunk.DocID == added.ID {
			t.Fatalf("deleted document remained text-visible: results=%+v", deletedResults)
		}
	}

	failedProjection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if failedProjection.PolicyVersion != staged.PolicyVersion || failedProjection.DesiredRevision == nil ||
		failedProjection.DesiredRevision.State != VectorIndexFailed ||
		failedProjection.DesiredRevision.RevisionID != *staged.DesiredRevisionID {
		t.Fatalf("CRUD changed failed desired audit state: %+v", failedProjection)
	}
	var activeReplacementJobs, abandonedRows int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE kind='embed_document' AND document_id='doc-1' AND document_generation=2
		  AND target_revision_id=? AND state='queued'`, h.active).Scan(&activeReplacementJobs); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_documents
		WHERE revision_id=? AND document_id IN ('doc-1','doc-after-failure')
		  AND content_generation>1`, *staged.DesiredRevisionID).Scan(&abandonedRows); err != nil {
		t.Fatal(err)
	}
	if activeReplacementJobs != 1 || abandonedRows != 0 {
		t.Fatalf("terminal desired routing: active replacement jobs=%d abandoned rows=%d",
			activeReplacementJobs, abandonedRows)
	}
	if _, err := h.service.CancelJob(h.ctx, "owner-1", *staged.JobID); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE kind='embed_document' AND document_id='doc-1' AND document_generation=2
		  AND target_revision_id=?`, h.active).Scan(&activeReplacementJobs); err != nil {
		t.Fatal(err)
	}
	if activeReplacementJobs != 1 {
		t.Fatalf("cancel duplicated active catch-up jobs=%d, want 1", activeReplacementJobs)
	}
}

func TestCompleteIngestDocumentRoutesPastFailedDesiredToActive(t *testing.T) {
	h := newSemanticMutationHarness(t)
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_310_000, 0).UTC()
	h.runWorker(now, "worker-active-before-async-failure")
	policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", policy.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	if err != nil || staged.JobID == nil || staged.DesiredRevisionID == nil {
		t.Fatalf("stage desired revision: result=%+v err=%v", staged, err)
	}
	rebuild, ok, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "worker-fail-desired-before-ingest", now.Add(time.Second), time.Minute,
	)
	if err != nil || !ok || rebuild.JobID != *staged.JobID {
		t.Fatalf("claim desired rebuild: job=%+v ok=%v err=%v", rebuild, ok, err)
	}
	if _, err := h.repo.FailJob(h.ctx, rebuild.Lease(), now.Add(2*time.Second), "provider rejected"); err != nil {
		t.Fatal(err)
	}

	if err := h.service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}
	const content = "async text remains publishable after desired revision failure"
	accepted, err := h.service.CreateDocument(h.ctx, "owner-1", "default", CreateDocumentInput{
		IdempotencyKey: "failed-desired-async-ingest", Filename: "after-failure.txt",
		MediaType: "text/plain", SizeBytes: int64(len(content)), Body: strings.NewReader(content),
	})
	if err != nil {
		t.Fatal(err)
	}
	ingest, ok, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "worker-ingest-after-failure", now.Add(3*time.Second), time.Minute,
	)
	if err != nil || !ok || ingest.JobID != accepted.JobID {
		t.Fatalf("claim ingest after desired failure: job=%+v ok=%v err=%v", ingest, ok, err)
	}
	prepared := PreparedIngestDocument{
		Document:  &Document{ID: accepted.DocumentID, Content: content},
		Chunks:    []*Chunk{{ID: "chunk-after-failed-desired-ingest", DocID: accepted.DocumentID, Content: content}},
		PageCount: 1,
	}
	if err := h.repo.CompleteIngestDocument(h.ctx, ingest.Lease(), now.Add(4*time.Second), prepared); err != nil {
		t.Fatalf("publish text after desired failure: %v", err)
	}

	results, err := h.store.TextSearch(h.ctx, "publishable", 10, Filter{})
	if err != nil || len(results) != 1 || results[0].Chunk.DocID != accepted.DocumentID {
		t.Fatalf("published text results=%+v err=%v", results, err)
	}
	var activeJobs, abandonedRows int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE parent_job_id=? AND kind='embed_document' AND target_revision_id=?
		  AND document_id=? AND document_generation=1 AND state='queued'`,
		accepted.JobID, h.active, accepted.DocumentID).Scan(&activeJobs); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_documents
		WHERE revision_id=? AND document_id=?`, *staged.DesiredRevisionID, accepted.DocumentID).
		Scan(&abandonedRows); err != nil {
		t.Fatal(err)
	}
	if activeJobs != 1 || abandonedRows != 0 {
		t.Fatalf("terminal desired ingest routing: active jobs=%d abandoned rows=%d", activeJobs, abandonedRows)
	}
}

func TestSQLiteStoreSemanticFailedDesiredWithoutActiveKeepsAddTextOnly(t *testing.T) {
	h := newSemanticMutationHarness(t)
	policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", policy.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionDisabled})
	if err != nil {
		t.Fatal(err)
	}
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", disabled.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-a"})
	if err != nil || staged.ActiveRevisionID != nil || staged.JobID == nil {
		t.Fatalf("stage without active: result=%+v err=%v", staged, err)
	}
	now := time.Unix(1_800_309_000, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "worker-fail-no-active", now, time.Minute,
	)
	if err != nil || !ok {
		t.Fatalf("claim no-active desired: job=%+v ok=%v err=%v", job, ok, err)
	}
	if _, err := h.repo.FailJob(h.ctx, job.Lease(), now.Add(time.Second), "provider rejected"); err != nil {
		t.Fatal(err)
	}
	added := &Document{
		ID: "doc-text-only", Title: "Text only", Content: "textonlyafterfailure",
		Source: "manual-text-only", SourceType: "manual", ChunkCount: 1, Status: "indexed",
		CreatedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(2 * time.Second),
	}
	if err := h.store.Add(h.ctx, added, []*Chunk{{
		ID: "chunk-text-only", DocID: added.ID, Content: added.Content, Index: 0,
		CreatedAt: added.CreatedAt,
	}}); err != nil {
		t.Fatalf("text-only add after desired failure: %v", err)
	}
	results, err := h.store.TextSearch(h.ctx, "textonlyafterfailure", 10, Filter{})
	if err != nil || len(results) != 1 {
		t.Fatalf("text-only search results=%+v err=%v", results, err)
	}
	var semanticRows int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_documents
		WHERE document_id='doc-text-only'`).Scan(&semanticRows); err != nil {
		t.Fatal(err)
	}
	if semanticRows != 0 {
		t.Fatalf("no-active failed desired received semantic rows=%d, want 0", semanticRows)
	}
}

func newNoActiveFailedDesiredIngestHarness(t *testing.T) (*semanticMutationHarness, string, time.Time) {
	t.Helper()
	h := newSemanticMutationHarness(t)
	policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", policy.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionDisabled})
	if err != nil {
		t.Fatal(err)
	}
	doc, chunks := semanticMutationDocument()
	if err := h.store.Add(h.ctx, doc, chunks); err != nil {
		t.Fatal(err)
	}
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", disabled.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-a"})
	if err != nil || staged.ActiveRevisionID != nil || staged.DesiredRevisionID == nil || staged.JobID == nil {
		t.Fatalf("stage desired without active: result=%+v err=%v", staged, err)
	}
	now := time.Unix(1_800_312_000, 0).UTC()
	job, ok, err := h.repo.ClaimNextJobForCorpus(
		h.ctx, "owner-1", "default", "worker-fail-no-active-ingest", now, time.Minute,
	)
	if err != nil || !ok || job.JobID != *staged.JobID {
		t.Fatalf("claim no-active desired: job=%+v ok=%v err=%v", job, ok, err)
	}
	if _, err := h.repo.FailJob(h.ctx, job.Lease(), now.Add(time.Second), "provider rejected"); err != nil {
		t.Fatal(err)
	}
	if err := h.service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatal(err)
	}
	return h, *staged.DesiredRevisionID, now
}

func createNoActiveFailedDesiredIngest(t *testing.T, h *semanticMutationHarness, key string) CreateDocumentResult {
	t.Helper()
	const content = "async document remains text only after desired revision failure"
	result, err := h.service.CreateDocument(h.ctx, "owner-1", "default", CreateDocumentInput{
		IdempotencyKey: key,
		Filename:       "text-only-after-failure.txt",
		MediaType:      "text/plain",
		SizeBytes:      int64(len(content)),
		Body:           strings.NewReader(content),
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAsyncIngestFailedDesiredWithoutActiveReportsAndPersistsTextOnly(t *testing.T) {
	t.Run("first response is failed rather than permanently pending", func(t *testing.T) {
		h, _, _ := newNoActiveFailedDesiredIngestHarness(t)
		accepted := createNoActiveFailedDesiredIngest(t, h, "no-active-first-response")
		if accepted.TextIndexState != TextIndexPending || accepted.VectorIndexState != VectorIndexFailed {
			t.Fatalf("accepted states=%+v, want text pending/vector failed", accepted)
		}
	})

	t.Run("idempotency replay preserves failed vector state", func(t *testing.T) {
		h, _, _ := newNoActiveFailedDesiredIngestHarness(t)
		first := createNoActiveFailedDesiredIngest(t, h, "no-active-idempotency-replay")
		replayed := createNoActiveFailedDesiredIngest(t, h, "no-active-idempotency-replay")
		if replayed != first || replayed.VectorIndexState != VectorIndexFailed {
			t.Fatalf("idempotency replay=%+v first=%+v, want identical vector failed response", replayed, first)
		}
	})

	t.Run("completion publishes text projection without semantic target", func(t *testing.T) {
		h, desiredRevisionID, now := newNoActiveFailedDesiredIngestHarness(t)
		accepted := createNoActiveFailedDesiredIngest(t, h, "no-active-completion")
		ingest, ok, err := h.repo.ClaimNextJobForCorpus(
			h.ctx, "owner-1", "default", "worker-complete-no-active-ingest", now.Add(2*time.Second), time.Minute,
		)
		if err != nil || !ok || ingest.JobID != accepted.JobID {
			t.Fatalf("claim text-only ingest: job=%+v ok=%v err=%v", ingest, ok, err)
		}
		const content = "async document remains text only after desired revision failure"
		prepared := PreparedIngestDocument{
			Document:  &Document{ID: accepted.DocumentID, Content: content},
			Chunks:    []*Chunk{{ID: "chunk-no-active-async", DocID: accepted.DocumentID, Content: content}},
			PageCount: 1,
		}
		if err := h.repo.CompleteIngestDocument(h.ctx, ingest.Lease(), now.Add(3*time.Second), prepared); err != nil {
			t.Fatalf("complete text-only ingest: %v", err)
		}
		projection, err := h.service.GetIngestDocumentProjectionForCorpus(
			h.ctx, "owner-1", "default", accepted.DocumentID,
		)
		if err != nil || projection.TextIndexState != TextIndexReady || projection.PageCount == nil || *projection.PageCount != 1 {
			t.Fatalf("completed text projection=%+v err=%v", projection, err)
		}
		var revisionRows, embedJobs int64
		if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_documents
			WHERE document_id=?`, accepted.DocumentID).Scan(&revisionRows); err != nil {
			t.Fatal(err)
		}
		if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
			WHERE kind='embed_document' AND document_id=?`, accepted.DocumentID).Scan(&embedJobs); err != nil {
			t.Fatal(err)
		}
		policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
		if err != nil || policy.ActiveRevision != nil || policy.DesiredRevision == nil ||
			policy.DesiredRevision.RevisionID != desiredRevisionID || policy.DesiredRevision.State != VectorIndexFailed {
			t.Fatalf("completed text-only policy=%+v err=%v", policy, err)
		}
		if revisionRows != 0 || embedJobs != 0 {
			t.Fatalf("text-only completion semantic rows=%d embed jobs=%d, want 0/0", revisionRows, embedJobs)
		}
	})
}

func TestLegacyBindingFailedDesiredWithoutActiveStaysTextOnly(t *testing.T) {
	h, desiredRevisionID, _ := newNoActiveFailedDesiredIngestHarness(t)
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_documents(id,title,content,deleted)
		VALUES('legacy-after-no-active-failure','Legacy text only','legacytextonly',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_chunks(id,doc_id,content,chunk_index,embedding)
		VALUES('legacy-chunk-after-no-active-failure','legacy-after-no-active-failure','legacytextonly',0,NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.BindLegacyDefaultCorpus(h.ctx, "owner-1", "default"); err != nil {
		t.Fatalf("bind legacy text-only document: %v", err)
	}
	results, err := h.store.TextSearch(h.ctx, "legacytextonly", 10, Filter{})
	if err != nil || len(results) != 1 || results[0].Chunk.DocID != "legacy-after-no-active-failure" {
		t.Fatalf("legacy text-only search results=%+v err=%v", results, err)
	}
	var revisionRows, embedJobs int64
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_documents
		WHERE document_id='legacy-after-no-active-failure'`).Scan(&revisionRows); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE kind='embed_document' AND document_id='legacy-after-no-active-failure'`).Scan(&embedJobs); err != nil {
		t.Fatal(err)
	}
	policy, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil || policy.ActiveRevision != nil || policy.DesiredRevision == nil ||
		policy.DesiredRevision.RevisionID != desiredRevisionID || policy.DesiredRevision.State != VectorIndexFailed {
		t.Fatalf("legacy text-only policy=%+v err=%v", policy, err)
	}
	if revisionRows != 0 || embedJobs != 0 {
		t.Fatalf("legacy text-only semantic rows=%d embed jobs=%d, want 0/0", revisionRows, embedJobs)
	}
}
