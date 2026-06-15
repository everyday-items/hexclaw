package cron

// BUG-20260615 (P2): the compiler now emits Starlark (pure-Go, zero-dep, no
// python) instead of python. These lock the full compile path for a Starlark
// spec — parse -> normalize (runtime defaults to starlark) -> validate via the
// StarlarkEngine that will actually run it — plus the system prompt steering the
// model to Starlark builtins with a golden example.

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

const validStarlark = `def run():
    return {"status": "success", "data": {"n": 3}}
emit(run())
`

func TestBug20260615_CompileStarlark_EndToEnd(t *testing.T) {
	fp := &fakeProvider{
		resp: &llm.CompletionResponse{
			Content: `{"runtime":"starlark","script":"` + escapeJSON(validStarlark) + `","timeout_s":45}`,
		},
	}
	c := NewLLMCompilerStatic(fp, "glm-4-flash")
	spec, err := c.Compile(context.Background(), "采集某榜单入库", CompileHints{LocalAPIBase: "http://127.0.0.1:16060"})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if spec.Runtime != RuntimeStarlark {
		t.Errorf("runtime = %q, want starlark", spec.Runtime)
	}
	if !strings.Contains(spec.Script, "emit(") {
		t.Errorf("compiled script must keep the emit contract: %q", spec.Script)
	}
	// The compiled Starlark must pass the engine that will run it.
	if err := NewStarlarkEngine().Validate(spec.Script); err != nil {
		t.Errorf("compiled starlark must pass StarlarkEngine.Validate: %v", err)
	}
	if spec.TimeoutSec != 45 {
		t.Errorf("timeout = %d, want 45", spec.TimeoutSec)
	}
}

func TestBug20260615_CompilePrompt_IsStarlark(t *testing.T) {
	p := buildCompileSystemPrompt(CompileHints{LocalAPIBase: "http://127.0.0.1:16060"})
	for _, must := range []string{"starlark", "http_get", "json_decode", "emit(", "def run():"} {
		if !strings.Contains(p, must) {
			t.Errorf("starlark compile prompt must contain %q", must)
		}
	}
	if strings.Contains(p, "urllib.request") || strings.Contains(p, "print(json.dumps") {
		t.Error("starlark prompt must not carry the python contract (urllib/print)")
	}
}

func TestBug20260615_CompileStarlark_DefaultsRuntimeWhenOmitted(t *testing.T) {
	fp := &fakeProvider{
		resp: &llm.CompletionResponse{
			Content: `{"script":"` + escapeJSON(validStarlark) + `"}`, // no runtime field
		},
	}
	c := NewLLMCompilerStatic(fp, "glm-4-flash")
	spec, err := c.Compile(context.Background(), "x", CompileHints{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if spec.Runtime != RuntimeStarlark {
		t.Errorf("omitted runtime must default to starlark, got %q", spec.Runtime)
	}
}
