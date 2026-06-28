package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// OrchestrateSkill Orchestrator 闭环: 派发→并行→收集→汇总
//
// 主 Agent 调用 orchestrate 将子任务分发给多个专业 Agent，并行执行后收集结果汇总。
// 对标 Claude Code Workflow + OpenClaw fan-out。支持每子任务工具收窄、注册表登记与续接
// （resume_run_id：复用上次已成功的子 Agent，只重跑失败/未达的）。
type OrchestrateSkill struct {
	executeFunc SubAgentExecFunc
	registry    *SubAgentRegistry
}

// NewOrchestrateSkill 创建 Orchestrate 工具
func NewOrchestrateSkill(execFn SubAgentExecFunc, reg *SubAgentRegistry) *OrchestrateSkill {
	return &OrchestrateSkill{executeFunc: execFn, registry: reg}
}

func (o *OrchestrateSkill) Name() string { return "orchestrate" }
func (o *OrchestrateSkill) Description() string {
	return "Dispatch subtasks to specialized agents in parallel, collect results"
}
func (o *OrchestrateSkill) Match(_ string) bool { return false }

func (o *OrchestrateSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("orchestrate",
		"Dispatch subtasks to specialized agents in parallel, wait for all results, and return combined output. Use when a task needs multiple agents to collaborate. Pass resume_run_id to retry a prior run, reusing already-succeeded subagents.",
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
							"tools": {
								Type:        "array",
								Description: "Optional whitelist of tools this subagent may use (narrows your own tools).",
								Items:       &llm.Schema{Type: "string"},
							},
						},
						Required: []string{"agent", "task"},
					},
				},
				"resume_run_id": {Type: "string", Description: "Resume a prior orchestrate run id: reuse succeeded subagents, re-run only failed/unreached ones."},
				"goal":          {Type: "string", Description: "Optional overall objective these subtasks serve. Required to enable the supervisor feedback loop (max_rounds>1): after each round a supervisor judges the goal and dispatches follow-up subtasks if unmet."},
				"max_rounds":    {Type: "integer", Description: "Optional max supervisor rounds (default 1 = single fan-out). With a goal and max_rounds>1, results are iteratively refined until the goal is met or the cap is hit."},
			},
			Required: []string{"subtasks"},
		})
}

// subtask 子任务定义
type subtask struct {
	Agent string
	Task  string
	Tools []string
}

// agentResult 子 Agent 执行结果
type agentResult struct {
	Agent    string
	Task     string
	Output   string
	Err      error
	Duration time.Duration
	Reused   bool // 续接复用（跳过重跑）
}

