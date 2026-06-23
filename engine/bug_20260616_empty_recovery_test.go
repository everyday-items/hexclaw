package engine

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

// End-to-end coverage for the 2026-06-16 empty-after-tool self-heal: when the
// model returns empty content right after a tool result, the finalize path
// re-prompts once (recoverReasoningOnly) and backfills the answer instead of
// dead-ending with "模型未返回有效内容".

func finalizeEmpty(t *testing.T, provider hexagon.Provider, reqMessages []hexagon.Message) (string, map[string]string) {
	t.Helper()
	eng := newEngineWithProviderAndSkills(t, provider, skill.NewRegistry())
	msg := &adapter.Message{ID: "m-1", SessionID: "s-1", UserID: "u-1"}
	content, _, meta, _, _ := eng.finalizeRuntimeStreamResult(
		context.Background(),
		"s-1",
		msg,
		provider,
		hexagon.CompletionRequest{Messages: reqMessages},
		&hruntime.Result{Content: "", Reasoning: ""},
		"test",
		"mock-model",
		"cache-key",
		false,
	)
	return content, meta
}

func TestEmptyAfterTool_RecoversFromContext(t *testing.T) {
	var calls int
	provider := mockllm.NewLLMProvider("test").WithResponseFn(
		func(_ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			calls++
			return &hexagon.CompletionResponse{Content: "百度热搜 TOP3：A、B、C"}, nil
		})

	// Empty result, but the conversation already carries a tool result.
	content, _ := finalizeEmpty(t, provider, []hexagon.Message{
		{Role: hexagon.RoleUser, Content: "采集百度热搜"},
		{Role: hexagon.RoleTool, Content: "<huge noisy page text>"},
	})

	if calls != 1 {
		t.Fatalf("recovery should re-prompt exactly once, got %d provider calls", calls)
	}
	if !strings.Contains(content, "百度热搜 TOP3") {
		t.Fatalf("empty-after-tool must be backfilled by recovery, got %q", content)
	}
	if strings.Contains(content, "未返回有效内容") {
		t.Errorf("recovered answer must not be the dead-end error, got %q", content)
	}
}

func TestEmptyWithoutTool_DoesNotRetryAndErrors(t *testing.T) {
	var calls int
	provider := mockllm.NewLLMProvider("test").WithResponseFn(
		func(_ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			calls++
			return &hexagon.CompletionResponse{Content: "should-not-be-called"}, nil
		})

	// Empty result with NO tool context → gating skips the retry.
	content, _ := finalizeEmpty(t, provider, []hexagon.Message{
		{Role: hexagon.RoleUser, Content: "hello"},
	})

	if calls != 0 {
		t.Fatalf("no tool context → recovery must not fire, got %d provider calls", calls)
	}
	if !strings.Contains(content, "未返回有效内容") {
		t.Fatalf("plain empty response must surface the error, got %q", content)
	}
}
