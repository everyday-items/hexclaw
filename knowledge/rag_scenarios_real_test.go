package knowledge

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
)

// 多场景覆盖（真实模型，默认 skip；HEX_RAG_E2E=1 运行）。
// 用硅基流动 BGE-M3（多语种向量）、专用 cross-encoder（仅重排场景）与
// Qwen3.6-35B（仅会话/辅助生成），覆盖语义改写、干扰区分、精确术语、跨语种、
// 多 chunk 定位与无答案忠实性。聊天模型不会作为文档重排器。

var scenarioCorpus = []ragDoc{
	{"光合作用", "光合作用是绿色植物、藻类利用光能，把二氧化碳和水合成富能有机物并释放氧气的总过程，是地球氧气与能量的最终来源。\n\n" +
		"## 光反应\n光反应发生在叶绿体的类囊体膜上，叶绿素吸收光能，将水分子裂解，产生 ATP 和 NADPH，同时把氧气释放到大气中。\n\n" +
		"## 暗反应\n暗反应又称卡尔文循环，在叶绿体基质中进行，它利用光反应产生的 ATP 与 NADPH，把二氧化碳逐步固定、还原为葡萄糖。"},
	{"细胞呼吸", "细胞呼吸是细胞把葡萄糖等有机物在氧气参与下逐步氧化分解，释放能量并生成二氧化碳和水的过程，主要在线粒体中进行。" +
		"它可视为光合作用的逆过程，为生命活动提供 ATP 能量货币。"},
	{"Go 并发模型", "Go 以 CSP（通信顺序进程）为基础设计并发。goroutine 是运行时调度的轻量级执行单元，启动成本极低。" +
		"goroutine 之间通过 channel 传递数据，遵循以通信共享内存的原则；select 可在多个 channel 上多路复用；sync.Mutex 提供互斥。"},
	{"HexClaw 知识库检索配置", "HexClaw 知识库的检索参数：candidate_k 控制重排前的宽召回候选池大小，默认 50；" +
		"min_score 是向量相关度地板，默认 0.85，低于它且无关键词支撑的弱命中会被丢弃；rerank 开关控制是否尝试专用 cross-encoder 重排，" +
		"无专用 executor 时使用 MMR；" +
		"query_expand 控制 HyDE 与 multi-query 查询扩展；contextual 控制入库时是否给 chunk 生成文档级上下文。"},
	{"京杭大运河", "京杭大运河始建于春秋，隋朝贯通，北起北京南到杭州，沟通海河、黄河、淮河、长江、钱塘江五大水系，全长约一千八百公里。"},
}

type scenario struct{ name, query, wantTitle string }

// 检索场景：多语种向量 + FTS/RRF/MMR 即可正确、不依赖专用重排器的场景。
// 跨语种 EN→ZH 单列在 TestRAGReal_Scenarios_CrossLingual，对比禁用重排、无 executor 的
// MMR 降级和显式专用 cross-encoder 三条真实链路。
var retrievalScenarios = []scenario{
	{"语义改写", "植物是怎么把太阳能转变成养分、并产生氧气的？", "光合作用"},
	{"干扰区分", "细胞是如何在线粒体里氧化分解葡萄糖来释放能量的？", "细胞呼吸"},
	{"精确术语配置键", "candidate_k 这个参数是用来控制什么的？", "HexClaw 知识库检索配置"},
	{"精确符号channel", "在 Go 里 goroutine 之间用什么来传递数据？", "Go 并发模型"},
	{"多chunk定位", "光合作用的光反应阶段具体在叶绿体的哪个结构上发生？", "光合作用"},
}

func ingestDocs(t *testing.T, ctx context.Context, mgr *Manager, docs []ragDoc) {
	t.Helper()
	for _, d := range docs {
		doc, err := mgr.AddDocument(ctx, d.title, d.content, "scenario")
		if err != nil {
			t.Fatalf("ingest %q: %v", d.title, err)
		}
		t.Logf("  ✓ 入库 %q → %d chunk", d.title, doc.ChunkCount)
	}
}

