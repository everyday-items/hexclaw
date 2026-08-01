package knowledge

import (
	"context"
	"testing"
)

func TestRetrievalMetrics_RecordsFTSFallbackAndLaneLatency(t *testing.T) {
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
	if metrics.FTS.Calls == 0 || metrics.FTS.Empty == 0 || metrics.FTS.Fallbacks == 0 ||
		metrics.FTS.FallbackRate <= 0 || metrics.FTS.TotalLatencyMS <= 0 {
		t.Fatalf("应记录 FTS 空命中及 LIKE 降级: %+v", metrics)
	}
	if metrics.Like.Calls == 0 || metrics.Like.Hits == 0 || metrics.Like.HitRate <= 0 ||
		metrics.Like.TotalLatencyMS <= 0 {
		t.Fatalf("应记录 LIKE 命中和延迟: %+v", metrics)
	}
}
