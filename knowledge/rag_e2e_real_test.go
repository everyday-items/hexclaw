package knowledge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/rag/splitter"
	_ "modernc.org/sqlite"
)

// 真实模型端到端测试。默认 skip（离线/CI 不受影响）。运行：
//
//	HEX_RAG_E2E=1 go test ./knowledge/ -run TestRAGReal -v -timeout 40m
//
// 分两层（把“向量检索对不对”与“LLM 阶段能不能跑”解耦，前者快且确定、后者慢但只跑 1 query）：
//   - TestRAGReal_CoreRetrieval：纯向量+RRF（rerank/HyDE/contextual 全关、无需 chat LLM），
//     跑全部 query，验证“真 embedding 的向量语义召回”在本地 nomic 与云端 GLM embedding 上都对。
//   - TestRAGReal_FullPipeline：完整管线全开 + 真 chat LLM 会话落地，每个 chat provider 只跑 1 query。
//
// provider 经环境变量注入（密钥不入库）：HEX_E2E_OLLAMA_BASE/_CHAT/_EMBED，
// HEX_E2E_<NAME>_BASE/_KEY/_CHAT，HEX_E2E_GLM_EMBED。

type httpChatLLM struct {
	base, key, model string
	client           *http.Client
}

func (c *httpChatLLM) Complete(ctx context.Context, prompt string) (string, error) {
	return c.complete(ctx, prompt, true)
}

// complete 对真机云端 API 的瞬时抖动（EOF/超时/429/5xx）做有限重试。withThinking 控制是否
// 带 enable_thinking 参数：部分老模型（GLM-4-0414/Hunyuan-MT 等）不支持该参数会返 400，
// 命中即去参重试一次——既让 Qwen3.x 推理模型关思考拿正文，又兼容不支持该参数的模型。
func (c *httpChatLLM) complete(ctx context.Context, prompt string, withThinking bool) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt*2) * time.Second):
			}
		}
		out, retryable, err := c.completeOnce(ctx, prompt, withThinking)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if withThinking && strings.Contains(err.Error(), "enable_thinking") {
			return c.complete(ctx, prompt, false) // 该模型不支持 enable_thinking，去参重试
		}
		if !retryable {
			return "", err
		}
	}
	return "", fmt.Errorf("chat 重试 3 次仍失败: %w", lastErr)
}

func (c *httpChatLLM) completeOnce(ctx context.Context, prompt string, withThinking bool) (string, bool, error) {
	body := map[string]any{
		"model":       c.model,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"stream":      false,
		"temperature": 0,
		"max_tokens":  2048,
	}
	if withThinking {
		// Qwen3.x 推理模型思考开启时 content 可能为空（长 prompt 尤甚）→ 关思考稳定拿正文。
		// SiliconFlow 可选字段；不支持的模型会在 Complete 里被识别并去参重试。
		body["enable_thinking"] = false
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST",
		strings.TrimRight(c.base, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", true, err // 网络层错误（EOF/超时/连接重置）可重试
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		retryable := resp.StatusCode == 429 || resp.StatusCode >= 500
		return "", retryable, fmt.Errorf("chat %d: %s", resp.StatusCode, snippet(raw))
	}
	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", false, fmt.Errorf("decode: %w (%s)", err, snippet(raw))
	}
	if len(out.Choices) == 0 {
		return "", false, fmt.Errorf("no choices: %s", snippet(raw))
	}
	content := strings.TrimSpace(out.Choices[0].Message.Content)
	if content == "" {
		// 空正文（推理模型偶发把预算耗在思考上）按瞬时抖动重试，而非误判为功能失败。
		return "", true, fmt.Errorf("empty content: %s", snippet(raw))
	}
	return content, false, nil
}

func snippet(b []byte) string {
	if len(b) > 240 {
		return string(b[:240])
	}
	return string(b)
}

