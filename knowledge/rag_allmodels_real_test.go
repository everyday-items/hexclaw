package knowledge

import (
	"context"
	"fmt"
	"image/color"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon/rag/reranker"
)

// 全模型真机覆盖（默认 skip，HEX_RAG_E2E=1 运行）。
//
// 按用户真实操作流程，把 SiliconFlow 账号上**每一类模型**都跑过它在 RAG/会话/重排/多模态
// 流水线里的真实角色，逐模型 🟢/🟡 + 质量指标：
//   - 全 chat 模型 → 检索→grounded 会话→无答案拒答→忠实性裁判（固定强裁判）
//   - 全 embedding 模型 → 见 TestRAGReal_EmbedModelMatrix（recall@1/@3/MRR 横评）
//   - 全 reranker 模型 → cross-encoder 把跨语种难例精排到 rank-1
//   - 全 vision 模型 → 图片 caption→入库→按色跨模态召回
//
// 模型清单经 env 注入（HEX_E2E_SF_{CHAT,RERANK,VL}_MODELS，逗号分隔），由运行脚本从 /models
// 实时拉全量；单模型能力弱（指令跟随差）记 🟡 不阻断（非代码 bug），系统性断裂才 🔴。
//
//	HEX_RAG_E2E=1 HEX_E2E_SF_* go test ./knowledge/ -run 'TestRAGReal_All' -v -timeout 120m

// modelResult 单模型一行结果。
type modelResult struct {
	model  string
	status string // 🟢 / 🟡 / ⚪skip
	detail string
}

func logModelTable(t *testing.T, title string, rows []modelResult) {
	t.Helper()
	var green, yellow, skip int
	t.Logf("──────── %s ────────", title)
	for _, r := range rows {
		switch {
		case strings.HasPrefix(r.status, "🟢"):
			green++
		case strings.HasPrefix(r.status, "🟡"):
			yellow++
		default:
			skip++
		}
		t.Logf("  %s  %-40s %s", r.status, r.model, r.detail)
	}
	t.Logf("──────── 小结：🟢%d 🟡%d ⚪%d / 共%d ────────", green, yellow, skip, len(rows))
}

func sfJudge(t *testing.T, base, key string) RerankLLM {
	t.Helper()
	model := envOr("HEX_E2E_SF_JUDGE_MODEL", "deepseek-ai/DeepSeek-V3.2")
	return &httpChatLLM{base: base, key: key, model: model, client: &http.Client{Timeout: 180 * time.Second}}
}

