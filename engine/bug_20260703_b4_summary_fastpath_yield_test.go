package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
)

// BUG-20260703 B4（引擎层锁）：多轮会话里"总结下乐知这家公司"承接上文，summary
// 关键词快路径不得劫持——必须落 LLM 主路径带会话上下文作答，绝不能直吐
// "摘要：下乐知这家公司" 回声垃圾。
func TestBug20260703_B4_ConversationalSummaryFallsToLLM(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{Content: "乐知新创是一家企业培训公司……（接上文语境作答）", Usage: hexagon.Usage{TotalTokens: 8}}, nil
	})
	skills := skill.NewRegistry()
	if err := skills.Register(builtin.NewSummarySkill()); err != nil {
		t.Fatalf("注册 SummarySkill 失败: %v", err)
	}
	eng := newEngineWithProviderAndSkills(t, provider, skills)

	// 先造一轮历史（承接上文语境）。
	if _, err := eng.Process(context.Background(), &adapter.Message{
		ID: "msg-b4-1", Platform: adapter.PlatformAPI, UserID: "user-001", SessionID: "sess-b4",
		Content: "乐知新创是做什么的？",
	}); err != nil {
		t.Fatalf("Process (历史轮) 失败: %v", err)
	}
	callsAfterHistory := provider.CallCount()

	reply, err := eng.Process(context.Background(), &adapter.Message{
		ID: "msg-b4-2", Platform: adapter.PlatformAPI, UserID: "user-001", SessionID: "sess-b4",
		Content: "总结下乐知这家公司",
	})
	if err != nil {
		t.Fatalf("Process (对话式总结) 失败: %v", err)
	}

	if strings.HasPrefix(reply.Content, "摘要：") {
		t.Fatalf("B4: 对话式后续被 summary 快路径劫持，直吐回声垃圾：%q", reply.Content)
	}
	if provider.CallCount() == callsAfterHistory {
		t.Fatalf("B4: 应落 LLM 主路径带上下文作答，provider 未被调用")
	}
}

// 流式路径锁：skill 快路径在 ProcessStream（react.go 的独立调用点）同样必须让路——
// 两条路径共享 skills.Match，但调用点各自独立，防未来分叉。
func TestBug20260703_B4_ConversationalSummaryFallsToLLM_Stream(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{Content: "乐知新创是一家企业培训公司……（接上文语境作答）", Usage: hexagon.Usage{TotalTokens: 8}}, nil
	})
	skills := skill.NewRegistry()
	if err := skills.Register(builtin.NewSummarySkill()); err != nil {
		t.Fatalf("注册 SummarySkill 失败: %v", err)
	}
	eng := newEngineWithProviderAndSkills(t, provider, skills)

	ch, err := eng.ProcessStream(context.Background(), &adapter.Message{
		ID: "msg-b4-stream", Platform: adapter.PlatformAPI, UserID: "user-001", SessionID: "sess-b4-stream",
		Content: "总结下乐知这家公司",
	})
	if err != nil {
		t.Fatalf("ProcessStream 失败: %v", err)
	}
	var full strings.Builder
	for chunk := range ch {
		full.WriteString(chunk.Content)
	}
	if strings.HasPrefix(full.String(), "摘要：") {
		t.Fatalf("B4: 流式路径被 summary 快路径劫持：%q", full.String())
	}
	if provider.CallCount() == 0 {
		t.Fatalf("B4: 流式应落 LLM 主路径，provider 未被调用")
	}
}