func realEmbedder(base, key, model string, dim int) hexagon.VectorEmbedder {
	if key == "" {
		key = "ollama"
	}
	prov := hexagon.NewOpenAI(key, hexagon.OpenAIWithBaseURL(base))
	emb := hexagon.NewOpenAIEmbedder(prov,
		hexagon.WithEmbedderModel(model), hexagon.WithEmbedderDimension(dim))
	// 与生产同构(cache+截断) + 真机抗瞬时网络抖动(TLS 超时/EOF)的有限重试
	return &retryingEmbedder{inner: NewTruncatingEmbedder(hexagon.NewCachedEmbedder(emb), 0), tries: 4}
}

// retryingEmbedder 对真机云端嵌入的瞬时错误（TLS 握手超时/EOF/5xx）做有限重试。
type retryingEmbedder struct {
	inner hexagon.VectorEmbedder
	tries int
}

func (e *retryingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	var err error
	for i := 0; i < e.tries; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(i*2) * time.Second):
			}
		}
		var out [][]float32
		if out, err = e.inner.Embed(ctx, texts); err == nil {
			return out, nil
		}
	}
	return nil, err
}

func (e *retryingEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	var err error
	for i := 0; i < e.tries; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(i*2) * time.Second):
			}
		}
		var out []float32
		if out, err = e.inner.EmbedOne(ctx, text); err == nil {
			return out, nil
		}
	}
	return nil, err
}

func (e *retryingEmbedder) Dimension() int { return e.inner.Dimension() }

// ── 语料：主题语义可分；查询用同义改写（不与原文共享关键词）逼真考验向量召回 ──

type ragDoc struct{ title, content string }

var e2eCorpus = []ragDoc{
	{"光合作用", "光合作用是绿色植物、藻类和某些细菌利用光能，把二氧化碳和水合成为富能有机物（主要是葡萄糖），" +
		"同时释放氧气的过程。它主要在叶绿体中完成，分为光反应与暗反应两个阶段。光反应发生在类囊体膜上，" +
		"借助叶绿素吸收光能，将水裂解并产生 ATP 和 NADPH，同时放出氧气；暗反应在叶绿体基质中进行，" +
		"利用前一阶段的能量把二氧化碳固定为葡萄糖。光合作用是地球上几乎所有生物能量的最终来源。"},
	{"Go 并发模型", "Go 语言以 CSP（通信顺序进程）为理论基础设计并发。goroutine 是由运行时调度的轻量级执行单元，" +
		"启动成本极低，单进程可同时运行成千上万个。goroutine 之间通过 channel 传递数据，遵循“以通信共享内存，" +
		"而非以共享内存通信”的原则。配合 select 语句可在多个通道上多路复用，sync 包提供互斥锁等低层原语。"},
	{"京杭大运河", "京杭大运河是世界上里程最长、工程最大的古代运河，始建于春秋时期，隋朝大规模贯通，元朝裁弯取直。" +
		"它北起北京、南到杭州，沟通了海河、黄河、淮河、长江与钱塘江五大水系，全长约一千八百公里，" +
		"两千多年来在南北漕运与经济文化交流中发挥了巨大作用。"},
}

type ragQuery struct{ q, wantTitle, wantKeyword string }

var e2eQueries = []ragQuery{
	{"植物是怎么把太阳能转变成养分、并产生氧气的？", "光合作用", "氧气"},
	{"在 Go 里如何低成本地同时跑很多并行任务？", "Go 并发模型", "goroutine"},
	{"古代中国连通南北、贯穿五大水系的人工水道是什么？", "京杭大运河", "运河"},
}

func newRealManager(t *testing.T, cfg HybridConfig, embedder hexagon.VectorEmbedder, llm RerankLLM) *Manager {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	sp := splitter.NewMarkdownSplitter(
		splitter.WithMarkdownChunkSize(400), splitter.WithMarkdownChunkOverlap(80),
		splitter.WithHeadersToSplit([]string{"#", "##", "###"}))
	opts := []ManagerOption{WithSplitter(sp), WithHybridConfig(cfg)}
	if llm != nil {
		opts = append(opts, WithLLM(llm))
	}
	return NewManager(store, store, embedder, opts...)
}

