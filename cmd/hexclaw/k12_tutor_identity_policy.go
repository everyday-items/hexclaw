package main

import (
	"context"

	"github.com/hexagon-codes/hexclaw/engine"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type tutorIdentityAgentStore interface {
	SaveAgent(context.Context, *agentrouter.AgentConfig) error
}

type k12TutorIdentityPolicy struct {
	router *agentrouter.Dispatcher
	store  tutorIdentityAgentStore
}

func newK12TutorIdentityPolicy(
	router *agentrouter.Dispatcher,
	store tutorIdentityAgentStore,
) *k12TutorIdentityPolicy {
	return &k12TutorIdentityPolicy{router: router, store: store}
}

func (p *k12TutorIdentityPolicy) CompileTerminalDirective(
	ctx context.Context,
	input engine.AgentSystemPromptPolicyInput,
) (engine.AgentSystemPromptDirective, error) {
	if input.Agent.Metadata["scenario"] != k12TutorScenario {
		return engine.AgentSystemPromptDirective{}, nil
	}
	if input.Agent.Metadata[k12.MetaKeyPromptContractVersion] == k12.TutorIdentityPromptContractVersion {
		content, err := k12.CompileTutorIdentityDirective(input.Agent.Metadata)
		return engine.AgentSystemPromptDirective{
			Key: k12.TutorIdentityPromptContractVersion, Content: content,
		}, err
	}

	var content string
	shouldPersist := false
	err := p.router.UpdateAgentPersisted(input.Agent.Name,
		func(current agentrouter.AgentConfig) (agentrouter.AgentConfig, error) {
			if current.Metadata["scenario"] != k12TutorScenario {
				content = ""
				return current, nil
			}
			compiled, err := k12.CompileTutorIdentityDirective(current.Metadata)
			if err != nil {
				return current, err
			}
			content = compiled
			if current.Metadata[k12.MetaKeyPromptContractVersion] == k12.TutorIdentityPromptContractVersion {
				return current, nil
			}
			meta := make(map[string]string, len(current.Metadata)+1)
			for key, value := range current.Metadata {
				meta[key] = value
			}
			meta[k12.MetaKeyPromptContractVersion] = k12.TutorIdentityPromptContractVersion
			current.Metadata = meta
			shouldPersist = true
			return current, nil
		},
		func(updated *agentrouter.AgentConfig) error {
			if !shouldPersist || p.store == nil {
				return nil
			}
			return p.store.SaveAgent(ctx, updated)
		},
	)
	if err != nil {
		return engine.AgentSystemPromptDirective{}, err
	}
	if content == "" {
		return engine.AgentSystemPromptDirective{}, nil
	}
	return engine.AgentSystemPromptDirective{
		Key: k12.TutorIdentityPromptContractVersion, Content: content,
	}, nil
}