func (o *OrchestrateSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	rawSubtasks, ok := args["subtasks"].([]any)
	if !ok || len(rawSubtasks) == 0 {
		return nil, fmt.Errorf("subtasks array is required")
	}

	// 深度闸（对齐 OpenClaw maxSpawnDepth）：到顶的 leaf 子 Agent 不得再 fan-out。
	if spawnDepthExceeded(ctx) {
		return &skill.Result{Content: spawnDepthLimitMessage(ctx, "orchestrate")}, nil
	}

	// 解析子任务（含每子任务工具收窄）
	var subtasks []subtask
	for _, raw := range rawSubtasks {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		agent, _ := m["agent"].(string)
		task, _ := m["task"].(string)
		if agent != "" && task != "" {
			subtasks = append(subtasks, subtask{Agent: agent, Task: task, Tools: stringSliceArg(m["tools"])})
		}
	}
	if len(subtasks) == 0 {
		return nil, fmt.Errorf("no valid subtasks found")
	}

	// 数量闸（对齐 OpenClaw maxChildrenPerAgent）：单次拆分超上限即截断，挡住「总量失控」。
	truncatedNote := ""
	if len(subtasks) > maxChildrenPerAgent {
		truncatedNote = fmt.Sprintf("\n\n> ⚠️ 已截断：请求 %d 个子任务，超过单次上限 %d，仅执行前 %d 个；其余请分批或自行处理。\n", len(subtasks), maxChildrenPerAgent, maxChildrenPerAgent)
		subtasks = subtasks[:maxChildrenPerAgent]
	}

	// 跨树总量闸：顶层 orchestrate 即根，铸一个共享预算挂到 ctx；嵌套 orchestrate/spawn 复用同一预算，
	// 跨深度跨监工轮累加。childCtx 由此 ctx 派生 → 经 executeFunc→Process 把同一计数器透传给所有后代。
	ctx = withFanoutBudget(ctx)

	// 登记本次 orchestrate 父运行；子运行 ParentID 指向它（构成派生树，支撑续接）。
	orchRunID := "orch-" + idgen.NanoID()
	o.registry.Start(&SubAgentRunRecord{
		ID:     orchRunID,
		Agent:  "orchestrate",
		Role:   roleForDepth(spawnDepthFromContext(ctx)),
		Depth:  spawnDepthFromContext(ctx),
		Mode:   "run",
		Status: subAgentStatusRunning,
	})
	childCtx := withCurrentRunID(ctx, orchRunID)

	// P2 防卡死：给整次 orchestrate（fan-out + supervisor 多轮 + reduce 合成）套一个总墙钟上限。
	// 到点取消所有在飞子 Agent——单用户桌面前别把学生晾着干等。纯延迟保护，非成本/安全闸。
	runCtx, cancelRun := context.WithTimeout(childCtx, orchestrateMaxWall)
	defer cancelRun()

	// 续接：resume_run_id 指向上次 orchestrate 运行——其已成功的子 Agent 直接复用，不重跑。
	// 🟢 key 碰撞修复：上次运行可能有多个相同 (agent,task) 的成功子，map[key]→单值会只留最后一条、
	// 漏复用其余、逼着它们重跑。改为按 key 排队 (FIFO slice)，同 key 的多个成功子各被复用一次。
	reuse := map[string][]agentResult{}
	if rid, _ := args["resume_run_id"].(string); rid != "" && o.registry != nil {
		for _, ch := range o.registry.Children(rid) {
			if ch.Status == subAgentStatusOK {
				k := ch.Agent + "\x00" + ch.Task
				reuse[k] = append(reuse[k], agentResult{Agent: ch.Agent, Task: ch.Task, Output: ch.Output, Reused: true})
			}
		}
	}

	// #5 supervisor 反馈环：传 goal + max_rounds>1 时，每轮 fan-out 后由监工对照总目标评判——未达成
	// 则补派针对性子任务进下一轮，直至达成/无后续/触顶轮数/预算尽。默认 max_rounds=1（或无 goal）
	// 退化为既有「一次散活」单轮。
	goal, _ := args["goal"].(string)
	maxRounds := clampRounds(intArg(args["max_rounds"], 1))

	var allCollected []agentResult
	var allDispatched []subtask // 跨轮累计的全部已派子任务（供完成度统计 + 超时补齐）
	roundSubtasks := subtasks
	roundReuse := reuse
	for round := 1; round <= maxRounds; round++ {
		allDispatched = append(allDispatched, roundSubtasks...)
		allCollected = append(allCollected, o.runRound(runCtx, roundSubtasks, roundReuse)...)
		roundReuse = nil // 续接复用只作用于首轮

		// 还有下一轮？未触顶 + ctx 未止（含墙钟到点）+ 有 goal 可评判。
		if round == maxRounds || runCtx.Err() != nil || strings.TrimSpace(goal) == "" {
			break
		}
		dec := o.supervise(runCtx, goal, allCollected)
		if dec.Done || len(dec.Next) == 0 {
			break
		}
		// 程序级去重（设计③）：监工补派里剔除「已派过的」(agent,task)——不只靠 prompt 口头约束，
		// 防逐轮重派同一缺口空耗。全去重后无新活 = 收工。
		next := dedupSubtasks(dec.Next, allDispatched)
		if len(next) == 0 {
			break
		}
		if len(next) > maxChildrenPerAgent { // 数量闸：每轮补派同样受单次拆分上限约束
			next = next[:maxChildrenPerAgent]
		}
		roundSubtasks = next
	}

	// 汇总：逐子拼接（裸拼接基线）+ 收集成功子供 reduce 合成（跨所有轮）。
	var sb strings.Builder
	completed := 0
	var successful []agentResult
	for _, r := range allCollected {
		tag := ""
		if r.Reused {
			tag = "（续接复用）"
		}
		sb.WriteString(fmt.Sprintf("## %s Agent%s\n", r.Agent, tag))
		sb.WriteString(fmt.Sprintf("**Task**: %s\n\n", r.Task))
		switch {
		case r.Err != nil:
			sb.WriteString(fmt.Sprintf("**Error**: %v\n\n", r.Err))
		default:
			sb.WriteString(r.Output + "\n\n")
			completed++
			successful = append(successful, r)
		}
	}

	// 尾注（完成度）与正文分离：使其能附在「裸拼接」或「reduce 合成」之后。
	var tb strings.Builder
	if completed < len(allDispatched) {
		fmt.Fprintf(&tb, "\n---\n[%d/%d subtasks completed", completed, len(allDispatched))
		if len(allCollected) < len(allDispatched) {
			fmt.Fprintf(&tb, ", %d timed out", len(allDispatched)-len(allCollected))
		}
		tb.WriteString("]\n")
	}

	// #4 reduce 合成：满足条件时用 synthesizer 归并（含冲突检测）替代裸拼接正文；否则回退裸拼接。
	bodyText := sb.String()
	if synth := o.maybeSynthesize(runCtx, successful); synth != "" {
		bodyText = synth
	}

	// 结构化回执（Ph2，对齐 OpenClaw announce）：编码进哨兵块供前端折叠协作面板渲染（跨所有轮）。
	reports := make([]SubAgentReport, 0, len(allDispatched))
	for _, r := range allCollected {
		reports = append(reports, newSubAgentReport(r.Agent, r.Output, r.Err, r.Duration))
	}
	if len(allCollected) < len(allDispatched) {
		matched := make([]bool, len(allDispatched))
		for _, r := range allCollected {
			for i, st := range allDispatched {
				if !matched[i] && st.Agent == r.Agent && st.Task == r.Task {
					matched[i] = true
					break
				}
			}
		}
		for i, st := range allDispatched {
			if !matched[i] {
				reports = append(reports, SubAgentReport{Agent: st.Agent, Status: subAgentStatusTimeout})
			}
		}
	}

	o.registry.Finish(orchRunID, subAgentStatusOK, fmt.Sprintf("%d/%d subtasks completed", completed, len(allDispatched)), "", "")
	return &skill.Result{
		Content:  bodyText + tb.String() + truncatedNote + encodeSubAgentReports(reports),
		Metadata: map[string]string{"orchestrate_run_id": orchRunID},
	}, nil
}

