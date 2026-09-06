package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/localinfer"
	"github.com/hexagon-codes/hexclaw/resourcegov"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

type workerTestResolver struct {
	profiles map[string]EmbeddingProfileSnapshot
}

func (r *workerTestResolver) Resolve(
	_ context.Context,
	_, _ string,
	selection EmbeddingSelection,
) (EmbeddingProfileSnapshot, error) {
	profileID := selection.ProfileID
	if selection.Kind == EmbeddingSelectionAuto {
		profileID = "profile-a"
	}
	profile, ok := r.profiles[profileID]
	if !ok {
		return EmbeddingProfileSnapshot{}, ErrProfileUnavailable
	}
	return profile, nil
}

func (r *workerTestResolver) Catalog(context.Context, string, string) (EmbeddingProfileCatalog, error) {
	return EmbeddingProfileCatalog{}, nil
}

type scriptedWorkerExecutor struct {
	dimension int
	failAt    int
	failAll   bool
	calls     int
	batches   [][]string
	purposes  []EmbeddingPurpose
	afterCall func()
}

func (e *scriptedWorkerExecutor) Embed(_ context.Context, texts []string) ([][]float32, error) {
	return e.embed(texts)
}

func (e *scriptedWorkerExecutor) EmbedForPurpose(
	_ context.Context,
	purpose EmbeddingPurpose,
	texts []string,
) ([][]float32, error) {
	e.purposes = append(e.purposes, purpose)
	return e.embed(texts)
}

func (e *scriptedWorkerExecutor) embed(texts []string) ([][]float32, error) {
	e.calls++
	e.batches = append(e.batches, append([]string(nil), texts...))
	if e.afterCall != nil {
		e.afterCall()
	}
	if e.failAll || (e.failAt > 0 && e.calls == e.failAt) {
		return nil, errors.New("scripted provider interruption")
	}
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = make([]float32, e.dimension)
		vectors[i][i%e.dimension] = 1
	}
	return vectors, nil
}

type contextBlockingWorkerExecutor struct {
	started chan struct{}
}

