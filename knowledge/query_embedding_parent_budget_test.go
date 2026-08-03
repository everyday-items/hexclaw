package knowledge

import (
	"context"
	"sync"
	"testing"
	"time"
)

type parentBudgetRevisionSearcher struct {
	mu        sync.Mutex
	calls     int
	remaining []time.Duration
}

func (*parentBudgetRevisionSearcher) QueryEmbeddingTimeout() time.Duration {
	return 50 * time.Millisecond
}
func (*parentBudgetRevisionSearcher) AutoInjectionMinScore() float64 { return 0.1 }
func (*parentBudgetRevisionSearcher) AutoInjectionMaxResults() int   { return 3 }
func (s *parentBudgetRevisionSearcher) Search(ctx context.Context, _ string, _ int, _ Filter) ([]*SearchResult, bool, error) {
	s.mu.Lock()
	s.calls++
	if deadline, ok := ctx.Deadline(); ok {
		s.remaining = append(s.remaining, time.Until(deadline))
	}
	s.mu.Unlock()
	select {
	case <-time.After(30 * time.Millisecond):
		return nil, true, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}
func (*parentBudgetRevisionSearcher) TextSearch(context.Context, string, int, Filter) ([]*SearchResult, error) {
	return nil, nil
}

func TestExpandedQueriesShareOneQueryEmbeddingParentBudget(t *testing.T) {
	searcher := &parentBudgetRevisionSearcher{}
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = true
	cfg.RerankEnabled = false
	cfg.MinScore = 0
	manager := NewManager(
		semanticManagerRepo{}, leakingLegacySearcher{}, nil,
		WithHybridConfig(cfg),
		WithRevisionSemanticSearcher(searcher),
		WithLLM(RerankLLMFunc(func(context.Context, string) (string, error) {
			return "q2\nq3\nq4", nil
		})),
	)

	started := time.Now()
	if _, err := manager.Search(context.Background(), "q1", 3); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	searcher.mu.Lock()
	defer searcher.mu.Unlock()
	if searcher.calls < 2 {
		t.Fatalf("expanded search calls=%d, want at least 2", searcher.calls)
	}
	if elapsed > 90*time.Millisecond {
		t.Fatalf("expanded embeddings reset the 50ms budget per query: elapsed=%v calls=%d", elapsed, searcher.calls)
	}
	for i := 1; i < len(searcher.remaining); i++ {
		if searcher.remaining[i] > searcher.remaining[i-1]+5*time.Millisecond {
			t.Fatalf("embedding deadline was reset: remaining=%v", searcher.remaining)
		}
	}
}
