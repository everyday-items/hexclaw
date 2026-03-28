package engine

import (
	"context"
	"fmt"
	"log"

	"github.com/hexagon-codes/ai-core/llm"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/skill"
)

// HandoffSkill 单向 Agent 切换工具
//
// LLM 调用 transfer_to_agent 将对话转交给另一个 Agent。
// 对标 OpenAI Agents SDK 的 Handoff 模式。
type HandoffSkill struct {
	dispatcher *agentrouter.Dispatcher
}

// NewHandoffSkill 创建 Handoff 工具
func NewHandoffSkill(dispatcher *agentrouter.Dispatcher) *HandoffSkill {
	return &HandoffSkill{dispatcher: dispatcher}
}

func (h *HandoffSkill) Name() string        { return "transfer_to_agent" }
func (h *HandoffSkill) Description() string  { return "Transfer conversation to a specialized agent" }
func (h *HandoffSkill) Match(_ string) bool  { return false }

func (h *HandoffSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("transfer_to_agent",
		"Transfer the current conversation to a specialized agent. Use when the user's request requires expertise you don't have.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"agent_name": {Type: "string", Description: "Target agent name"},
				"context":    {Type: "string", Description: "Brief context for the target agent"},
			},
			Required: []string{"agent_name", "context"},
		})
}

func (h *HandoffSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	agentName, _ := args["agent_name"].(string)
	handoffCtx, _ := args["context"].(string)

	if agentName == "" {
		return nil, fmt.Errorf("agent_name is required")
	}

	if h.dispatcher == nil {
		return nil, fmt.Errorf("agent router not available")
	}

	log.Printf("Agent handoff: → %s (context: %s)", agentName, handoffCtx)

	return &skill.Result{
		Content: fmt.Sprintf("Conversation transferred to agent '%s'. Context: %s", agentName, handoffCtx),
		Metadata: map[string]string{
			"handoff_to":      agentName,
			"handoff_context": handoffCtx,
		},
	}, nil
}
