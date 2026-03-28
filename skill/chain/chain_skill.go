package chain

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// ChainSkill wraps a ChainDef as a Skill, making it callable by LLM.
//
// The LLM sees it as a single tool. Internally it runs multiple steps.
type ChainSkill struct {
	def      *ChainDef
	executor *Executor
}

// NewChainSkill wraps a chain definition as a Skill.
func NewChainSkill(def *ChainDef, executor *Executor) *ChainSkill {
	return &ChainSkill{def: def, executor: executor}
}

func (s *ChainSkill) Name() string        { return s.def.Name }
func (s *ChainSkill) Description() string  { return s.def.Description }
func (s *ChainSkill) Match(_ string) bool  { return false }

func (s *ChainSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(s.def.Name, s.def.Description, &llm.Schema{
		Type: "object",
		Properties: map[string]*llm.Schema{
			"input": {Type: "string", Description: "Input for the chain"},
		},
		Required: []string{"input"},
	})
}

func (s *ChainSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	input, _ := args["input"].(string)
	if input == "" {
		return nil, fmt.Errorf("input is required")
	}

	output, _, err := s.executor.Run(ctx, s.def, input)
	if err != nil {
		return &skill.Result{Content: output}, err
	}
	return &skill.Result{Content: output}, nil
}
