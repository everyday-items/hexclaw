package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// OrchestrateSkill Orchestrator 闭环: 派发→并行→收集→汇总
//
// 主 Agent 调用 orchestrate 将子任务分发给多个专业 Agent，
// 并行执行后收集结果，返回给主 Agent 做最终汇总。
// 对标 Claude Code Team Lead + LangGraph Supervisor。
type OrchestrateSkill struct {
	// executeFunc 执行单个子任务 (注入，避免循环依赖)
	executeFunc func(ctx context.Context, agentName, task string) (string, error)
}

// NewOrchestrateSkill 创建 Orchestrate 工具
func NewOrchestrateSkill(execFn func(ctx context.Context, agentName, task string) (string, error)) *OrchestrateSkill {
	return &OrchestrateSkill{executeFunc: execFn}
}

func (o *OrchestrateSkill) Name() string        { return "orchestrate" }
func (o *OrchestrateSkill) Description() string  { return "Dispatch subtasks to specialized agents in parallel, collect results" }
func (o *OrchestrateSkill) Match(_ string) bool  { return false }

func (o *OrchestrateSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("orchestrate",
		"Dispatch subtasks to specialized agents, wait for all results, and return combined output. Use when the task requires multiple agents to collaborate.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"subtasks": {
					Type:        "array",
					Description: "List of subtasks to dispatch in parallel",
					Items: &llm.Schema{
						Type: "object",
						Properties: map[string]*llm.Schema{
							"agent": {Type: "string", Description: "Target agent name"},
							"task":  {Type: "string", Description: "Task description"},
						},
						Required: []string{"agent", "task"},
					},
				},
			},
			Required: []string{"subtasks"},
		})
}

// subtask 子任务定义
type subtask struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

// agentResult 子 Agent 执行结果
type agentResult struct {
	Agent  string
	Task   string
	Output string
	Err    error
}

func (o *OrchestrateSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	rawSubtasks, ok := args["subtasks"].([]any)
	if !ok || len(rawSubtasks) == 0 {
		return nil, fmt.Errorf("subtasks array is required")
	}

	// 解析子任务
	var subtasks []subtask
	for _, raw := range rawSubtasks {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		agent, _ := m["agent"].(string)
		task, _ := m["task"].(string)
		if agent != "" && task != "" {
			subtasks = append(subtasks, subtask{Agent: agent, Task: task})
		}
	}
	if len(subtasks) == 0 {
		return nil, fmt.Errorf("no valid subtasks found")
	}

	// 并行执行
	results := make(chan agentResult, len(subtasks))
	for _, st := range subtasks {
		go func(st subtask) {
			if o.executeFunc != nil {
				output, err := o.executeFunc(ctx, st.Agent, st.Task)
				results <- agentResult{Agent: st.Agent, Task: st.Task, Output: output, Err: err}
			} else {
				results <- agentResult{Agent: st.Agent, Task: st.Task, Err: fmt.Errorf("executor not available")}
			}
		}(st)
	}

	// 收集结果 (含超时处理)
	var collected []agentResult
	remaining := len(subtasks)
collectLoop:
	for remaining > 0 {
		select {
		case r := <-results:
			collected = append(collected, r)
			remaining--
		case <-ctx.Done():
			break collectLoop
		}
	}
	// Drain remaining results to prevent goroutine leaks (buffered channel)
	if remaining > 0 {
		go func() {
			for i := 0; i < remaining; i++ {
				<-results
			}
		}()
	}

	// 汇总
	var sb strings.Builder
	completed := 0
	for _, r := range collected {
		sb.WriteString(fmt.Sprintf("## %s Agent\n", r.Agent))
		sb.WriteString(fmt.Sprintf("**Task**: %s\n\n", r.Task))
		if r.Err != nil {
			sb.WriteString(fmt.Sprintf("**Error**: %v\n\n", r.Err))
		} else {
			sb.WriteString(r.Output + "\n\n")
			completed++
		}
	}

	if completed < len(subtasks) {
		sb.WriteString(fmt.Sprintf("\n---\n[%d/%d subtasks completed", completed, len(subtasks)))
		if len(collected) < len(subtasks) {
			sb.WriteString(fmt.Sprintf(", %d timed out", len(subtasks)-len(collected)))
		}
		sb.WriteString("]\n")
	}

	return &skill.Result{Content: sb.String()}, nil
}
