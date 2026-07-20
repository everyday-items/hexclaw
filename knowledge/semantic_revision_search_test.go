package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/resourcegov"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

type semanticExecutor struct {
	dimension int
	vector    []float32
	ready     *bool
	calls     int
	inputs    [][]string
	purposes  []EmbeddingPurpose
}

func (e *semanticExecutor) EmbeddingReady(context.Context) bool {
	return e.ready == nil || *e.ready
}

func (e *semanticExecutor) Embed(_ context.Context, texts []string) ([][]float32, error) {
	return e.embed(texts)
}

func (e *semanticExecutor) EmbedForPurpose(
	_ context.Context,
	purpose EmbeddingPurpose,
	texts []string,
) ([][]float32, error) {
	e.purposes = append(e.purposes, purpose)
	return e.embed(texts)
}

func (e *semanticExecutor) embed(texts []string) ([][]float32, error) {
	e.calls++
	copyOfTexts := append([]string(nil), texts...)
	e.inputs = append(e.inputs, copyOfTexts)
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		if e.vector != nil {
			vectors[i] = append([]float32(nil), e.vector...)
			continue
		}
		vectors[i] = make([]float32, e.dimension)
		vectors[i][0] = 1
	}
	return vectors, nil
}

type semanticExecutorRegistry struct {
	executors map[string]*semanticExecutor
	profiles  []string
}

type revisionSearchResolver struct {
	profiles map[string]EmbeddingProfileSnapshot
}

func (r *revisionSearchResolver) Resolve(
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

func (r *revisionSearchResolver) Catalog(context.Context, string, string) (EmbeddingProfileCatalog, error) {
	return EmbeddingProfileCatalog{}, nil
}

func revisionSearchProfile(
	profileID, providerID, modelName string,
	location ProviderLocation,
	availability ProfileAvailability,
	dimension int,
	configHash string,
) EmbeddingProfileSnapshot {
	return EmbeddingProfileSnapshot{
		Profile: EmbeddingProfile{
			ProfileID: profileID, ModelName: modelName,
			ProviderID: providerID, ProviderName: providerID,
			Location: location, Capability: "embedding", Dimension: dimension,
			Availability: availability,
		},
		Normalization:     "l2",
		ChunkConfigHash:   "chunk-v1",
		ProfileConfigHash: configHash,
	}
}

type countingLegacyEmbedder struct {
	calls int
}

func (e *countingLegacyEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	result := make([][]float32, len(texts))
	for i := range result {
		result[i] = []float32{1, 0, 0}
	}
	return result, nil
}

func (e *countingLegacyEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	result, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return result[0], nil
}

func (e *countingLegacyEmbedder) Dimension() int { return 3 }

func (r *semanticExecutorRegistry) ExecutorForProfile(
	_ context.Context,
	profile EmbeddingProfileSnapshot,
) (ProfileEmbeddingExecutor, error) {
	r.profiles = append(r.profiles, profile.Profile.ProfileID)
	executor, ok := r.executors[profile.Profile.ProfileID]
	if !ok {
		return nil, ErrProfileUnavailable
	}
	return executor, nil
}

type revisionSearchHarness struct {
	t       *testing.T
	ctx     context.Context
	db      *sql.DB
	store   *SQLiteStore
	repo    *SQLiteSemanticIndexRepository
	service *SemanticIndexService
}

func newRevisionSearchHarness(t *testing.T) *revisionSearchHarness {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "revision-search.db") +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	store := NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init legacy knowledge store: %v", err)
	}
	if err := migrate.Run(ctx, db, semanticIndexTestMigrations()); err != nil {
		t.Fatalf("migrate semantic index: %v", err)
	}
	profiles := map[string]EmbeddingProfileSnapshot{
		"profile-a": revisionSearchProfile("profile-a", "ollama", "bge-m3", ProviderLocationLocal, ProfileAvailabilityInstalled, 3, "hash-a"),
		"profile-b": revisionSearchProfile("profile-b", "openai", "text-embedding-3-small", ProviderLocationCloud, ProfileAvailabilityConnected, 4, "hash-b"),
	}
	resolver := &revisionSearchResolver{profiles: profiles}
	repo := NewSQLiteSemanticIndexRepository(db)
	return &revisionSearchHarness{
		t: t, ctx: ctx, db: db, store: store, repo: repo,
		service: NewSemanticIndexService(repo, resolver),
	}
}

