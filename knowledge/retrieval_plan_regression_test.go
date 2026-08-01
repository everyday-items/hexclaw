package knowledge

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// REG-KNOW-RETRIEVAL-PLAN-20260801-001 and
// REG-KNOW-MULTIQUERY-CITATION-20260801-002 exercise the public Manager
// boundary. The controlled query expansion produces exactly q1 then q2, so a
// revision switch or lexical-first merge is deterministic and does not depend
// on timing.

type retrievalRegressionRepo struct{}

func (retrievalRegressionRepo) Init(context.Context) error { return nil }
func (retrievalRegressionRepo) Add(context.Context, *Document, []*Chunk) error {
	return nil
}
func (retrievalRegressionRepo) Get(context.Context, string) (*Document, error) {
	return nil, nil
}
func (retrievalRegressionRepo) List(context.Context) ([]*Document, error) {
	return nil, nil
}
func (retrievalRegressionRepo) GetBySourceTitle(context.Context, string, string) (*Document, error) {
	return nil, nil
}
func (retrievalRegressionRepo) Replace(context.Context, *Document, []*Chunk) error {
	return nil
}
func (retrievalRegressionRepo) Delete(context.Context, string) error { return nil }
func (retrievalRegressionRepo) HasSearchableDocuments(context.Context) (bool, error) {
	return true, nil
}

type retrievalRegressionLegacySearcher struct{}

func (retrievalRegressionLegacySearcher) VectorSearch(context.Context, []float32, int, Filter) ([]*SearchResult, error) {
	return nil, nil
}
func (retrievalRegressionLegacySearcher) TextSearch(context.Context, string, int, Filter) ([]*SearchResult, error) {
	return nil, nil
}

type retrievalRegressionLLM struct{}

func (retrievalRegressionLLM) Complete(_ context.Context, prompt string) (string, error) {
	if strings.Contains(prompt, "Alternative queries:") {
		return "q2", nil
	}
	// HyDE returning blank deterministically degrades to q1, which Manager
	// de-duplicates against the original query.
	return "", nil
}

type retrievalMutationLLM struct {
	once   sync.Once
	mutate func()
}

func (l *retrievalMutationLLM) Complete(_ context.Context, prompt string) (string, error) {
	l.once.Do(l.mutate)
	if strings.Contains(prompt, "Alternative queries:") {
		return "q2", nil
	}
	return "", nil
}

type retrievalRegressionRoute struct {
	vectorByQuery   map[string][]*SearchResult
	textByQuery     map[string][]*SearchResult
	revisionByQuery map[string]string
	searchCalls     int
}

type retrievalRegressionPlannerRoute struct {
	activeRevision string
	freezeCalls    int
	legacyCalls    int
	vectorPlans    []string
	textPlans      []string
	vectorFilters  []Filter
	textFilters    []Filter
}

func (r *retrievalRegressionPlannerRoute) FreezeRetrievalPlan(
	_ context.Context,
	expectedRevisionID string,
) (activeRevisionSearchPlan, bool, error) {
	r.freezeCalls++
	revisionID := r.activeRevision
	if expectedRevisionID != "" {
		revisionID = expectedRevisionID
	}
	return activeRevisionSearchPlan{
		corpusUID: "corpus-a",
		revision:  revisionID,
	}, true, nil
}

func (*retrievalRegressionPlannerRoute) RetrievalPlanReady(
	context.Context,
	activeRevisionSearchPlan,
) (bool, error) {
	return true, nil
}

func (*retrievalRegressionPlannerRoute) ValidateRetrievalPlan(
	context.Context,
	activeRevisionSearchPlan,
) error {
	return nil
}

