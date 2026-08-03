package knowledge

import (
	"context"
	"errors"
	"testing"

	hrag "github.com/hexagon-codes/hexagon/rag"
)

type rerankMetricsDouble struct{ err error }

func (r rerankMetricsDouble) Name() string { return "fixed-test-reranker" }
func (r rerankMetricsDouble) Rerank(_ context.Context, _ string, documents []hrag.Document) ([]hrag.Document, error) {
	if r.err != nil {
		return nil, r.err
	}
	return documents, nil
}

func rerankMetricCandidates() []*SearchResult {
	return []*SearchResult{
		{Chunk: &Chunk{ID: "a", DocID: "doc", Content: "secret-canary-a", Score: 0.9}},
		{Chunk: &Chunk{ID: "b", DocID: "doc", Content: "secret-canary-b", Score: 0.8}},
	}
}

func TestRerankMetricsSeparateConfiguredEligibleExecutedAndSkipReason(t *testing.T) {
	manager := newRetrievalMgr(t, &rpFakeSearcher{}, baseCfg(), nil)

	manager.rerankTopK(context.Background(), "secret-query-canary", rerankMetricCandidates(), 1)
	disabled := manager.RetrievalMetricsSnapshot().Rerank
	if disabled.Configured != 0 || disabled.Executed != 0 || disabled.Skipped[RerankSkipDisabled] != 1 {
		t.Fatalf("disabled rerank metrics=%+v", disabled)
	}

	cfg := manager.cfg()
	cfg.RerankEnabled = true
	manager.SetHybridConfig(cfg)
	manager.rerankTopK(context.Background(), "secret-query-canary", rerankMetricCandidates(), 1)
	missing := manager.RetrievalMetricsSnapshot().Rerank
	if missing.Configured != 1 || missing.Eligible != 1 || missing.Executed != 0 ||
		missing.Skipped[RerankSkipNoExecutor] != 1 {
		t.Fatalf("missing executor rerank metrics=%+v", missing)
	}

	WithDocReranker(rerankMetricsDouble{})(manager)
	manager.rerankTopK(context.Background(), "secret-query-canary", rerankMetricCandidates(), 1)
	succeeded := manager.RetrievalMetricsSnapshot().Rerank
	if succeeded.Executed != 1 || succeeded.Succeeded != 1 || succeeded.Failed != 0 {
		t.Fatalf("successful rerank metrics=%+v", succeeded)
	}

	WithDocReranker(rerankMetricsDouble{err: errors.New("secret-upstream-error")})(manager)
	manager.rerankTopK(context.Background(), "secret-query-canary", rerankMetricCandidates(), 1)
	failed := manager.RetrievalMetricsSnapshot().Rerank
	if failed.Executed != 2 || failed.Succeeded != 1 || failed.Failed != 1 ||
		failed.Skipped[RerankSkipExecutionFailed] != 1 {
		t.Fatalf("failed rerank metrics=%+v", failed)
	}
}

func TestRerankRequiresDedicatedExecutorAndNeverFallsBackToChatLLM(t *testing.T) {
	manager := newRetrievalMgr(t, &rpFakeSearcher{}, baseCfg(), nil)
	cfg := manager.cfg()
	cfg.RerankEnabled = true
	manager.SetHybridConfig(cfg)
	calls := 0
	WithLLM(RerankLLMFunc(func(context.Context, string) (string, error) {
		calls++
		return `[{"id":"a","score":1}]`, nil
	}))(manager)

	results := manager.rerankTopK(
		context.Background(), "query-canary", rerankMetricCandidates(), 1,
	)
	if calls != 0 {
		t.Fatalf("chat LLM was used as reranker: calls=%d", calls)
	}
	if len(results) != 1 {
		t.Fatalf("MMR fallback results=%d, want 1", len(results))
	}
	metrics := manager.RetrievalMetricsSnapshot().Rerank
	if metrics.Executed != 0 || metrics.Failed != 0 || metrics.Skipped[RerankSkipNoExecutor] != 1 {
		t.Fatalf("missing dedicated reranker metrics=%+v", metrics)
	}
}