func ingestCorpus(t *testing.T, ctx context.Context, mgr *Manager) {
	t.Helper()
	for _, d := range e2eCorpus {
		doc, err := mgr.AddDocument(ctx, d.title, d.content, "e2e")
		if err != nil {
			t.Fatalf("ingest %q: %v", d.title, err)
		}
		if doc.ChunkCount == 0 {
			t.Fatalf("ingest %q: 0 chunks", d.title)
		}
		t.Logf("  ✓ 入库 %q → %d chunk", d.title, doc.ChunkCount)
	}
}

// TestRAGReal_CoreRetrieval：纯向量+RRF（无 LLM 阶段），跑全部 query，证明真 embedding 的语义召回正确。
func TestRAGReal_CoreRetrieval(t *testing.T) {
	if os.Getenv("HEX_RAG_E2E") != "1" {
		t.Skip("real-model E2E：设 HEX_RAG_E2E=1 运行")
	}
	coreCfg := func() HybridConfig {
		c := DefaultHybridConfig()
		c.RerankEnabled, c.ExpandEnabled, c.ContextualEnabled = false, false, false
		return c
	}

	run := func(t *testing.T, embedder hexagon.VectorEmbedder) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		mgr := newRealManager(t, coreCfg(), embedder, nil)
		// 探针：embedder 不可用（如云端余额不足）则跳过，避免误判为召回 bug
		if vv, err := embedder.Embed(ctx, []string{"探针"}); err != nil || len(vv) == 0 || len(vv[0]) == 0 {
			t.Skipf("embedder 不可用，跳过：%v", err)
		}
		ingestCorpus(t, ctx, mgr)
		correct := 0
		for _, qc := range e2eQueries {
			hits, err := mgr.Search(ctx, qc.q, 3)
			if err != nil {
				t.Errorf("search %q: %v", qc.q, err)
				continue
			}
			if len(hits) == 0 {
				t.Errorf("query %q：0 命中", qc.q)
				continue
			}
			ok := hits[0].DocTitle == qc.wantTitle
			if ok {
				correct++
			} else {
				t.Errorf("query %q：top=%q 期望=%q（向量语义召回错位）", qc.q, hits[0].DocTitle, qc.wantTitle)
			}
			t.Logf("  检索 %q → top=%q (相关度 %.0f%%) %s", clip(qc.q, 28), hits[0].DocTitle, hits[0].Score*100, tick(ok))
		}
		if correct == len(e2eQueries) {
			t.Logf("  ✅ 向量语义召回 %d/%d 全对", correct, len(e2eQueries))
		}
	}

	ollamaBase := envOr("HEX_E2E_OLLAMA_BASE", "http://localhost:11434/v1")
	t.Run("ollama_nomic_embed", func(t *testing.T) {
		run(t, realEmbedder(ollamaBase, "", envOr("HEX_E2E_OLLAMA_EMBED", "nomic-embed-text"), 768))
	})
	if base, key, _ := envProvider("GLM"); key != "" {
		if em := os.Getenv("HEX_E2E_GLM_EMBED"); em != "" {
			t.Run("cloud_glm_embed", func(t *testing.T) {
				run(t, realEmbedder(base, key, em, embedDim(em)))
			})
		}
	}
	if base, key, _ := envProvider("SF"); key != "" {
		t.Run("cloud_siliconflow_bge", func(t *testing.T) {
			em := envOr("HEX_E2E_SF_EMBED", "BAAI/bge-m3")
			run(t, realEmbedder(base, key, em, embedDim(em)))
		})
	}
}

