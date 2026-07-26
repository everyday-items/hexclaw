package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestBUG20260726029_FinalProviderPromptKeepsRealityAndEndsWithExactAssistantIdentity(t *testing.T) {
	eng, skills := newEngineForSkillAudit(t)
	const (
		agentName     = "k12-tutor-xiaoming"
		customPrompt  = "保持温和；老师布置的作业和回复老师消息必须保留现实人物语义。"
		mountedMarker = "MOUNTED_PERSONA：先肯定，再渐进提示。"
		userQuery     = "老师布置的作业怎么回复？"
	)
	if err := skills.Register(&fakePersonaSkill{name: "温和辅导", body: mountedMarker}); err != nil {
		t.Fatalf("注册 persona: %v", err)
	}
	dispatcher := agentrouter.New()
	if err := dispatcher.Register(agentrouter.AgentConfig{
		Name: agentName, SystemPrompt: customPrompt,
		Metadata: map[string]string{
			"scenario": "k12-tutor",
			k12.MetaKeyChildName: "小明",
		},
	}); err != nil {
		t.Fatalf("注册 agent: %v", err)
	}
	directive, err := k12.CompileTutorIdentityDirective(map[string]string{
		k12.MetaKeyChildName: "小明",
	})
	if err != nil {
		t.Fatalf("编译 K12 身份合同: %v", err)
	}
	eng.SetAgentRouter(dispatcher)
	eng.SetAgentSystemPromptPolicy(stubAgentSystemPromptPolicy{
		directive: AgentSystemPromptDirective{
			Key:     k12.TutorIdentityPromptContractVersion,
			Content: directive,
		},
	})

	msg := &adapter.Message{
		Content: userQuery,
		Metadata: map[string]string{
			"role": agentName, "skills": "温和辅导", "agent_mode": "deep",
		},
	}
	if err := eng.prepareAgentSystemPromptPolicy(context.Background(), msg); err != nil {
		t.Fatalf("准备终端身份合同: %v", err)
	}
	messages := eng.buildStreamMessages(
		context.Background(), agentName, nil, "", msg.Content, msg.Metadata, nil,
	)
	if len(messages) < 2 {
		t.Fatalf("Provider 请求消息不完整: %#v", messages)
	}
	system := messages[0].Content
	const exactReply = "你好，我是小明的辅导助手。"
	if strings.Count(system, exactReply) != 1 {
		t.Fatalf("精确自我介绍必须在最终 prompt 中恰好一次: %q", system)
	}
	if !strings.HasSuffix(strings.TrimSpace(system), strings.TrimSpace(directive)) {
		t.Fatalf("K12 身份合同必须是 Provider system prompt 的最终内容: %q", system)
	}
	for _, preserved := range []string{customPrompt, mountedMarker, "老师布置的作业", "回复老师消息"} {
		if !strings.Contains(system, preserved) {
			t.Fatalf("最终 prompt 误伤现实老师/自定义 Persona 语义 %q: %q", preserved, system)
		}
	}
	if got := messages[len(messages)-1].Content; strings.Count(got, userQuery) != 1 ||
		!strings.HasSuffix(strings.TrimSpace(got), userQuery) {
		t.Fatalf("身份合同不得改写或复制用户原文: got=%q want suffix=%q", got, userQuery)
	}
}
