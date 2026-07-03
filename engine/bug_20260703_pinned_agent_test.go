package engine

// BUG-20260703（实机#1「Agent 抢答」）：用户在桌面端明确选了默认助理（或某个
// 命名 Agent），RouteWithFallback 仍按消息内容跑路由——一句翻译腔的话就被
// 「专业翻译」专属 Agent 接管（provider/model/人设全被改派）。
//
// 契约：聊天请求 metadata 带 pinned_agent 即为显式锁定，内容路由必须跳过：
//   - pinned_agent=default        → 默认助理（不注入 agent_prompt、不改 provider）
//   - pinned_agent=<Agent 名>     → 直取该 Agent 配置（等效于路由命中它）
//   - pinned_agent=<不存在的名字> → 按默认助理处理（绝不回退内容路由）
//   - 不带 pinned_agent           → 内容路由保持现状（IM/旧链路不受影响）

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

// newPinnedAgentEngine 构造带「抢答型」翻译 Agent 的引擎：
// translator 绑定独立 provider，且有一条 platform=api 的兜底规则会命中所有桌面消息。
func newPinnedAgentEngine(t *testing.T) (eng *ReActEngine, defaultLLM, translatorLLM *mockllm.LLMProvider) {
	t.Helper()

	defaultLLM = mockllm.NewLLMProvider("test").WithResponseFn(func(_ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{Content: "default-reply", Usage: hexagon.Usage{TotalTokens: 5}}, nil
	})
	translatorLLM = mockllm.NewLLMProvider("translator-llm").WithResponseFn(func(_ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{Content: "translator-reply", Usage: hexagon.Usage{TotalTokens: 5}}, nil
	})

	eng = newEngineWithProviders(t, map[string]hexagon.Provider{
		"test":           defaultLLM,
		"translator-llm": translatorLLM,
	}, map[string]config.LLMProviderConfig{
		"test":           {Model: "mock-model"},
		"translator-llm": {Model: "mock-translator"},
	}, "test")

	dispatcher := agentrouter.New()
	if err := dispatcher.Register(agentrouter.AgentConfig{
		Name:         "translator",
		Provider:     "translator-llm",
		Model:        "mock-translator",
		SystemPrompt: "你是专业翻译",
	}); err != nil {
		t.Fatalf("注册 translator Agent 失败: %v", err)
	}
	if err := dispatcher.AddRule(agentrouter.Rule{Platform: "api", AgentName: "translator", Priority: 1}); err != nil {
		t.Fatalf("添加抢答规则失败: %v", err)
	}
	eng.SetAgentRouter(dispatcher)
	return eng, defaultLLM, translatorLLM
}

func pinnedChatMessage(id, pinned string) *adapter.Message {
	msg := &adapter.Message{
		ID: id, Platform: adapter.PlatformAPI,
		UserID: "user-1", ChatID: "chat-" + id,
		Content: "帮我把这句话翻译成英文：你好",
	}
	if pinned != "" {
		msg.Metadata = map[string]string{"pinned_agent": pinned}
	}
	return msg
}

// UT-PIN-001: 锁定默认助理时，翻译 Agent 不得接管（provider 与人设都不许改派）。
func TestBug20260703_PinnedDefaultSkipsAgentRouting(t *testing.T) {
	eng, defaultLLM, translatorLLM := newPinnedAgentEngine(t)

	msg := pinnedChatMessage("pin-default", "default")
	reply, err := eng.Process(context.Background(), msg)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	if translatorLLM.CallCount() != 0 {
		t.Errorf("[BUG-20260703] 锁定默认助理仍被翻译 Agent 抢答（translator 调用 %d 次）", translatorLLM.CallCount())
	}
	if defaultLLM.CallCount() != 1 {
		t.Errorf("默认 provider 应恰好调用 1 次，实际 %d", defaultLLM.CallCount())
	}
	if reply.Content != "default-reply" {
		t.Errorf("回复应来自默认助理，实际 %q", reply.Content)
	}
	if got := msg.Metadata["routed_agent"]; got != "" {
		t.Errorf("锁定默认助理不应有 routed_agent，实际 %q", got)
	}
	if got := msg.Metadata["agent_prompt"]; got != "" {
		t.Errorf("锁定默认助理不应注入 agent_prompt（人设被翻译 Agent 接管的根源），实际 %q", got)
	}
}

// UT-PIN-002: 锁定命名 Agent 时，直取其配置（provider/model），不跑内容路由。
func TestBug20260703_PinnedNamedAgentUsesItsConfig(t *testing.T) {
	eng, defaultLLM, translatorLLM := newPinnedAgentEngine(t)

	msg := pinnedChatMessage("pin-named", "translator")
	reply, err := eng.Process(context.Background(), msg)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	if translatorLLM.CallCount() != 1 {
		t.Errorf("锁定 translator 应调用其 provider 1 次，实际 %d", translatorLLM.CallCount())
	}
	if defaultLLM.CallCount() != 0 {
		t.Errorf("默认 provider 不应被调用，实际 %d", defaultLLM.CallCount())
	}
	if reply.Content != "translator-reply" {
		t.Errorf("回复应来自 translator，实际 %q", reply.Content)
	}
	if got := msg.Metadata["routed_agent"]; got != "translator" {
		t.Errorf("routed_agent 应为 translator，实际 %q", got)
	}
}

// UT-PIN-003: 锁定不存在的 Agent 名（陈旧配置）→ 按默认助理处理，绝不回退内容路由。
func TestBug20260703_PinnedUnknownAgentFallsBackToDefault(t *testing.T) {
	eng, defaultLLM, translatorLLM := newPinnedAgentEngine(t)

	msg := pinnedChatMessage("pin-ghost", "ghost-agent")
	reply, err := eng.Process(context.Background(), msg)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	if translatorLLM.CallCount() != 0 {
		t.Errorf("[BUG-20260703] 未知锁定名回退到了内容路由（translator 调用 %d 次）——回退即重新引入抢答", translatorLLM.CallCount())
	}
	if defaultLLM.CallCount() != 1 {
		t.Errorf("默认 provider 应调用 1 次，实际 %d", defaultLLM.CallCount())
	}
	if reply.Content != "default-reply" {
		t.Errorf("回复应来自默认助理，实际 %q", reply.Content)
	}
}

// UT-PIN-004（回归守护）: 不带 pinned_agent 的消息保持内容路由现状（IM/旧链路）。
func TestBug20260703_NoPinKeepsContentRouting(t *testing.T) {
	eng, _, translatorLLM := newPinnedAgentEngine(t)

	msg := pinnedChatMessage("no-pin", "")
	reply, err := eng.Process(context.Background(), msg)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	if translatorLLM.CallCount() != 1 {
		t.Errorf("无锁定信号时规则路由应照常命中 translator，实际调用 %d 次", translatorLLM.CallCount())
	}
	if reply.Content != "translator-reply" {
		t.Errorf("回复应来自 translator，实际 %q", reply.Content)
	}
}