func (h *revisionSearchHarness) addLegacyDocument(docID, content string, legacyVector []float32) {
	h.t.Helper()
	now := time.Unix(1_800_100_000, 0).UTC()
	doc := &Document{
		ID: docID, Title: docID, Content: content, Source: "manual",
		SourceType: "manual", ChunkCount: 1, Status: "indexed",
		CreatedAt: now, UpdatedAt: now,
	}
	chunk := &Chunk{
		ID: docID + "-chunk-0", DocID: docID, Content: content,
		Index: 0, Embedding: legacyVector, CreatedAt: now,
	}
	if err := h.store.Add(h.ctx, doc, []*Chunk{chunk}); err != nil {
		h.t.Fatalf("add legacy document: %v", err)
	}
}

func (h *revisionSearchHarness) bindDocument(ownerID, corpusUID, documentID string) {
	h.t.Helper()
	now := time.Unix(1_800_100_001, 0).UnixMilli()
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_semantic_document_bindings
		(document_id,owner_id,corpus_uid,content_generation,lifecycle_state,text_state,version,created_at,updated_at)
		VALUES(?,?,?,1,'active','ready',1,?,?)`, documentID, ownerID, corpusUID, now, now); err != nil {
		h.t.Fatalf("bind semantic document: %v", err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_semantic_document_generations
		(owner_id,corpus_uid,document_id,content_generation,created_at)
		VALUES(?,?,?,1,?)`, ownerID, corpusUID, documentID, now); err != nil {
		h.t.Fatalf("persist semantic document generation: %v", err)
	}
}

func (h *revisionSearchHarness) seedVisibleRevisionVector(
	revisionID, corpusUID, documentID string,
	vector []float32,
) {
	h.t.Helper()
	var snapshotID, profileHash, providerID, location, model string
	var dimension int
	if err := h.db.QueryRowContext(h.ctx, `SELECT s.profile_snapshot_id,s.profile_config_hash,
		s.provider_id,s.provider_location,s.model_name,s.dimension
		FROM kb_index_revisions r JOIN kb_embedding_profile_snapshots s
		ON s.profile_snapshot_id=r.profile_snapshot_id
		WHERE r.revision_id=? AND r.corpus_uid=?`, revisionID, corpusUID).Scan(
		&snapshotID, &profileHash, &providerID, &location, &model, &dimension,
	); err != nil {
		h.t.Fatal(err)
	}
	if len(vector) != dimension {
		h.t.Fatalf("fixture vector dimension=%d, want %d", len(vector), dimension)
	}
	chunkID := documentID + "-chunk-0"
	now := time.Unix(1_800_100_002, 0).UnixMilli()
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_revision_documents
		(revision_id,corpus_uid,document_id,content_generation,vector_state,
		expected_chunks,embedded_chunks,failed_chunks,visible_at,updated_at)
		VALUES(?,?,?,1,'ready',1,1,0,?,?)`, revisionID, corpusUID, documentID, now, now); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_revision_vectors
		(revision_id,corpus_uid,document_id,content_generation,chunk_id,chunk_index,
		chunk_content_hash,profile_snapshot_id,profile_config_hash,provider_id,
		provider_location,model_name,dimension,embedding,created_at)
		VALUES(?,?,?,1,?,0,?,?,?,?,?,?,?,?,?)`, revisionID, corpusUID, documentID,
		chunkID, "content-hash-"+documentID, snapshotID, profileHash, providerID,
		location, model, dimension, encodeFloat32Slice(vector), now); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `UPDATE kb_index_revisions
		SET expected_chunks=1,embedded_chunks=1,failed_chunks=0,
		chunk_set_digest=?,indexed_through_version=(SELECT content_version FROM kb_semantic_corpora WHERE corpus_uid=?)
		WHERE revision_id=?`, "digest-"+revisionID, corpusUID, revisionID); err != nil {
		h.t.Fatal(err)
	}
}

