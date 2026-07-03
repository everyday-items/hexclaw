package cron

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

func TestBug20260702_BaiduHotsearchKnowledgePromptUsesTemplate(t *testing.T) {
	c := NewLLMCompilerStatic(nil, "")
	spec, err := c.Compile(context.Background(), "每天早上9点采集百度热搜榜并写入知识库", CompileHints{})
	if err != nil {
		t.Fatalf("template compile should not need LLM provider: %v", err)
	}
	if spec.Runtime != RuntimeStarlark {
		t.Fatalf("runtime=%q, want starlark", spec.Runtime)
	}
	if spec.Compiled.Model != "builtin-template" {
		t.Fatalf("model=%q, want builtin-template", spec.Compiled.Model)
	}
	if !strings.Contains(spec.Script, "s-data") || !strings.Contains(spec.Script, "kb_ingest") {
		t.Fatalf("template should be the archived Baidu hotsearch collector, got:\n%s", spec.Script)
	}
	if err := NewStarlarkEngine().Validate(spec.Script); err != nil {
		t.Fatalf("template must pass Starlark validation: %v", err)
	}
}

// 契约反转（BUG-20260704）：本测试原断言「泛化采集 prompt（无知识库意图）仍走 LLM」。
// 实机证据推翻了该契约——「百度热搜 TOP20 采集」走 LLM 后编译出臆造解析路径的脚本，
// 每 tick 报「no items found in data structure」，自愈重编译同样盲猜、永不收敛
// （E2E 实机 run 历史：error×3 → healed → error）。已知数据源的采集 prompt 现在
// 命中 collect-only 确定性模板：零 token、路径经真机验证、不静默写知识库。
func TestBug20260704_BaiduHotsearchGenericPromptUsesCollectTemplate(t *testing.T) {
	fp := &fakeProvider{
		resp: &llm.CompletionResponse{Content: validSpecOutput},
	}
	c := NewLLMCompilerStatic(fp, "test-model")
	spec, err := c.Compile(context.Background(), "每天采集百度热搜", CompileHints{})
	if err != nil {
		t.Fatalf("collect template compile should not need LLM provider: %v", err)
	}
	if fp.last != nil {
		t.Fatal("[BUG-20260704] 采集 prompt 不得再走 LLM 盲猜路径（真实页面结构 LLM 不可知）")
	}
	if spec.Compiled.Model != "builtin-template" {
		t.Fatalf("model=%q, want builtin-template", spec.Compiled.Model)
	}
	if !strings.Contains(spec.Script, "s-data") || strings.Contains(spec.Script, "kb_ingest(") {
		t.Fatalf("应为 collect-only 模板（走 s-data 稳定路径、不写知识库），got:\n%s", spec.Script)
	}
}
