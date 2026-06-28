package knowledge

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// 多模型 + 全功能覆盖（真实模型，硅基流动；默认 skip，HEX_RAG_E2E=1 运行）。

func sfBaseKey(t *testing.T) (string, string) {
	t.Helper()
	if os.Getenv("HEX_RAG_E2E") != "1" {
		t.Skip("real-model E2E：设 HEX_RAG_E2E=1 运行")
	}
	base, key := os.Getenv("HEX_E2E_SF_BASE"), os.Getenv("HEX_E2E_SF_KEY")
	if key == "" {
		t.Skip("HEX_E2E_SF_KEY 未设")
	}
	return base, key
}

func splitCSV(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func sanitizeModel(m string) string { return strings.ReplaceAll(m, "/", "_") }

func coreCfgNoLLM() HybridConfig {
	c := DefaultHybridConfig()
	c.RerankEnabled, c.ExpandEnabled, c.ContextualEnabled = false, false, false
	return c
}

func hasTitle(hits []SearchHit, title string) bool {
	for _, h := range hits {
		if h.DocTitle == title {
			return true
		}
	}
	return false
}

// ① 更多 embedding 模型：recall@1/@3/MRR 矩阵（纯检索层，无 LLM 阶段）。
func TestRAGReal_EmbedModelMatrix(t *testing.T) {
	base, key := sfBaseKey(t)
	models := splitCSV(envOr("HEX_E2E_SF_EMBED_MODELS",
		"BAAI/bge-m3,Qwen/Qwen3-Embedding-8B,Qwen/Qwen3-Embedding-4B,Qwen/Qwen3-Embedding-0.6B,BAAI/bge-large-zh-v1.5,BAAI/bge-large-en-v1.5"))
	for _, model := range models {
		t.Run(sanitizeModel(model), func(t *testing.T) {
			emb := realEmbedder(base, key, model, embedDim(model))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			vv, err := emb.Embed(ctx, []string{"探针"})
			if err != nil || len(vv) == 0 {
				t.Skipf("embedder %s 不可用：%v", model, err)
			}
			mgr := newRealManager(t, coreCfgNoLLM(), emb, nil)
			if err := IngestGolden(ctx, mgr); err != nil {
				t.Fatalf("ingest: %v", err)
			}
			rep, err := RunEval(ctx, mgr, GoldenDataset(), 3)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			t.Logf("  [%-26s] dim=%d  golden(N=%d) recall@1=%.2f recall@3=%.2f MRR=%.3f  非top1:%v",
				model, len(vv[0]), rep.N, rep.RecallAt1, rep.RecallAtK, rep.MRR, missesOf(rep))

			// 压力场景：只测量、不硬断言（探鲁棒性极限）
			srep, _ := RunEval(ctx, mgr, StressDataset(), 3)
			t.Logf("       └─ stress(N=%d)  recall@1=%.2f recall@3=%.2f MRR=%.3f  非top3:%v",
				srep.N, srep.RecallAt1, srep.RecallAtK, srep.MRR, missesOf(srep))

			// 护栏只设灾难下限 0.5（多语种主力模型在 EvalGuard 另有 0.85 严格闸）；
			// 纯中文/纯英文模型在混合语料上偏弱属预期，本矩阵主要用于横评。
			if rep.RecallAtK < 0.5 {
				t.Errorf("%s golden recall@3=%.2f < 0.5（灾难）", model, rep.RecallAtK)
			}
		})
	}
}

func missesOf(rep EvalReport) []string {
	var m []string
	for _, c := range rep.Cases {
		if c.Rank != 1 {
			m = append(m, fmt.Sprintf("%s(r%d)", c.Name, c.Rank))
		}
	}
	return m
}

// ② 全功能覆盖：upsert 幂等 / 删除 / contextual 入库 / query-expand（BGE-M3 + Qwen3.6-35B）。
func TestRAGReal_Functions(t *testing.T) {
	emb := requireE2E(t)
	llm := sfChatLLM(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	t.Run("upsert_idempotent", func(t *testing.T) {
		mgr := newRealManager(t, coreCfgNoLLM(), emb, nil)
		d := GoldenCorpus()[0]
		a, err := mgr.AddDocument(ctx, d.Title, d.Content, "s")
		if err != nil {
			t.Fatal(err)
		}
		b, err := mgr.AddDocument(ctx, d.Title, d.Content, "s") // 同 (title,source) 再灌
		if err != nil {
			t.Fatal(err)
		}
		if a.ID != b.ID {
			t.Errorf("upsert 应复用同一 doc ID，got %s vs %s", a.ID, b.ID)
		}
		list, _ := mgr.ListDocuments(ctx)
		if len(list) != 1 {
			t.Errorf("再次入库应 upsert 不新增，list=%d", len(list))
		}
		if a.ChunkCount != b.ChunkCount {
			t.Errorf("chunk 数应稳定，%d vs %d", a.ChunkCount, b.ChunkCount)
		}
		t.Logf("  ✓ upsert 幂等：1 doc，chunk=%d", b.ChunkCount)
	})

	t.Run("delete_then_absent", func(t *testing.T) {
		mgr := newRealManager(t, coreCfgNoLLM(), emb, nil)
		doc, err := mgr.AddDocument(ctx, "临时签证文档", "美国旅游签证申请需要 DS-160 表格与面签预约。", "tmp")
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.DeleteDocument(ctx, doc.ID); err != nil {
			t.Fatal(err)
		}
		list, _ := mgr.ListDocuments(ctx)
		if len(list) != 0 {
			t.Errorf("删除后 list 应为空，got %d", len(list))
		}
		hits, _ := mgr.Search(ctx, "美国签证 DS-160 怎么申请", 3)
		for _, h := range hits {
			if h.DocID == doc.ID {
				t.Errorf("已删除文档不应被检索到")
			}
		}
		t.Logf("  ✓ 删除后不可检索（list=%d, 命中=%d）", len(list), len(hits))
	})

	t.Run("contextual_ingest_on", func(t *testing.T) {
		cfg := DefaultHybridConfig()
		cfg.RerankEnabled, cfg.ExpandEnabled = false, false
		cfg.ContextualEnabled = true // 多 chunk 文档入库时真调 LLM 生成情境
		mgr := newRealManager(t, cfg, emb, llm)
		if err := IngestGolden(ctx, mgr); err != nil {
			t.Fatal(err)
		}
		hits, _ := mgr.Search(ctx, "光合作用的光反应在叶绿体哪个结构进行", 3)
		if len(hits) == 0 || hits[0].DocTitle != "光合作用" {
			t.Errorf("contextual 入库后检索错位：%v", titles(hits))
		}
		t.Logf("  ✓ contextual 入库后检索正确；top chunk 前缀=%q", clip(hits[0].Content, 36))
	})

	t.Run("query_expand_on_vague", func(t *testing.T) {
		cfg := DefaultHybridConfig()
		cfg.RerankEnabled, cfg.ContextualEnabled = false, false
		cfg.ExpandEnabled = true // HyDE + multi-query
		mgr := newRealManager(t, cfg, emb, llm)
		if err := IngestGolden(ctx, mgr); err != nil {
			t.Fatal(err)
		}
		hits, _ := mgr.Search(ctx, "那个在叶绿体里把光变成糖、还会放出气体的过程", 3) // 含糊、零术语
		if !hasTitle(hits, "光合作用") {
			t.Errorf("query-expand 应召回「光合作用」：%v", titles(hits))
		}
		t.Logf("  ✓ query-expand 含糊查询召回正确：top=%q", firstTitle(hits))
	})
}

// ③ 更多 chat 模型：完整管线(rerank+contextual) + 会话 grounded + 无答案忠实性。
func TestRAGReal_ChatModelMatrix(t *testing.T) {
	emb := requireE2E(t)
	base, key := sfBaseKey(t)
	models := splitCSV(envOr("HEX_E2E_SF_CHAT_MODELS",
		"Qwen/Qwen3.6-35B-A3B,deepseek-ai/DeepSeek-V4-Pro,zai-org/GLM-5.2,deepseek-ai/DeepSeek-V3.2"))
	httpc := &http.Client{Timeout: 180 * time.Second}
	for _, model := range models {
		t.Run(sanitizeModel(model), func(t *testing.T) {
			llm := &httpChatLLM{base: base, key: key, model: model, client: httpc}
			ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
			defer cancel()
			if _, err := llm.Complete(ctx, "回复:ok"); err != nil {
				t.Skipf("chat %s 不可用：%v", model, err)
			}

			cfg := DefaultHybridConfig()
			// 控时：本矩阵聚焦各 chat 模型的「重排 + 会话 + 忠实性」；
			// contextual 入库已在 TestRAGReal_Functions 覆盖，这里关掉以省每模型 ~160s。
			cfg.ExpandEnabled, cfg.ContextualEnabled = false, false
			mgr := newRealManager(t, cfg, emb, llm)
			if err := IngestGolden(ctx, mgr); err != nil {
				t.Fatal(err)
			}

			// 跨语种 + 完整管线（融合修复 + 重排都应 nail）
			hits, _ := mgr.Search(ctx, "How do plants use sunlight to produce food and release oxygen?", 3)
			if len(hits) == 0 || hits[0].DocTitle != "光合作用" {
				t.Errorf("[%s] 跨语种完整管线检索错位：%v", model, titles(hits))
			}

			// 会话 grounded
			kbCtx, _ := mgr.Query(ctx, "植物怎么产生氧气", 3)
			ans, err := llm.Complete(ctx, "仅据资料一句话回答：\n"+kbCtx+"\n问题：植物怎么产生氧气")
			if err != nil {
				t.Fatalf("answer: %v", err)
			}
			grounded := strings.Contains(ans, "氧")
			t.Logf("  [%s] 会话grounded=%v ans=%q", model, grounded, clip(ans, 60))
			if !grounded {
				t.Errorf("[%s] 会话应提到氧气，got %q", model, ans)
			}

			// 无答案忠实性
			negCtx, _ := mgr.Query(ctx, "怎么申请美国签证", 3)
			ans2, err := llm.Complete(ctx, "仅据资料回答，资料没有就说“资料中没有相关信息”，绝不编造：\n"+negCtx+"\n问题：怎么申请美国旅游签证")
			if err != nil {
				t.Fatalf("neg answer: %v", err)
			}
			refused := containsAnyOf(ans2, []string{"没有", "未涵盖", "无法", "不足", "未提及", "抱歉", "未包含"})
			t.Logf("  [%s] 无答案拒绝=%v ans=%q", model, refused, clip(ans2, 50))
			if !refused {
				t.Errorf("[%s] 无答案应拒绝不编造，got %q", model, ans2)
			}
		})
	}
}

func firstTitle(hits []SearchHit) string {
	if len(hits) > 0 {
		return hits[0].DocTitle
	}
	return ""
}