func TestRevisionSemanticSearchUsesOnlyActiveSnapshotAndNeverLegacyVectors(t *testing.T) {
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
	h.addLegacyDocument("active-doc", "active semantic evidence", []float32{-1, 0, 0})
	h.addLegacyDocument("legacy-only-doc", "unrelated legacy payload", []float32{1, 0, 0})
	h.bindDocument("owner-1", corpusUID, "active-doc")
	h.bindDocument("owner-1", corpusUID, "legacy-only-doc")
	h.seedVisibleRevisionVector(*boot.ActiveRevisionID, corpusUID, "active-doc", []float32{1, 0, 0})

	if _, err := h.db.ExecContext(h.ctx, `UPDATE kb_semantic_corpora SET content_version=1 WHERE corpus_uid=?`, corpusUID); err != nil {
		t.Fatal(err)
	}
	staged, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionProfile, ProfileID: "profile-b"})
	if err != nil {
		t.Fatal(err)
	}
	if staged.DesiredRevisionID == nil {
		t.Fatal("profile switch did not create staged revision")
	}

	a := &semanticExecutor{dimension: 3}
	b := &semanticExecutor{dimension: 4}
	registry := &semanticExecutorRegistry{executors: map[string]*semanticExecutor{
		"profile-a": a,
		"profile-b": b,
	}}
	searcher := NewSQLiteRevisionSemanticSearcher(h.db, "owner-1", "default", registry)
	results, routeRan, err := searcher.Search(h.ctx, "private query", 5, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if !routeRan {
		t.Fatal("active revision exists but semantic route did not run")
	}
	if len(results) != 1 || results[0].Chunk.ID != "active-doc-chunk-0" {
		t.Fatalf("revision search results = %+v, want only active revision vector", results)
	}
	if a.calls != 1 || b.calls != 0 {
		t.Fatalf("executor calls: active A=%d staged B=%d, want 1/0", a.calls, b.calls)
	}
	if got := a.inputs[0]; len(got) != 1 || got[0] != "private query" {
		t.Fatalf("query embedding input = %#v; must contain normalized query only", got)
	}
	if !reflect.DeepEqual(a.purposes, []EmbeddingPurpose{EmbeddingPurposeQuery}) {
		t.Fatalf("query embedding purposes = %v, want query", a.purposes)
	}
}

