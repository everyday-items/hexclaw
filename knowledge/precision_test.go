package knowledge

import (
	"context"
	"testing"
)

// 精确率/抗噪的确定性单测：precision@k 指标计算 + MinScore 地板对纯弱向量噪声的拦截。
// 真模型在 distractor 语料上的 precision@k 跑分见 rag_precision_real_test.go。

// ① precision@k 指标计算（fakeEvalSearcher 预设有序标题，模型无关）。
func TestRunPrecisionEval_Metrics(t *testing.T) {
	ds := PrecisionDatasetT{
		{"t1", "q1", []string{"A", "B", "C", "D"}}, // top3 命中 A,B；X 是干扰
		{"t2", "q2", []string{"P"}},                // top3 全是干扰
	}
	fake := &fakeEvalSearcher{results: map[string][]string{
		"q1": {"A", "B", "X"},
		"q2": {"Z", "Y", "W"},
	}}
	rep, err := RunPrecisionEval(context.Background(), fake, ds, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !approxEq(rep.Cases[0].PrecisionK, 2.0/3) {
		t.Errorf("t1 precision@3 应为 2/3，got %v", rep.Cases[0].PrecisionK)
	}
	if rep.Cases[0].RelevantN != 2 || len(rep.Cases[0].Leaked) != 1 || rep.Cases[0].Leaked[0] != "X" {
		t.Errorf("t1 干扰泄漏应为 [X]，got %+v", rep.Cases[0])
	}
	if !approxEq(rep.Cases[1].PrecisionK, 0) || len(rep.Cases[1].Leaked) != 3 {
		t.Errorf("t2 precision@3 应为 0、泄漏 3，got %+v", rep.Cases[1])
	}
	if !approxEq(rep.MeanPrecK, (2.0/3)/2) {
		t.Errorf("MeanPrecK 应为 1/3，got %v", rep.MeanPrecK)
	}
	if rep.TotalLeaked != 4 {
		t.Errorf("TotalLeaked 应为 4，got %d", rep.TotalLeaked)
	}
}

// scriptedEmbedder 按精确文本返回预置向量（未命中返回与查询基正交的"远"向量），
// 用于工程化控制余弦相似度，确定性地验证 MinScore 地板。
type scriptedEmbedder struct {
	dim  int
	vecs map[string][]float32
}

func (s *scriptedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = s.lookup(t)
	}
	return out, nil
}

func (s *scriptedEmbedder) EmbedOne(_ context.Context, t string) ([]float32, error) {
	return s.lookup(t), nil
}

func (s *scriptedEmbedder) Dimension() int { return s.dim }

func (s *scriptedEmbedder) lookup(t string) []float32 {
	if v, ok := s.vecs[t]; ok {
		return v
	}
	v := make([]float32, s.dim) // 默认：与查询基（dim0）正交，未知文本一律"远"
	v[s.dim-1] = 1
	return v
}

func floorMgr(t *testing.T, minScore float64) *Manager {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	// query 与 relevant 同向（cos=1 → score 1.0 过地板）；noise 与 query 正交
	// （cos=0 → score 0.5 < 0.55 不过地板），且与 query 无任何 FTS 词元（TextScore=0）。
	emb := &scriptedEmbedder{dim: 4, vecs: map[string][]float32{
		"alpha signal":          {1, 0, 0, 0},
		"alpha signal relevant": {1, 0, 0, 0},
		"zeta omega unrelated":  {0, 1, 0, 0},
	}}
	cfg := HybridConfig{
		VectorWeight: 0.7, TextWeight: 0.3, MMRLambda: 0.7, TimeDecayDays: 0,
		MinScore: minScore, CandidateK: 50, RRFK: 60, UseRRF: true,
	}
	return NewManager(store, store, emb, WithSplitter(testSplitter()), WithHybridConfig(cfg))
}

// ② MinScore 地板拦截纯弱向量噪声：地板开 → 噪声被挡；地板关 → 噪声出现（对照证明地板是成因）。
func TestMinScoreFloor_RejectsBelowFloorNoise(t *testing.T) {
	ctx := context.Background()
	const relevant = "alpha signal relevant"
	const noise = "zeta omega unrelated"
	const query = "alpha signal"

	ingest := func(m *Manager) {
		if _, err := m.AddDocument(ctx, "RELEVANT", relevant, "test"); err != nil {
			t.Fatal(err)
		}
		if _, err := m.AddDocument(ctx, "NOISE", noise, "test"); err != nil {
			t.Fatal(err)
		}
	}
	has := func(hits []SearchHit, title string) bool {
		for _, h := range hits {
			if h.DocTitle == title {
				return true
			}
		}
		return false
	}

	// 地板开（0.55）：相关命中保留，纯弱向量噪声被挡（0.5 < 0.55 且无关键词支撑）。
	on := floorMgr(t, 0.55)
	ingest(on)
	hitsOn, err := on.Search(ctx, query, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !has(hitsOn, "RELEVANT") {
		t.Fatalf("地板开应保留相关文档，got %v", titlesOf(hitsOn))
	}
	if has(hitsOn, "NOISE") {
		t.Fatalf("地板开应挡掉纯弱向量噪声，却出现 NOISE：%v", titlesOf(hitsOn))
	}

	// 地板关（0）：同一噪声出现 → 证明拦截确由 MinScore 地板所致，而非别的环节。
	off := floorMgr(t, 0)
	ingest(off)
	hitsOff, err := off.Search(ctx, query, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !has(hitsOff, "NOISE") {
		t.Fatalf("地板关时噪声应出现（对照），got %v", titlesOf(hitsOff))
	}
	t.Logf("✓ MinScore 地板抗噪：开→{%v} 关→{%v}", titlesOf(hitsOn), titlesOf(hitsOff))
}

func titlesOf(hits []SearchHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.DocTitle
	}
	return out
}
