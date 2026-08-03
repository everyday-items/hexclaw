package knowledge

import (
	"context"
	"testing"
	"time"
)

// 精确率/抗噪真模型评测（默认 skip，HEX_RAG_E2E=1 运行）。
//
// 在含话题相邻 distractor 的语料上量化 top-3 纯度。两路对照：
//   - retrieval（rerank 关）：此路用 MMR 多样性兜底排序（rerankTopK 在无重排器时退 mmrSelect），
//     MMRLambda=0.7 会**主动牺牲精确率换多样性**——故 top-3 会掺入不相关簇，precision@3 偏低，
//     这是设计使然，不是 bug。此路只测量+记录（宽松灾难floor），作为"为何需要重排"的证据。
//   - with_dedicated_reranker：显式配置的真实 cross-encoder 按纯相关度排序，干扰被挤出
//     top-3。这是专用重排能力的硬护栏；聊天 LLM 不参与该链路。
//
// 即「专用 executor 真实执行后保证 top-3 纯净」是被锁的不变量；仅打开 rerank 开关但
// 未配置 executor 时是 MMR 降级，不得冒充此测试的 cross-encoder 证据。
//
//	HEX_RAG_E2E=1 HEX_E2E_SF_* go test ./knowledge/ -run TestRAGReal_Precision -v
func TestRAGReal_Precision(t *testing.T) {
	emb := requireE2E(t)

	ingest := func(mgr *Manager) {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		for _, d := range DistractorCorpus() {
			if _, err := mgr.AddDocument(ctx, d.Title, d.Content, "precision"); err != nil {
				t.Fatalf("ingest %q: %v", d.Title, err)
			}
		}
	}

	logRep := func(label string, rep PrecisionReport) {
		for _, c := range rep.Cases {
			t.Logf("    [%s/%-14s] precision@3=%.2f 相关=%d/%d 泄漏=%v",
				label, c.Name, c.PrecisionK, c.RelevantN, c.Retrieved, c.Leaked)
		}
		t.Logf("  ▶ [%s] mean precision@3=%.2f 干扰泄漏总数=%d", label, rep.MeanPrecK, rep.TotalLeaked)
	}

	// ① 纯检索 + MMR 多样性兜底（rerank 关）：只测量、宽松灾难floor。
	t.Run("retrieval_mmr", func(t *testing.T) {
		cfg := coreCfgNoLLM()
		mgr := newRealManager(t, cfg, emb, nil)
		ingest(mgr)
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		rep, err := RunPrecisionEval(ctx, mgr, PrecisionDataset(), 3)
		if err != nil {
			t.Fatal(err)
		}
		logRep("retrieval+mmr", rep)
		// MMR 主动多样化 → precision 偏低属预期；只设宽松灾难floor 捕获"全错位"。
		if rep.MeanPrecK < 0.2 {
			t.Errorf("纯检索+MMR mean precision@3=%.2f < 0.2（疑似检索全错位，非 MMR 多样化）", rep.MeanPrecK)
		}
	})

	// ② 显式专用 cross-encoder：top-3 应基本纯净——硬护栏。
	t.Run("with_dedicated_reranker", func(t *testing.T) {
		cfg := DefaultHybridConfig()
		cfg.ExpandEnabled, cfg.ContextualEnabled = false, false
		cfg.RerankEnabled = true
		mgr := newRerankMgr(t, cfg, emb, sfDedicatedReranker(t, cfg.CandidateK))
		ingest(mgr)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		rep, err := RunPrecisionEval(ctx, mgr, PrecisionDataset(), 3)
		if err != nil {
			t.Fatal(err)
		}
		logRep("rerank", rep)
		metrics := mgr.RetrievalMetricsSnapshot().Rerank
		if metrics.Executed == 0 || metrics.Succeeded == 0 {
			t.Fatalf("专用 cross-encoder 未真实执行成功，metrics=%+v", metrics)
		}
		if rep.MeanPrecK < 0.67 {
			t.Errorf("专用 cross-encoder mean precision@3=%.2f < 0.67（近义干扰污染过重）", rep.MeanPrecK)
		}
	})
}
