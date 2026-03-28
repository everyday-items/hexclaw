package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// SpawnSkill spawns a child agent to handle a long-running task.
//
// Unlike OrchestrateSkill (parallel fan-out), SpawnSkill runs a single
// sub-agent with its own budget. The parent waits for the child to complete.
type SpawnSkill struct {
	executeFunc func(ctx context.Context, agentName, task string) (string, error)
}

// NewSpawnSkill creates a SpawnSkill.
func NewSpawnSkill(execFn func(ctx context.Context, agentName, task string) (string, error)) *SpawnSkill {
	return &SpawnSkill{executeFunc: execFn}
}

func (s *SpawnSkill) Name() string        { return "spawn_agent" }
func (s *SpawnSkill) Description() string  { return "Spawn a child agent to handle a task" }
func (s *SpawnSkill) Match(_ string) bool  { return false }

func (s *SpawnSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("spawn_agent",
		"Spawn a child agent to handle a specific task. Use when a subtask requires a different agent's expertise and you want to wait for the result.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"agent_name": {Type: "string", Description: "Agent to spawn (e.g., 'coder', 'researcher')"},
				"task":       {Type: "string", Description: "Task description for the child agent"},
				"timeout":    {Type: "string", Description: "Timeout (e.g., '5m'), default 5m"},
			},
			Required: []string{"agent_name", "task"},
		})
}

func (s *SpawnSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	agentName, _ := args["agent_name"].(string)
	task, _ := args["task"].(string)
	timeoutStr, _ := args["timeout"].(string)

	if agentName == "" || task == "" {
		return nil, fmt.Errorf("agent_name and task are required")
	}

	if s.executeFunc == nil {
		return nil, fmt.Errorf("agent executor not available")
	}

	timeout := 5 * time.Minute
	if timeoutStr != "" {
		if d, err := time.ParseDuration(timeoutStr); err == nil {
			timeout = d
		}
	}

	childCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	output, err := s.executeFunc(childCtx, agentName, task)
	if err != nil {
		if childCtx.Err() != nil {
			return &skill.Result{
				Content: fmt.Sprintf("Agent '%s' timed out after %v. Partial result: %s", agentName, timeout, output),
			}, nil
		}
		return nil, fmt.Errorf("agent '%s' failed: %w", agentName, err)
	}

	return &skill.Result{
		Content: output,
		Metadata: map[string]string{
			"agent": agentName,
		},
	}, nil
}
