package cron

// AB re-baseline for F-3's compile-prompt change (http_post KB → kb_ingest):
// does steering the model to the in-process kb_ingest builtin regress first-shot
// compile success vs the pre-F-3 prompt? Controlled A/B — same prompt set, same
// model (glm-4-flash), two system-prompt arms, single-shot (no self-repair) so
// the measured delta is the PROMPT's effect, not the repair loop's.
//
// Real LLM calls — gated behind AB_COMPILE_TEST=1. Run:
//
//	GLM_API_KEY=... AB_COMPILE_TEST=1 go test ./cron/ -run TestAB_CompileBaseline -v -timeout 20m
//
// Re-baselines F-1 (pre-F-3: simple JSON 100% / complex HTML 25% / total 62%).

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
)

// abPrompts mirror F-1's three categories: mechanical JSON-API collectors (easy),
// HTML-scrape collectors (hard), and knowledge-base ingest tasks (the part F-3
// actually changed — most sensitive to the prompt edit).
var abPrompts = []struct{ category, prompt string }{
	{"json", "用 http_get 获取 https://api.github.com/repos/golang/go ，取 stargazers_count，整理成一句话 emit"},
	{"json", "GET https://api.coindesk.com/v1/bpi/currentprice.json ，json_decode 后取 USD 价格，emit 成功结果"},
	{"json", "抓取 https://hacker-news.firebaseio.com/v0/topstories.json 的前 10 个 id，emit 列表"},
	{"json", "GET https://api.exchangerate.host/latest?base=USD ，取 CNY 汇率 emit"},
	{"html", "采集 https://news.ycombinator.com/ 首页标题 TOP10，emit 标题列表"},
	{"html", "抓取 https://www.zhihu.com/hot 热榜标题，解析页面内嵌 JSON，emit TOP20"},
	{"html", "采集 https://github.com/trending/go 今日趋势仓库名，emit 列表"},
	{"html", "抓取 https://top.baidu.com/board?tab=realtime 百度热搜，解析 s-data，emit TOP20"},
	{"ingest", "采集 https://api.github.com/repos/golang/go 的 star 数写入知识库，source 用 cron-ab"},
	{"ingest", "每天抓 https://news.ycombinator.com/ TOP10 标题整理成 markdown 入知识库"},
	{"ingest", "GET https://hacker-news.firebaseio.com/v0/topstories.json 取前 5 条，写入知识库 source=cron-hn"},
	{"ingest", "采集百度热搜 TOP20 入知识库（Starlark 脚本）"},
}

// legacyCompilePrompt reconstructs the pre-F-3 system prompt by reversing the two
// F-3 edits on the current prompt. It asserts each reversal matched exactly, so
// the harness fails loudly if the prompt drifts rather than testing stale text.
func legacyCompilePrompt(t *testing.T, base string) string {
	t.Helper()
	p := buildCompileSystemPrompt(CompileHints{LocalAPIBase: base})

	kbLine := "- kb_ingest(title, content, source) -> {\"id\": str}  # 写入本地知识库（in-process，免鉴权）；**禁止** http_post 到 127.0.0.1/localhost（已被沙箱拦截）\n"
	if !strings.Contains(p, kbLine) {
		t.Fatal("legacy reconstruction: kb_ingest builtin line not found (prompt drifted)")
	}
	p = strings.Replace(p, kbLine, "", 1)

	newBlock := `
# 入知识库（in-process 内建，免鉴权）——需要入库时调用 kb_ingest，**禁止** http_post 到 127.0.0.1/localhost（已被沙箱 SSRF 拦截）：
    res = kb_ingest(title = "标题", content = "正文", source = "cron-xxx")
    # 成功返回 {"id": "<doc-id>"}；失败直接抛错（如知识库未启用）。入库成功后再 emit success。
`
	oldBlock := fmt.Sprintf(`
# 入知识库（沙箱可达，无需鉴权）——需要入库时在脚本里 http_post：
    payload = json_encode({"title": "...", "content": "...", "source": "cron-xxx"})
    post = http_post("%s/api/v1/knowledge/documents", body = payload, headers = {"Content-Type": "application/json"})
    if post["status"] < 200 or post["status"] >= 300:
        return {"status": "error", "error": "ingest non-2xx: %%d" %% post["status"]}
# 写入成功（2xx）后再 emit success。
`, base)
	if !strings.Contains(p, newBlock) {
		t.Fatal("legacy reconstruction: kb_ingest ingest block not found (prompt drifted)")
	}
	return strings.Replace(p, newBlock, oldBlock, 1)
}

