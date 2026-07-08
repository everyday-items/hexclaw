// bug_cron_unified_test 守卫 v0.4.x cron unified arch（D1.2 / D1.3 / D2.3）。
//
// RED 场景（修复前都失败）：
//   - 旧 cron handler 暴露 Anthropic 400 + tool_use_id 给用户
//   - 30/user 配额没有，K12 家长可 spam 创建几千个
//   - update/list/run 各自单独 endpoint，前端 4 个一组心智负担大
//   - idempotency 缺失，网络抖动重传 = 重复创建
//
// GREEN 守护：
//   - humanizeError 永不漏 tool_use_id / api error 文本
//   - 同 idempotency_key 第二次请求返同 status + body 且加 X-Idempotent-Replay
//   - 7 action 全在 POST /api/v1/cronjob
//   - action=create 在 jobs 达 CronQuotaPerUser 时返 429 + CRON_QUOTA_EXCEEDED
package api

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestHumanizeError_ToolUseIDNeverLeaks 守卫 D2.3：tool_use_id 必须被翻译成中文，
// 不能原样漏给 K12 家长。修复前 cron 400 直接抖 Anthropic 错文。
func TestHumanizeError_ToolUseIDNeverLeaks(t *testing.T) {
	cases := []string{
		"400 Bad Request, body: {\"error\":{\"message\":\"unexpected tool_use_id found in tool_result blocks\"}}",
		"runtime stream 失败: llm complete: openai api error: 400 tool_use_id mismatch",
		"toolu_01TBZS... not found",
	}
	for _, in := range cases {
		out := humanizeError(errors.New(in))
		if out == "" {
			t.Errorf("input=%q → empty output", in)
		}
		if strings.Contains(out, "tool_use_id") {
			t.Errorf("tool_use_id LEAKED in: %q", out)
		}
		if strings.Contains(out, "tool_call_id") {
			t.Errorf("tool_call_id LEAKED in: %q", out)
		}
		if strings.Contains(out, "toolu_") {
			t.Errorf("Anthropic tool id prefix LEAKED in: %q", out)
		}
	}
}

// TestHumanizeError_KnownPatternsMapped 验证已知错误模式正确翻译。
func TestHumanizeError_KnownPatternsMapped(t *testing.T) {
	cases := []struct {
		in        string
		expectSub string
	}{
		{"context deadline exceeded", "超时"},
		{"connection refused", "服务暂时不可用"},
		{"429 too many requests", "请求太频繁"},
		{"sql: no rows in result set", "找不到对应记录"},
		{"unique constraint failed: cron_jobs.name", "已存在同名"},
	}
	for _, tc := range cases {
		out := humanizeError(errors.New(tc.in))
		if !strings.Contains(out, tc.expectSub) {
			t.Errorf("input=%q expected to contain %q, got %q", tc.in, tc.expectSub, out)
		}
	}
}

// TestHumanizeError_UnknownTruncated 未知错误 fallback 不暴露 stack + 截断长文本。
func TestHumanizeError_UnknownTruncated(t *testing.T) {
	long := strings.Repeat("abc", 300) // 900 chars
	out := humanizeError(errors.New(long))
	if len(out) > 220 {
		t.Errorf("long error not truncated: %d chars", len(out))
	}
	// 多行 stack 不应漏出
	stack := "panic: runtime error\n\tgoroutine 1 [running]:\n\t/usr/local/go/src/main.go:42 +0x1a"
	out2 := humanizeError(errors.New(stack))
	if strings.Contains(out2, "goroutine") || strings.Contains(out2, ".go:") {
		t.Errorf("stack trace LEAKED: %q", out2)
	}
}

// TestIdempCache_ReplayHits 同 key 二次写入返同结果。
func TestIdempCache_ReplayHits(t *testing.T) {
	c := &idempCache{entries: make(map[string]idempEntry)}
	c.put("user1::abc", 200, []byte(`{"ok":true}`))

	status, body, hit := c.get("user1::abc")
	if !hit {
		t.Fatal("应命中")
	}
	if status != 200 || string(body) != `{"ok":true}` {
		t.Errorf("got status=%d body=%s", status, string(body))
	}

	// 不同 user 隔离
	_, _, hit2 := c.get("user2::abc")
	if hit2 {
		t.Error("跨 user 不应命中")
	}
}

