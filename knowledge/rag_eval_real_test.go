package knowledge

import (
	"context"
	"testing"
	"time"
)

// 真模型评测护栏：golden 语料 + 查询集，跑 recall@1/@3 + MRR，断言阈值，防召回回归。
// 纯检索层（rerank/expand 关）跑分——验证 #11 融合修复后不靠重排也达标（含跨语种）。
//
//	HEX_RAG_E2E=1 HEX_E2E_SF_* go test ./knowledge/ -run TestRAGReal_EvalGuard -v
func TestRAGReal_EvalGuard(t *testing.T) {
	emb := requireE2E(t) // 门控 HEX_RAG_E2E + SiliconFlow(BGE-M3)

	cfg := DefaultHybridConfig()
	cfg.RerankEnabled, cfg.ExpandEnabled, cfg.ContextualEnabled = false, false, false

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	mgr := newRealManager(t, cfg, emb, nil)
	if err := IngestGolden(ctx, mgr); err != nil {
		t.Fatalf("ingest golden: %v", err)
	}

	rep, err := RunEval(ctx, mgr, GoldenDataset(), 3)
	if err != nil {
		t.Fatalf("run eval: %v", err)
	}
	for _, c := range rep.Cases {
		t.Logf("  [%-12s] rank=%d top=%q", c.Name, c.Rank, c.TopTitle)
	}
	t.Logf("  ▶ recall@1=%.2f  recall@3=%.2f  MRR=%.3f  (N=%d, 纯检索层无重排)",
		rep.RecallAt1, rep.RecallAtK, rep.MRR, rep.N)

	// 护栏阈值：纯检索层 recall@3 ≥ 0.85（#11 融合修复后跨语种也应进 top-3）
	if rep.RecallAtK < 0.85 {
		t.Errorf("recall@3=%.2f 低于护栏阈值 0.85（召回回归）", rep.RecallAtK)
	}
}
