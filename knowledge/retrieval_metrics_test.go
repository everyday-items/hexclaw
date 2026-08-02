package knowledge

import (
	"context"
	"testing"
)

func TestRetrievalMetrics_CJKFTSHitsWithoutLIKEFallback(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(store, store, nil, WithSplitter(testSplitter())).AddDocument(
		ctx, "教材", "牛顿第一定律描述物体运动状态的规律。", "upload:textbook.md",
	); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(store, store, nil, WithSplitter(testSplitter()))
	if _, _, err := mgr.SearchWithFilterReceipt(ctx, "牛顿定律", 3, Filter{}); err != nil {
		t.Fatal(err)
	}

	metrics := mgr.RetrievalMetricsSnapshot()
	if metrics.FTS.Calls == 0 || metrics.FTS.Hits == 0 || metrics.FTS.Empty != 0 ||
		metrics.FTS.Fallbacks != 0 || metrics.FTS.FallbackRate != 0 || metrics.FTS.TotalLatencyMS <= 0 {
		t.Fatalf("中文 bigram 应直接命中 FTS，不得降级 LIKE: %+v", metrics)
	}
	if metrics.Like.Calls != 0 || metrics.Like.Hits != 0 || metrics.Like.Fallbacks != 0 {
		t.Fatalf("中文 FTS 命中时 LIKE lane 必须为零调用: %+v", metrics)
	}
}