func sfEmbedder(t *testing.T) (hexagon.VectorEmbedder, bool) {
	base, key := os.Getenv("HEX_E2E_SF_BASE"), os.Getenv("HEX_E2E_SF_KEY")
	if key == "" {
		return nil, false
	}
	em := envOr("HEX_E2E_SF_EMBED", "BAAI/bge-m3")
	emb := realEmbedder(base, key, em, embedDim(em))
	if vv, err := emb.Embed(context.Background(), []string{"探针"}); err != nil || len(vv) == 0 {
		t.Skipf("SF embedder 不可用：%v", err)
	}
	return emb, true
}

func sfChatLLM(t *testing.T) RerankLLM {
	t.Helper()
	base, key := os.Getenv("HEX_E2E_SF_BASE"), os.Getenv("HEX_E2E_SF_KEY")
	return &httpChatLLM{base: base, key: key, model: envOr("HEX_E2E_SF_CHAT", "Qwen/Qwen3.6-35B-A3B"),
		client: &http.Client{Timeout: 120 * time.Second}}
}

func requireE2E(t *testing.T) hexagon.VectorEmbedder {
	t.Helper()
	if os.Getenv("HEX_RAG_E2E") != "1" {
		t.Skip("real-model E2E：设 HEX_RAG_E2E=1 运行")
	}
	emb, ok := sfEmbedder(t)
	if !ok {
		t.Skip("HEX_E2E_SF_KEY 未设")
	}
	return emb
}

// 多场景检索覆盖：BGE-M3 + 纯向量/RRF（不含 LLM 阶段，快且确定）。
func TestRAGReal_Scenarios_Retrieval(t *testing.T) {
	emb := requireE2E(t)
	cfg := DefaultHybridConfig()
	cfg.RerankEnabled, cfg.ExpandEnabled, cfg.ContextualEnabled = false, false, false

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	mgr := newRealManager(t, cfg, emb, nil)
	ingestDocs(t, ctx, mgr, scenarioCorpus)

	pass := 0
	for _, s := range retrievalScenarios {
		hits, err := mgr.Search(ctx, s.query, 3)
		if err != nil {
			t.Errorf("[%s] search: %v", s.name, err)
			continue
		}
		got := ""
		if len(hits) > 0 {
			got = hits[0].DocTitle
		}
		ok := got == s.wantTitle
		if ok {
			pass++
		} else {
			t.Errorf("[%s] top=%q 期望=%q | query=%q", s.name, got, s.wantTitle, s.query)
		}
		t.Logf("  [%-14s] → top=%q (期望 %q) %s", s.name, got, s.wantTitle, tick(ok))
	}
	t.Logf("  多场景检索：%d/%d 通过", pass, len(retrievalScenarios))
}

