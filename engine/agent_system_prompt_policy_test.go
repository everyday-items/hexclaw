package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

type stubAgentSystemPromptPolicy struct {
	directive AgentSystemPromptDirective
	err       error
}

func (p stubAgentSystemPromptPolicy) CompileTerminalDirective(
	_ context.Context,
	_ AgentSystemPromptPolicyInput,
) (AgentSystemPromptDirective, error) {
	return p.directive, p.err
}

func TestAgentSystemPromptPolicy_TerminalLastExactlyOnceAfterPersona(t *testing.T) {
	eng, skills := newEngineForSkillAudit(t)
	const (
		agentName     = "k12-tutor-xiaoming"
		customPrompt  = "CUSTOM_PERSONA：像老师一样有耐心；现实中的老师布置作业时要尊重原文。"
		mountedMarker = "MOUNTED_PERSONA：保持简洁温和。"
		terminal      = "TERMINAL_IDENTITY：只以小明的辅导助手自称。"
	)
	if err := skills.Register(&fakePersonaSkill{name: "温和辅导", body: mountedMarker}); err != nil {
		t.Fatalf("注册 persona: %v", err)
	}
	dispatcher := agentrouter.New()
	if err := dispatcher.Register(agentrouter.AgentConfig{
		Name: agentName, SystemPrompt: customPrompt,
		Metadata: map[string]string{"scenario": "k12-tutor", "k12.child_name": "小明"},
	}); err != nil {
		t.Fatalf("注册 agent: %v", err)
	}
	eng.SetAgentRouter(dispatcher)
	eng.SetAgentSystemPromptPolicy(stubAgentSystemPromptPolicy{
		directive: AgentSystemPromptDirective{Key: "test-identity-v1", Content: terminal},
	})

	msg := &adapter.Message{
		Content: "你是谁？",
		Metadata: map[string]string{
			"role": agentName, "skills": "温和辅导", "agent_mode": "deep",
		},
	}
	if err := eng.prepareAgentSystemPromptPolicy(context.Background(), msg); err != nil {
		t.Fatalf("prepare policy: %v", err)
	}
	messages := eng.buildStreamMessages(
		context.Background(), agentName, nil, "", msg.Content, msg.Metadata, nil,
	)
	if len(messages) == 0 {
		t.Fatal("缺少 system message")
	}
	system := messages[0].Content
	if !strings.Contains(system, customPrompt) || !strings.Contains(system, mountedMarker) {
		t.Fatalf("自定义或挂载 persona 被覆盖：%q", system)
	}
	if !strings.Contains(system, "现实中的老师布置作业") {
		t.Fatalf("真实老师语义被错误改写：%q", system)
	}
	if got := strings.Count(system, terminal); got != 1 {
		t.Fatalf("terminal directive 必须恰好一次，实际 %d：%q", got, system)
	}
	if !strings.HasSuffix(strings.TrimSpace(system), terminal) {
		t.Fatalf("terminal directive 必须是最终 system prompt 的最后内容：%q", system)
	}
}

func TestAgentSystemPromptPolicy_ErrorStopsBeforeProviderCall(t *testing.T) {
	provider := &fastKBChatProvider{}
	eng := newEngineWithProviders(t,
		map[string]hexagon.Provider{"test": provider},
		map[string]config.LLMProviderConfig{"test": {Model: "mock-model"}},
		"test",
	)
	dispatcher := agentrouter.New()
	const agentName = "k12-tutor-missing-child"
	if err := dispatcher.Register(agentrouter.AgentConfig{
		Name: agentName, SystemPrompt: "custom prompt",
		Metadata: map[string]string{"scenario": "k12-tutor"},
	}); err != nil {
		t.Fatalf("注册 agent: %v", err)
	}
	eng.SetAgentRouter(dispatcher)
	sentinel := errors.New("K12 辅导助手缺少孩子姓名")
	eng.SetAgentSystemPromptPolicy(stubAgentSystemPromptPolicy{err: sentinel})

	_, err := eng.Process(context.Background(), &adapter.Message{
		ID: "identity-missing-child", Platform: adapter.PlatformAPI,
		UserID: "u1", SessionID: "s1", Content: "你是谁？",
		Metadata: map[string]string{"role": agentName},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("应返回策略原始错误，实际 %v", err)
	}
	if got := len(provider.seen); got != 0 {
		t.Fatalf("策略失败后 Provider 调用数必须为 0，实际 %d", got)
	}
}