// ① 全 chat 模型：真实 RAG 会话流程（检索→grounded→无答案拒答→忠实性裁判）。
func TestRAGReal_AllChatModels(t *testing.T) {
	base, key := sfBaseKey(t)
	emb := requireE2E(t)
	models := splitCSV(envOr("HEX_E2E_SF_CHAT_MODELS",
		"Qwen/Qwen3.6-35B-A3B,deepseek-ai/DeepSeek-V3.2,zai-org/GLM-5.2"))

	// 检索与模型无关：用 bge-m3 入库 golden、预算两条上下文，复用于所有 chat 模型（省时）。
	cfg := coreCfgNoLLM()
	ingCtx, ingCancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer ingCancel()
	mgr := newRealManager(t, cfg, emb, nil)
	if err := IngestGolden(ingCtx, mgr); err != nil {
		t.Fatalf("ingest golden: %v", err)
	}
	const groundedQ = "植物怎么把太阳能变成养分并产生氧气？"
	const noAnsQ = "怎么申请美国旅游签证？需要哪些材料？"
	groundedCtx, _ := mgr.Query(ingCtx, groundedQ, 3)
	noAnsCtx, _ := mgr.Query(ingCtx, noAnsQ, 3)

	judge := sfJudge(t, base, key)
	httpc := &http.Client{Timeout: 180 * time.Second}
	var rows []modelResult
	refuseWords := []string{"没有相关", "没有", "未涵盖", "未提及", "无法", "不足", "抱歉", "未包含", "不包含"}

	for _, model := range models {
		t.Run(sanitizeModel(model), func(t *testing.T) {
			llm := &httpChatLLM{base: base, key: key, model: model, client: httpc}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			if _, err := llm.Complete(ctx, "回复:ok"); err != nil {
				rows = append(rows, modelResult{model, "⚪skip", "不可用：" + clip(err.Error(), 50)})
				t.Skipf("chat %s 不可用：%v", model, err)
			}

			// grounded 会话
			ans, err := llm.Complete(ctx, "你是知识库助手。仅依据下面【资料】回答；若资料未涵盖该问题，必须直说\"资料中没有相关信息\"，绝不编造。\n【资料】\n"+groundedCtx+"\n\n【问题】"+groundedQ)
			if err != nil {
				rows = append(rows, modelResult{model, "🟡", "grounded 调用失败：" + clip(err.Error(), 40)})
				t.Logf("[%s] grounded 调用失败：%v", model, err)
				return
			}
			grounded := strings.Contains(ans, "氧")

			// 无答案拒答（忠实性）
			ans2, err := llm.Complete(ctx, "你是知识库助手。仅依据下面【资料】回答；若资料未涵盖该问题，必须直说\"资料中没有相关信息\"，绝不编造。\n【资料】\n"+noAnsCtx+"\n\n【问题】"+noAnsQ)
			refused := false
			if err == nil {
				refused = containsAnyOf(ans2, refuseWords) && !containsAnyOf(ans2, []string{"DS-160", "面签预约", "I-20", "EVUS"})
			}

			// 固定强裁判给 grounded 答案打忠实性分
			fscore := -1.0
			if s, jerr := EvalFaithfulness(ctx, judge, FaithfulnessCase{Name: model, Question: groundedQ, Context: groundedCtx, Answer: ans}); jerr == nil {
				fscore = s.Faithfulness
			}

			status := "🟢"
			if !grounded || !refused {
				status = "🟡" // 模型能力（指令跟随/拒答）弱，非代码 bug
			}
			detail := fmt.Sprintf("grounded=%v 拒答=%v faithfulness=%.2f | ans=%q", grounded, refused, fscore, clip(ans, 36))
			rows = append(rows, modelResult{model, status, detail})
			t.Logf("[%s] %s", model, detail)
		})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].model < rows[j].model })
	logModelTable(t, "全 chat 模型 RAG 会话覆盖", rows)
	avail := 0
	greenN := 0
	for _, r := range rows {
		if r.status != "⚪skip" {
			avail++
		}
		if strings.HasPrefix(r.status, "🟢") {
			greenN++
		}
	}
	if avail == 0 {
		t.Skip("无可用 chat 模型")
	}
	// 系统性护栏：可用模型里至少半数能完成 grounded RAG 会话（防 RAG 会话流水线整体断裂）。
	if float64(greenN)/float64(avail) < 0.5 {
		t.Errorf("全 chat 覆盖：仅 %d/%d 模型完成 grounded RAG 会话（<50%%，疑似流水线系统性断裂）", greenN, avail)
	}
}

