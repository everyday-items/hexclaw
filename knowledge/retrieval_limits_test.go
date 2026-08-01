package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type retrievalBudgetSpySearcher struct{ textTopK int }

func (s *retrievalBudgetSpySearcher) VectorSearch(context.Context, []float32, int, Filter) ([]*SearchResult, error) {
	return nil, nil
}

func (s *retrievalBudgetSpySearcher) TextSearch(_ context.Context, _ string, topK int, _ Filter) ([]*SearchResult, error) {
	s.textTopK = topK
	return nil, nil
}

// HTTP 校验不是唯一边界：内部调用、旧 YAML 或未来调用方也不得把候选池放大到无上限。
func TestSearchResults_ClampsInternalCandidateBudget(t *testing.T) {
	cfg := DefaultHybridConfig()
	cfg.CandidateK = 999
	cfg.ExpandEnabled = false
	cfg.RerankEnabled = false
	spy := &retrievalBudgetSpySearcher{}
	mgr := NewManager(rpFakeRepo{}, spy, nil, WithHybridConfig(cfg))

	if _, _, err := mgr.SearchWithFilterReceipt(context.Background(), "algebra", 999, Filter{}); err != nil {
		t.Fatal(err)
	}
	if spy.textTopK != 100 {
		t.Fatalf("内部候选池应钳到 100，得 %d", spy.textTopK)
	}
	if got := mgr.GetHybridConfig().CandidateK; got != 100 {
		t.Fatalf("运行态配置应钳到 100，得 %d", got)
	}
}

func TestSearchResults_RejectsOverlongInternalQuery(t *testing.T) {
	spy := &retrievalBudgetSpySearcher{}
	mgr := NewManager(rpFakeRepo{}, spy, nil)
	_, _, err := mgr.SearchWithFilterReceipt(
		context.Background(), strings.Repeat("代", MaxSearchQueryRunes+1), 3, Filter{},
	)
	if !errors.Is(err, ErrSearchQueryBudgetExceeded) {
		t.Fatalf("内部超长 query 应返回预算错误，得 %v", err)
	}
	if spy.textTopK != 0 {
		t.Fatalf("拒绝前不得进入文本检索，received topK=%d", spy.textTopK)
	}
}
