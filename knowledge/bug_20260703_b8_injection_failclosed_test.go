package knowledge

import (
	"context"
	"strings"
	"testing"
)

// BUG-20260703 B8：RAG 串知识库——问"九河科技地址"，自动注入却召回《乐知新创公司介绍》
// 的城市列表，模型把别家公司的信息当答案，直接答错（最伤信任）。
//
// 机械路径（三层 fail-open 叠加）：
//  1. FTS5 用 OR 连接分词 → "九河科技 公司地址" 里的通用词（公司/地址）命中任何
//     公司文档，TextScore > 0；
//  2. applyMinScore 对 TextScore > 0 的候选一律放行（地板只拦"纯弱向量命中"），
//     且 BM25 分是结果集内 min-max 归一——最佳垃圾恒为 1.0，无法当相关性用；
//  3. 过滤清空时"放宽回退"把全部噪声还回来，保证注入永不为空。
//
// 契约（fail-closed 注入）：Query/QueryWithFilter 是聊天自动注入专用路径（引擎
// react.go 的 kbContext），必须宁缺勿滥——没有语义相关度过地板（VectorScore >=
// MinScore）的候选时返回空串，让模型如实答"未找到"，绝不拿弱相关文档编答案。
// 显式检索路径（Search/SearchWithFilter：桌面 KB 页、knowledge_search 工具）保持
// 宽召回不变，用户自己看得到相关度。
func TestBug20260703_B8_InjectionFailClosedOnWeakGenericOverlap(t *testing.T) {
	ctx := context.Background()

	// 乐知类比文档：与查询仅共享通用词 "company"（TextScore>0），语义正交（cos=0 → 0.5 < 0.55）。
	const noiseDoc = "lezhi company intro: delivery teams in huzhou sanya ningde suzhou changchun hefei"
	const query = "jiuhe company address"

	emb := &scriptedEmbedder{dim: 4, vecs: map[string][]float32{
		query:    {1, 0, 0, 0},
		noiseDoc: {0, 1, 0, 0},
	}}
	mgr := b8Mgr(t, emb)
	if _, err := mgr.AddDocument(ctx, "乐知新创公司介绍", noiseDoc, "test"); err != nil {
		t.Fatal(err)
	}

	// ① 注入路径：无强相关命中 → 必须返回空（fail-closed），不得把乐知文档端给模型。
	injected, err := mgr.QueryWithFilter(ctx, query, 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if injected != "" {
		t.Fatalf("B8: 注入路径把弱相关文档端给了模型（串知识库）。QueryWithFilter = %q", injected)
	}

	// ② 显式检索路径保持宽召回：用户主动搜索仍可见弱相关命中（带相关度自行判断）。
	hits, err := mgr.SearchWithFilter(ctx, query, 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatalf("B8: 显式检索不应被注入地板收紧（宽召回契约被误伤）")
	}
}

// 对照：语义过地板的真命中必须照常注入（fail-closed 不等于失聪）。
func TestBug20260703_B8_InjectionKeepsStrongSemanticHit(t *testing.T) {
	ctx := context.Background()
	const relevantDoc = "jiuhe company address: hangzhou west lake district cloud town"
	const query = "jiuhe company address"

	emb := &scriptedEmbedder{dim: 4, vecs: map[string][]float32{
		query:       {1, 0, 0, 0},
		relevantDoc: {1, 0, 0, 0}, // cos=1 → 归一分 1.0 ≥ 0.55
	}}
	mgr := b8Mgr(t, emb)
	if _, err := mgr.AddDocument(ctx, "九河科技介绍", relevantDoc, "test"); err != nil {
		t.Fatal(err)
	}

	injected, err := mgr.QueryWithFilter(ctx, query, 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if injected == "" || !strings.Contains(injected, "hangzhou west lake district") {
		t.Fatalf("B8 对照: 强语义命中应照常注入, got %q", injected)
	}
}

// 混合场景：强命中与弱噪声并存 → 注入只含强命中，噪声不搭车。
func TestBug20260703_B8_InjectionDropsWeakNoiseAlongsideStrongHit(t *testing.T) {
	ctx := context.Background()
	const relevantDoc = "jiuhe company address: hangzhou west lake district cloud town"
	const noiseDoc = "lezhi company intro: delivery teams in huzhou sanya ningde suzhou"
	const query = "jiuhe company address"

	emb := &scriptedEmbedder{dim: 4, vecs: map[string][]float32{
		query:       {1, 0, 0, 0},
		relevantDoc: {1, 0, 0, 0},
		noiseDoc:    {0, 1, 0, 0},
	}}
	mgr := b8Mgr(t, emb)
	if _, err := mgr.AddDocument(ctx, "九河科技介绍", relevantDoc, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AddDocument(ctx, "乐知新创公司介绍", noiseDoc, "test"); err != nil {
		t.Fatal(err)
	}

	injected, err := mgr.QueryWithFilter(ctx, query, 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(injected, "hangzhou west lake district") {
		t.Fatalf("B8: 强命中丢失, got %q", injected)
	}
	if strings.Contains(injected, "huzhou sanya") {
		t.Fatalf("B8: 弱噪声搭强命中的车混进注入上下文, got %q", injected)
	}
}

// b8Mgr 构建带默认地板（0.85）的 Manager，配置与生产默认对齐（RRF + MMR，无辅助 LLM/专用重排器）。
func b8Mgr(t *testing.T, emb *scriptedEmbedder) *Manager {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	cfg := HybridConfig{
		VectorWeight: 0.7, TextWeight: 0.3, MMRLambda: 0.7, TimeDecayDays: 0,
		MinScore: 0.85, CandidateK: 50, RRFK: 60, UseRRF: true,
	}
	return NewManager(store, store, emb, WithSplitter(testSplitter()), WithHybridConfig(cfg))
}