func (e *contextBlockingWorkerExecutor) EmbedForPurpose(
	ctx context.Context,
	_ EmbeddingPurpose,
	_ []string,
) ([][]float32, error) {
	if e.started != nil {
		select {
		case <-e.started:
		default:
			close(e.started)
		}
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

type readinessWorkerExecutor struct {
	coordinator    *localinfer.Coordinator
	ready          bool
	readinessCalls int
	embedAttempts  int
	embedCalls     int
	probeErr       error
}

func (e *readinessWorkerExecutor) EmbeddingReady(ctx context.Context) bool {
	e.readinessCalls++
	if e.ready || e.coordinator == nil {
		return e.ready
	}
	operation := localinfer.OperationFromContext(ctx, localinfer.OperationProbe)
	_, lease, err := e.coordinator.Acquire(localinfer.WithOperation(ctx, operation), operation)
	if err != nil {
		e.probeErr = err
		return false
	}
	lease.Finish(nil)
	e.ready = true
	return true
}

func (e *readinessWorkerExecutor) EmbedForPurpose(
	ctx context.Context,
	_ EmbeddingPurpose,
	texts []string,
) (vectors [][]float32, err error) {
	e.embedAttempts++
	if !e.EmbeddingReady(ctx) {
		return nil, ErrEmbeddingUnavailable
	}
	e.embedCalls++
	operation := localinfer.OperationFromContext(ctx, localinfer.OperationDocumentEmbedding)
	_, lease, err := e.coordinator.Acquire(localinfer.WithOperation(ctx, operation), operation)
	if err != nil {
		return nil, err
	}
	defer func() { lease.Finish(err) }()
	vectors = make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = []float32{1, 0, 0}
	}
	return vectors, nil
}

type workerExecutorRegistry struct {
	executors map[string]ProfileEmbeddingExecutor
}

func (r *workerExecutorRegistry) ExecutorForProfile(
	_ context.Context,
	profile EmbeddingProfileSnapshot,
) (ProfileEmbeddingExecutor, error) {
	executor, ok := r.executors[profile.Profile.ProfileID]
	if !ok {
		return nil, ErrProfileUnavailable
	}
	return executor, nil
}

type workerHarness struct {
	t       *testing.T
	ctx     context.Context
	db      *sql.DB
	repo    *SQLiteSemanticIndexRepository
	service *SemanticIndexService
}

// workerPostManifestRenewRepository proves the provider-side durable fence:
// after a manifest is prepared, every local or cloud provider invocation must
// renew the job lease before it marks that manifest in-flight.
type workerPostManifestRenewRepository struct {
	SemanticIndexWorkerRepository
	forceCloud           bool
	beginCalls           int
	manifestPrepared     bool
	renewedAfterManifest bool
}

func (r *workerPostManifestRenewRepository) LoadJobExecutionPlan(
	ctx context.Context,
	lease JobLease,
	now time.Time,
) (JobExecutionPlan, error) {
	plan, err := r.SemanticIndexWorkerRepository.LoadJobExecutionPlan(ctx, lease, now)
	if err == nil && r.forceCloud {
		plan.Snapshot.Profile.Location = ProviderLocationCloud
		plan.Snapshot.Profile.Availability = ProfileAvailabilityConnected
	}
	return plan, err
}

func (r *workerPostManifestRenewRepository) CreateEmbeddingBatchManifest(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	manifest EmbeddingBatchManifest,
) (EmbeddingBatchManifest, error) {
	created, err := r.SemanticIndexWorkerRepository.CreateEmbeddingBatchManifest(ctx, lease, now, manifest)
	if err == nil {
		r.manifestPrepared = true
		r.renewedAfterManifest = false
	}
	return created, err
}

func (r *workerPostManifestRenewRepository) RenewJobLease(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	leaseDuration time.Duration,
) (JobLease, error) {
	renewer := r.SemanticIndexWorkerRepository.(semanticIndexLeaseRenewer)
	renewed, err := renewer.RenewJobLease(ctx, lease, now, leaseDuration)
	if err == nil && r.manifestPrepared {
		r.renewedAfterManifest = true
	}
	return renewed, err
}

func (r *workerPostManifestRenewRepository) BeginEmbeddingBatch(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	batchID string,
) error {
	r.beginCalls++
	if r.manifestPrepared && !r.renewedAfterManifest {
		return errors.New("provider boundary crossed without post-manifest lease renewal")
	}
	return r.SemanticIndexWorkerRepository.BeginEmbeddingBatch(ctx, lease, now, batchID)
}

func newWorkerHarness(t *testing.T, chunks ...string) *workerHarness {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "semantic-worker.db") +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE kb_documents (
		id TEXT PRIMARY KEY,title TEXT NOT NULL,content TEXT NOT NULL,source TEXT NOT NULL DEFAULT '',
		deleted INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE kb_chunks (
		id TEXT PRIMARY KEY,doc_id TEXT NOT NULL,content TEXT NOT NULL,
		chunk_index INTEGER NOT NULL,embedding BLOB
	)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, db, semanticIndexTestMigrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_documents(id,title,content,deleted)
		VALUES('legacy-doc','Legacy','legacy body',0)`); err != nil {
		t.Fatal(err)
	}
	for i, content := range chunks {
		if _, err := db.ExecContext(ctx, `INSERT INTO kb_chunks(id,doc_id,content,chunk_index,embedding)
			VALUES(?,?,?,?,NULL)`, "chunk-"+string(rune('a'+i)), "legacy-doc", content, i); err != nil {
			t.Fatal(err)
		}
	}
	profiles := map[string]EmbeddingProfileSnapshot{
		"profile-a": revisionSearchProfile("profile-a", "ollama", "bge-m3",
			ProviderLocationLocal, ProfileAvailabilityInstalled, 3, "hash-a"),
		"profile-download": revisionSearchProfile("profile-download", "ollama", "nomic-embed-text",
			ProviderLocationLocal, ProfileAvailabilityDownloadable, 5, "hash-download"),
	}
	repo := NewSQLiteSemanticIndexRepository(db)
	service := NewSemanticIndexService(repo, &workerTestResolver{profiles: profiles})
	if _, err := repo.BindLegacyDefaultCorpus(ctx, "owner-1", "default"); err != nil {
		t.Fatalf("bind legacy corpus: %v", err)
	}
	if _, err := service.EnsureDefaultPolicy(ctx, "owner-1", "default"); err != nil {
		t.Fatalf("stage legacy backfill: %v", err)
	}
	return &workerHarness{t: t, ctx: ctx, db: db, repo: repo, service: service}
}

func workerConfig(now *time.Time, workerID string, batchSize int) SemanticIndexWorkerConfig {
	return SemanticIndexWorkerConfig{
		OwnerID: "owner-1", CorpusID: "default", WorkerID: workerID,
		BatchSize: batchSize, LeaseDuration: time.Minute, RetryDelay: 30 * time.Second,
		Now: func() time.Time { return *now },
	}
}

func TestEmbeddingBatchIdentityIncludesDocumentGeneration(t *testing.T) {
	plan := JobExecutionPlan{
		RevisionID: "revision-a",
		Snapshot: revisionSearchProfile("profile-a", "ollama", "bge-m3",
			ProviderLocationLocal, ProfileAvailabilityInstalled, 3, "hash-a"),
	}
	genOne := []RevisionChunkInput{{
		DocumentID: "doc-1", ContentGeneration: 1, ChunkID: "chunk-stable",
		ChunkIndex: 0, Content: "same", ContentHash: "same-hash",
	}}
	genTwo := append([]RevisionChunkInput(nil), genOne...)
	genTwo[0].ContentGeneration = 2
	first := makeEmbeddingBatchManifest("job-a", plan, genOne, []string{"same"})
	second := makeEmbeddingBatchManifest("job-a", plan, genTwo, []string{"same"})
	if first.ChunkIDsDigest == second.ChunkIDsDigest || first.ClientRequestKey == second.ClientRequestKey {
		t.Fatalf("generation change reused batch identity: first=%+v second=%+v", first, second)
	}
}

func TestSemanticIndexWorkerCommitsStableBatchesAndPublishesRevision(t *testing.T) {
	h := newWorkerHarness(t, "alpha chunk", "beta chunk", "gamma chunk")
	now := time.Unix(1_800_200_000, 0).UTC()
	executor := &scriptedWorkerExecutor{dimension: 3}
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": executor},
	}, workerConfig(&now, "worker-1", 2))

	processed, err := worker.RunOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("RunOnce: processed=%v err=%v", processed, err)
	}
	if want := [][]string{{"alpha chunk", "beta chunk"}, {"gamma chunk"}}; !reflect.DeepEqual(executor.batches, want) {
		t.Fatalf("provider batches = %#v, want %#v", executor.batches, want)
	}
	if want := []EmbeddingPurpose{EmbeddingPurposeDocument, EmbeddingPurposeDocument}; !reflect.DeepEqual(executor.purposes, want) {
		t.Fatalf("provider purposes = %v, want %v", executor.purposes, want)
	}
	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if projection.ActiveRevision == nil || projection.DesiredRevision != nil {
		t.Fatalf("worker did not publish staged revision: %+v", projection)
	}
	var vectors, manifests, legacyVectors int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_vectors
		WHERE revision_id=? AND profile_config_hash='hash-a' AND dimension=3`,
		projection.ActiveRevision.RevisionID).Scan(&vectors); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_embedding_batch_manifests
		WHERE state='succeeded'`).Scan(&manifests); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_chunks WHERE embedding IS NOT NULL`).Scan(&legacyVectors); err != nil {
		t.Fatal(err)
	}
	if vectors != 3 || manifests != 2 || legacyVectors != 0 {
		t.Fatalf("persisted counts: vectors=%d manifests=%d legacy=%d, want 3/2/0",
			vectors, manifests, legacyVectors)
	}
}

