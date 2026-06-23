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

// AUDIT 2026-06-23：达到工具轮次上限时优雅降级 —— 不再“请求失败”，而是返回模型已产出的
// 部分内容 + 尾部追加轮次上限提示，且 finish_reason=max_turns。直接验证流式 finalize。
func TestAudit_MaxTurnsGraceful_StreamFinalizeAppendsNotice_20260623(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(
		func(_ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			return &hexagon.CompletionResponse{Content: "不应被调用"}, nil
		})
	eng := newEngineWithProviderAndSkills(t, provider, skill.NewRegistry())
	msg := &adapter.Message{ID: "m-1", SessionID: "s-1", UserID: "u-1"}

	partial := "我已经查了前两步，结果是 A 和 B"
	content, streamTail, meta, _, _ := eng.finalizeRuntimeStreamResult(
		context.Background(),
		"s-1",
		msg,
		provider,
		hexagon.CompletionRequest{Messages: []hexagon.Message{{Role: hexagon.RoleUser, Content: "继续"}}},
		&hruntime.Result{Content: partial, StopReason: hruntime.StopReasonMaxTurns},
		"test",
		"mock-model",
		"cache-key",
		true, // maxTurnsHit
	)

	if !strings.Contains(content, partial) {
		t.Fatalf("应保留模型已产出的部分内容，got %q", content)
	}
	if !strings.Contains(content, maxTurnsReachedNotice) {
		t.Errorf("落库内容应尾部追加轮次上限提示，got %q", content)
	}
	if !strings.Contains(streamTail, maxTurnsReachedNotice) {
		t.Errorf("streamTail 应携带提示作为额外 chunk 补发，got %q", streamTail)
	}
	if meta["finish_reason"] != "max_turns" {
		t.Errorf("finish_reason 应为 max_turns，got %q", meta["finish_reason"])
	}
}

// 默认工具轮次上限锁定为 25（旧 5 太保守，导致带工具的 agent 频繁触发 max_turns）。
func TestAudit_DefaultAgentMaxTurns_20260623(t *testing.T) {
	if defaultAgentMaxTurns != 25 {
		t.Fatalf("defaultAgentMaxTurns = %d, want 25", defaultAgentMaxTurns)
	}
	if strings.TrimSpace(maxTurnsReachedNotice) == "" {
		t.Fatal("maxTurnsReachedNotice 不应为空")
	}
}
