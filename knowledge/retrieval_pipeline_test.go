package knowledge

import (
	"context"
	"sync"
	"testing"
)

// ─── 测试替身 ───────────────────────────────────────────

// rpFakeRepo 满足 DocumentRepository（读路径测试用空实现）。
type rpFakeRepo struct{}

func (rpFakeRepo) Init(context.Context) error                     { return nil }
func (rpFakeRepo) Add(context.Context, *Document, []*Chunk) error { return nil }
func (rpFakeRepo) Get(context.Context, string) (*Document, error) { return nil, nil }
func (rpFakeRepo) List(context.Context) ([]*Document, error)      { return nil, nil }
func (rpFakeRepo) GetBySourceTitle(context.Context, string, string) (*Document, error) {
	return nil, nil
}
func (rpFakeRepo) Replace(context.Context, *Document, []*Chunk) error { return nil }
func (rpFakeRepo) Delete(context.Context, string) error               { return nil }

// rpFakeSearcher 返回受控的向量 / 文本结果（每次调用返回独立副本，避免分数互染）。
// lastFilter 记录最近一次收到的 Filter，便于断言 Manager 是否把过滤透传到了搜索层。
type rpFakeSearcher struct {
	vec        []*SearchResult
	text       []*SearchResult
	vecCalls   int
	txtCalls   int
	lastFilter Filter
}

func (f *rpFakeSearcher) VectorSearch(_ context.Context, _ []float32, topK int, filter Filter) ([]*SearchResult, error) {
	f.vecCalls++
	f.lastFilter = filter
	return cloneResults(f.vec, topK), nil
}

func (f *rpFakeSearcher) TextSearch(_ context.Context, _ string, topK int, filter Filter) ([]*SearchResult, error) {
	f.txtCalls++
	f.lastFilter = filter
	return cloneResults(f.text, topK), nil
}

func cloneResults(in []*SearchResult, topK int) []*SearchResult {
	out := make([]*SearchResult, 0, len(in))
	for _, r := range in {
		if topK > 0 && len(out) >= topK {
			break
		}
		c := *r.Chunk
		out = append(out, &SearchResult{Chunk: &c, VectorScore: r.VectorScore, TextScore: r.TextScore})
	}
	return out
}

func sr(id string, vScore, tScore float64) *SearchResult {
	return &SearchResult{
		Chunk:       &Chunk{ID: id, DocID: "d-" + id, Content: "content " + id},
		VectorScore: vScore,
		TextScore:   tScore,
	}
}

// rpFakeLLM 记录查询扩展发出的 prompt。multi-query 与 HyDE 会并发调用 Complete，
// 故 calls 必须加锁，否则 `go test -race` 会在并发 append 上报数据竞争。
type rpFakeLLM struct {
	reply string
	mu    sync.Mutex
	calls []string
}

func (f *rpFakeLLM) Complete(_ context.Context, prompt string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, prompt)
	f.mu.Unlock()
	return f.reply, nil
}

// callCount 线程安全地返回已记录的调用数（供断言用）。
func (f *rpFakeLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newRetrievalMgr(t *testing.T, searcher ChunkSearcher, cfg HybridConfig, llm RerankLLM) *Manager {
	t.Helper()
	opts := []ManagerOption{WithHybridConfig(cfg)}
	if llm != nil {
		opts = append(opts, WithLLM(llm))
	}
	return NewManager(rpFakeRepo{}, searcher, &fakeEmbedder{dim: 3}, opts...)
}

// baseCfg：隔离被测维度——默认关掉 rerank / expand（无 LLM 噪声），其余开。
func baseCfg() HybridConfig {
	c := DefaultHybridConfig()
	c.RerankEnabled = false
	c.ExpandEnabled = false
	c.MinScore = 0
	return c
}

// ─── #9 RRF 融合 ───────────────────────────────────────

// 同时命中向量+文本两路的 chunk，RRF 融合分应高于只命中一路的 chunk。
func TestSearch_RRFFusion_BothListsRankFirst(t *testing.T) {
	searcher := &rpFakeSearcher{
		vec:  []*SearchResult{sr("A", 0.9, 0), sr("B", 0.8, 0)}, // A rank1, B rank2
		text: []*SearchResult{sr("B", 0, 1.0), sr("C", 0, 0.5)}, // B rank1, C rank2
	}
	mgr := newRetrievalMgr(t, searcher, baseCfg(), nil)

	hits, err := mgr.Search(context.Background(), "q", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits, got %d", len(hits))
	}
	// B 命中两路（RRF 累加）→ 应排第一；A 只命中向量一路 → 第二；C 被 topK=2 截掉。
	if hits[0].ChunkID != "B" {
		t.Errorf("RRF: 双路命中的 B 应排第一，got %s", hits[0].ChunkID)
	}
	if hits[1].ChunkID != "A" {
		t.Errorf("RRF: 单路命中的 A 应排第二，got %s", hits[1].ChunkID)
	}
}

// ─── #3 相关度地板 ─────────────────────────────────────

// 纯弱向量命中（向量分低于地板且无关键词支撑）应被丢弃。
func TestSearch_MinScoreFloor_DropsWeakVectorOnly(t *testing.T) {
	searcher := &rpFakeSearcher{
		vec:  []*SearchResult{sr("A", 0.9, 0), sr("W", 0.55, 0)}, // W 向量 0.55 < 地板 0.6，无文本
		text: []*SearchResult{sr("A", 0, 0.8)},
	}
	cfg := baseCfg()
	cfg.MinScore = 0.6
	mgr := newRetrievalMgr(t, searcher, cfg, nil)

	hits, err := mgr.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.ChunkID == "W" {
			t.Fatalf("弱向量命中 W 应被相关度地板丢弃，但出现在结果中")
		}
	}
	if len(hits) != 1 || hits[0].ChunkID != "A" {
		t.Fatalf("应只保留达标的 A，got %+v", chunkIDs(hits))
	}
}