func TestSemanticIndexWorkerRestartSkipsCommittedBatch(t *testing.T) {
	h := newWorkerHarness(t, "alpha chunk", "beta chunk", "gamma chunk")
	now := time.Unix(1_800_201_000, 0).UTC()
	interrupted := &scriptedWorkerExecutor{dimension: 3, failAt: 2}
	first := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": interrupted},
	}, workerConfig(&now, "worker-before-restart", 1))
	processed, err := first.RunOnce(h.ctx)
	if !processed || err == nil {
		t.Fatalf("interrupted RunOnce: processed=%v err=%v, want processed error", processed, err)
	}
	var vectors int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_vectors`).Scan(&vectors); err != nil || vectors != 1 {
		t.Fatalf("first committed checkpoint vectors=%d err=%v, want 1", vectors, err)
	}
	// 恢复时只有旧租约的调用开始事实，没有 Provider 完成结果。
	if _, err := h.db.ExecContext(h.ctx, `UPDATE kb_embedding_batch_manifests
		SET state='in_flight',next_attempt_at=NULL,last_error='' WHERE state='retry_wait'`); err != nil {
		t.Fatal(err)
	}

	now = now.Add(30 * time.Second)
	restartedExecutor := &scriptedWorkerExecutor{dimension: 3}
	restarted := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": restartedExecutor},
	}, workerConfig(&now, "worker-after-restart", 1))
	processed, err = restarted.RunOnce(h.ctx)
	if !errors.Is(err, ErrEmbeddingBatchOutcomeUnknown) || !processed {
		t.Fatalf("restart must park stale in-flight batch once: processed=%v err=%v", processed, err)
	}
	if len(restartedExecutor.batches) != 0 {
		t.Fatalf("restart resent unknown batch: %#v", restartedExecutor.batches)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_vectors`).Scan(&vectors); err != nil || vectors != 1 {
		t.Fatalf("restart changed committed vectors=%d err=%v, want 1", vectors, err)
	}
	var state, lastError string
	if err := h.db.QueryRowContext(h.ctx, `SELECT state,last_error FROM kb_embedding_batch_manifests
		WHERE state<>'succeeded'`).Scan(&state, &lastError); err != nil ||
		state != string(EmbeddingBatchOutcomeUnknown) || !strings.Contains(lastError, "lease") {
		t.Fatalf("stale batch outcome=%q cause=%q err=%v", state, lastError, err)
	}
	if processed, err := restarted.RunOnce(h.ctx); processed || err != nil {
		t.Fatalf("stale unknown batch entered another retry: processed=%v err=%v", processed, err)
	}
}

