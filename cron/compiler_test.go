package cron

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/streamx"
	"github.com/hexagon-codes/hexclaw/egress"
)

// fakeProvider 实现 hexagon.Provider（llm.Provider 接口），仅返回预设 Content / err。
type fakeProvider struct {
	resp *llm.CompletionResponse
	err  error
	last *llm.CompletionRequest
	ctx  context.Context
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	f.ctx = ctx
	f.last = &req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func (f *fakeProvider) Stream(_ context.Context, _ llm.CompletionRequest) (*llm.Stream, error) {
	return nil, nil
}

func (f *fakeProvider) Models() []llm.ModelInfo { return nil }

func (f *fakeProvider) CountTokens(_ []llm.Message) (int, error) { return 0, nil }

// validScript 在校验链中能通过的最小脚本：
// - 合规 Python 语法
// - 不使用禁用调用
// - 输出契约 print(json.dumps(...))
const validScript = `import json
def main():
    return {"items": [1,2,3]}
print(json.dumps({"status": "success", "data": main()}))
`

// ── 2.1 接口空 prompt 立即拒收 ─────────────────────────

func TestLLMCompiler_RejectsEmptyPrompt(t *testing.T) {
	c := NewLLMCompilerStatic(&fakeProvider{}, "")
	_, err := c.Compile(context.Background(), "  ", CompileHints{})
	if err == nil || !strings.Contains(err.Error(), "prompt 不能为空") {
		t.Fatalf("空 prompt 应被拒收，实际 err=%v", err)
	}
}

// ── 2.2 parseCompiledSpec 解析常规 + markdown 围栏两种形态 ──

func TestParseCompiledSpec_ExtractsRuntimeAndScript(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "纯 JSON",
			raw:  `{"runtime":"python3","script":"print('hi')\n","deps":[],"timeout_s":60}`,
		},
		{
			name: "markdown 围栏",
			raw:  "```json\n{\"runtime\":\"python3\",\"script\":\"print('hi')\\n\",\"deps\":[\"requests\"],\"timeout_s\":120}\n```",
		},
		{
			name: "首行无 json 标识",
			raw:  "```\n{\"runtime\":\"python3\",\"script\":\"print('hi')\\n\"}\n```",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := parseCompiledSpec(tc.raw)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if spec.Runtime != "python3" {
				t.Errorf("Runtime: %q", spec.Runtime)
			}
			if !strings.Contains(spec.Script, "print('hi')") {
				t.Errorf("Script: %q", spec.Script)
			}
		})
	}
}

func TestParseCompiledSpec_RejectsEmptyScript(t *testing.T) {
	_, err := parseCompiledSpec(`{"runtime":"python3","script":""}`)
	if err == nil || !strings.Contains(err.Error(), "script 字段为空") {
		t.Fatalf("空 script 应被拒收，err=%v", err)
	}
}

// ── 2.3 ~ 2.5 校验链 ─────────────────────────────────

func TestValidator_RejectsSyntaxError(t *testing.T) {
	err := validatePythonSyntax("def broken(:\n    pass\n")
	if err == nil {
		t.Fatal("语法错的脚本应被拒")
	}
}

func TestValidator_RejectsSubprocessImport(t *testing.T) {
	bad := `import subprocess
print("evil")
`
	err := validateNoForbiddenImports(bad)
	if err == nil || !strings.Contains(err.Error(), "subprocess") {
		t.Fatalf("import subprocess 应被拒收，err=%v", err)
	}

	bad2 := `from subprocess import Popen
print("evil")
`
	err = validateNoForbiddenImports(bad2)
	if err == nil || !strings.Contains(err.Error(), "subprocess") {
		t.Fatalf("from subprocess 应被拒收，err=%v", err)
	}

	bad3 := `import os
os.system("rm -rf /")
`
	err = validateNoForbiddenImports(bad3)
	if err == nil || !strings.Contains(err.Error(), "system") {
		t.Fatalf("os.system 应被拒收，err=%v", err)
	}

	bad4 := `eval("1+1")
`
	err = validateNoForbiddenImports(bad4)
	if err == nil || !strings.Contains(err.Error(), "eval") {
		t.Fatalf("eval 应被拒收，err=%v", err)
	}

	// 合规脚本不应误伤
	if err := validateNoForbiddenImports(validScript); err != nil {
		t.Errorf("合规脚本被误伤: %v", err)
	}
}

func TestValidator_RejectsMissingJSONOutput(t *testing.T) {
	if err := validateOutputContract("print('hello')\n"); err == nil {
		t.Fatal("缺 print(json.dumps(...)) 应被拒")
	}
	if err := validateOutputContract(validScript); err != nil {
		t.Errorf("合规脚本被误伤: %v", err)
	}
}

// ── 2.6 端到端（fake provider 注入）─────────────────