func TestRevisionSemanticQueryUsesInteractiveAcceleratorPermit(t *testing.T) {
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
	h.addLegacyDocument("active-doc", "interactive semantic evidence", nil)
	h.bindDocument("owner-1", corpusUID, "active-doc")
	h.seedVisibleRevisionVector(*boot.ActiveRevisionID, corpusUID, "active-doc", []float32{1, 0, 0})

	governor, err := resourcegov.New(resourcegov.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	hold, err := governor.Acquire(h.ctx, resourcegov.ResourceAccelerator, resourcegov.PriorityBackground)
	if err != nil {
		t.Fatal(err)
	}
	executor := &semanticExecutor{dimension: 3}
	searcher := NewSQLiteRevisionSemanticSearcher(
		h.db, "owner-1", "default",
		&semanticExecutorRegistry{executors: map[string]*semanticExecutor{"profile-a": executor}},
		WithRevisionSearchResourceGovernor(governor),
	)
	type searchResult struct {
		results []*SearchResult
		ran     bool
		err     error
	}
	done := make(chan searchResult, 1)
	go func() {
		results, ran, searchErr := searcher.Search(h.ctx, "interactive query", 3, Filter{})
		done <- searchResult{results: results, ran: ran, err: searchErr}
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if governor.Snapshot().Resources[resourcegov.ResourceAccelerator].QueuedInteractive == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	metric := governor.Snapshot().Resources[resourcegov.ResourceAccelerator]
	if metric.InUse != 1 || metric.QueuedInteractive != 1 || metric.QueuedBackground != 0 {
		t.Fatalf("query did not enter interactive accelerator queue: %+v", metric)
	}
	hold.Release()
	got := <-done
	if got.err != nil || !got.ran || len(got.results) != 1 {
		t.Fatalf("query result=%+v ran=%v err=%v", got.results, got.ran, got.err)
	}
	if !reflect.DeepEqual(executor.purposes, []EmbeddingPurpose{EmbeddingPurposeQuery}) {
		t.Fatalf("query purpose=%v", executor.purposes)
	}
}

func TestRevisionSemanticSearchDateFilterUsesDocumentCreationTime(t *testing.T) {
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
	h.addLegacyDocument("dated-doc", "dated semantic evidence", nil)
	h.bindDocument("owner-1", corpusUID, "dated-doc")
	h.seedVisibleRevisionVector(*boot.ActiveRevisionID, corpusUID, "dated-doc", []float32{1, 0, 0})
	documentCreatedAt := time.Unix(1_700_000_000, 0).UTC()
	chunkCreatedAt := documentCreatedAt.Add(48 * time.Hour)
	if _, err := h.db.ExecContext(h.ctx, `UPDATE kb_documents SET created_at=? WHERE id='dated-doc'`, documentCreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `UPDATE kb_chunks SET created_at=? WHERE doc_id='dated-doc'`, chunkCreatedAt); err != nil {
		t.Fatal(err)
	}

	executor := &semanticExecutor{dimension: 3}
	searcher := NewSQLiteRevisionSemanticSearcher(h.db, "owner-1", "default",
		&semanticExecutorRegistry{executors: map[string]*semanticExecutor{"profile-a": executor}})
	filter := Filter{
		CreatedAfter: documentCreatedAt.Add(24 * time.Hour),
	}
	results, routeRan, err := searcher.Search(h.ctx, "dated query", 5, filter)
	if err != nil || !routeRan || len(results) != 0 {
		t.Fatalf("document-date filter results=%+v route=%v err=%v, want empty successful vector route",
			results, routeRan, err)
	}
	textResults, err := searcher.TextSearch(h.ctx, "dated", 5, filter)
	if err != nil || len(textResults) != 0 {
		t.Fatalf("document-date text filter results=%+v err=%v, want empty", textResults, err)
	}
}

func TestRevisionSemanticSearchRejectsNonFinitePersistedVector(t *testing.T) {
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
	h.addLegacyDocument("corrupt-doc", "corrupt semantic evidence", nil)
	h.bindDocument("owner-1", corpusUID, "corrupt-doc")
	h.seedVisibleRevisionVector(*boot.ActiveRevisionID, corpusUID, "corrupt-doc",
		[]float32{float32(math.NaN()), 0, 1})

	searcher := NewSQLiteRevisionSemanticSearcher(h.db, "owner-1", "default",
		&semanticExecutorRegistry{executors: map[string]*semanticExecutor{
			"profile-a": {dimension: 3},
		}})
	results, routeRan, err := searcher.Search(h.ctx, "corrupt query", 5, Filter{})
	if !errors.Is(err, ErrInvalidEmbeddingResult) || routeRan || len(results) != 0 {
		t.Fatalf("non-finite persisted vector results=%+v route=%v err=%v",
			results, routeRan, err)
	}
}

func TestRevisionSemanticSearchDisabledMakesZeroEmbeddingCalls(t *testing.T) {
	h := newRevisionSearchHarness(t)
	boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", boot.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionDisabled}); err != nil {
		t.Fatal(err)
	}
	executor := &semanticExecutor{dimension: 3}
	registry := &semanticExecutorRegistry{executors: map[string]*semanticExecutor{"profile-a": executor}}
	searcher := NewSQLiteRevisionSemanticSearcher(h.db, "owner-1", "default", registry)
	results, routeRan, err := searcher.Search(h.ctx, "must stay local text-only", 5, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if routeRan || len(results) != 0 || executor.calls != 0 || len(registry.profiles) != 0 {
		t.Fatalf("disabled search leaked semantic work: route=%v results=%d calls=%d profiles=%v",
			routeRan, len(results), executor.calls, registry.profiles)
	}
}

func TestRevisionSemanticSearchRejectsNonFiniteQueryVector(t *testing.T) {
	h := newRevisionSearchHarness(t)
	if _, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default"); err != nil {
		t.Fatal(err)
	}
	executor := &semanticExecutor{dimension: 3, vector: []float32{float32(math.NaN()), 0, 1}}
	searcher := NewSQLiteRevisionSemanticSearcher(h.db, "owner-1", "default",
		&semanticExecutorRegistry{executors: map[string]*semanticExecutor{"profile-a": executor}})
	results, routeRan, err := searcher.Search(h.ctx, "non-finite must fail closed", 5, Filter{})
	if !errors.Is(err, ErrInvalidEmbeddingResult) || routeRan || len(results) != 0 {
		t.Fatalf("non-finite query result=%+v route=%v err=%v", results, routeRan, err)
	}
}

func TestRevisionSemanticSearchActiveOfflineExecutorIsTextOnlyStandby(t *testing.T) {
	h := newRevisionSearchHarness(t)
	if _, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default"); err != nil {
		t.Fatal(err)
	}
	offline := false
	executor := &semanticExecutor{dimension: 3, ready: &offline}
	searcher := NewSQLiteRevisionSemanticSearcher(h.db, "owner-1", "default",
		&semanticExecutorRegistry{executors: map[string]*semanticExecutor{"profile-a": executor}})
	ready, err := searcher.HasActiveRevision(h.ctx)
	if err != nil || ready {
		t.Fatalf("offline active readiness=%v err=%v, want false/nil", ready, err)
	}
	results, routeRan, err := searcher.Search(h.ctx, "must not hit offline embedder", 5, Filter{})
	if !errors.Is(err, ErrEmbeddingUnavailable) || routeRan || len(results) != 0 || executor.calls != 0 {
		t.Fatalf("offline search: results=%+v route=%v calls=%d err=%v",
			results, routeRan, executor.calls, err)
	}
}

