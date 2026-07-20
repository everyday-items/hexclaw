package knowledge

import (
	"context"
	"testing"

	hrag "github.com/hexagon-codes/hexagon/rag"
)

type semanticManagerRepo struct{}

func (semanticManagerRepo) Init(context.Context) error                     { return nil }
func (semanticManagerRepo) Add(context.Context, *Document, []*Chunk) error { return nil }
func (semanticManagerRepo) Get(context.Context, string) (*Document, error) { return nil, nil }
func (semanticManagerRepo) List(context.Context) ([]*Document, error)      { return nil, nil }
func (semanticManagerRepo) GetBySourceTitle(context.Context, string, string) (*Document, error) {
	return nil, nil
}
func (semanticManagerRepo) Replace(context.Context, *Document, []*Chunk) error   { return nil }
func (semanticManagerRepo) Delete(context.Context, string) error                 { return nil }
func (semanticManagerRepo) HasSearchableDocuments(context.Context) (bool, error) { return true, nil }

type leakingLegacySearcher struct{}

func (leakingLegacySearcher) VectorSearch(context.Context, []float32, int, Filter) ([]*SearchResult, error) {
	return nil, nil
}
func (leakingLegacySearcher) TextSearch(context.Context, string, int, Filter) ([]*SearchResult, error) {
	return []*SearchResult{{
		Chunk:     &Chunk{ID: "owner-two-chunk", DocID: "owner-two-doc", Content: "sharedtoken"},
		TextScore: 1,
	}}, nil
}

type scopedManagerSearcher struct {
	textCalls int
	queries   []string
}

func (s *scopedManagerSearcher) Search(_ context.Context, query string, _ int, _ Filter) ([]*SearchResult, bool, error) {
	s.queries = append(s.queries, query)
	return nil, false, nil
}
func (s *scopedManagerSearcher) TextSearch(context.Context, string, int, Filter) ([]*SearchResult, error) {
	s.textCalls++
	return []*SearchResult{{
		Chunk:     &Chunk{ID: "owner-one-chunk", DocID: "owner-one-doc", Content: "sharedtoken"},
		TextScore: 1,
	}}, nil
}

func TestManagerUsesCorpusScopedTextRouteWhenRevisionRuntimeIsInstalled(t *testing.T) {
	scoped := &scopedManagerSearcher{}
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = false
	cfg.RerankEnabled = false
	cfg.MinScore = 0
	cfg.EmbedQueryPrefix = "legacy-query-prefix: "
	manager := NewManager(semanticManagerRepo{}, leakingLegacySearcher{}, nil,
		WithHybridConfig(cfg), WithRevisionSemanticSearcher(scoped))

	hits, err := manager.Search(context.Background(), "sharedtoken", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].DocID != "owner-one-doc" || scoped.textCalls != 1 {
		t.Fatalf("hits=%+v scopedTextCalls=%d, want owner-one corpus route", hits, scoped.textCalls)
	}
	if len(scoped.queries) != 1 || scoped.queries[0] != "sharedtoken" {
		t.Fatalf("revision query route received %#v, want raw query exactly once", scoped.queries)
	}
}

type lowScoreRevisionSearcher struct{}

func (lowScoreRevisionSearcher) Search(context.Context, string, int, Filter) ([]*SearchResult, bool, error) {
	return []*SearchResult{{
		Chunk:       &Chunk{ID: "weak-chunk", DocID: "weak-doc", Content: "weak"},
		VectorScore: 0.2,
	}}, true, nil
}

func (lowScoreRevisionSearcher) TextSearch(context.Context, string, int, Filter) ([]*SearchResult, error) {
	return nil, nil
}

func TestManagerStrictMinScoreAppliesToRevisionRouteWithoutLegacyEmbedder(t *testing.T) {
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = false
	cfg.RerankEnabled = false
	cfg.MinScore = 0.8
	manager := NewManager(semanticManagerRepo{}, leakingLegacySearcher{}, nil,
		WithHybridConfig(cfg), WithRevisionSemanticSearcher(lowScoreRevisionSearcher{}))

	_, hits, err := manager.QueryHits(context.Background(), "unrelated", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("strict revision min-score admitted weak hit: %+v", hits)
	}
}

type standbyRevisionSearcher struct {
	searchCalls int
	textCalls   int
}

func (s *standbyRevisionSearcher) HasActiveRevision(context.Context) (bool, error) {
	return false, nil
}

func (s *standbyRevisionSearcher) Search(context.Context, string, int, Filter) ([]*SearchResult, bool, error) {
	s.searchCalls++
	return nil, false, nil
}

func (s *standbyRevisionSearcher) TextSearch(context.Context, string, int, Filter) ([]*SearchResult, error) {
	s.textCalls++
	return []*SearchResult{
		{Chunk: &Chunk{ID: "text-only-a", DocID: "doc-text-a", Content: "text evidence a"}, TextScore: 1},
		{Chunk: &Chunk{ID: "text-only-b", DocID: "doc-text-b", Content: "text evidence b"}, TextScore: 0.8},
	}, nil
}

type countingRerankLLM struct{ calls int }

func (l *countingRerankLLM) Complete(context.Context, string) (string, error) {
	l.calls++
	return "expanded query", nil
}

type countingDocReranker struct{ calls int }

func (*countingDocReranker) Name() string { return "counting" }

func (r *countingDocReranker) Rerank(
	_ context.Context,
	_ string,
	documents []hrag.Document,
) ([]hrag.Document, error) {
	r.calls++
	return documents, nil
}

func TestManagerTextOnlyRevisionStandbySkipsAuxiliaryQueryExpansion(t *testing.T) {
	standby := &standbyRevisionSearcher{}
	llm := &countingRerankLLM{}
	docReranker := &countingDocReranker{}
	legacyEmbedder := &countingLegacyEmbedder{}
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = true
	cfg.RerankEnabled = true
	cfg.MinScore = 0
	manager := NewManager(semanticManagerRepo{}, leakingLegacySearcher{}, legacyEmbedder,
		WithHybridConfig(cfg), WithRevisionSemanticSearcher(standby), WithLLM(llm), WithDocReranker(docReranker))
	hits, err := manager.Search(context.Background(), "text evidence", 5)
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 0 || docReranker.calls != 0 || legacyEmbedder.calls != 0 ||
		standby.searchCalls != 1 || standby.textCalls != 1 || len(hits) != 2 {
		t.Fatalf("standby calls: llm=%d reranker=%d embedder=%d semantic=%d text=%d hits=%d",
			llm.calls, docReranker.calls, legacyEmbedder.calls,
			standby.searchCalls, standby.textCalls, len(hits))
	}
}
