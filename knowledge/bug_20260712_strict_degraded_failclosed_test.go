package knowledge

import (
	"context"
	"testing"
)

// BUG-20260712-I：真机取证——问「明天天气怎么样」，聊天自动注入命中《Go面试题new》PDF，
// 前端会话里赫然一张无关「知识库命中」卡（模型上下文同时被污染）。
//
// 机械路径：B8 的 fail-closed 只在「向量路真实跑通」时生效——
//
//	searchResultsMode: applyMinScore(candidates, strictFloor && vectorRouteRan)
//	applyMinScore:     if minScore <= 0 || m.embedder == nil { return candidates }
//
// 于是两条降级路都把注入模式放宽成宽召回：
//
//	① embedder 未配置（本地部署常态）→ 免地板；
//	② embedder 在场但 Embed 失败/超时（端点抖动）→ vectorRouteRan=false → strict=false。
//
// 降级态下 BM25 是结果集内 min-max 归一分（最佳垃圾恒 1.0），天气 query 的通用分词
// 命中任意文档即被当强命中注入。
//
// 契约（B8 延伸到降级态）：聊天自动注入（Query/QueryWithFilter/QueryHits）没有语义证据
// （向量路未跑通）时必须返回空——宁缺勿滥，让模型如实作答；显式检索（Search*）保持宽召回。
func TestBug20260712_StrictInjection_FailClosedWithoutEmbedder(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	cfg := HybridConfig{
		VectorWeight: 0.7, TextWeight: 0.3, MMRLambda: 0.7,
		MinScore: 0.55, CandidateK: 50, RRFK: 60, UseRRF: true,
	}
	// ① embedder 未配置（纯词法降级态）
	mgr := NewManager(store, store, nil, WithSplitter(testSplitter()), WithHybridConfig(cfg))
	const noiseDoc = "go interview questions: goroutine channel mutex redis setnx expire distributed lock"
	if _, err := mgr.AddDocument(ctx, "Go面试题new", noiseDoc, "upload"); err != nil {
		t.Fatal(err)
	}

	const weatherQuery = "weather tomorrow go out" // 与文档仅共享通用词 go

	injected, err := mgr.QueryWithFilter(ctx, weatherQuery, 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if injected != "" {
		t.Fatalf("降级态①（无 embedder）注入未 fail-closed：天气 query 注入了 Go 面试题。got %q", injected)
	}
	_, hits, err := mgr.QueryHits(ctx, weatherQuery, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("降级态①：QueryHits 应为空（前端不显示无关「知识库命中」卡），got %d", len(hits))
	}

	// 对照：显式检索保持宽召回（用户自己看得到相关度）
	explicit, err := mgr.SearchWithFilter(ctx, weatherQuery, 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(explicit) == 0 {
		t.Fatalf("显式检索不得被降级态收紧（宽召回契约误伤）")
	}
}

func TestBug20260712_StrictInjection_FailClosedOnEmbedFailure(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	cfg := HybridConfig{
		VectorWeight: 0.7, TextWeight: 0.3, MMRLambda: 0.7,
		MinScore: 0.55, CandidateK: 50, RRFK: 60, UseRRF: true,
	}
	// ② embedder 在场但持续失败（端点挂了/超时）→ 查询时向量路跑不通
	mgr := NewManager(store, store, failingEmbedder{}, WithSplitter(testSplitter()), WithHybridConfig(cfg))
	const noiseDoc = "go interview questions: goroutine channel mutex redis setnx expire distributed lock"
	if _, err := mgr.AddDocument(ctx, "Go面试题new", noiseDoc, "upload"); err != nil {
		t.Fatal(err)
	}

	const weatherQuery = "weather tomorrow go out"

	injected, err := mgr.QueryWithFilter(ctx, weatherQuery, 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if injected != "" {
		t.Fatalf("降级态②（Embed 失败）注入未 fail-closed：got %q", injected)
	}

	explicit, err := mgr.SearchWithFilter(ctx, weatherQuery, 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(explicit) == 0 {
		t.Fatalf("显式检索不得被降级态收紧")
	}
}