// dedupSubtasks 从 next 里剔除与 dispatched 中任一 (agent,task) 相同的子任务（设计③：监工补派去重）。
func dedupSubtasks(next, dispatched []subtask) []subtask {
	seen := make(map[string]bool, len(dispatched))
	for _, st := range dispatched {
		seen[st.Agent+"\x00"+st.Task] = true
	}
	out := next[:0:0]
	for _, st := range next {
		k := st.Agent + "\x00" + st.Task
		if seen[k] {
			continue
		}
		seen[k] = true // 同一轮内的重复也去掉
		out = append(out, st)
	}
	return out
}

// runRound 跑一轮 fan-out：有界并行执行 subtasks（reuse 非空时复用其中已成功的子，不重跑），收集结果。
// 由 supervisor 反馈环（#5）逐轮调用。ctx 取消时尽快返回已收到的部分（未返回的留给调用方补齐超时）。
func (o *OrchestrateSkill) runRound(ctx context.Context, subtasks []subtask, reuse map[string][]agentResult) []agentResult {
	results := make(chan agentResult, len(subtasks))
	budget := fanoutBudgetFromContext(ctx) // 跨树总量闸：真派发前逐个扣减，触顶即拒（复用不占名额）
	var toRun []subtask
	for _, st := range subtasks {
		if reuse != nil {
			k := st.Agent + "\x00" + st.Task
			if q := reuse[k]; len(q) > 0 {
				results <- q[0]  // 复用：立即入结果（不占并发槽、不调模型、不扣预算）
				reuse[k] = q[1:] // FIFO 出队：同 key 的下一个相同子任务复用下一条
				continue
			}
		}
		// 总量闸：触顶则拒派该子任务（返回带 budget 错误的结果，计入完成度统计为未完成），不真起 goroutine。
		if !budget.tryAcquire() {
			results <- agentResult{Agent: st.Agent, Task: st.Task, Err: errFanoutBudgetExceeded}
			continue
		}
		toRun = append(toRun, st)
	}

	// 有界并行（对齐 Hermes max_concurrent_children / Claude Code Workflow 并发上限）。
	limit := maxOrchestrateConcurrency
	if len(toRun) < limit {
		limit = len(toRun)
	}
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	for _, st := range toRun {
		go func(st subtask) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- agentResult{Agent: st.Agent, Task: st.Task, Err: ctx.Err()}
				return
			}
			if o.executeFunc == nil {
				results <- agentResult{Agent: st.Agent, Task: st.Task, Err: fmt.Errorf("executor not available")}
				return
			}
			spec := childSpec(ctx, "sub-"+idgen.NanoID(), st.Agent, st.Task, st.Tools, nil, "run", "")
			o.registry.Start(newSubAgentRecord(ctx, spec))
			start := time.Now()
			// #1 重试/退避 + #8 per-子超时：瞬时错误(429/超时/抖动)自动重试，单子独立超时不拖整批。
			res, err := runSubAgentWithRetry(ctx, o.executeFunc, spec, defaultSubAgentTimeout)
			status := subAgentStatusOK
			errStr := ""
			if err != nil {
				status, errStr = subAgentStatusError, err.Error()
			}
			o.registry.Finish(spec.RunID, status, res.Output, errStr, res.SessionID)
			results <- agentResult{Agent: st.Agent, Task: st.Task, Output: res.Output, Err: err, Duration: time.Since(start)}
		}(st)
	}

	// 收集结果（含超时处理）：ctx 取消即停收，剩余在后台抽干避免 goroutine 泄漏。
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
	if remaining > 0 {
		go func() {
			for i := 0; i < remaining; i++ {
				<-results
			}
		}()
	}
	return collected
}
