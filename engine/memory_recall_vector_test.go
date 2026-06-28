package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/memory/recall"
)

// 增量 G①：长期记忆召回向量 hybrid（0.7 向量 + 0.3 BM25）。
// 这些测试钉死：①配了 embedder → 语义相关但字面不重叠的事实被召回并压过字面重叠的干扰；
//              ②没配 embedder → 纯 BM25 行为不回归；③embedder 出错 → 软降级不阻断。

// topicEmbedder 是确定性假 embedder：把文本投到「海滨/山野/其他」三维语义轴之一。
// 让「三亚海景」与「海边度假」即使字面零重叠也落在同一轴（cosine=1），从而可测向量召回。
type topicEmbedder struct{ calls int }

func (t *topicEmbedder) Dimension() int { return 3 }

func (t *topicEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	t.calls++
	out := make([][]float32, len(texts))
	for i, s := range texts {
		out[i] = topicVecFor(s)
	}
	return out, nil
}

func topicVecFor(s string) []float32 {
	switch {
	case containsAnyOf(s, "海边", "海景", "三亚", "度假", "海岛"):
		return []float32{1, 0, 0} // 海滨度假轴
	case containsAnyOf(s, "爬山", "登山", "团建"):
		return []float32{0, 1, 0} // 山野轴
	default:
		return []float32{0, 0, 1} // 其他轴
	}
}

func containsAnyOf(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

type errEmbedder struct{}

func (errEmbedder) Dimension() int { return 3 }
func (errEmbedder) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return nil, errors.New("embed boom")
}

func factEntry(id, content string) recall.Entry {
	return recall.Entry{ID: id, Type: recall.TypeFact, Content: content}
}

// 单元：配了 embedder，Candidates 给出向量分；语义同轴 cosine≈1，跨轴≈0。
func TestMemEntrySource_FillsVectorScores(t *testing.T) {
	facts := []recall.Entry{
		factEntry("m-1", "用户在三亚买了海景房"),
		factEntry("m-2", "用户上周公司团建去爬山"),
	}
	src := memEntrySource{entries: facts, embedder: &topicEmbedder{}}

	cands, err := src.Candidates(context.Background(), "", "", "周末想去海边城市度假", 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(cands))
	}
	for _, c := range cands {
		if !c.HasVector {
			t.Fatalf("配了 embedder，候选应带向量分: %+v", c)
		}
	}
	// 三亚海景 与 海边度假 同轴 → cosine≈1；爬山团建跨轴 → ≈0。
	if cands[0].VectorScore < 0.99 {
		t.Fatalf("语义同轴应 cosine≈1，实际 %v", cands[0].VectorScore)
	}
	if cands[1].VectorScore > 0.01 {
		t.Fatalf("语义跨轴应 cosine≈0，实际 %v", cands[1].VectorScore)
	}
}

// 单元：没配 embedder → 纯 BM25，HasVector 全 false（不回归）。
func TestMemEntrySource_NoEmbedderPureBM25(t *testing.T) {
	facts := []recall.Entry{factEntry("m-1", "用户喜欢深色主题")}
	src := memEntrySource{entries: facts} // embedder=nil

	cands, err := src.Candidates(context.Background(), "", "", "深色主题", 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if cands[0].HasVector {
		t.Fatal("没配 embedder 不应带向量分")
	}
	if cands[0].BM25Score <= 0 {
		t.Fatalf("字面重叠应有 BM25 分，实际 %v", cands[0].BM25Score)
	}
}

// 单元：embedder 出错 → 软降级纯 BM25，不阻断、不 panic。
func TestMemEntrySource_EmbedErrorDegrades(t *testing.T) {
	facts := []recall.Entry{factEntry("m-1", "用户在三亚买了海景房")}
	src := memEntrySource{entries: facts, embedder: errEmbedder{}}

	cands, err := src.Candidates(context.Background(), "", "", "海边度假", 10)
	if err != nil {
		t.Fatalf("出错应软降级而非返回 err: %v", err)
	}
	if cands[0].HasVector {
		t.Fatal("embedder 出错时不应标 HasVector")
	}
}

// 集成：向量召回让「语义相关·字面不重叠」事实压过「字面重叠·语义无关」干扰；
// 关掉 embedder 则排序翻转（证明是向量在起作用，非别的）。
func TestRankFacts_VectorFlipsBM25Ranking(t *testing.T) {
	fm := newFileMem(t, 200)
	mustSave(t, fm, "用户在三亚买了海景房", "fact") // 语义相关(海滨)，与 query 字面零重叠
	mustSave(t, fm, "周末公司团建去爬山", "fact")  // 字面重叠(周末)，语义无关(山野)
	eng := engineWithFileMem(t, fm)

	query := "周末想去海边城市度假"

	// 关 embedder（纯 BM25）：字面重叠(周末)的干扰应排在海景事实之前。
	blockBM25 := eng.buildLongTermMemoryBlock(context.Background(), "", query)
	if !(strings.Index(blockBM25, "团建") < strings.Index(blockBM25, "海景")) {
		t.Fatalf("纯 BM25 下字面重叠干扰应在前:\n%s", blockBM25)
	}

	// 开 embedder（hybrid + 相关性地板）：语义相关的海景事实应被召回到前面；
	// 字面重叠但跨语义轴的「团建」干扰相关度低于地板(RecallMinScore)，被砍掉或排在海景之后。
	eng.SetMemoryEmbedder(&topicEmbedder{})
	blockHybrid := eng.buildLongTermMemoryBlock(context.Background(), "", query)
	pi, di := strings.Index(blockHybrid, "海景"), strings.Index(blockHybrid, "团建")
	if pi < 0 {
		t.Fatalf("海景事实应被向量召回:\n%s", blockHybrid)
	}
	if di >= 0 && di < pi {
		t.Fatalf("hybrid 下语义相关事实应压过字面干扰: 海景=%d 团建=%d\n%s", pi, di, blockHybrid)
	}
}

// 回归锁（真机 S2 bug）：相关性地板绝不砍到空——所有事实都低于地板时，退回不设地板、仍召回，不漏召到空。
func TestRankFacts_FloorNeverEmpties(t *testing.T) {
	fm := newFileMem(t, 200)
	mustSave(t, fm, "用户在三亚买了海景房", "fact") // 海滨轴
	eng := engineWithFileMem(t, fm)
	eng.SetMemoryEmbedder(&topicEmbedder{}) // 配 embedder → 地板 0.3 生效
	// query 落「山野」轴，与唯一事实「海滨」cosine=0、字面零重叠 → relevance≈0 < 地板 → 本会砍到空。
	block := eng.buildLongTermMemoryBlock(context.Background(), "", "周末去爬山登山团建")
	if !strings.Contains(block, "海景") {
		t.Fatalf("🔴 相关性地板砍到空（真机 S2 回归）：唯一事实被地板滤掉、召回为空。地板不该砍到空。\n%q", block)
	}
}
