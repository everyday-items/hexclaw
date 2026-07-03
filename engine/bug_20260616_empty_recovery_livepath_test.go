package engine

// BUG-20260616: the empty-after-tool self-heal gated on
// messagesHaveToolResult(req.Messages). On the live stream path req.Messages is
// the initial request — the runtime accumulates the tool transcript internally
// and never writes it back — so the gate was always false and the self-heal
// never fired in production (the existing tests only passed by injecting a
// RoleTool into req, which the live path never does). The gate must key off the
// turn's actual tool activity (result.ToolCalls), and the retry must carry the
// reconstructed transcript so the model can synthesize an answer.

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	hruntime "github.com/hexagon-codes/hexagon/runtime"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
)

func TestBug20260616_EmptyAfterToolRecoversOnLivePath(t *testing.T) {
	var calls int
	var sawToolOutput bool
	provider := mockllm.NewLLMProvider("test").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			calls++
			for _, m := range req.Messages {
				if m.Role == hexagon.RoleTool && strings.Contains(m.Content, "TOOL_OUTPUT_XYZ") {
					sawToolOutput = true
				}
			}
			return &hexagon.CompletionResponse{Content: "synthesized from tools"}, nil
		})

	eng := newEngineWithProviderAndSkills(t, provider, skill.NewRegistry())
	msg := &adapter.Message{ID: "m-1", SessionID: "s-1", UserID: "u-1"}

	// Live-path wiring: req.Messages carries NO tool result, but the turn ran a
	// tool (recorded in result.ToolCalls with its output).
	content, _, _, _, _ := eng.finalizeRuntimeStreamResult(
		context.Background(),
		"s-1",
		msg,
		provider,
		hexagon.CompletionRequest{Messages: []hexagon.Message{
			{Role: hexagon.RoleUser, Content: "采集百度热搜"},
		}},
		&hruntime.Result{Content: "", Reasoning: "", ToolCalls: []hruntime.ToolCallRecord{
			{ID: "c1", Name: "fetch", Arguments: "{}", Result: hruntime.ToolResult{Content: "TOOL_OUTPUT_XYZ"}},
		}},
		"test",
		"mock-model",
		"cache-key",
		false,
		0,
	)

	if calls != 1 {
		t.Fatalf("live-path empty-after-tool must re-prompt exactly once, got %d provider calls", calls)
	}
	if !sawToolOutput {
		t.Fatalf("the retry must carry the reconstructed tool transcript")
	}
	if !strings.Contains(content, "synthesized from tools") || strings.Contains(content, "未返回有效内容") {
		t.Fatalf("empty-after-tool must be backfilled from tool context, got %q", content)
	}
}