func TestAB_CompileBaseline(t *testing.T) {
	if os.Getenv("AB_COMPILE_TEST") == "" {
		t.Skip("AB harness — set AB_COMPILE_TEST=1 + GLM_API_KEY to run")
	}
	key := os.Getenv("GLM_API_KEY")
	if key == "" {
		t.Fatal("GLM_API_KEY required")
	}
	baseURL := envOr("GLM_BASE_URL", "https://open.bigmodel.cn/api/paas/v4")
	model := envOr("GLM_MODEL", "glm-4-flash")
	localAPI := "http://127.0.0.1:16060"

	provider := hexagon.NewOpenAI(key, hexagon.OpenAIWithBaseURL(baseURL))
	newSys := buildCompileSystemPrompt(CompileHints{LocalAPIBase: localAPI})
	oldSys := legacyCompilePrompt(t, localAPI)

	arms := []struct{ name, sys string }{{"OLD(http_post)", oldSys}, {"NEW(kb_ingest)", newSys}}
	type tally struct{ total, ok, kbIngest, loopback int }
	results := map[string]map[string]*tally{} // arm -> category -> tally
	for _, a := range arms {
		results[a.name] = map[string]*tally{"json": {}, "html": {}, "ingest": {}}
	}

	zero := 0.0
	for _, a := range arms {
		for _, p := range abPrompts {
			tl := results[a.name][p.category]
			tl.total++
			resp, err := provider.Complete(context.Background(), llm.CompletionRequest{
				Model: model, Messages: llm.NewMessages(a.sys, p.prompt), MaxTokens: 4096, Temperature: &zero,
			})
			if err != nil {
				t.Logf("[%s/%s] LLM error: %v", a.name, p.category, err)
				continue
			}
			spec, perr := parseCompiledSpec(strings.TrimSpace(resp.Content))
			if perr != nil {
				continue // first-shot parse failure
			}
			normalizeCompiledSpec(spec)
			if verr := validateCompiledScript(spec); verr != nil {
				continue // first-shot validation failure
			}
			tl.ok++
			if strings.Contains(spec.Script, "kb_ingest") {
				tl.kbIngest++
			}
			if strings.Contains(spec.Script, "127.0.0.1") || strings.Contains(spec.Script, "localhost") {
				tl.loopback++
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n=== F-3 编译成功率 A/B（model=%s，单发无自纠）===\n", model)
	for _, a := range arms {
		var st, sok, ik, lb int
		fmt.Fprintf(&b, "\n[%s]\n", a.name)
		for _, cat := range []string{"json", "html", "ingest"} {
			tl := results[a.name][cat]
			rate := 0.0
			if tl.total > 0 {
				rate = float64(tl.ok) / float64(tl.total) * 100
			}
			fmt.Fprintf(&b, "  %-7s 成功 %d/%d (%.0f%%)  kb_ingest=%d  loopback=%d\n", cat, tl.ok, tl.total, rate, tl.kbIngest, tl.loopback)
			st += tl.total
			sok += tl.ok
			ik += tl.kbIngest
			lb += tl.loopback
		}
		fmt.Fprintf(&b, "  TOTAL   成功 %d/%d (%.0f%%)  kb_ingest=%d  loopback=%d\n", sok, st, float64(sok)/float64(st)*100, ik, lb)
	}
	t.Log(b.String())
	// Write the raw table for the AB report artifact.
	_ = os.WriteFile("/tmp/ab_compile_result.txt", []byte(b.String()), 0644)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