func TestManagerRoutesSemanticQueriesThroughActiveRevisionInsteadOfLegacyEmbedder(t *testing.T) {
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
	h.addLegacyDocument("active-doc", "active semantic evidence", []float32{-1, 0, 0})
	h.addLegacyDocument("legacy-only-doc", "unrelated legacy payload", []float32{1, 0, 0})
	h.bindDocument("owner-1", corpusUID, "active-doc")
	h.bindDocument("owner-1", corpusUID, "legacy-only-doc")
	h.seedVisibleRevisionVector(*boot.ActiveRevisionID, corpusUID, "active-doc", []float32{1, 0, 0})

	executor := &semanticExecutor{dimension: 3}
	registry := &semanticExecutorRegistry{executors: map[string]*semanticExecutor{"profile-a": executor}}
	revisionSearcher := NewSQLiteRevisionSemanticSearcher(h.db, "owner-1", "default", registry)
	legacy := &countingLegacyEmbedder{}
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = false
	cfg.RerankEnabled = false
	cfg.UseRRF = false
	cfg.MinScore = 0
	manager := NewManager(h.store, h.store, legacy,
		WithHybridConfig(cfg),
		WithRevisionSemanticSearcher(revisionSearcher),
	)

	hits, err := manager.Search(h.ctx, "private query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ChunkID != "active-doc-chunk-0" {
		t.Fatalf("manager hits = %+v, want active revision result only", hits)
	}
	if legacy.calls != 0 || executor.calls != 1 {
		t.Fatalf("embedder calls: legacy=%d active-snapshot=%d, want 0/1", legacy.calls, executor.calls)
	}
}

