package cron

// BUG-20260611 (install-test finding #5): glm-4-flash systematically answers
// web-scraping prompts with a ```python fenced SCRIPT instead of the required
// JSON spec — compile fails with "解析编译输出失败" on every attempt, so the
// user-facing "retry usually fixes it" advice is wrong for this prompt class.
//
// Contract: on a malformed compile output, the compiler performs exactly ONE
// self-correction round (feed the malformed output back with a reformat
// instruction). Success on the repair round compiles normally; failure keeps
// the original error shape (stable "解析编译输出失败" prefix for the
// stage/classify/humanize mappings).

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

// seqProvider returns scripted responses in order and records every request.
type seqProvider struct {
	responses []string
	usage     llm.Usage // per-call usage attached to every response
	reqs      []llm.CompletionRequest
}

func (f *seqProvider) Name() string { return "seq" }

func (f *seqProvider) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	f.reqs = append(f.reqs, req)
	idx := len(f.reqs) - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	return &llm.CompletionResponse{Content: f.responses[idx], Usage: f.usage}, nil
}

func (f *seqProvider) Stream(_ context.Context, _ llm.CompletionRequest) (*llm.Stream, error) {
	return nil, nil
}

func (f *seqProvider) Models() []llm.ModelInfo { return nil }

func (f *seqProvider) CountTokens(_ []llm.Message) (int, error) { return 0, nil }

const fencedPythonOutput = "```python\nimport requests\nprint('scrape')\n```"

const validSpecOutput = `{"runtime":"python3","script":"import json\nprint(json.dumps({\"ok\":true}))","deps":[],"timeout_s":60}`

func TestBug20260611_CompileSelfCorrectsMalformedOutput(t *testing.T) {
	seq := &seqProvider{responses: []string{fencedPythonOutput, validSpecOutput}}
	c := NewLLMCompilerStatic(seq, "test-model")

	spec, err := c.Compile(context.Background(), "抓取 https://example.com 首页标题保存为文本文件", CompileHints{})
	if err != nil {
		t.Fatalf("[BUG-20260611] compile should self-correct a fenced-script output, got: %v", err)
	}
	if len(seq.reqs) != 2 {
		t.Fatalf("want exactly 1 self-correction round (2 LLM calls), got %d", len(seq.reqs))
	}
	if spec.Script == "" || !strings.Contains(spec.Script, "json.dumps") {
		t.Errorf("repaired spec should carry the second response's script, got %q", spec.Script)
	}

	// The repair request must carry the malformed output back as assistant
	// context plus a reformat instruction.
	repair := seq.reqs[1]
	if len(repair.Messages) < 4 {
		t.Fatalf("repair request should extend the conversation (system+user+assistant+user), got %d messages", len(repair.Messages))
	}
	if !strings.Contains(repair.Messages[len(repair.Messages)-2].Content, "import requests") {
		t.Errorf("repair request should echo the malformed output as assistant turn")
	}
	if !strings.Contains(repair.Messages[len(repair.Messages)-1].Content, "JSON") {
		t.Errorf("repair instruction should restate the JSON-only requirement")
	}
}

func TestBug20260611_CompileSelfCorrectIsBounded(t *testing.T) {
	seq := &seqProvider{responses: []string{fencedPythonOutput, fencedPythonOutput}}
	c := NewLLMCompilerStatic(seq, "test-model")

	_, err := c.Compile(context.Background(), "抓取网页标题", CompileHints{})
	if err == nil {
		t.Fatal("two malformed outputs must still fail")
	}
	if len(seq.reqs) != 2 {
		t.Fatalf("self-correction must be bounded to exactly 1 extra call, got %d calls", len(seq.reqs))
	}
	if !strings.Contains(err.Error(), "解析编译输出失败") {
		t.Errorf("error must keep the stable 解析编译输出失败 prefix for stage/classify mappings, got %q", err.Error())
	}
}

// Review L3: when the repair round succeeds, CompileMeta must account for
// BOTH LLM calls (initial + repair), not only the repair call.
func TestBug20260611_SelfCorrectTokenAccountingSumsBothCalls(t *testing.T) {
	seq := &seqProvider{
		responses: []string{fencedPythonOutput, validSpecOutput},
		usage:     llm.Usage{PromptTokens: 100, CompletionTokens: 40},
	}
	c := NewLLMCompilerStatic(seq, "test-model")

	spec, err := c.Compile(context.Background(), "抓取网页标题", CompileHints{})
	if err != nil {
		t.Fatalf("compile should self-correct, got: %v", err)
	}
	if spec.Compiled.TokensIn != 200 || spec.Compiled.TokensOut != 80 {
		t.Errorf("token accounting must sum both calls, got in=%d out=%d (want 200/80)",
			spec.Compiled.TokensIn, spec.Compiled.TokensOut)
	}
}
