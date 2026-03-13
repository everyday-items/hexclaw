package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/everyday-items/hexagon"
	"github.com/everyday-items/hexclaw/agents"
	"github.com/everyday-items/hexclaw/llmrouter"
)

// ExecutionMode 多 Agent 协作模式
type ExecutionMode string

const (
	// ModePipeline 顺序管道: A → B → C
	ModePipeline ExecutionMode = "pipeline"
	// ModeParallel 并行扇出: A,B,C → 合并
	ModeParallel ExecutionMode = "parallel"
	// ModeRouter 路由分发: 首个 Agent 决定由谁处理
	ModeRouter ExecutionMode = "router"
)

// Workflow 多 Agent 协作工作流
type Workflow struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Mode        ExecutionMode `json:"mode"`
	Steps       []Step        `json:"steps"`
}

// Step 工作流步骤
type Step struct {
	AgentRole   string `json:"agent_role"`
	Provider    string `json:"provider"`
	Instruction string `json:"instruction"`
}

// StepResult 单步执行结果
type StepResult struct {
	AgentRole string        `json:"agent_role"`
	Content   string        `json:"content"`
	Duration  time.Duration `json:"duration"`
	Error     error         `json:"error,omitempty"`
}

// OrchestratorResult 编排执行结果
type OrchestratorResult struct {
	WorkflowName  string        `json:"workflow_name"`
	FinalContent  string        `json:"final_content"`
	Steps         []StepResult  `json:"steps"`
	TotalDuration time.Duration `json:"total_duration"`
}

// Orchestrator 多 Agent 编排器
//
// 基于 Hexagon compose (Pipe/Parallel/Branch) 的理念，
// 提供 HexClaw Agent 层面的协作编排。
type Orchestrator struct {
	factory   *agents.Factory
	router    *llmrouter.Selector
	mu        sync.RWMutex
	workflows map[string]*Workflow
}

// NewOrchestrator 创建编排器
func NewOrchestrator(factory *agents.Factory, router *llmrouter.Selector) *Orchestrator {
	return &Orchestrator{
		factory:   factory,
		router:    router,
		workflows: make(map[string]*Workflow),
	}
}

// RegisterWorkflow 注册工作流
func (o *Orchestrator) RegisterWorkflow(wf *Workflow) error {
	if wf.Name == "" {
		return fmt.Errorf("工作流名称不能为空")
	}
	if len(wf.Steps) == 0 {
		return fmt.Errorf("工作流至少需要一个步骤")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.workflows[wf.Name] = wf
	return nil
}

// RemoveWorkflow 移除工作流
func (o *Orchestrator) RemoveWorkflow(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.workflows, name)
}

// GetWorkflow 获取工作流
func (o *Orchestrator) GetWorkflow(name string) (*Workflow, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	wf, ok := o.workflows[name]
	return wf, ok
}

// ListWorkflows 列出所有工作流
func (o *Orchestrator) ListWorkflows() []*Workflow {
	o.mu.RLock()
	defer o.mu.RUnlock()
	wfs := make([]*Workflow, 0, len(o.workflows))
	for _, wf := range o.workflows {
		wfs = append(wfs, wf)
	}
	return wfs
}

// Execute 执行工作流
func (o *Orchestrator) Execute(ctx context.Context, workflowName string, input string) (*OrchestratorResult, error) {
	o.mu.RLock()
	wf, ok := o.workflows[workflowName]
	o.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("工作流 %s 不存在", workflowName)
	}

	start := time.Now()
	var result *OrchestratorResult
	var err error

	switch wf.Mode {
	case ModePipeline:
		result, err = o.executePipeline(ctx, wf, input)
	case ModeParallel:
		result, err = o.executeParallel(ctx, wf, input)
	case ModeRouter:
		result, err = o.executeRouter(ctx, wf, input)
	default:
		return nil, fmt.Errorf("未知执行模式: %s", wf.Mode)
	}

	if err != nil {
		return nil, err
	}
	result.WorkflowName = wf.Name
	result.TotalDuration = time.Since(start)
	return result, nil
}

