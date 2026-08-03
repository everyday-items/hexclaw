package knowledge

import (
	"context"
	"errors"
	"testing"
	"time"

	hrag "github.com/hexagon-codes/hexagon/rag"
)

type bug20260728QwenPolicySearcher struct {
	results           []*SearchResult
	searchErr         error
	textResults       []*SearchResult
	deadlineRemaining time.Duration
	searchCalls       int
	textCalls         int
}

func (s *bug20260728QwenPolicySearcher) QueryEmbeddingTimeout() time.Duration {
	return 60 * time.Second
}

func (s *bug20260728QwenPolicySearcher) AutoInjectionMinScore() float64 {
	return 0.65
}

func (s *bug20260728QwenPolicySearcher) AutoInjectionMaxResults() int {
	return 1
}

func (s *bug20260728QwenPolicySearcher) Search(
	ctx context.Context,
	_ string,
	_ int,
	_ Filter,
) ([]*SearchResult, bool, error) {
	s.searchCalls++
	if deadline, ok := ctx.Deadline(); ok {
		s.deadlineRemaining = time.Until(deadline)
	}
	if s.searchErr != nil {
		return nil, false, s.searchErr
	}
	return s.results, true, nil
}

func (s *bug20260728QwenPolicySearcher) TextSearch(
	context.Context,
	string,
	int,
	Filter,
) ([]*SearchResult, error) {
	s.textCalls++
	return s.textResults, nil
}

type bug20260728LegacyPolicySearcher struct {
	results []*SearchResult
}

func (s bug20260728LegacyPolicySearcher) Search(
	context.Context,
	string,
	int,
	Filter,
) ([]*SearchResult, bool, error) {
	return s.results, true, nil
}

func (bug20260728LegacyPolicySearcher) TextSearch(
	context.Context,
	string,
	int,
	Filter,
) ([]*SearchResult, error) {
	return nil, nil
}

func bug20260728Result(id string, score float64) *SearchResult {
	return &SearchResult{
		Chunk:       &Chunk{ID: id + "-chunk", DocID: id, Content: id},
		VectorScore: score,
	}
}

func bug20260728Manager(searcher RevisionSemanticSearcher) *Manager {
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = false
	cfg.RerankEnabled = false
	cfg.UseRRF = false
	return NewManager(
		semanticManagerRepo{},
		leakingLegacySearcher{},
		nil,
		WithHybridConfig(cfg),
		WithRevisionSemanticSearcher(searcher),
	)
}

type bug20260803QwenTop1Reranker struct {
	calls    int
	inputIDs []string
}

func (r *bug20260803QwenTop1Reranker) Name() string { return "qwen-top1-stage-order" }

func (r *bug20260803QwenTop1Reranker) Rerank(
	_ context.Context,
	_ string,
	docs []hrag.Document,
) ([]hrag.Document, error) {
	r.calls++
	r.inputIDs = r.inputIDs[:0]
	for _, doc := range docs {
		r.inputIDs = append(r.inputIDs, doc.ID)
	}
	if len(docs) < 2 {
		return docs, nil
	}
	return []hrag.Document{docs[1], docs[0]}, nil
}

func TestBug20260728QwenQueryBudgetAndTop1FloorAreProfileScoped(t *testing.T) {
	globalBefore := DefaultHybridConfig().MinScore
	if globalBefore != 0.85 {
		t.Fatalf("test precondition global MinScore=%v, want existing Nomic-calibrated 0.85", globalBefore)
	}

	qwen := &bug20260728QwenPolicySearcher{results: []*SearchResult{
		bug20260728Result("relevant", 0.70),
		bug20260728Result("near-tie", 0.699),
	}}
	_, hits, err := bug20260728Manager(qwen).QueryHits(context.Background(), "held-out query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if qwen.deadlineRemaining < 59*time.Second || qwen.deadlineRemaining > 60*time.Second {
		t.Errorf("Qwen query deadline=%v, want profile-scoped 60s", qwen.deadlineRemaining)
	}
	if len(hits) != 1 || hits[0].DocID != "relevant" {
		t.Errorf("Qwen auto-injection hits=%v, want only Top-1 score 0.70 above scoped 0.65 floor", hits)
	}

	legacy := bug20260728LegacyPolicySearcher{results: []*SearchResult{
		bug20260728Result("legacy-below-floor", 0.70),
	}}
	_, legacyHits, err := bug20260728Manager(legacy).QueryHits(context.Background(), "legacy query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacyHits) != 0 {
		t.Errorf("Qwen scoped floor leaked into legacy/Nomic route: %+v", legacyHits)
	}
	if got := DefaultHybridConfig().MinScore; got != globalBefore {
		t.Fatalf("Qwen query mutated global MinScore: before=%v after=%v", globalBefore, got)
	}
}