// ② 全 reranker 模型：cross-encoder 把跨语种难例（无重排会被虚假 BM25 带偏）精排到 rank-1。
func TestRAGReal_AllRerankers(t *testing.T) {
	base, key := sfBaseKey(t)
	emb := requireE2E(t)
	models := splitCSV(envOr("HEX_E2E_SF_RERANK_MODELS", "BAAI/bge-reranker-v2-m3"))
	rerankBase := strings.TrimSuffix(strings.TrimSuffix(base, "/"), "/v1")

	const q = "How do plants use sunlight to produce food and release oxygen?" // 跨语种 EN→ZH 难例
	var rows []modelResult
	for _, model := range models {
		t.Run(sanitizeModel(model), func(t *testing.T) {
			cfg := DefaultHybridConfig()
			cfg.ExpandEnabled, cfg.ContextualEnabled = false, false
			cfg.RerankEnabled = true
			rr := reranker.NewCohereReranker(key,
				reranker.WithCohereBaseURL(rerankBase),
				reranker.WithCohereModel(model),
				reranker.WithCohereTopK(cfg.CandidateK))
			db := newRerankMgr(t, cfg, emb, rr)
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			defer cancel()
			if err := IngestGolden(ctx, db); err != nil {
				t.Fatalf("ingest: %v", err)
			}
			start := time.Now()
			hits, err := db.Search(ctx, q, 3)
			if err != nil {
				rows = append(rows, modelResult{model, "⚪skip", "不可用：" + clip(err.Error(), 50)})
				t.Skipf("reranker %s 不可用：%v", model, err)
			}
			top := ""
			if len(hits) > 0 {
				top = hits[0].DocTitle
			}
			ok := top == "光合作用"
			status := "🟢"
			if !ok {
				status = "🟡"
			}
			detail := fmt.Sprintf("跨语种 top1=%q（期望 光合作用）耗时=%v", top, time.Since(start).Round(time.Millisecond))
			rows = append(rows, modelResult{model, status, detail})
			t.Logf("[%s] %s", model, detail)
		})
	}
	logModelTable(t, "全 reranker 模型跨语种精排覆盖", rows)
}

// newRerankMgr 同 newRealManager 但注入专用 cross-encoder 重排器。
func newRerankMgr(t *testing.T, cfg HybridConfig, emb interface {
	Embed(context.Context, []string) ([][]float32, error)
	EmbedOne(context.Context, string) ([]float32, error)
	Dimension() int
}, rr reranker.Reranker) *Manager {
	t.Helper()
	m := newRealManager(t, cfg, emb, nil)
	WithDocReranker(rr)(m)
	return m
}

// ③ 全 vision 模型：图片 caption→入库→按色跨模态召回（真实"上传图片"用户流程）。
func TestRAGReal_AllVision(t *testing.T) {
	base, key := sfBaseKey(t)
	emb := requireE2E(t)
	models := splitCSV(envOr("HEX_E2E_SF_VL_MODELS", "Qwen/Qwen3-VL-8B-Instruct"))

	red := newCanvas(160, 160)
	fillCircle(red, 80, 80, 64, color.RGBA{255, 0, 0, 255})
	img := pngBytes(t, red)

	var rows []modelResult
	for _, model := range models {
		t.Run(sanitizeModel(model), func(t *testing.T) {
			cap := CaptionerFunc(func(ctx context.Context, im []byte, mime string) (string, error) {
				return vlmCaption(ctx, base, key, model, im, mime)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			caption, err := cap.Caption(ctx, img, "image/png")
			if err != nil {
				rows = append(rows, modelResult{model, "⚪skip", "不可用：" + clip(err.Error(), 50)})
				t.Skipf("vision %s 不可用：%v", model, err)
			}
			mgr := newImageRealMgr(t, emb, cap)
			if _, aerr := mgr.AddImageDocument(ctx, "红图", img, "image/png", "red.png"); aerr != nil {
				rows = append(rows, modelResult{model, "🟡", "入库失败：" + clip(aerr.Error(), 40)})
				return
			}
			colorOK := strings.Contains(caption, "红")
			hits, _ := mgr.Search(ctx, "哪张图片是红色的？", 3)
			retrieved := len(hits) > 0 && hits[0].DocTitle == "红图" && hits[0].Metadata["source_type"] == "image"
			status := "🟢"
			if !colorOK || !retrieved {
				status = "🟡"
			}
			detail := fmt.Sprintf("识别红色=%v 跨模态召回=%v | caption=%q", colorOK, retrieved, clip(caption, 40))
			rows = append(rows, modelResult{model, status, detail})
			t.Logf("[%s] %s", model, detail)
		})
	}
	logModelTable(t, "全 vision 模型 caption→召回覆盖", rows)
}