func TestLLMCompiler_EndToEnd_WithFakeProvider(t *testing.T) {
	fp := &fakeProvider{
		resp: &llm.CompletionResponse{
			Content: `{"runtime":"python3","script":"` + escapeJSON(validScript) + `","deps":["requests"],"timeout_s":90}`,
			Usage:   streamx.Usage{PromptTokens: 123, CompletionTokens: 456},
		},
	}
	c := NewLLMCompilerStatic(fp, "claude-haiku-4-5")
	spec, err := c.Compile(context.Background(), "采集微博 TOP10 并入库", CompileHints{
		AvailableSkills: []string{"web-search", "knowledge-write"},
		LocalAPIBase:    "http://127.0.0.1:16060",
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if spec.Runtime != "python3" {
		t.Errorf("Runtime: %q", spec.Runtime)
	}
	if !strings.Contains(spec.Script, "print(json.dumps") {
		t.Errorf("Script 缺输出契约: %q", spec.Script)
	}
	// BUG-20260613 (M5): deps are forced empty at the compile boundary — the
	// sandbox is stdlib-only, so a third-party dep like "requests" is
	// unsatisfiable and would only drive the executor's dead pip path.
	if len(spec.Deps) != 0 {
		t.Errorf("Deps must be forced empty (stdlib-only sandbox), got %v", spec.Deps)
	}
	if spec.TimeoutSec != 90 {
		t.Errorf("TimeoutSec: %d", spec.TimeoutSec)
	}
	if spec.Compiled.Model != "claude-haiku-4-5" {
		t.Errorf("Compiled.Model: %s", spec.Compiled.Model)
	}
	if spec.Compiled.TokensIn != 123 || spec.Compiled.TokensOut != 456 {
		t.Errorf("Compiled tokens: in=%d out=%d", spec.Compiled.TokensIn, spec.Compiled.TokensOut)
	}
	if spec.Compiled.Hash == "" || len(spec.Compiled.Hash) != 64 {
		t.Errorf("Compiled.Hash 应为 sha256 hex: %q", spec.Compiled.Hash)
	}
	// F-3: LocalAPIBase no longer appears verbatim — ingest uses the in-process
	// kb_ingest builtin, not an HTTP POST to the local API. The hint now gates
	// whether the ingest guidance section is emitted.
	if fp.last == nil || !strings.Contains(fp.last.Messages[0].Content, "入知识库") {
		t.Error("system prompt 应在 LocalAPIBase 存在时包含入知识库指引")
	}
	if fp.last == nil || !strings.Contains(fp.last.Messages[0].Content, "web-search") {
		t.Error("system prompt 应列出 AvailableSkills")
	}
}

func TestLLMCompiler_StampsAutomationEgressEnvelope(t *testing.T) {
	fp := &fakeProvider{resp: &llm.CompletionResponse{
		Content: `{"runtime":"python3","script":"` + escapeJSON(validScript) + `","timeout_s":30}`,
	}}
	if _, err := NewLLMCompilerStatic(fp, "test").Compile(context.Background(), "do x", CompileHints{}); err != nil {
		t.Fatal(err)
	}
	requests, ok := egress.RequestsFromContext(fp.ctx)
	if !ok || len(requests) != 1 || requests[0].Purpose != egress.PurposeAutomationBuild || requests[0].DataClass != egress.ClassGeneral {
		t.Fatalf("compiler egress envelope=%+v ok=%v", requests, ok)
	}
}

func TestLLMCompiler_DisablesThinkingForStructuredCompile(t *testing.T) {
	fp := &fakeProvider{
		resp: &llm.CompletionResponse{
			Content: `{"runtime":"python3","script":"` + escapeJSON(validScript) + `","timeout_s":60}`,
		},
	}
	c := NewLLMCompilerStatic(fp, "Qwen/Qwen3.6-35B-A3B")
	// 百度热搜采集类 prompt 自 BUG-20260704 起命中确定性模板不走 LLM，
	// 本测试对象是 LLM 请求的 thinking 元数据，换用非模板数据源。
	if _, err := c.Compile(context.Background(), "每天采集微博热搜", CompileHints{}); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if fp.last == nil {
		t.Fatal("provider 未收到请求")
	}
	if got := fp.last.Metadata["thinking"]; got != "off" {
		t.Fatalf("compile request thinking metadata = %#v, want off", got)
	}
	if got := fp.last.Metadata["enable_thinking"]; got != false {
		t.Fatalf("compile request enable_thinking metadata = %#v, want false", got)
	}
	if fp.last.ResponseFormat == nil || fp.last.ResponseFormat.Type != "json_object" {
		t.Fatalf("compile request response_format = %#v, want json_object", fp.last.ResponseFormat)
	}
	if len(fp.last.Messages) == 0 || !strings.Contains(fp.last.Messages[0].Content, "/no_think") {
		t.Fatalf("compile system prompt must include /no_think, got %#v", fp.last.Messages)
	}
}

func TestLLMCompiler_RejectsLLMOutputWithSubprocess(t *testing.T) {
	maliciousScript := `import subprocess
subprocess.run(["rm","-rf","/"])
print(json.dumps({"status":"success"}))
`
	fp := &fakeProvider{
		resp: &llm.CompletionResponse{
			Content: `{"runtime":"python3","script":"` + escapeJSON(maliciousScript) + `"}`,
		},
	}
	c := NewLLMCompilerStatic(fp, "")
	_, err := c.Compile(context.Background(), "做点坏事", CompileHints{})
	if err == nil || !strings.Contains(err.Error(), "subprocess") {
		t.Fatalf("含 subprocess 的 LLM 产物应被拒收，err=%v", err)
	}
}

// escapeJSON 把脚本字面量转义为可直接拼入 JSON 字符串的形式。
func escapeJSON(s string) string {
	r := strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"\n", "\\n",
		"\t", "\\t",
		"\r", "\\r",
	)
	return r.Replace(s)
}
