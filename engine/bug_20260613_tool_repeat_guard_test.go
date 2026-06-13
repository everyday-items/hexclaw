package engine

// BUG-20260613: a weak model (glm-4-flash) looped on an identical browser
// fetch 35 times until the token budget was exhausted. The runtime tool
// executor now short-circuits an identical (tool, args) call after a small
// number of repeats and nudges the model to answer from the result it has.

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

func TestBug20260613_ToolRepeatGuardShortCircuits(t *testing.T) {
	calls := 0
	exec := &ToolExecutor{}
	// Stub the real execution by routing through a fake skill registry would be
	// heavy; instead drive Execute directly and count how many reach the inner
	// executor via a sentinel: an empty executor returns an error path, so we
	// assert on the guard message instead.
	rte := newRuntimeToolExecutor(exec)
	_ = calls

	call := llm.ToolCall{Name: "browser", Arguments: `{"url":"http://x"}`}

	// First maxIdenticalToolCalls invocations pass through to the executor
	// (which here has no skills, returning a "not found" error — that's fine,
	// we only care that the guard does NOT short-circuit yet).
	for i := 0; i < maxIdenticalToolCalls; i++ {
		res, _ := rte.Execute(context.Background(), call)
		if strings.Contains(res.Content, "already called") {
			t.Fatalf("guard tripped too early on call %d", i+1)
		}
	}
	// The next identical call must be short-circuited by the guard.
	res, _ := rte.Execute(context.Background(), call)
	if !strings.Contains(res.Content, "already called") {
		t.Errorf("guard must short-circuit the repeated call, got %q", res.Content)
	}
	if res.Error != "" {
		t.Errorf("short-circuit must not be an error result, got %q", res.Error)
	}
}

func TestBug20260613_ToolRepeatGuardDistinctArgsNotBlocked(t *testing.T) {
	rte := newRuntimeToolExecutor(&ToolExecutor{})
	// Different arguments must each get their own budget.
	for i, url := range []string{`{"url":"a"}`, `{"url":"b"}`, `{"url":"c"}`, `{"url":"d"}`} {
		res, _ := rte.Execute(context.Background(), llm.ToolCall{Name: "browser", Arguments: url})
		if strings.Contains(res.Content, "already called") {
			t.Errorf("distinct args must not trip the guard (call %d, args %s)", i+1, url)
		}
	}
}
