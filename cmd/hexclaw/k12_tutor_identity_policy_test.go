package main

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/engine"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type tutorIdentitySavingStore struct {
	saves []agentrouter.AgentConfig
}

func (s *tutorIdentitySavingStore) SaveAgent(_ context.Context, cfg *agentrouter.AgentConfig) error {
	s.saves = append(s.saves, *cfg)
	return nil
}

func TestK12TutorIdentityPolicy_StampsVersionWithoutRewritingSystemPrompt(t *testing.T) {
	dispatcher := agentrouter.New()
	store := &tutorIdentitySavingStore{}
	const (
		agentName    = "k12-tutor-xiaoming"
		customPrompt = "我的自定义教学法；保留老师布置作业的现实语义。"
	)
	if err := dispatcher.Register(agentrouter.AgentConfig{
		Name: agentName, SystemPrompt: customPrompt,
		Metadata: map[string]string{
			"scenario": k12TutorScenario, k12.MetaKeyChildName: "小明",
		},
	}); err != nil {
		t.Fatalf("注册 agent: %v", err)
	}
	policy := newK12TutorIdentityPolicy(dispatcher, store)

	cfg, _ := dispatcher.GetAgent(agentName)
	directive, err := policy.CompileTerminalDirective(context.Background(),
		engine.AgentSystemPromptPolicyInput{Agent: *cfg, UserQuery: "你是谁？"})
	if err != nil {
		t.Fatalf("CompileTerminalDirective: %v", err)
	}
	if directive.Key != k12.TutorIdentityPromptContractVersion || directive.Content == "" {
		t.Fatalf("directive 不完整：%+v", directive)
	}
	updated, _ := dispatcher.GetAgent(agentName)
	if updated.Metadata[k12.MetaKeyPromptContractVersion] != k12.TutorIdentityPromptContractVersion {
		t.Fatalf("未写入 contract version：%#v", updated.Metadata)
	}
	if updated.SystemPrompt != customPrompt {
		t.Fatalf("metadata stamp 不得改写 SystemPrompt：%q", updated.SystemPrompt)
	}
	if len(store.saves) != 1 || store.saves[0].SystemPrompt != customPrompt {
		t.Fatalf("持久化必须一次且保留 SystemPrompt：%#v", store.saves)
	}

	fresh, _ := dispatcher.GetAgent(agentName)
	if _, err := policy.CompileTerminalDirective(context.Background(),
		engine.AgentSystemPromptPolicyInput{Agent: *fresh, UserQuery: "介绍下你"}); err != nil {
		t.Fatalf("重复编译: %v", err)
	}
	if len(store.saves) != 1 {
		t.Fatalf("版本 stamp 必须幂等，保存次数=%d", len(store.saves))
	}
}

func TestK12TutorIdentityPolicy_MissingChildDoesNotStamp(t *testing.T) {
	dispatcher := agentrouter.New()
	store := &tutorIdentitySavingStore{}
	const agentName = "k12-tutor-missing-child"
	if err := dispatcher.Register(agentrouter.AgentConfig{
		Name: agentName, SystemPrompt: "custom",
		Metadata: map[string]string{"scenario": k12TutorScenario},
	}); err != nil {
		t.Fatalf("注册 agent: %v", err)
	}
	policy := newK12TutorIdentityPolicy(dispatcher, store)
	cfg, _ := dispatcher.GetAgent(agentName)
	if _, err := policy.CompileTerminalDirective(context.Background(),
		engine.AgentSystemPromptPolicyInput{Agent: *cfg}); err == nil {
		t.Fatal("缺少 child_name 必须失败")
	}
	if len(store.saves) != 0 {
		t.Fatalf("失败时不得 stamp，保存次数=%d", len(store.saves))
	}
}

