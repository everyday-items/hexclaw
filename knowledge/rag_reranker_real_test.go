package knowledge

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon/rag/reranker"
	"github.com/hexagon-codes/hexagon/rag/splitter"
	_ "modernc.org/sqlite"
)

// 专用 cross-encoder 重排（SiliconFlow /rerank, BAAI/bge-reranker-v2-m3）真机验证：
// 1) 检索质量（含跨语种应被精排到 rank-1）；2) 延迟（应远快于 LLM 重排的 ~100s）。
func TestRAGReal_CrossEncoderReranker(t *testing.T) {
	emb := requireE2E(t)
	base, key := sfBaseKey(t)
	rerankBase := strings.TrimSuffix(strings.TrimSuffix(base, "/"), "/v1")
	rr := reranker.NewCohereReranker(key,
		reranker.WithCohereBaseURL(rerankBase),
		reranker.WithCohereModel(envOr("HEX_E2E_SF_RERANK", "BAAI/bge-reranker-v2-m3")),
		reranker.WithCohereTopK(50))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	sp := splitter.NewMarkdownSplitter(splitter.WithMarkdownChunkSize(400),
		splitter.WithMarkdownChunkOverlap(80), splitter.WithHeadersToSplit([]string{"#", "##", "###"}))
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled, cfg.ContextualEnabled = false, false // 重排开；隔离纯检索+重排
	// 注入专用 cross-encoder（无 LLM → 走 cross-encoder 而非 LLM 重排）
	mgr := NewManager(store, store, emb, WithSplitter(sp), WithHybridConfig(cfg), WithDocReranker(rr))
	if err := IngestGolden(ctx, mgr); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// 单查询延迟
	t0 := time.Now()
	if _, err := mgr.Search(ctx, "植物怎么产生氧气", 3); err != nil {
		t.Fatalf("search: %v", err)
	}
	lat := time.Since(t0)

	rep, err := RunEval(ctx, mgr, GoldenDataset(), 3)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	for _, c := range rep.Cases {
		if c.Name == "跨语种EN→ZH" && (c.Rank == 0 || c.Rank > 3) {
			t.Errorf("cross-encoder 应把跨语种召回进 top-3，got rank=%d", c.Rank)
		}
	}
	t.Logf("  ✓ cross-encoder 重排：单查询延迟=%v（vs LLM 重排 ~100s）", lat.Round(time.Millisecond))
	t.Logf("  ✓ recall@1=%.2f recall@3=%.2f MRR=%.3f  非top1:%v", rep.RecallAt1, rep.RecallAtK, rep.MRR, missesOf(rep))
	if rep.RecallAtK < 0.9 {
		t.Errorf("cross-encoder recall@3=%.2f < 0.9", rep.RecallAtK)
	}
	if lat > 15*time.Second {
		t.Errorf("cross-encoder 单查询延迟 %v 偏高（预期 << LLM 重排）", lat)
	}
}