func TestManagerCorpusScopedTextSearchPreventsOwnerLeakAndSurvivesDisabled(t *testing.T) {
	h := newRevisionSearchHarness(t)
	ownerOne, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-2", "default"); err != nil {
		t.Fatal(err)
	}
	var corpusOne, corpusTwo string
	if err := h.db.QueryRowContext(h.ctx, `SELECT corpus_uid FROM kb_semantic_corpora
		WHERE owner_id='owner-1' AND corpus_alias='default'`).Scan(&corpusOne); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRowContext(h.ctx, `SELECT corpus_uid FROM kb_semantic_corpora
		WHERE owner_id='owner-2' AND corpus_alias='default'`).Scan(&corpusTwo); err != nil {
		t.Fatal(err)
	}
	h.addLegacyDocument("owner-one-doc", "sharedtoken owner one", nil)
	h.addLegacyDocument("owner-two-doc", "sharedtoken owner two", nil)
	h.bindDocument("owner-1", corpusOne, "owner-one-doc")
	h.bindDocument("owner-2", corpusTwo, "owner-two-doc")

	executor := &semanticExecutor{dimension: 3}
	registry := &semanticExecutorRegistry{executors: map[string]*semanticExecutor{"profile-a": executor}}
	searcher := NewSQLiteRevisionSemanticSearcher(h.db, "owner-1", "default", registry)
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = false
	cfg.RerankEnabled = false
	cfg.MinScore = 0
	manager := NewManager(h.store, h.store, nil,
		WithHybridConfig(cfg),
		WithRevisionSemanticSearcher(searcher),
	)

	hits, err := manager.Search(h.ctx, "sharedtoken", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].DocID != "owner-one-doc" {
		t.Fatalf("owner-1 text hits = %+v, want only owner-one-doc", hits)
	}

	if _, err := h.service.ApplyPolicy(h.ctx, "owner-1", "default", ownerOne.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionDisabled}); err != nil {
		t.Fatal(err)
	}
	callsBefore := executor.calls
	hits, err = manager.Search(h.ctx, "sharedtoken", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].DocID != "owner-one-doc" {
		t.Fatalf("disabled owner-1 text hits = %+v, want scoped FTS result", hits)
	}
	if executor.calls != callsBefore {
		t.Fatalf("disabled corpus made %d embedding calls, want zero", executor.calls-callsBefore)
	}
}

func TestCorpusScopedTextSearchDefensivelyHidesDeletedDocument(t *testing.T) {
	h := newRevisionSearchHarness(t)
	if _, err := h.db.ExecContext(h.ctx, `ALTER TABLE kb_documents
		ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		t.Fatal(err)
	}
	if _, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default"); err != nil {
		t.Fatal(err)
	}
	var corpusUID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT corpus_uid FROM kb_semantic_corpora
		WHERE owner_id='owner-1' AND corpus_alias='default'`).Scan(&corpusUID); err != nil {
		t.Fatal(err)
	}
	h.addLegacyDocument("deleted-doc", "deletedtoken private", nil)
	h.bindDocument("owner-1", corpusUID, "deleted-doc")
	if _, err := h.db.ExecContext(h.ctx, `UPDATE kb_documents SET deleted=1 WHERE id='deleted-doc'`); err != nil {
		t.Fatal(err)
	}
	searcher := NewSQLiteRevisionSemanticSearcher(h.db, "owner-1", "default", &semanticExecutorRegistry{
		executors: map[string]*semanticExecutor{"profile-a": {dimension: 3}},
	})
	results, err := searcher.TextSearch(h.ctx, "deletedtoken", 5, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("deleted document remained text-visible: %+v", results)
	}
}
