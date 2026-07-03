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

func TestBug20260702_BaiduHotsearchGenericPromptStillUsesLLM(t *testing.T) {
	fp := &fakeProvider{
		resp: &llm.CompletionResponse{Content: validSpecOutput},
	}
	c := NewLLMCompilerStatic(fp, "test-model")
	if _, err := c.Compile(context.Background(), "每天采集百度热搜", CompileHints{}); err != nil {
		t.Fatalf("generic prompt should still use LLM path: %v", err)
	}
	if fp.last == nil {
		t.Fatal("generic prompt unexpectedly bypassed provider")
	}
}