func TestSemanticIndexWorkerCancelledDuringProviderCallCommitsZeroVectors(t *testing.T) {
	h := newWorkerHarness(t, "alpha chunk")
	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil || projection.DesiredRevision == nil || projection.DesiredRevision.JobID == nil {
		t.Fatalf("load staged job: projection=%+v err=%v", projection, err)
	}
	now := time.Unix(1_800_202_000, 0).UTC()
	executor := &scriptedWorkerExecutor{dimension: 3}
	executor.afterCall = func() {
		if _, cancelErr := h.service.CancelJob(h.ctx, "owner-1", *projection.DesiredRevision.JobID); cancelErr != nil {
			t.Errorf("cancel during provider call: %v", cancelErr)
		}
	}
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": executor},
	}, workerConfig(&now, "worker-cancelled", 1))

	processed, err := worker.RunOnce(h.ctx)
	if !processed || !errors.Is(err, ErrJobFenced) {
		t.Fatalf("cancelled RunOnce: processed=%v err=%v, want ErrJobFenced", processed, err)
	}
	var vectors int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_vectors`).Scan(&vectors); err != nil || vectors != 0 {
		t.Fatalf("cancelled late result wrote vectors=%d err=%v", vectors, err)
	}
}

func TestSemanticIndexWorkerRejectsWrongProviderDimensionWithoutWrites(t *testing.T) {
	h := newWorkerHarness(t, "alpha chunk")
	now := time.Unix(1_800_203_000, 0).UTC()
	executor := &scriptedWorkerExecutor{dimension: 2}
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": executor},
	}, workerConfig(&now, "worker-wrong-dimension", 1))

	processed, err := worker.RunOnce(h.ctx)
	if !processed || !errors.Is(err, ErrInvalidEmbeddingResult) {
		t.Fatalf("wrong-dimension RunOnce: processed=%v err=%v", processed, err)
	}
	var vectors int
	if err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM kb_revision_vectors`).Scan(&vectors); err != nil || vectors != 0 {
		t.Fatalf("wrong-dimensional provider wrote vectors=%d err=%v", vectors, err)
	}
	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil || projection.DesiredRevision == nil || projection.DesiredRevision.State != VectorIndexFailed {
		t.Fatalf("permanent dimension failure projection=%+v err=%v", projection, err)
	}
}

