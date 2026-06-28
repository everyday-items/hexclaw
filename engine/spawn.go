package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// SpawnSkill spawns a child agent to handle a long-running task.
//
// Unlike OrchestrateSkill (parallel fan-out), SpawnSkill runs a single
// sub-agent with its own budget. The parent waits for the child to complete.
// 支持工具收窄(tools)、run/session 模式与 session 续聊，并把每次运行登记到注册表。
type SpawnSkill struct {
	executeFunc SubAgentExecFunc
	registry    *SubAgentRegistry
}

// NewSpawnSkill creates a SpawnSkill.
func NewSpawnSkill(execFn SubAgentExecFunc, reg *SubAgentRegistry) *SpawnSkill {
	return &SpawnSkill{executeFunc: execFn, registry: reg}
}

func (s *SpawnSkill) Name() string        { return "spawn_agent" }
func (s *SpawnSkill) Description() string { return "Spawn a child agent to handle a task" }
func (s *SpawnSkill) Match(_ string) bool { return false }

func (s *SpawnSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("spawn_agent",
		"Spawn a child agent to handle a specific task and wait for its result. Optionally restrict the child's tools, or run in session mode to keep the child alive for follow-up turns.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"agent_name": {Type: "string", Description: "Agent to spawn (e.g., 'coder', 'researcher')"},
				"task":       {Type: "string", Description: "Task description for the child agent"},
				"timeout":    {Type: "string", Description: "Timeout (e.g., '5m'), default 5m"},
				"tools": {
					Type:        "array",
					Description: "Optional whitelist of tool names the child may use (narrows inherited tools; the child can never exceed your own tools).",
					Items:       &llm.Schema{Type: "string"},
				},
				"mode":       {Type: "string", Description: "'run' (one-shot, default) or 'session' (keep child alive for follow-up)"},
				"session_id": {Type: "string", Description: "Continue a prior session-mode child by its session id (multi-turn)."},
			},
			Required: []string{"agent_name", "task"},
		})
}

func (s *SpawnSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	agentName, _ := args["agent_name"].(string)
	task, _ := args["task"].(string)
	timeoutStr, _ := args["timeout"].(string)
	mode, _ := args["mode"].(string)
	sessionID, _ := args["session_id"].(string)
	reqAllow := stringSliceArg(args["tools"])

	if agentName == "" || task == "" {
		return nil, fmt.Errorf("agent_name and task are required")
	}

	// 深度闸（对齐 OpenClaw maxSpawnDepth / Hermes delegation.max_spawn_depth）：到顶的
	// 子 Agent（leaf）不得再派生。返回正常结果而非 error，让 LLM 据此自己完成子任务。
	if spawnDepthExceeded(ctx) {
		return &skill.Result{Content: spawnDepthLimitMessage(ctx, "spawn_agent")}, nil
	}
	if s.executeFunc == nil {
		return nil, fmt.Errorf("agent executor not available")
	}

	// 跨树总量闸：顶层 spawn 即根铸预算、嵌套 spawn/orchestrate 复用同一预算。占一个名额，触顶即优雅拒派
	// （返回正常结果而非 error，让 LLM 自己完成）。spawn 的子经 Process 透传同一计数器，跨树累加。
	ctx = withFanoutBudget(ctx)
	if !fanoutBudgetFromContext(ctx).tryAcquire() {
		return &skill.Result{Content: fanoutBudgetExceededMessage("spawn_agent")}, nil
	}

	timeout := 5 * time.Minute
	if timeoutStr != "" {
		if d, err := time.ParseDuration(timeoutStr); err == nil {
			timeout = d
		}
	}

	spec := childSpec(ctx, "spawn-"+idgen.NanoID(), agentName, task, reqAllow, nil, mode, sessionID)
	s.registry.Start(newSubAgentRecord(ctx, spec))

	start := time.Now()
	// #1 重试/退避 + #8 per-尝试超时：瞬时错误(429/超时/抖动)自动重试。
	res, err := runSubAgentWithRetry(ctx, s.executeFunc, spec, timeout)
	elapsed := time.Since(start)

	if err != nil {
		if isDeadlineErr(err) {
			report := SubAgentReport{Agent: agentName, Status: subAgentStatusTimeout, Duration: fmtSubAgentDuration(elapsed), Output: res.Output}
			s.registry.Finish(spec.RunID, subAgentStatusTimeout, res.Output, "timeout", res.SessionID)
			return &skill.Result{
				Content: fmt.Sprintf("Agent '%s' timed out after %v. Partial result: %s", agentName, timeout, res.Output) + encodeSubAgentReports([]SubAgentReport{report}),
			}, nil
		}
		// 优雅降级（设计⑦）：非超时失败也返回部分结果 + 错误回执，与 orchestrate 一致——不返回硬错中断
		// 父 Agent 的工具循环，让主 Agent 据回执自行重试/绕行。
		s.registry.Finish(spec.RunID, subAgentStatusError, "", err.Error(), res.SessionID)
		report := SubAgentReport{Agent: agentName, Status: subAgentStatusError, Error: err.Error(), Duration: fmtSubAgentDuration(elapsed), Output: res.Output}
		return &skill.Result{
			Content: fmt.Sprintf("Agent '%s' 执行失败：%v。可换一种方式重试或自行完成该子任务。", agentName, err) + encodeSubAgentReports([]SubAgentReport{report}),
		}, nil
	}

	s.registry.Finish(spec.RunID, subAgentStatusOK, res.Output, "", res.SessionID)
	report := newSubAgentReport(agentName, res.Output, nil, elapsed)
	content := res.Output + encodeSubAgentReports([]SubAgentReport{report})
	// session-mode：把子会话 id 透给 LLM，便于后续 spawn_agent(session_id=…) 续聊。
	if isSessionMode(mode) && res.SessionID != "" {
		content += fmt.Sprintf("\n\n[session-mode] child session_id=%s — 后续可用 spawn_agent(session_id=\"%s\") 继续与该子 Agent 对话。", res.SessionID, res.SessionID)
	}
	return &skill.Result{
		Content:  content,
		Metadata: map[string]string{"agent": agentName, "subagent_run_id": spec.RunID, "subagent_session_id": res.SessionID},
	}, nil
}

func isSessionMode(mode string) bool { return mode == "session" }

// stringSliceArg 把工具调用里的数组参数（[]any of string）转成 []string。
func stringSliceArg(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