// 地板把所有候选清空时应放宽回退，保证不返回空。
func TestSearch_MinScoreFloor_RelaxedFallbackWhenEmpty(t *testing.T) {
	searcher := &rpFakeSearcher{
		vec:  []*SearchResult{sr("A", 0.5, 0), sr("W", 0.4, 0)}, // 全部低于地板且无文本
		text: nil,
	}
	cfg := baseCfg()
	cfg.MinScore = 0.9
	mgr := newRetrievalMgr(t, searcher, cfg, nil)

	hits, err := mgr.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("地板清空后应放宽回退返回全部 2 条，got %d (%v)", len(hits), chunkIDs(hits))
	}
}

// ─── #6 专用重排与 MMR 降级 ──────────────────────────────

// 启用 rerank 但未注入专用 cross-encoder 时，聊天 LLM 不得被隐式复用为重排器；
// 排序应确定性降级到 MMR。
func TestSearch_RerankWithoutDedicatedExecutorNeverInvokesChatLLM(t *testing.T) {
	searcher := &rpFakeSearcher{
		vec:  []*SearchResult{sr("A", 0.9, 0), sr("B", 0.8, 0), sr("C", 0.7, 0)},
		text: []*SearchResult{sr("A", 0, 0.6)},
	}
	cfg := baseCfg()
	cfg.RerankEnabled = true
	llm := &rpFakeLLM{reply: ""}
	mgr := newRetrievalMgr(t, searcher, cfg, llm)

	hits, err := mgr.Search(context.Background(), "q", 2)
	if err != nil {
		t.Fatal(err)
	}
	if llm.callCount() != 0 {
		t.Fatalf("未配专用 reranker 时不得调用聊天 LLM，calls=%d", llm.callCount())
	}
	if len(hits) == 0 || len(hits) > 2 {
		t.Fatalf("降级后仍应返回 1~2 条结果，got %d", len(hits))
	}
}

// ─── #8 查询扩展 ───────────────────────────────────────

// 启用 query-expand + 注入 LLM 时，应通过 LLM 生成扩展 query（HyDE/multi-query）。
func TestSearch_QueryExpand_InvokesLLM(t *testing.T) {
	searcher := &rpFakeSearcher{
		vec:  []*SearchResult{sr("A", 0.9, 0)},
		text: []*SearchResult{sr("A", 0, 0.6)},
	}
	cfg := baseCfg()
	cfg.ExpandEnabled = true
	llm := &rpFakeLLM{reply: "扩展查询词"}
	mgr := newRetrievalMgr(t, searcher, cfg, llm)

	if _, err := mgr.Search(context.Background(), "原始问题", 3); err != nil {
		t.Fatal(err)
	}
	if llm.callCount() == 0 {
		t.Fatal("query-expand 启用时应调用 LLM 生成扩展 query，但 LLM 未被调用")
	}
}

// ─── 降级：无 LLM ──────────────────────────────────────

// 不注入 LLM 时，query-expand 自动关闭，重排在无专用 executor 时走 MMR；
// RRF + 地板路径仍正常工作。
func TestSearch_NoLLMDegradesToHybridRetrievalAndMMR(t *testing.T) {
	searcher := &rpFakeSearcher{
		vec:  []*SearchResult{sr("A", 0.9, 0), sr("B", 0.7, 0)},
		text: []*SearchResult{sr("A", 0, 0.5)},
	}
	cfg := DefaultHybridConfig() // 默认 rerank/expand=true，但无 LLM 应自动降级
	mgr := newRetrievalMgr(t, searcher, cfg, nil)

	hits, err := mgr.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("无 LLM 降级路径不应返回空")
	}
	if searcher.vecCalls != 1 {
		t.Errorf("无 LLM 时不做 query-expand，向量检索应只调 1 次，got %d", searcher.vecCalls)
	}
}

func chunkIDs(hits []SearchHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ChunkID
	}
	return out
}