func TestSemanticIndexWorkerCompletesActiveDocumentJobWithoutRepublishingRevision(t *testing.T) {
	h := newRevisionSearchHarness(t)
	boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	var corpusUID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT corpus_uid FROM kb_semantic_corpora
		WHERE owner_id='owner-1' AND corpus_alias='default'`).Scan(&corpusUID); err != nil {
		t.Fatal(err)
	}
	h.addLegacyDocument("incremental-doc", "incremental semantic text", nil)
	h.bindDocument("owner-1", corpusUID, "incremental-doc")
	now := time.Unix(1_800_204_000, 0).UTC()
	if _, err := h.db.ExecContext(h.ctx, `UPDATE kb_semantic_corpora
		SET content_version=1,updated_at=? WHERE corpus_uid=?`, now.UnixMilli(), corpusUID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `UPDATE kb_index_revisions
		SET expected_chunks=1 WHERE revision_id=?`, *boot.ActiveRevisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_revision_documents
		(revision_id,corpus_uid,document_id,content_generation,vector_state,
		expected_chunks,embedded_chunks,failed_chunks,visible_at,last_error,updated_at)
		VALUES(?,?,?,1,'pending',1,0,0,NULL,'',?)`, *boot.ActiveRevisionID,
		corpusUID, "incremental-doc", now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_knowledge_jobs
		(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,document_generation,
		target_revision_id,idempotency_key,state,stage,attempt,cancel_requested,
		lease_owner,lease_epoch,last_error,created_at,updated_at)
		VALUES('job-incremental',NULL,'embed_document','owner-1',?,'incremental-doc',1,
		?,'incremental-v1','queued','embedding',0,0,'',0,'',?,?)`, corpusUID,
		*boot.ActiveRevisionID, now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	executor := &scriptedWorkerExecutor{dimension: 3}
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": executor},
	}, workerConfig(&now, "worker-incremental", 8))
	processed, err := worker.RunOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("incremental RunOnce: processed=%v err=%v", processed, err)
	}
	after, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if after.ActiveRevision == nil || after.ActiveRevision.RevisionID != *boot.ActiveRevisionID || after.DesiredRevision != nil {
		t.Fatalf("incremental job republished policy/revision: before=%+v after=%+v", boot, after)
	}
	job, err := h.service.GetJob(h.ctx, "owner-1", "job-incremental")
	if err != nil || job.State != KnowledgeJobSucceeded {
		t.Fatalf("incremental job=%+v err=%v, want succeeded", job, err)
	}
	if !reflect.DeepEqual(executor.purposes, []EmbeddingPurpose{EmbeddingPurposeDocument}) {
		t.Fatalf("incremental embedding purposes=%v", executor.purposes)
	}
}

func TestSemanticIndexWorkerMarksUnsupportedDownloadJobFailedInsteadOfRetryingForever(t *testing.T) {
	h := newWorkerHarness(t, "alpha chunk")
	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil || projection.DesiredRevision == nil || projection.DesiredRevision.JobID == nil {
		t.Fatalf("load staged revision: projection=%+v err=%v", projection, err)
	}
	// Policy apply can no longer create download_model jobs; seed one as a
	// legacy/corrupt durable row to prove the worker still fails it terminally.
	var corpusUID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT corpus_uid FROM kb_semantic_corpora
		WHERE owner_id='owner-1' AND corpus_alias='default'`).Scan(&corpusUID); err != nil {
		t.Fatal(err)
	}
	seedTime := time.Unix(1_800_204_900, 0).UTC().UnixMilli()
	if _, err := h.db.ExecContext(h.ctx, `UPDATE kb_knowledge_jobs
		SET state='cancelled',cancel_requested=1,finished_at=?,updated_at=?
		WHERE job_id=?`, seedTime, seedTime, *projection.DesiredRevision.JobID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_knowledge_jobs
		(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,document_generation,
		 target_revision_id,idempotency_key,state,stage,attempt,cancel_requested,
		 lease_owner,lease_epoch,last_error,created_at,updated_at)
		VALUES('legacy-download-job',NULL,'download_model','owner-1',?,NULL,NULL,?,
		 'legacy-download','queued','embedding',0,0,'',0,'',?,?)`, corpusUID,
		projection.DesiredRevision.RevisionID, seedTime+1, seedTime+1); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_205_000, 0).UTC()
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{executors: map[string]ProfileEmbeddingExecutor{}},
		workerConfig(&now, "worker-no-downloader", 8))
	processed, err := worker.RunOnce(h.ctx)
	if !processed || !errors.Is(err, ErrUnsupportedKnowledgeJob) {
		t.Fatalf("unsupported download RunOnce: processed=%v err=%v", processed, err)
	}
	job, err := h.service.GetJob(h.ctx, "owner-1", "legacy-download-job")
	if err != nil || job.State != KnowledgeJobFailed || job.NextAttemptAt != nil {
		t.Fatalf("unsupported download job=%+v err=%v, want terminal failed", job, err)
	}
}

func TestSemanticIndexWorkerUsesCappedExponentialRetryAndStopsAtMaxAttempts(t *testing.T) {
	h := newWorkerHarness(t, "alpha chunk")
	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil || projection.DesiredRevision == nil || projection.DesiredRevision.JobID == nil {
		t.Fatalf("load staged job: projection=%+v err=%v", projection, err)
	}
	jobID := *projection.DesiredRevision.JobID
	now := time.Unix(1_800_208_000, 0).UTC()
	config := workerConfig(&now, "worker-bounded-retry", 1)
	config.RetryDelay = 10 * time.Second
	config.MaxRetryDelay = 15 * time.Second
	config.MaxAttempts = 3
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{executors: map[string]ProfileEmbeddingExecutor{
		"profile-a": &scriptedWorkerExecutor{dimension: 3, failAll: true},
	}}, config)

	for attempt, wantDelay := range []time.Duration{10 * time.Second, 15 * time.Second} {
		processed, runErr := worker.RunOnce(h.ctx)
		if !processed || runErr == nil {
			t.Fatalf("attempt %d: processed=%v err=%v", attempt+1, processed, runErr)
		}
		job, getErr := h.service.GetJob(h.ctx, "owner-1", jobID)
		if getErr != nil || job.State != KnowledgeJobRetryWait || job.NextAttemptAt == nil {
			t.Fatalf("attempt %d job=%+v err=%v", attempt+1, job, getErr)
		}
		wantNext := now.Add(wantDelay)
		if !job.NextAttemptAt.Equal(wantNext) {
			t.Fatalf("attempt %d next=%v, want %v", attempt+1, job.NextAttemptAt, wantNext)
		}
		now = wantNext
	}
	processed, runErr := worker.RunOnce(h.ctx)
	if !processed || runErr == nil {
		t.Fatalf("terminal attempt: processed=%v err=%v", processed, runErr)
	}
	job, err := h.service.GetJob(h.ctx, "owner-1", jobID)
	if err != nil || job.State != KnowledgeJobFailed || job.NextAttemptAt != nil || job.Attempt != 3 {
		t.Fatalf("terminal retry job=%+v err=%v", job, err)
	}
}

func TestSemanticIndexWorkerBatchTimeoutPersistsOutcomeUnknownWithoutBlindRetry(t *testing.T) {
	h := newWorkerHarness(t, "alpha chunk")
	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil || projection.DesiredRevision == nil || projection.DesiredRevision.JobID == nil {
		t.Fatalf("load staged job: projection=%+v err=%v", projection, err)
	}
	now := time.Unix(1_800_209_000, 0).UTC()
	config := workerConfig(&now, "worker-batch-timeout", 1)
	config.EmbeddingTimeout = 10 * time.Millisecond
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{executors: map[string]ProfileEmbeddingExecutor{
		"profile-a": &contextBlockingWorkerExecutor{},
	}}, config)
	processed, runErr := worker.RunOnce(h.ctx)
	if !processed || !errors.Is(runErr, context.DeadlineExceeded) {
		t.Fatalf("timed batch: processed=%v err=%v", processed, runErr)
	}
	job, err := h.service.GetJob(h.ctx, "owner-1", *projection.DesiredRevision.JobID)
	if err != nil || job.State != KnowledgeJobFailed || job.LeaseOwner != "" || job.LeaseExpiresAt != nil ||
		!strings.Contains(job.LastError, "outcome unknown") {
		t.Fatalf("timed batch did not stop for reconciliation: job=%+v err=%v", job, err)
	}
	var batchState, batchError string
	if err := h.db.QueryRowContext(h.ctx, `SELECT state,last_error
		FROM kb_embedding_batch_manifests WHERE job_id=?`, job.JobID).Scan(&batchState, &batchError); err != nil {
		t.Fatal(err)
	}
	if batchState != string(EmbeddingBatchOutcomeUnknown) || !strings.Contains(batchError, "deadline exceeded") {
		t.Fatalf("timed batch state=%q error=%q, want durable outcome_unknown", batchState, batchError)
	}
	processed, err = worker.RunOnce(h.ctx)
	if err != nil || processed {
		t.Fatalf("outcome-unknown batch was blindly retried: processed=%v err=%v", processed, err)
	}
}

func TestSemanticIndexWorkerCancellationWhileQueuedDoesNotBeginProviderInvocation(t *testing.T) {
	h := newWorkerHarness(t, "alpha chunk")
	now := time.Now().UTC()
	governorConfig := resourcegov.DefaultConfig()
	governorConfig.Limits[resourcegov.ResourceAccelerator] = 1
	governor, err := resourcegov.New(governorConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	hold, err := governor.Acquire(h.ctx, resourcegov.ResourceAccelerator, resourcegov.PriorityInteractive)
	if err != nil {
		t.Fatal(err)
	}

	executor := &scriptedWorkerExecutor{dimension: 3}
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": executor},
	}, workerConfig(&now, "worker-resource-cancel", 1),
		WithSemanticWorkerResourceGovernor(governor))
	workerCtx, cancelWorker := context.WithCancel(h.ctx)
	type result struct {
		processed bool
		err       error
	}
	done := make(chan result, 1)
	go func() {
		processed, runErr := worker.RunOnce(workerCtx)
		done <- result{processed: processed, err: runErr}
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if governor.Snapshot().Resources[resourcegov.ResourceAccelerator].QueuedBackground == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := governor.Snapshot().Resources[resourcegov.ResourceAccelerator].QueuedBackground; got != 1 {
		t.Fatalf("background embedding did not queue: %d", got)
	}
	cancelWorker()
	got := <-done
	if !got.processed || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("RunOnce cancellation: processed=%v err=%v", got.processed, got.err)
	}
	hold.Release()
	if executor.calls != 0 {
		t.Fatalf("provider was invoked while resource wait was cancelled: calls=%d", executor.calls)
	}
	var batchState string
	if err := h.db.QueryRowContext(h.ctx, `SELECT state FROM kb_embedding_batch_manifests`).Scan(&batchState); err != nil {
		t.Fatal(err)
	}
	if batchState != string(EmbeddingBatchPrepared) {
		t.Fatalf("resource cancellation crossed BeginEmbeddingBatch boundary: state=%q", batchState)
	}
	metric := governor.Snapshot().Resources[resourcegov.ResourceAccelerator]
	if metric.InUse != 0 || metric.QueuedBackground != 0 {
		t.Fatalf("accelerator permit leaked after cancellation: %+v", metric)
	}
}

func TestSemanticIndexWorkerRejectsUnavailableExecutorBeforeLocalAdmissionAndDurableBegin(t *testing.T) {
	h := newWorkerHarness(t, "offline local chunk")
	now := time.Unix(1_800_209_250, 0).UTC()
	governor, err := resourcegov.New(resourcegov.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	coordinator := localinfer.New(governor)
	repository := &workerPostManifestRenewRepository{SemanticIndexWorkerRepository: h.repo}
	executor := &readinessWorkerExecutor{ready: false}
	worker := NewSemanticIndexWorker(repository, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": executor},
	}, workerConfig(&now, "worker-readiness-offline", 1),
		WithSemanticWorkerLocalInferenceCoordinator(coordinator))

	processed, runErr := worker.RunOnce(h.ctx)
	if !processed || !errors.Is(runErr, ErrEmbeddingUnavailable) {
		t.Fatalf("offline RunOnce: processed=%v err=%v, want ErrEmbeddingUnavailable", processed, runErr)
	}
	if executor.readinessCalls != 1 || executor.embedAttempts != 0 {
		t.Fatalf("offline executor calls: readiness=%d embed=%d, want 1/0",
			executor.readinessCalls, executor.embedAttempts)
	}
	if repository.beginCalls != 0 {
		t.Fatalf("offline executor crossed durable BeginEmbeddingBatch: calls=%d", repository.beginCalls)
	}
	metric := coordinator.Snapshot().Operations[localinfer.OperationDocumentEmbedding]
	if metric.Attempts != 0 || metric.Admitted != 0 {
		t.Fatalf("offline executor acquired document embedding capacity: %+v", metric)
	}
}

func TestSemanticIndexWorkerReadinessProbeDoesNotConsumeProviderPrelease(t *testing.T) {
	h := newWorkerHarness(t, "probe then embed")
	now := time.Unix(1_800_209_375, 0).UTC()
	governor, err := resourcegov.New(resourcegov.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	coordinator := localinfer.New(governor)
	repository := &workerPostManifestRenewRepository{SemanticIndexWorkerRepository: h.repo}
	executor := &readinessWorkerExecutor{coordinator: coordinator}
	worker := NewSemanticIndexWorker(repository, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": executor},
	}, workerConfig(&now, "worker-readiness-probe", 1),
		WithSemanticWorkerLocalInferenceCoordinator(coordinator))

	processed, runErr := worker.RunOnce(h.ctx)
	if runErr != nil || !processed {
		t.Fatalf("probe-before-prelease RunOnce: processed=%v err=%v probe_err=%v",
			processed, runErr, executor.probeErr)
	}
	if executor.readinessCalls != 2 || executor.embedAttempts != 1 || executor.embedCalls != 1 {
		t.Fatalf("probe/embed calls: readiness=%d attempts=%d physical=%d, want 2/1/1",
			executor.readinessCalls, executor.embedAttempts, executor.embedCalls)
	}
	if repository.beginCalls != 1 || !repository.renewedAfterManifest {
		t.Fatalf("durable provider fence: begin=%d renewed_after_manifest=%v, want 1/true",
			repository.beginCalls, repository.renewedAfterManifest)
	}
	snapshot := coordinator.Snapshot().Operations
	if probe := snapshot[localinfer.OperationProbe]; probe.Attempts != 1 || probe.Admitted != 1 || probe.Completed != 1 {
		t.Fatalf("readiness probe admission=%+v, want one independent completed call", probe)
	}
	if embed := snapshot[localinfer.OperationDocumentEmbedding]; embed.Attempts != 1 || embed.Admitted != 1 || embed.Completed != 1 {
		t.Fatalf("document embedding admission=%+v, want one completed owner/prelease", embed)
	}
}

func TestSemanticIndexWorkerCloudBatchRenewsLeaseAfterOptionalAdmission(t *testing.T) {
	h := newWorkerHarness(t, "cloud chunk")
	now := time.Unix(1_800_209_500, 0).UTC()
	governor, err := resourcegov.New(resourcegov.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	coordinator := localinfer.New(governor)
	repository := &workerPostManifestRenewRepository{
		SemanticIndexWorkerRepository: h.repo,
		forceCloud:                    true,
	}
	executor := &scriptedWorkerExecutor{dimension: 3}
	worker := NewSemanticIndexWorker(repository, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": executor},
	}, workerConfig(&now, "worker-cloud-renew-fence", 1),
		WithSemanticWorkerLocalInferenceCoordinator(coordinator))

	processed, runErr := worker.RunOnce(h.ctx)
	if runErr != nil || !processed {
		t.Fatalf("cloud RunOnce crossed durable fence: processed=%v err=%v", processed, runErr)
	}
	if executor.calls != 1 {
		t.Fatalf("cloud provider calls=%d, want 1", executor.calls)
	}
	if got := coordinator.Snapshot().Operations[localinfer.OperationDocumentEmbedding].Attempts; got != 0 {
		t.Fatalf("cloud document embedding acquired local slot: attempts=%d", got)
	}
}

func TestSemanticIndexWorkerShutdownCancellationReleasesRunningLease(t *testing.T) {
	h := newWorkerHarness(t, "alpha chunk")
	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil || projection.DesiredRevision == nil || projection.DesiredRevision.JobID == nil {
		t.Fatalf("load staged job: projection=%+v err=%v", projection, err)
	}
	now := time.Unix(1_800_210_000, 0).UTC()
	started := make(chan struct{})
	executor := &contextBlockingWorkerExecutor{started: started}
	worker := NewSemanticIndexWorker(h.repo, &workerExecutorRegistry{executors: map[string]ProfileEmbeddingExecutor{
		"profile-a": executor,
	}}, workerConfig(&now, "worker-shutdown", 1))
	ctx, cancel := context.WithCancel(h.ctx)
	go func() {
		<-started
		cancel()
	}()
	processed, runErr := worker.RunOnce(ctx)
	if !processed || !errors.Is(runErr, context.Canceled) {
		t.Fatalf("shutdown RunOnce: processed=%v err=%v", processed, runErr)
	}
	job, err := h.service.GetJob(h.ctx, "owner-1", *projection.DesiredRevision.JobID)
	if err != nil || job.State == KnowledgeJobRunning || job.LeaseOwner != "" || job.LeaseExpiresAt != nil {
		t.Fatalf("shutdown retained running lease: job=%+v err=%v", job, err)
	}
}

func TestSemanticIndexWorkerUnavailableSnapshotExecutorFailsTerminally(t *testing.T) {
	h := newWorkerHarness(t, "alpha chunk")
	projection, err := h.service.GetPolicy(h.ctx, "owner-1", "default")
	if err != nil || projection.DesiredRevision == nil || projection.DesiredRevision.JobID == nil {
		t.Fatalf("load staged job: projection=%+v err=%v", projection, err)
	}
	now := time.Unix(1_800_211_000, 0).UTC()
	worker := NewSemanticIndexWorker(h.repo,
		&workerExecutorRegistry{executors: map[string]ProfileEmbeddingExecutor{}},
		workerConfig(&now, "worker-unavailable-snapshot", 1))
	processed, runErr := worker.RunOnce(h.ctx)
	if !processed || !errors.Is(runErr, ErrProfileUnavailable) {
		t.Fatalf("unavailable executor: processed=%v err=%v", processed, runErr)
	}
	job, err := h.service.GetJob(h.ctx, "owner-1", *projection.DesiredRevision.JobID)
	if err != nil || job.State != KnowledgeJobFailed || job.NextAttemptAt != nil {
		t.Fatalf("unavailable executor job=%+v err=%v", job, err)
	}
}
