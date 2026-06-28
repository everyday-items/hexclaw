package knowledge

import (
	"testing"
	"time"
)

// mgrWithConfig 构造一个仅用于评分/选取单元测试的 Manager（repo/searcher 用空桩，embedder nil）。
// config 字段已改为 atomic.Pointer，不能再用结构体字面量直接设，走 WithHybridConfig 选项。
func mgrWithConfig(c HybridConfig) *Manager {
	return NewManager(stubRepo{}, stubSearcher{}, nil, WithHybridConfig(c))
}

// TestBugTimeDecay_ZeroCreatedAtNotPenalized 回归锁定 [BUG-KB-TIMEDECAY]：
// 开启时间衰减（TimeDecayDays>0）时，CreatedAt 为零值（未设置时间戳）的 chunk
// 不应被当成"无限旧"而把分数衰减到 0、导致永不召回。
//
// 修复前：time.Since(零值) ≈ 2000+ 年 → exp(-λ·age) ≈ 0 → 分数塌成 0。
func TestBugTimeDecay_ZeroCreatedAtNotPenalized(t *testing.T) {
	m := mgrWithConfig(HybridConfig{TimeDecayDays: 30}) // embedder nil → 纯 textScore

	zeroTime := &SearchResult{Chunk: &Chunk{}, TextScore: 1.0} // CreatedAt 零值
	score := m.hybridScore(zeroTime)

	// 期望：无时间戳 → 不施加衰减惩罚，分数保留（≈ textScore 1.0）
	if score < 0.5 {
		t.Errorf("零值 CreatedAt 的 chunk 不应被时间衰减清零, score=%.4f（期望≈1.0）", score)
	}
}

// TestTimeDecay_RecentVsOld 验证有正常时间戳时，越新分数越高（衰减方向正确）。
func TestTimeDecay_RecentVsOld(t *testing.T) {
	m := mgrWithConfig(HybridConfig{TimeDecayDays: 30})

	recent := &SearchResult{Chunk: &Chunk{CreatedAt: time.Now()}, TextScore: 1.0}
	old := &SearchResult{Chunk: &Chunk{CreatedAt: time.Now().Add(-60 * 24 * time.Hour)}, TextScore: 1.0}

	sRecent := m.hybridScore(recent)
	sOld := m.hybridScore(old)
	if sRecent <= sOld {
		t.Errorf("较新 chunk 分数应高于较旧: recent=%.4f old=%.4f", sRecent, sOld)
	}
	// 半衰期 30 天，60 天 ≈ 0.25
	if sOld > 0.4 {
		t.Errorf("60 天前的 chunk 应衰减到约 0.25, got %.4f", sOld)
	}
}

// TestMMRSelect_NoDuplicateNoDrop 覆盖此前零测试的 MMR 贪心选择：
// topK < 候选数时返回恰好 topK 个、互不重复、不丢不越界。
func TestMMRSelect_NoDuplicateNoDrop(t *testing.T) {
	m := mgrWithConfig(HybridConfig{MMRLambda: 0.7})

	// 4 个带 embedding 的候选，topK=2
	cands := []*SearchResult{
		{Chunk: &Chunk{ID: "a", Embedding: []float32{1, 0, 0}, Score: 0.9}},
		{Chunk: &Chunk{ID: "b", Embedding: []float32{0.95, 0.1, 0}, Score: 0.85}}, // 与 a 高度相似
		{Chunk: &Chunk{ID: "c", Embedding: []float32{0, 1, 0}, Score: 0.6}},       // 与 a 正交
		{Chunk: &Chunk{ID: "d", Embedding: []float32{0, 0, 1}, Score: 0.5}},
	}
	got := m.mmrSelect(cands, 2)

	if len(got) != 2 {
		t.Fatalf("应返回恰好 2 个, got %d", len(got))
	}
	seen := map[string]bool{}
	for _, r := range got {
		if seen[r.Chunk.ID] {
			t.Errorf("MMR 选择出现重复: %s", r.Chunk.ID)
		}
		seen[r.Chunk.ID] = true
	}
	// 首选应是相关性最高的 a；多样性应使第二个偏向与 a 不相似的（c/d）而非高度相似的 b
	if !seen["a"] {
		t.Error("相关性最高的 a 应被选中")
	}
	if seen["b"] {
		t.Error("与 a 高度相似的 b 不应在 lambda=0.7 多样性下优先于正交的 c/d")
	}
}

// TestMMRSelect_FewerThanTopK 边界：候选数 ≤ topK 时全返回（按分排序）。
func TestMMRSelect_FewerThanTopK(t *testing.T) {
	m := mgrWithConfig(HybridConfig{MMRLambda: 0.7})
	cands := []*SearchResult{
		{Chunk: &Chunk{ID: "a", Embedding: []float32{1, 0}, Score: 0.5}},
		{Chunk: &Chunk{ID: "b", Embedding: []float32{0, 1}, Score: 0.9}},
	}
	got := m.mmrSelect(cands, 5)
	if len(got) != 2 {
		t.Fatalf("候选少于 topK 应全返回, got %d", len(got))
	}
}