func (r *retrievalRegressionPlannerRoute) SearchWithPlanReceipt(
	_ context.Context,
	plan activeRevisionSearchPlan,
	query string,
	_ int,
	filter Filter,
) ([]*SearchResult, bool, *QueryEmbeddingReceipt, error) {
	r.vectorPlans = append(r.vectorPlans, plan.revision)
	r.vectorFilters = append(r.vectorFilters, filter)
	if query == "q1" {
		// Simulate PublishRevisionCAS moving the mutable active pointer after
		// the first expanded query has begun.
		r.activeRevision = "revision-b"
	}
	return []*SearchResult{{
			Chunk:       &Chunk{ID: "chunk-" + query, DocID: "doc-" + query, Content: query},
			VectorScore: 0.99,
		}}, true, &QueryEmbeddingReceipt{
			Operation:  "query_embedding",
			Status:     "succeeded",
			RevisionID: plan.revision,
		}, nil
}

func (r *retrievalRegressionPlannerRoute) TextSearchWithPlan(
	_ context.Context,
	plan activeRevisionSearchPlan,
	_ string,
	_ int,
	filter Filter,
) ([]*SearchResult, error) {
	r.textPlans = append(r.textPlans, plan.revision)
	r.textFilters = append(r.textFilters, filter)
	return nil, nil
}

func (r *retrievalRegressionPlannerRoute) Search(
	context.Context,
	string,
	int,
	Filter,
) ([]*SearchResult, bool, error) {
	r.legacyCalls++
	return nil, false, errors.New("mutable search path must not run")
}

func (r *retrievalRegressionPlannerRoute) TextSearch(
	context.Context,
	string,
	int,
	Filter,
) ([]*SearchResult, error) {
	r.legacyCalls++
	return nil, errors.New("mutable text path must not run")
}

func (r *retrievalRegressionRoute) HasActiveRevision(context.Context) (bool, error) {
	return true, nil
}

func (r *retrievalRegressionRoute) Search(
	ctx context.Context,
	query string,
	topK int,
	filter Filter,
) ([]*SearchResult, bool, error) {
	results, ran, _, err := r.SearchWithReceipt(ctx, query, topK, filter)
	return results, ran, err
}

func (r *retrievalRegressionRoute) SearchWithReceipt(
	_ context.Context,
	query string,
	topK int,
	_ Filter,
) ([]*SearchResult, bool, *QueryEmbeddingReceipt, error) {
	r.searchCalls++
	results := cloneRetrievalRegressionResults(r.vectorByQuery[query], topK)
	if len(results) == 0 {
		return nil, false, nil, nil
	}
	revisionID := r.revisionByQuery[query]
	return results, true, &QueryEmbeddingReceipt{
		Operation:   "query_embedding",
		Status:      "succeeded",
		RevisionID:  revisionID,
		QueryDigest: "sha256:" + query,
	}, nil
}

func (r *retrievalRegressionRoute) TextSearch(
	_ context.Context,
	query string,
	topK int,
	_ Filter,
) ([]*SearchResult, error) {
	return cloneRetrievalRegressionResults(r.textByQuery[query], topK), nil
}

func cloneRetrievalRegressionResults(in []*SearchResult, topK int) []*SearchResult {
	out := make([]*SearchResult, 0, len(in))
	for _, result := range in {
		if topK > 0 && len(out) >= topK {
			break
		}
		chunk := *result.Chunk
		out = append(out, &SearchResult{
			Chunk:       &chunk,
			VectorScore: result.VectorScore,
			TextScore:   result.TextScore,
		})
	}
	return out
}

func newRetrievalRegressionManager(route RevisionSemanticSearcher) *Manager {
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = true
	cfg.RerankEnabled = false
	cfg.UseRRF = false
	cfg.MinScore = 0
	cfg.TimeDecayDays = 0
	return NewManager(
		retrievalRegressionRepo{},
		retrievalRegressionLegacySearcher{},
		nil,
		WithHybridConfig(cfg),
		WithLLM(retrievalRegressionLLM{}),
		WithRevisionSemanticSearcher(route),
	)
}