// TestIdempKey_CrossUserIsolation 确保 user-A 的 idempotency_key 不会泄露给 user-B。
func TestIdempKey_CrossUserIsolation(t *testing.T) {
	if idempKey("a", "k") == idempKey("b", "k") {
		t.Error("cross-user idempotency key 必须不同")
	}
}

// TestIsMutationAction 守卫 list 不走 idempotency cache（避免缓存陈旧列表）。
func TestIsMutationAction(t *testing.T) {
	if isMutationAction("list") {
		t.Error("list 不应被认作 mutation")
	}
	mustMutate := []string{"create", "update", "remove", "pause", "resume", "run"}
	for _, a := range mustMutate {
		if !isMutationAction(a) {
			t.Errorf("%q 应为 mutation", a)
		}
	}
}

// TestCronQuotaConstantSentinel 配额常量值必须保持 30（决策 B）。
// 修改此常量需同步前端 useCronCompileLabel 配额预警 + UI 字样。
func TestCronQuotaConstantSentinel(t *testing.T) {
	if CronQuotaPerUser != 30 {
		t.Fatalf("配额常量被改：want 30, got %d — 同步前端 + 文档", CronQuotaPerUser)
	}
}

// TestCronErrCode_Carries 验证 cronErr 类型能携带 code。
func TestCronErrCode_Carries(t *testing.T) {
	err := &cronErr{code: CodeCronQuotaExceeded, msg: "已达上限"}
	if errToCode(err) != CodeCronQuotaExceeded {
		t.Errorf("errToCode 应识别 cronErr")
	}
	wrapped := fmt.Errorf("outer: %w", err)
	if errToCode(wrapped) != CodeCronQuotaExceeded {
		t.Errorf("errToCode 应通过 errors.As 解包")
	}
}

