package api

// BUG-20260611 (install-test finding #3): cron create failures were flattened
// into "参数不合法" — the LLM compiler occasionally emits non-JSON output, the
// parse error contains "invalid character …", and humanizeError's generic
// {"invalid" → "参数不合法"} catch-all hijacks it. Users are misled into
// thinking THEIR parameters were wrong, when the actual cause is a flaky
// model output that a simple retry (or model switch) would fix.
//
// Contract: compile-pipeline failures must surface a cause-specific message
// and a pipeline stage, never the generic parameter-blame text.

import (
	"fmt"
	"strings"
	"testing"
)

func TestBug20260611_CompileParseErrorNotFlattened(t *testing.T) {
	// Exact shape produced by cron.AddJobFromPromptWithProgress wrapping
	// llm_compiler's parse failure on malformed model output.
	err := fmt.Errorf("编译失败: 解析编译输出失败: invalid character 'h' looking for beginning of value —— LLM 原文:\nhello not json")

	got := humanizeError(err)
	if got == "参数不合法" {
		t.Fatalf("[BUG-20260611] compile parse error flattened to parameter-blame text: %q", got)
	}
	if !strings.Contains(got, "脚本") && !strings.Contains(got, "重试") {
		t.Errorf("message should tell the user it is a script-generation issue and suggest retry, got %q", got)
	}
}

func TestBug20260611_ScriptValidationErrorNotFlattened(t *testing.T) {
	err := fmt.Errorf("编译失败: 脚本校验失败: 脚本含禁用调用: os.system —— LLM 原文:\n...")

	got := humanizeError(err)
	if got == "参数不合法" {
		t.Fatalf("[BUG-20260611] validation error flattened to parameter-blame text: %q", got)
	}
	if !strings.Contains(got, "安全校验") {
		t.Errorf("message should mention the security validation stage, got %q", got)
	}
}

// Review finding: the error message embeds the raw LLM output ("LLM 原文"),
// and generated scripts routinely contain strings like "429" or "404 Not
// Found" (retry/error-handling code). Generic HTTP-status patterns earlier in
// the humanize list must not hijack the compile-specific message.
func TestBug20260611_EmbeddedRawOutputDoesNotHijackHumanize(t *testing.T) {
	err := fmt.Errorf("编译失败: 解析编译输出失败: 非合规 JSON: invalid character 'i' —— LLM 原文:\n" +
		"```python\nif resp.status_code == 429:\n    retry()\nelif resp.status_code == 404:\n    print('404 Not Found')\n```")

	got := humanizeError(err)
	if !strings.Contains(got, "脚本格式异常") {
		t.Fatalf("[BUG-20260611] compile-specific message hijacked by a generic pattern, got %q", got)
	}
}

func TestBug20260611_StageOfCreateError(t *testing.T) {
	cases := []struct {
		err   string
		stage string
	}{
		{"编译失败: 解析编译输出失败: invalid character 'h'", "calling_llm"},
		{"编译失败: LLM 编译失败: openai api error: 429", "calling_llm"},
		{"编译失败: 脚本校验失败: 脚本含禁用调用: os.system", "validating"},
		{"无效的调度表达式: bad", "persisting"},
		{"保存任务失败: db locked", "persisting"},
		{"something else", ""},
	}
	for _, c := range cases {
		if got := stageOfCreateError(fmt.Errorf("%s", c.err)); got != c.stage {
			t.Errorf("stageOfCreateError(%q) = %q, want %q", c.err, got, c.stage)
		}
	}
}
