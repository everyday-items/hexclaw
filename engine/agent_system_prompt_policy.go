package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/adapter"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

const (
	metadataAgentSystemPromptPolicyKey       = "_agent_system_prompt_policy_key"
	metadataAgentSystemPromptPolicyDirective = "_agent_system_prompt_policy_directive"
)

// AgentSystemPromptPolicyInput is the generic engine boundary supplied to a
// scenario-owned terminal system-prompt policy.
type AgentSystemPromptPolicyInput struct {
	Agent     agentrouter.AgentConfig
	UserQuery string
}

// AgentSystemPromptDirective is appended after every editable and mounted
// prompt source. Key versions the semantic-cache boundary.
type AgentSystemPromptDirective struct {
	Key     string
	Content string
}

// AgentSystemPromptPolicy lets a composition root attach scenario invariants
// without importing the scenario package into engine.
type AgentSystemPromptPolicy interface {
	CompileTerminalDirective(context.Context, AgentSystemPromptPolicyInput) (AgentSystemPromptDirective, error)
}

// SetAgentSystemPromptPolicy installs the single terminal policy compiler.
func (e *ReActEngine) SetAgentSystemPromptPolicy(policy AgentSystemPromptPolicy) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.agentSystemPromptPolicy = policy
}

func (e *ReActEngine) getAgentSystemPromptPolicy() AgentSystemPromptPolicy {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.agentSystemPromptPolicy
}

// prepareAgentSystemPromptPolicy resolves the registered Agent after routing,
// compiles its terminal directive and overwrites the private request metadata.
// Client-supplied values under the private keys are always discarded.
func (e *ReActEngine) prepareAgentSystemPromptPolicy(ctx context.Context, msg *adapter.Message) error {
	if msg == nil {
		return nil
	}
	ensureMessageMetadata(msg)
	delete(msg.Metadata, metadataAgentSystemPromptPolicyKey)
	delete(msg.Metadata, metadataAgentSystemPromptPolicyDirective)

	policy := e.getAgentSystemPromptPolicy()
	if policy == nil {
		return nil
	}
	cfg, ok := e.agentConfigForSystemPromptPolicy(msg.Metadata)
	if !ok {
		return nil
	}
	directive, err := policy.CompileTerminalDirective(ctx, AgentSystemPromptPolicyInput{
		Agent: *cfg, UserQuery: msg.Content,
	})
	if err != nil {
		return fmt.Errorf("agent %q system prompt policy: %w", cfg.Name, err)
	}
	directive.Key = strings.TrimSpace(directive.Key)
	directive.Content = strings.TrimSpace(directive.Content)
	if directive.Content == "" {
		return nil
	}
	if directive.Key == "" || strings.ContainsAny(directive.Key, "\r\n") {
		return fmt.Errorf("agent %q system prompt policy 返回了非法 key", cfg.Name)
	}
	msg.Metadata[metadataAgentSystemPromptPolicyKey] = directive.Key
	msg.Metadata[metadataAgentSystemPromptPolicyDirective] = directive.Content
	return nil
}

func (e *ReActEngine) agentConfigForSystemPromptPolicy(metadata map[string]string) (*agentrouter.AgentConfig, bool) {
	if metadata == nil {
		return nil, false
	}
	roleName := strings.TrimSpace(metadata["role"])
	names := []string{
		roleName,
		strings.TrimSpace(metadata["routed_agent"]),
		strings.TrimSpace(metadata["pinned_agent"]),
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" || strings.EqualFold(name, "default") {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		if name == roleName {
			if _, builtIn := e.factory.GetRole(name); builtIn {
				continue
			}
		}
		if router := e.getAgentRouter(); router != nil {
			if cfg, ok := router.GetAgent(name); ok && cfg != nil {
				return cfg, true
			}
		}
	}
	return nil, false
}

func appendPreparedAgentSystemPromptDirective(base string, metadata map[string]string) string {
	if metadata == nil {
		return base
	}
	key := strings.TrimSpace(metadata[metadataAgentSystemPromptPolicyKey])
	directive := strings.TrimSpace(metadata[metadataAgentSystemPromptPolicyDirective])
	if key == "" || directive == "" {
		return base
	}
	return strings.TrimRight(base, "\n") +
		"\n\n[Agent system prompt terminal policy: " + key + "]\n" + directive
}

