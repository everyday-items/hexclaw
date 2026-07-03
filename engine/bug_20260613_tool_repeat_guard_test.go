package engine

// BUG-20260613: a weak model (glm-4-flash) looped on an identical browser
// fetch 35 times until the token budget was exhausted. The runtime tool
// executor now short-circuits an identical (tool, args) call after a small
// number of repeats and nudges the model to answer from the result it has.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	hruntime "github.com/hexagon-codes/hexagon/runtime"
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

func TestToolRepeatGuard_UnsafeMutatingToolShortCircuitsAfterOne(t *testing.T) {
	rte := newRuntimeToolExecutor(&ToolExecutor{})
	call := llm.ToolCall{Name: "cron_task", Arguments: `{"action":"create","prompt":"每天早上9点采集百度热搜榜","schedule":"每天早上9点"}`}

	first, _ := rte.Execute(context.Background(), call)
	if strings.Contains(first.Content, "already called") {
		t.Fatalf("first mutating tool call must be allowed, got %q", first.Content)
	}
	second, _ := rte.Execute(context.Background(), call)
	if !strings.Contains(second.Content, "already called") {
		t.Fatalf("second identical mutating tool call must be blocked, got %q", second.Content)
	}
}

type repeatToolProvider struct {
	calls int
}

func (p *repeatToolProvider) Name() string { return "repeat-tool" }
func (p *repeatToolProvider) Complete(context.Context, llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.calls++
	return &llm.CompletionResponse{
		ToolCalls: []llm.ToolCall{{
			ID:        fmt.Sprintf("call-%d", p.calls),
			Name:      "cron_task",
			Arguments: `{"action":"create","prompt":"每天早上9点采集百度热搜榜","schedule":"每天早上9点"}`,
		}},
	}, nil
}
func (p *repeatToolProvider) Stream(context.Context, llm.CompletionRequest) (*llm.Stream, error) {
	return nil, errors.New("unused")
}

func TestToolRepeatGuard_RuntimeStopsAfterRepeatedMutatingTool(t *testing.T) {
	provider := &repeatToolProvider{}
	runner := hruntime.NewRunner(hruntime.Config{
		ProviderSelector: hruntime.StaticProviderSelector{Provider: provider, Name: "repeat", Model: "m"},
		ToolExecutor:     newRuntimeToolExecutor(&ToolExecutor{}),
		Middleware:       []hruntime.Middleware{runtimeRepeatGuardStopMiddleware{}},
		DefaultMaxTurns:  25,
	})

	result, err := runner.Run(context.Background(), hruntime.Request{
		ID:       "repeat-mutating-tool",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "create task"}},
		Tools:    []llm.ToolDefinition{{Type: "function", Function: llm.ToolFunctionDef{Name: "cron_task"}}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if provider.calls > 2 {
		t.Fatalf("repeat guard must stop the runtime after the blocked repeat, provider calls=%d", provider.calls)
	}
	if result.StopReason != hruntime.StopReasonEndTurn {
		t.Fatalf("StopReason=%q, want end_turn", result.StopReason)
	}
	if !strings.Contains(result.Content, "already called") {
		t.Fatalf("final content should explain the repeat guard, got %q", result.Content)
	}
}