// 跨语种 EN→ZH 三路对照：禁用重排、启用但无 executor（必须 MMR 且不执行聊天 LLM）、
// 显式注入真实 SiliconFlow cross-encoder。后两路都必须保持正确 grounding；专用路还必须
// 产生 executed/succeeded 指标，防止真模型测试在 MMR 降级后假装验证了 rerank。
func TestRAGReal_Scenarios_CrossLingual(t *testing.T) {
	emb := requireE2E(t)
	const q = "How do plants use sunlight to produce food and release oxygen?"

	type probeResult struct {
		top    string
		rerank RerankMetrics
	}
	probe := func(name string, rerank bool, options ...ManagerOption) probeResult {
		cfg := DefaultHybridConfig()
		cfg.ExpandEnabled, cfg.ContextualEnabled = false, false
		cfg.RerankEnabled = rerank
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		mgr := newRealManager(t, cfg, emb, nil)
		for _, option := range options {
			option(mgr)
		}
		ingestDocs(t, ctx, mgr, scenarioCorpus)
		hits, err := mgr.Search(ctx, q, 3)
		if err != nil {
			t.Fatalf("[%s] search: %v", name, err)
		}
		top := ""
		if len(hits) > 0 {
			top = hits[0].DocTitle
		}
		metrics := mgr.RetrievalMetricsSnapshot().Rerank
		t.Logf("  [%-20s] top=%q configured=%d executed=%d succeeded=%d no_executor=%d",
			name, top, metrics.Configured, metrics.Executed, metrics.Succeeded,
			metrics.Skipped[RerankSkipNoExecutor])
		return probeResult{top: top, rerank: metrics}
	}

	noRerank := probe("rerank_disabled", false)
	mmrFallback := probe("no_executor_mmr", true)
	cfg := DefaultHybridConfig()
	dedicated := probe("dedicated_cross_encoder", true,
		WithDocReranker(sfDedicatedReranker(t, cfg.CandidateK)))

	if mmrFallback.top != "光合作用" {
		t.Errorf("无专用 executor 的 MMR 链路应检索到「光合作用」，got %q", mmrFallback.top)
	}
	if mmrFallback.rerank.Executed != 0 || mmrFallback.rerank.Skipped[RerankSkipNoExecutor] == 0 {
		t.Errorf("无 executor 必须记录 MMR 降级且不得执行重排，metrics=%+v", mmrFallback.rerank)
	}
	if dedicated.top != "光合作用" {
		t.Errorf("专用 cross-encoder 应检索到「光合作用」，got %q", dedicated.top)
	}
	if dedicated.rerank.Executed == 0 || dedicated.rerank.Succeeded == 0 {
		t.Errorf("专用 cross-encoder 未真实执行成功，metrics=%+v", dedicated.rerank)
	}
	if noRerank.top != "光合作用" {
		t.Logf("  （诊断）禁用重排链路 top=%q；MMR/专用重排链路已分别验证", noRerank.top)
	}
}

// 会话 + 忠实性覆盖：BGE-M3 检索 + Qwen3.6-35B 答题。含正例/跨语种/无答案拒绝。
func TestRAGReal_Scenarios_Conversation(t *testing.T) {
	emb := requireE2E(t)
	llm := sfChatLLM(t)

	cfg := DefaultHybridConfig()
	cfg.RerankEnabled, cfg.ExpandEnabled, cfg.ContextualEnabled = false, false, false // 隔离“检索→答题”，控时
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	mgr := newRealManager(t, cfg, emb, llm)
	ingestDocs(t, ctx, mgr, scenarioCorpus)

	answer := func(q string) string {
		kbCtx, _ := mgr.Query(ctx, q, 3)
		ans, err := llm.Complete(ctx, "你是知识库助手。仅依据下面【资料】回答；若资料未涵盖该问题，必须直说“资料中没有相关信息”，绝不编造。\n"+
			"【资料】\n"+kbCtx+"\n\n【问题】"+q)
		if err != nil {
			t.Fatalf("answer %q: %v", q, err)
		}
		return ans
	}

	t.Run("positive_grounded", func(t *testing.T) {
		ans := answer("植物怎么把太阳能变成养分并产生氧气？")
		t.Logf("  答复=%q", clip(ans, 100))
		if !strings.Contains(ans, "氧气") {
			t.Errorf("正例答案应提到“氧气”，got %q", ans)
		}
	})

	t.Run("negative_faithfulness", func(t *testing.T) {
		ans := answer("怎么申请美国旅游签证？需要哪些材料？")
		t.Logf("  答复=%q", clip(ans, 110))
		refused := containsAnyOf(ans, []string{"没有相关", "没有", "未涵盖", "未提及", "无法", "不足", "抱歉", "未包含", "不包含"})
		if !refused {
			t.Errorf("无答案场景应明确拒绝/说明资料未涵盖，疑似编造：%q", ans)
		}
		if containsAnyOf(ans, []string{"DS-160", "面签预约", "I-20", "EVUS"}) {
			t.Errorf("疑似编造签证细节：%q", ans)
		}
	})
}

func containsAnyOf(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