// executePipeline 顺序管道执行
func (o *Orchestrator) executePipeline(ctx context.Context, wf *Workflow, input string) (*OrchestratorResult, error) {
	var steps []StepResult
	current := input

	for _, step := range wf.Steps {
		start := time.Now()
		prompt := current
		if step.Instruction != "" {
			prompt = step.Instruction + "\n\n" + current
		}

		content, err := o.runAgent(ctx, step, prompt)
		sr := StepResult{
			AgentRole: step.AgentRole,
			Content:   content,
			Duration:  time.Since(start),
			Error:     err,
		}
		steps = append(steps, sr)

		if err != nil {
			return &OrchestratorResult{Steps: steps}, fmt.Errorf("步骤 %s 失败: %w", step.AgentRole, err)
		}
		current = content
	}

	return &OrchestratorResult{
		FinalContent: current,
		Steps:        steps,
	}, nil
}

// executeParallel 并行扇出执行
func (o *Orchestrator) executeParallel(ctx context.Context, wf *Workflow, input string) (*OrchestratorResult, error) {
	results := make([]StepResult, len(wf.Steps))
	var wg sync.WaitGroup

	for i, step := range wf.Steps {
		wg.Add(1)
		go func(idx int, s Step) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results[idx] = StepResult{
						AgentRole: s.AgentRole,
						Error:     fmt.Errorf("agent panic: %v", r),
					}
				}
			}()
			start := time.Now()
			prompt := input
			if s.Instruction != "" {
				prompt = s.Instruction + "\n\n" + input
			}
			content, err := o.runAgent(ctx, s, prompt)
			results[idx] = StepResult{
				AgentRole: s.AgentRole,
				Content:   content,
				Duration:  time.Since(start),
				Error:     err,
			}
		}(i, step)
	}
	wg.Wait()

	// 合并结果
	var parts []string
	for _, r := range results {
		if r.Error != nil {
			return &OrchestratorResult{Steps: results}, fmt.Errorf("步骤 %s 失败: %w", r.AgentRole, r.Error)
		}
		parts = append(parts, fmt.Sprintf("=== %s ===\n%s", r.AgentRole, r.Content))
	}

	return &OrchestratorResult{
		FinalContent: strings.Join(parts, "\n\n"),
		Steps:        results,
	}, nil
}

// executeRouter 路由分发执行
func (o *Orchestrator) executeRouter(ctx context.Context, wf *Workflow, input string) (*OrchestratorResult, error) {
	if len(wf.Steps) < 2 {
		return nil, fmt.Errorf("路由模式至少需要 2 个步骤（1 个路由器 + 1 个执行者）")
	}

	// 第一步：路由器决定由谁处理
	routerStep := wf.Steps[0]
	routerPrompt := fmt.Sprintf("根据以下请求，从可选角色中选择最合适的一个来处理。只返回角色名称，不要解释。\n\n可选角色: %s\n\n请求: %s",
		stepRoles(wf.Steps[1:]), input)

	start := time.Now()
	chosen, err := o.runAgent(ctx, routerStep, routerPrompt)
	routerResult := StepResult{
		AgentRole: routerStep.AgentRole,
		Content:   chosen,
		Duration:  time.Since(start),
		Error:     err,
	}
	if err != nil {
		return &OrchestratorResult{Steps: []StepResult{routerResult}}, err
	}

	// 查找选中的步骤
	chosen = strings.TrimSpace(chosen)
	var targetStep *Step
	for _, s := range wf.Steps[1:] {
		if strings.EqualFold(s.AgentRole, chosen) {
			targetStep = &s
			break
		}
	}
	if targetStep == nil {
		targetStep = &wf.Steps[1] // 默认用第二个
	}

	// 第二步：执行选中的 Agent
	start = time.Now()
	content, err := o.runAgent(ctx, *targetStep, input)
	execResult := StepResult{
		AgentRole: targetStep.AgentRole,
		Content:   content,
		Duration:  time.Since(start),
		Error:     err,
	}

	return &OrchestratorResult{
		FinalContent: content,
		Steps:        []StepResult{routerResult, execResult},
	}, err
}

// runAgent 执行单个 Agent
func (o *Orchestrator) runAgent(ctx context.Context, step Step, input string) (string, error) {
	provider, _, err := o.router.Route(ctx)
	if err != nil {
		return "", fmt.Errorf("llm 路由失败: %w", err)
	}

	agent, err := o.factory.CreateAgent(step.AgentRole, provider)
	if err != nil {
		return "", fmt.Errorf("创建 Agent %s 失败: %w", step.AgentRole, err)
	}

	output, err := agent.Run(ctx, hexagon.Input{Query: input})
	if err != nil {
		return "", err
	}
	return output.Content, nil
}

func stepRoles(steps []Step) string {
	var roles []string
	for _, s := range steps {
		roles = append(roles, s.AgentRole)
	}
	return strings.Join(roles, ", ")
}