func TestBug20260803QwenTop1CapAppliesAfterRerank(t *testing.T) {
	qwen := &bug20260728QwenPolicySearcher{results: []*SearchResult{
		bug20260728Result("vector-first", 0.70),
		bug20260728Result("rerank-winner", 0.69),
	}}
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = false
	cfg.RerankEnabled = true
	cfg.UseRRF = false
	rerank := &bug20260803QwenTop1Reranker{}
	manager := NewManager(
		semanticManagerRepo{},
		leakingLegacySearcher{},
		nil,
		WithHybridConfig(cfg),
		WithRevisionSemanticSearcher(qwen),
		WithDocReranker(rerank),
	)

	_, hits, err := manager.QueryHits(context.Background(), "held-out query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if rerank.calls != 1 {
		t.Fatalf("rerank calls=%d, want 1 before the Qwen Top-1 injection cap", rerank.calls)
	}
	if len(rerank.inputIDs) != 2 {
		t.Fatalf("rerank candidates=%v, want both above-floor candidates", rerank.inputIDs)
	}
	if len(hits) != 1 || hits[0].DocID != "rerank-winner" {
		t.Fatalf("Qwen auto-injection hits=%v, want reranked winner then Top-1 cap", hits)
	}
}

func TestBug20260803QwenFloorAppliesBeforeRerank(t *testing.T) {
	qwen := &bug20260728QwenPolicySearcher{results: []*SearchResult{
		bug20260728Result("above-floor-a", 0.70),
		bug20260728Result("above-floor-b", 0.69),
		bug20260728Result("below-floor", 0.64),
	}}
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = false
	cfg.RerankEnabled = true
	cfg.UseRRF = false
	rerank := &bug20260803QwenTop1Reranker{}
	manager := NewManager(
		semanticManagerRepo{},
		leakingLegacySearcher{},
		nil,
		WithHybridConfig(cfg),
		WithRevisionSemanticSearcher(qwen),
		WithDocReranker(rerank),
	)

	_, hits, err := manager.QueryHits(context.Background(), "held-out query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if rerank.calls != 1 {
		t.Fatalf("rerank calls=%d, want 1", rerank.calls)
	}
	if len(rerank.inputIDs) != 2 {
		t.Fatalf("rerank candidates=%v, want only two above-floor candidates", rerank.inputIDs)
	}
	for _, id := range rerank.inputIDs {
		if id == "below-floor-chunk" {
			t.Fatalf("below-floor candidate reached reranker: %v", rerank.inputIDs)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("Qwen auto-injection hits=%v, want final Top-1 cap", hits)
	}
}

func TestBug20260728QwenTimeoutDoesNotBecomeSilentTextInjection(t *testing.T) {
	qwen := &bug20260728QwenPolicySearcher{
		searchErr: context.DeadlineExceeded,
		textResults: []*SearchResult{{
			Chunk:     &Chunk{ID: "text-only-chunk", DocID: "text-only", Content: "weak token overlap"},
			TextScore: 1,
		}},
	}
	_, hits, err := bug20260728Manager(qwen).QueryHits(context.Background(), "timeout query", 5)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("timed-out vector route was silently counted as injectable text success: %+v", hits)
	}
	if qwen.searchCalls != 1 || qwen.textCalls != 1 {
		t.Fatalf("route calls search/text=%d/%d, want one diagnostic vector attempt and one text route",
			qwen.searchCalls, qwen.textCalls)
	}
}