func TestREGKNOWRETRIEVALPLAN20260801001_MixedRevisionReceiptsFailClosed(t *testing.T) {
	route := &retrievalRegressionRoute{
		vectorByQuery: map[string][]*SearchResult{
			"q1": {{
				Chunk:       &Chunk{ID: "chunk-a", DocID: "doc-a", Content: "revision a"},
				VectorScore: 0.99,
			}},
			"q2": {{
				Chunk:       &Chunk{ID: "chunk-b", DocID: "doc-b", Content: "revision b"},
				VectorScore: 0.99,
			}},
		},
		revisionByQuery: map[string]string{"q1": "revision-a", "q2": "revision-b"},
	}

	_, _, err := newRetrievalRegressionManager(route).SearchWithFilterReceipt(
		context.Background(), "q1", 5, Filter{},
	)
	if err == nil || !strings.Contains(err.Error(), "retrieval evidence conflict") {
		t.Fatalf("mixed q1/q2 revisions must fail closed, got err=%v", err)
	}
}

func TestREGKNOWRETRIEVALPLAN20260801001_ActiveSwitchKeepsOneFrozenPlan(t *testing.T) {
	route := &retrievalRegressionPlannerRoute{activeRevision: "revision-a"}

	_, receipts, err := newRetrievalRegressionManager(route).SearchWithFilterReceipt(
		context.Background(), "q1", 5, Filter{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if route.freezeCalls != 1 {
		t.Fatalf("retrieval plan freezes=%d, want exactly 1", route.freezeCalls)
	}
	if route.legacyCalls != 0 {
		t.Fatalf("mutable search path ran %d times", route.legacyCalls)
	}
	if got := strings.Join(route.vectorPlans, ","); got != "revision-a,revision-a" {
		t.Fatalf("vector plans=%q, want revision-a for q1 and q2", got)
	}
	if got := strings.Join(route.textPlans, ","); got != "revision-a,revision-a" {
		t.Fatalf("text plans=%q, want revision-a for q1 and q2", got)
	}
	if len(receipts) != 2 ||
		receipts[0].RevisionID != "revision-a" ||
		receipts[1].RevisionID != "revision-a" {
		t.Fatalf("receipts crossed frozen revision: %+v", receipts)
	}
}

type expectedRevisionQueryManager interface {
	QueryHitsWithFilterAtRevision(
		context.Context,
		string,
		string,
		int,
		Filter,
	) (string, []SearchHit, []QueryEmbeddingReceipt, error)
}

func TestREGKNOWRETRIEVALPLAN20260801001_ExpectedRevisionWithoutPlannerFailsClosed(t *testing.T) {
	route := &retrievalRegressionRoute{}
	manager := newRetrievalRegressionManager(route)
	pinned, ok := any(manager).(expectedRevisionQueryManager)
	if !ok {
		t.Fatal("Manager does not expose the pinned revision query boundary")
	}

	_, _, _, err := pinned.QueryHitsWithFilterAtRevision(
		context.Background(), "revision-a", "q1", 5, Filter{},
	)
	if err == nil || !strings.Contains(err.Error(), "pinned retrieval plan unavailable") {
		t.Fatalf("pinned query without a planner must fail closed, got err=%v", err)
	}
	if route.searchCalls != 0 {
		t.Fatalf("pinned query fell back to mutable search %d times", route.searchCalls)
	}
}

func TestREGKNOWRETRIEVALPLAN20260801001_EmptyExpectedRevisionFailsClosed(t *testing.T) {
	route := &retrievalRegressionRoute{}
	manager := newRetrievalRegressionManager(route)

	_, _, _, err := manager.QueryHitsWithFilterAtRevision(
		context.Background(), "  ", "q1", 5, Filter{},
	)
	if !errors.Is(err, ErrRetrievalPlanUnavailable) {
		t.Fatalf("empty expected revision must fail closed, got err=%v", err)
	}
	if route.searchCalls != 0 {
		t.Fatalf("empty pinned revision fell back to mutable search %d times", route.searchCalls)
	}
}

func TestREGKNOWRETRIEVALPLAN20260801001_PinnedSupersededRevisionIsReplayable(t *testing.T) {
	h := newRevisionSearchHarness(t)
	boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if boot.ActiveRevisionID == nil {
		t.Fatal("default semantic policy has no active revision")
	}
	revisionID := *boot.ActiveRevisionID
	var corpusUID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT corpus_uid FROM kb_semantic_corpora
		WHERE owner_id='owner-1' AND corpus_alias='default'`).Scan(&corpusUID); err != nil {
		t.Fatal(err)
	}
	h.addLegacyDocument("doc-a", "pinned revision evidence", nil)
	h.bindDocument("owner-1", corpusUID, "doc-a")
	h.seedVisibleRevisionVector(revisionID, corpusUID, "doc-a", []float32{1, 0, 0})

	if _, err := h.service.ApplyPolicy(
		h.ctx,
		"owner-1",
		"default",
		boot.PolicyVersion,
		EmbeddingSelection{Kind: EmbeddingSelectionDisabled},
	); err != nil {
		t.Fatal(err)
	}

	executor := &semanticExecutor{dimension: 3}
	searcher := NewSQLiteRevisionSemanticSearcher(
		h.db,
		"owner-1",
		"default",
		&semanticExecutorRegistry{executors: map[string]*semanticExecutor{
			"profile-a": executor,
		}},
	)
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = false
	cfg.RerankEnabled = false
	cfg.UseRRF = false
	cfg.MinScore = 0
	cfg.TimeDecayDays = 0
	manager := NewManager(
		h.store,
		h.store,
		nil,
		WithHybridConfig(cfg),
		WithRevisionSemanticSearcher(searcher),
	)

	_, hits, receipts, err := manager.QueryHitsWithFilterAtRevision(
		h.ctx, revisionID, "pinned revision evidence", 5, Filter{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ChunkID != "doc-a-chunk-0" {
		t.Fatalf("pinned superseded results=%+v", hits)
	}
	if hits[0].DocumentGeneration != 1 {
		t.Fatalf("pinned hit generation=%d, want persisted generation 1",
			hits[0].DocumentGeneration)
	}
	if hits[0].SemanticRevisionID != revisionID {
		t.Fatalf("pinned hit revision=%q, want %q",
			hits[0].SemanticRevisionID, revisionID)
	}
	if got := hits[0].Metadata["document_generation"]; got != int64(1) {
		t.Fatalf("pinned hit metadata generation=%v, want int64(1)", got)
	}
	if got := hits[0].Metadata["revision_id"]; got != revisionID {
		t.Fatalf("pinned hit metadata revision=%v, want %q", got, revisionID)
	}
	if len(receipts) != 1 || receipts[0].RevisionID != revisionID {
		t.Fatalf("pinned superseded receipts=%+v, want revision %q", receipts, revisionID)
	}
}

func TestREGKNOWPINNEDPROVENANCE20260801003_SQLiteLanesCarryExactGenerationAndOnlyVectorCarriesRevision(t *testing.T) {
	h := newRevisionSearchHarness(t)
	boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if boot.ActiveRevisionID == nil {
		t.Fatal("default semantic policy has no active revision")
	}
	var corpusUID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT corpus_uid FROM kb_semantic_corpora
		WHERE owner_id='owner-1' AND corpus_alias='default'`).Scan(&corpusUID); err != nil {
		t.Fatal(err)
	}
	h.addLegacyDocument("doc-provenance", "lane provenance evidence", nil)
	h.bindDocument("owner-1", corpusUID, "doc-provenance")
	h.seedVisibleRevisionVector(*boot.ActiveRevisionID, corpusUID, "doc-provenance", []float32{1, 0, 0})

	searcher := NewSQLiteRevisionSemanticSearcher(
		h.db,
		"owner-1",
		"default",
		&semanticExecutorRegistry{executors: map[string]*semanticExecutor{
			"profile-a": {dimension: 3},
		}},
	)
	plan, active, err := searcher.FreezeRetrievalPlan(h.ctx, *boot.ActiveRevisionID)
	if err != nil || !active {
		t.Fatalf("freeze pinned plan active=%v err=%v", active, err)
	}
	vectorResults, ran, _, err := searcher.SearchWithPlanReceipt(
		h.ctx, plan, "lane provenance evidence", 5, Filter{},
	)
	if err != nil || !ran || len(vectorResults) != 1 {
		t.Fatalf("vector lane ran=%v results=%+v err=%v", ran, vectorResults, err)
	}
	vectorChunk := vectorResults[0].Chunk
	if vectorChunk.DocumentGeneration != 1 ||
		vectorChunk.SemanticRevisionID != *boot.ActiveRevisionID {
		t.Fatalf("vector provenance generation=%d revision=%q, want 1/%q",
			vectorChunk.DocumentGeneration,
			vectorChunk.SemanticRevisionID,
			*boot.ActiveRevisionID,
		)
	}

	textResults, err := searcher.TextSearchWithPlan(
		h.ctx, plan, "lane provenance evidence", 5, Filter{},
	)
	if err != nil || len(textResults) != 1 {
		t.Fatalf("text lane results=%+v err=%v", textResults, err)
	}
	textChunk := textResults[0].Chunk
	if textChunk.DocumentGeneration != 1 {
		t.Fatalf("text provenance generation=%d, want 1", textChunk.DocumentGeneration)
	}
	if textChunk.SemanticRevisionID != "" {
		t.Fatalf("BM25-only hit invented semantic revision %q", textChunk.SemanticRevisionID)
	}
}

func TestREGKNOWRETRIEVALPLAN20260801001_PinnedTextLaneRejectsCurrentGenerationDrift(t *testing.T) {
	h := newRevisionSearchHarness(t)
	boot, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if boot.ActiveRevisionID == nil {
		t.Fatal("default semantic policy has no active revision")
	}
	var corpusUID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT corpus_uid FROM kb_semantic_corpora
		WHERE owner_id='owner-1' AND corpus_alias='default'`).Scan(&corpusUID); err != nil {
		t.Fatal(err)
	}
	h.addLegacyDocument("doc-generation-drift", "revision a evidence", nil)
	h.bindDocument("owner-1", corpusUID, "doc-generation-drift")
	h.seedVisibleRevisionVector(
		*boot.ActiveRevisionID,
		corpusUID,
		"doc-generation-drift",
		[]float32{1, 0, 0},
	)

	searcher := NewSQLiteRevisionSemanticSearcher(
		h.db,
		"owner-1",
		"default",
		&semanticExecutorRegistry{executors: map[string]*semanticExecutor{
			"profile-a": {dimension: 3},
		}},
	)
	plan, active, err := searcher.FreezeRetrievalPlan(h.ctx, *boot.ActiveRevisionID)
	if err != nil || !active {
		t.Fatalf("freeze pinned plan active=%v err=%v", active, err)
	}

	// Replace the mutable document/binding with generation 2 after the request
	// plan has frozen revision A. The old implementation searched the current
	// binding and therefore returned this B-only text through the A plan.
	now := int64(1_800_100_003_000)
	if _, err := h.db.ExecContext(h.ctx, `INSERT INTO kb_semantic_document_generations
		(owner_id,corpus_uid,document_id,content_generation,created_at)
		VALUES('owner-1',?,'doc-generation-drift',2,?)`, corpusUID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `UPDATE kb_semantic_document_bindings
		SET content_generation=2,version=version+1,updated_at=?
		WHERE owner_id='owner-1' AND corpus_uid=? AND document_id='doc-generation-drift'`,
		now, corpusUID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.ExecContext(h.ctx, `UPDATE kb_chunks
		SET content='revision b evidence'
		WHERE doc_id='doc-generation-drift'`); err != nil {
		t.Fatal(err)
	}

	results, err := searcher.TextSearchWithPlan(
		h.ctx,
		plan,
		"revision b evidence",
		5,
		Filter{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("pinned revision A leaked mutable generation B text: %+v", results)
	}
}

func TestREGKNOWRETRIEVALPLAN20260801001_ContentVersionDriftFailsWholeRequest(t *testing.T) {
	h := newRevisionSearchHarness(t)
	if _, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default"); err != nil {
		t.Fatal(err)
	}
	var corpusUID string
	if err := h.db.QueryRowContext(h.ctx, `SELECT corpus_uid FROM kb_semantic_corpora
		WHERE owner_id='owner-1' AND corpus_alias='default'`).Scan(&corpusUID); err != nil {
		t.Fatal(err)
	}
	searcher := NewSQLiteRevisionSemanticSearcher(
		h.db,
		"owner-1",
		"default",
		&semanticExecutorRegistry{executors: map[string]*semanticExecutor{
			"profile-a": {dimension: 3},
		}},
	)
	llm := &retrievalMutationLLM{mutate: func() {
		if _, err := h.db.ExecContext(h.ctx, `UPDATE kb_semantic_corpora
			SET content_version=content_version+1 WHERE corpus_uid=?`, corpusUID); err != nil {
			t.Fatalf("mutate corpus content version: %v", err)
		}
	}}
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = true
	cfg.RerankEnabled = false
	cfg.UseRRF = false
	cfg.MinScore = 0
	cfg.TimeDecayDays = 0
	manager := NewManager(
		retrievalRegressionRepo{},
		retrievalRegressionLegacySearcher{},
		nil,
		WithHybridConfig(cfg),
		WithLLM(llm),
		WithRevisionSemanticSearcher(searcher),
	)

	_, _, err := manager.SearchWithFilterReceipt(h.ctx, "q1", 5, Filter{})
	if !errors.Is(err, ErrRetrievalEvidenceConflict) {
		t.Fatalf("content-version drift must fail the whole request, got err=%v", err)
	}
}

func TestREGKNOWRETRIEVALPLAN20260801001_FilterWhitelistIsFrozenBeforeQueryExpansion(t *testing.T) {
	route := &retrievalRegressionPlannerRoute{activeRevision: "revision-a"}
	filter := Filter{
		DocumentGenerations: []DocumentGenerationRef{{
			DocumentID: "doc-a", DocumentGeneration: 1,
		}},
		ChunkIDs: []string{"chunk-a"},
	}
	llm := &retrievalMutationLLM{mutate: func() {
		filter.DocumentGenerations[0] = DocumentGenerationRef{
			DocumentID: "doc-b", DocumentGeneration: 2,
		}
		filter.ChunkIDs[0] = "chunk-b"
	}}
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = true
	cfg.RerankEnabled = false
	cfg.UseRRF = false
	cfg.MinScore = 0
	cfg.TimeDecayDays = 0
	manager := NewManager(
		retrievalRegressionRepo{}, retrievalRegressionLegacySearcher{}, nil,
		WithHybridConfig(cfg), WithLLM(llm), WithRevisionSemanticSearcher(route),
	)

	if _, _, err := manager.SearchWithFilterReceipt(
		context.Background(), "q1", 5, filter,
	); err != nil {
		t.Fatal(err)
	}
	for lane, filters := range map[string][]Filter{
		"vector": route.vectorFilters,
		"text":   route.textFilters,
	} {
		if len(filters) != 2 {
			t.Fatalf("%s lane filter count=%d, want 2", lane, len(filters))
		}
		for index, got := range filters {
			if len(got.DocumentGenerations) != 1 ||
				got.DocumentGenerations[0].DocumentID != "doc-a" ||
				got.DocumentGenerations[0].DocumentGeneration != 1 ||
				len(got.ChunkIDs) != 1 || got.ChunkIDs[0] != "chunk-a" {
				t.Fatalf("%s lane %d consumed mutable filter: %+v", lane, index, got)
			}
		}
	}
}

func TestREGKNOWMULTIQUERYCITATION20260801002_LexicalFirstVectorLaterPreservesCitation(t *testing.T) {
	content := "same immutable chunk"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	route := &retrievalRegressionRoute{
		vectorByQuery: map[string][]*SearchResult{
			"q2": {{
				Chunk: &Chunk{
					ID: "chunk-1", DocID: "doc-1", Content: content,
					DocumentGeneration: 7,
					SemanticRevisionID: "revision-a",
					CitationDigest:     digest,
				},
				VectorScore: 0.99,
			}},
		},
		textByQuery: map[string][]*SearchResult{
			"q1": {{
				Chunk: &Chunk{
					ID: "chunk-1", DocID: "doc-1", Content: content,
					DocumentGeneration: 7,
				},
				TextScore: 1,
			}},
		},
		revisionByQuery: map[string]string{"q2": "revision-a"},
	}

	hits, _, err := newRetrievalRegressionManager(route).SearchWithFilterReceipt(
		context.Background(), "q1", 5, Filter{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].CitationDigest != digest {
		t.Fatalf("lexical-first merge lost vector citation: hits=%+v want digest=%q", hits, digest)
	}
	if hits[0].DocumentGeneration != 7 || hits[0].SemanticRevisionID != "revision-a" {
		t.Fatalf("lexical-first merge lost pinned provenance: %+v", hits[0])
	}
}

func TestREGKNOWPINNEDPROVENANCE20260801003_ConflictingGenerationOrRevisionFailsClosed(t *testing.T) {
	tests := []struct {
		name        string
		textChunk   *Chunk
		vectorChunk *Chunk
	}{
		{
			name: "document generation",
			textChunk: &Chunk{
				ID: "chunk-1", DocID: "doc-1", Content: "immutable content",
				DocumentGeneration: 1,
			},
			vectorChunk: &Chunk{
				ID: "chunk-1", DocID: "doc-1", Content: "immutable content",
				DocumentGeneration: 2, SemanticRevisionID: "revision-a",
			},
		},
		{
			name: "semantic revision",
			textChunk: &Chunk{
				ID: "chunk-1", DocID: "doc-1", Content: "immutable content",
				DocumentGeneration: 1, SemanticRevisionID: "revision-b",
			},
			vectorChunk: &Chunk{
				ID: "chunk-1", DocID: "doc-1", Content: "immutable content",
				DocumentGeneration: 1, SemanticRevisionID: "revision-a",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route := &retrievalRegressionRoute{
				vectorByQuery: map[string][]*SearchResult{
					"q2": {{Chunk: tt.vectorChunk, VectorScore: 0.99}},
				},
				textByQuery: map[string][]*SearchResult{
					"q1": {{Chunk: tt.textChunk, TextScore: 1}},
				},
				revisionByQuery: map[string]string{"q2": "revision-a"},
			}

			_, _, err := newRetrievalRegressionManager(route).SearchWithFilterReceipt(
				context.Background(), "q1", 5, Filter{},
			)
			if !errors.Is(err, ErrRetrievalEvidenceConflict) {
				t.Fatalf("conflicting %s must fail closed, got err=%v", tt.name, err)
			}
		})
	}
}

func TestREGKNOWMULTIQUERYCITATION20260801002_ConflictingProvenanceFailsClosed(t *testing.T) {
	route := &retrievalRegressionRoute{
		vectorByQuery: map[string][]*SearchResult{
			"q2": {{
				Chunk: &Chunk{
					ID: "chunk-1", DocID: "doc-1", Content: "vector content",
					CitationDigest: "vector-digest",
				},
				VectorScore: 0.99,
			}},
		},
		textByQuery: map[string][]*SearchResult{
			"q1": {{
				Chunk:     &Chunk{ID: "chunk-1", DocID: "doc-1", Content: "lexical content"},
				TextScore: 1,
			}},
		},
		revisionByQuery: map[string]string{"q2": "revision-a"},
	}

	_, _, err := newRetrievalRegressionManager(route).SearchWithFilterReceipt(
		context.Background(), "q1", 5, Filter{},
	)
	if err == nil || !strings.Contains(err.Error(), "retrieval evidence conflict") {
		t.Fatalf("same chunk with conflicting content must fail closed, got err=%v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("provenance conflict was misclassified as cancellation: %v", err)
	}
}
