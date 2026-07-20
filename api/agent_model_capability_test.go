package api

import (
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

func TestValidateAgentLLMConfigRequiresProviderScopedTextCapability(t *testing.T) {
	const modelID = "shared-model"
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"vector": {
			Models: []string{modelID}, ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID: modelID, Capabilities: []string{config.LLMModelCapabilityEmbedding},
			}},
		},
		"chat": {
			Model: modelID, Models: []string{modelID}, ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID: modelID, Capabilities: []string{config.LLMModelCapabilityText},
			}},
		},
	}
	srv := NewServer(cfg, &mockEngine{}, nil, nil)

	vectorBinding := &agentrouter.AgentConfig{Provider: "vector", Model: modelID}
	if err := srv.validateAgentLLMConfig(vectorBinding); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "text") {
		t.Fatalf("embedding-only Agent binding error=%v, want text capability rejection", err)
	}

	chatBinding := &agentrouter.AgentConfig{Provider: "chat", Model: modelID}
	if err := srv.validateAgentLLMConfig(chatBinding); err != nil {
		t.Fatalf("same model ID on text provider rejected: %v", err)
	}
}