// TestItoa 守卫内联 cronItoa 行为（避免 panic / 边界错）。
func TestItoa(t *testing.T) {
	cases := map[int]string{0: "0", 1: "1", 30: "30", 100: "100", -5: "-5"}
	for in, want := range cases {
		if got := cronItoa(in); got != want {
			t.Errorf("cronItoa(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestBugSSE_UpstreamLeakNeverInUserError 守卫 2026-05-25 装机暴露的 D2.3 漏点：
//
// 复现：用户在 chat 创建任务，上游 (apimart / openrouter) 返 500 + request_id，
// 旧 handleAddCronJobSSE 把 err.Error() 原样塞 SSE error event → 用户看到
// "openai api error: 500 Internal Server Error, body: {...request id: ...}"
//
// 修复：handleAddCronJobSSE / handleAddCronJobJSON 都过 humanizeError。
// 本测试断言 humanizeError 对此场景翻译为友好中文。
func TestBugSSE_UpstreamLeakNeverInUserError(t *testing.T) {
	// 原始错误文本（取自装机截图）
	raw := `编译失败: LLM 编译失败: llmcall: 调用失败 (attempts=3): openai api error: 500 Internal Server Error, body: {"error":{"message":"Please wait and try again later. Thank you for your patience! (request id: 20260525204607555785074rXZJmvXQ)","type":"apimart_error","param":"","code":"get_channel_failed"}}`
	out := humanizeError(makeBugErr(raw))

	// 不能漏的关键 token
	bannedTokens := []string{
		"openai api error",
		"500 Internal Server Error",
		"request id:",
		"20260525204607555785074", // 真实 request_id 不能漏给用户
		"apimart_error",
		"get_channel_failed",
		`"error":{`,
		`"type":`,
		"attempts=3",
		"llmcall",
	}
	for _, banned := range bannedTokens {
		if strings.Contains(out, banned) {
			t.Errorf("D2.3 漏点：humanize 输出仍含 %q，全文：%q", banned, out)
		}
	}

	// 必须落在友好提示
	if !strings.Contains(out, "服务暂时出错") && !strings.Contains(out, "请稍后") {
		t.Errorf("D2.3 漏点：humanize 应翻成 500 服务暂时出错类，实际：%q", out)
	}
}

func makeBugErr(s string) error {
	return &bugErr{msg: s}
}

type bugErr struct{ msg string }

func (e *bugErr) Error() string { return e.msg }

// TestBugOllamaModelMissing_FriendlyHint 守卫 2026-05-27 装机暴露的 D2.3 三次漏点：
//
// 复现：sidecar 启动时锁定 cron compiler model = gemma4:e4b，但 Ollama 本地
// 根本没下载这个模型 → llmcall 重试 3 次 × 60s timeout → 抖出
// 'Post "http://localhost:11434/v1/chat/completions": net/http: timeout awaiting response headers'
//
// 修复：humanize patterns 加 "net/http: timeout awaiting response headers" + "model '"
// → 明确指向"模型未下载/换 chat 模型"。
func TestBugOllamaModelMissing_FriendlyHint(t *testing.T) {
	raw := `编译失败: LLM 编译失败: llmcall: 调用失败 (attempts=3): Post "http://localhost:11434/v1/chat/completions": net/http: timeout awaiting response headers`
	out := humanizeError(makeBugErr(raw))
	if !strings.Contains(out, "未下载") && !strings.Contains(out, "改选") {
		t.Errorf("应提示'未下载/改选'，实际：%q", out)
	}
	for _, banned := range []string{"localhost:11434", "net/http", "attempts=", "Post \"", "llmcall:"} {
		if strings.Contains(out, banned) {
			t.Errorf("不应漏 %q，全文：%q", banned, out)
		}
	}
}

// TestBugCogview_NonChatModel_FriendlyHint 守卫装机暴露的 D2.3 二次漏点：
//
// 复现：cron compiler 误选智谱 cogview-4 图像模型，上游回 content=array，
// openai client 报 "json: cannot unmarshal array into Go struct field .choices.message.content"。
// 旧版翻成「数据格式不对」对用户毫无指导意义。
//
// 修复：humanize patterns 加 "cannot unmarshal array into Go struct field
// .choices.message.content" → 明确指向"改选 chat 类模型"。
func TestBugCogview_NonChatModel_FriendlyHint(t *testing.T) {
	raw := `编译失败: LLM 编译失败: llmcall: 调用失败: json: cannot unmarshal array into Go struct field .choices.message.content of type string`
	out := humanizeError(makeBugErr(raw))
	if !strings.Contains(out, "对话补全") || !strings.Contains(out, "chat") {
		t.Errorf("应明确指向'chat 类模型'，实际：%q", out)
	}
	for _, banned := range []string{"unmarshal", "Go struct field", ".choices.", "llmcall"} {
		if strings.Contains(out, banned) {
			t.Errorf("不应漏 %q，全文：%q", banned, out)
		}
	}
}

// TestStripJSONFence 守卫围栏剥离（LLM 偶尔包 ```json …```）。
func TestStripJSONFence(t *testing.T) {
	cases := map[string]string{
		"{}":                      "{}",
		"```json\n{\"a\":1}\n```": `{"a":1}`,
		"```\n{\"a\":1}\n```":     `{"a":1}`,
		"  {\"a\":1}\n":           `{"a":1}`,
	}
	for in, want := range cases {
		if got := stripJSONFence(in); strings.TrimSpace(got) != strings.TrimSpace(want) {
			t.Errorf("stripJSONFence(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFirstLine 守卫任务名截断按 rune 不按 byte。
func TestFirstLine(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"采集新闻头条加入知识库", 6, "采集新闻头条"},
		{"hello world", 5, "hello"},
		{"", 10, "定时任务"},
		{"line1\nline2", 10, "line1"},
	}
	for _, tc := range cases {
		got := firstLine(tc.in, tc.n)
		if got != tc.want {
			t.Errorf("firstLine(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