// TestRAGReal_FullPipeline：完整管线全开（HyDE+multi-query+rerank+contextual）+ 真 chat LLM 会话落地，
// 每个 chat provider 只跑 1 query（控时），证明各阶段在真模型上能端到端跑通。
func TestRAGReal_FullPipeline(t *testing.T) {
	if os.Getenv("HEX_RAG_E2E") != "1" {
		t.Skip("real-model E2E：设 HEX_RAG_E2E=1 运行")
	}
	httpc := &http.Client{Timeout: 240 * time.Second}
	ollamaBase := envOr("HEX_E2E_OLLAMA_BASE", "http://localhost:11434/v1")
	nomic := realEmbedder(ollamaBase, "", envOr("HEX_E2E_OLLAMA_EMBED", "nomic-embed-text"), 768)
	qc := e2eQueries[0] // 只跑第一条，控时

	run := func(t *testing.T, llm RerankLLM) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		// 探针：chat LLM 不可用（如云端余额不足）则跳过
		if _, err := llm.Complete(ctx, "回复:ok"); err != nil {
			t.Skipf("chat LLM 不可用，跳过：%v", err)
		}
		mgr := newRealManager(t, DefaultHybridConfig(), nomic, llm) // 全开
		ingestCorpus(t, ctx, mgr)

		hits, err := mgr.Search(ctx, qc.q, 3)
		if err != nil {
			t.Fatalf("full-pipeline search %q: %v", qc.q, err)
		}
		if len(hits) == 0 || hits[0].DocTitle != qc.wantTitle {
			t.Fatalf("full-pipeline 检索错位：%v", titles(hits))
		}
		t.Logf("  ✓ 完整管线检索 %q → top=%q", clip(qc.q, 28), hits[0].DocTitle)

		// 会话落地：检索上下文喂真 chat LLM
		kbCtx, _ := mgr.Query(ctx, qc.q, 3)
		ans, err := llm.Complete(ctx, "仅根据下面资料用一句话回答，不要编造。\n资料：\n"+kbCtx+"\n\n问题："+qc.q)
		if err != nil {
			t.Fatalf("会话生成失败: %v", err)
		}
		t.Logf("  会话答复=%q  含关键词%q=%v", clip(ans, 90), qc.wantKeyword, strings.Contains(ans, qc.wantKeyword))
		if strings.TrimSpace(ans) == "" {
			t.Errorf("会话答复为空")
		}
	}

	// 本地 qwen 仅在 HEX_E2E_RUN_OLLAMA_CHAT=1 时跑（本机 9B 常冷启动超时，默认不跑免浪费时间）
	if os.Getenv("HEX_E2E_RUN_OLLAMA_CHAT") == "1" {
		t.Run("ollama_qwen", func(t *testing.T) {
			run(t, &httpChatLLM{base: ollamaBase, model: envOr("HEX_E2E_OLLAMA_CHAT", "qwen3.5:9b"), client: httpc})
		})
	}
	if base, key, model := envProvider("SF"); key != "" {
		t.Run("cloud_siliconflow_qwen", func(t *testing.T) {
			run(t, &httpChatLLM{base: base, key: key, model: envOr("HEX_E2E_SF_CHAT", model), client: httpc})
		})
	}
	if base, key, model := envProvider("GLM"); key != "" {
		t.Run("cloud_glm_chat", func(t *testing.T) {
			run(t, &httpChatLLM{base: base, key: key, model: model, client: httpc})
		})
	}
	if base, key, model := envProvider("OPENROUTER"); key != "" {
		t.Run("cloud_openrouter_chat", func(t *testing.T) {
			run(t, &httpChatLLM{base: base, key: key, model: model, client: httpc})
		})
	}
}

// ── helpers ──

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envProvider(name string) (base, key, model string) {
	return os.Getenv("HEX_E2E_" + name + "_BASE"),
		os.Getenv("HEX_E2E_" + name + "_KEY"),
		os.Getenv("HEX_E2E_" + name + "_CHAT")
}

func embedDim(model string) int {
	switch {
	case strings.Contains(model, "embedding-3"):
		return 2048
	case strings.Contains(model, "embedding-2"):
		return 1024
	default:
		return 1024
	}
}

func tick(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

func titles(hits []SearchHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.DocTitle
	}
	return out
}

func clip(s string, n int) string {
	r := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}
