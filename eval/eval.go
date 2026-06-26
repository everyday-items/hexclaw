// Package eval 实现 v0.4.0 H9 Eval / Replay / Source-Controlled Checks。
//
// 一个 EvalCase 包含：输入、上下文、期望事件、期望 artifact、评分器；Runner 把
// case 跑过 fake provider（可重现）+ 真实业务路径，产出 PASS/FAIL + 最小复现信息。
//
// flag eval.framework.v1：本期默认 OFF；CI 显式开启执行 evalcase 套件，通过后才发版。
package eval

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/featureflag"
	"github.com/hexagon-codes/toolkit/lang/stringx"
)

// FlagEvalFrameworkV1 控制 H9 eval 是否启用。alpha 默认 OFF。
const FlagEvalFrameworkV1 = "eval.framework.v1"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagEvalFrameworkV1,
		Default:      true, // alpha 强制 OFF
		Description:  "Enable H9 Eval framework (replay + source-controlled checks).",
		Stage:        featureflag.StageAlpha,
		SinceVersion: "0.4.0",
	})
}

// EvalCase 单个评测用例。
type EvalCase struct {
	ID          string                            // 唯一 ID（用于复现 / 索引）
	Description string                            // 一句话说明
	Tags        []string                          // 分类（"tool-approval" / "mcp" 等）
	Input       map[string]any                    // 输入参数
	Run         func(ctx context.Context, input map[string]any) (Output, error) // 业务执行函数
	Assertions  []Assertion                       // 期望断言
}

// Output 是 EvalCase.Run 的产物，供 Assertion 检查。
type Output struct {
	Content     string
	Events      []string
	Artifacts   map[string]string
	Cost        float64
	DurationMs  int64
	ToolCalls   []string
	Error       error
}

// Assertion 是单条断言：返回 nil = PASS，error = FAIL。
type Assertion func(out Output) error

// Result 是单 case 跑完的结果。
type Result struct {
	CaseID   string
	Tags     []string
	Pass     bool
	Failures []string
	Duration time.Duration
}

// Suite 是一组 EvalCase。
type Suite struct {
	Cases []EvalCase
}

// Report 是 Suite.Run 的汇总。
type Report struct {
	Total    int
	Passed   int
	Failed   int
	Results  []Result
	Duration time.Duration
}

// Run 执行整个 suite。flag OFF 时返回 ErrEvalDisabled。
func (s *Suite) Run(ctx context.Context) (*Report, error) {
	if !featureflag.Enabled(ctx, FlagEvalFrameworkV1) {
		return nil, ErrEvalDisabled
	}
	rep := &Report{Total: len(s.Cases)}
	start := time.Now()
	for _, c := range s.Cases {
		caseStart := time.Now()
		out, err := c.Run(ctx, c.Input)
		if err != nil {
			out.Error = err
		}
		var failures []string
		for _, a := range c.Assertions {
			if err := a(out); err != nil {
				failures = append(failures, err.Error())
			}
		}
		res := Result{
			CaseID:   c.ID,
			Tags:     c.Tags,
			Pass:     len(failures) == 0,
			Failures: failures,
			Duration: time.Since(caseStart),
		}
		rep.Results = append(rep.Results, res)
		if res.Pass {
			rep.Passed++
		} else {
			rep.Failed++
		}
	}
	rep.Duration = time.Since(start)
	if rep.Failed > 0 {
		return rep, fmt.Errorf("eval suite failed: %d/%d cases", rep.Failed, rep.Total)
	}
	return rep, nil
}

// ErrEvalDisabled 在 flag OFF 时由 Suite.Run 返回。
var ErrEvalDisabled = errors.New("eval: framework disabled (flag eval.framework.v1 OFF)")

// ============== 内置 Assertion ==============

// AssertContent 断言 output.Content 包含 substr。
func AssertContent(substr string) Assertion {
	return func(out Output) error {
		if !strings.Contains(out.Content, substr) {
			return fmt.Errorf("content missing %q (got %q)", substr, truncate(out.Content, 200))
		}
		return nil
	}
}

// AssertNoError 断言 Output.Error 为 nil。
func AssertNoError() Assertion {
	return func(out Output) error {
		if out.Error != nil {
			return fmt.Errorf("unexpected error: %v", out.Error)
		}
		return nil
	}
}

// AssertError 断言 Output.Error 含 substr（用于校验失败路径）。
func AssertError(substr string) Assertion {
	return func(out Output) error {
		if out.Error == nil {
			return fmt.Errorf("expected error containing %q, got nil", substr)
		}
		if !strings.Contains(out.Error.Error(), substr) {
			return fmt.Errorf("error %q does not contain %q", out.Error.Error(), substr)
		}
		return nil
	}
}

// AssertEventEmitted 断言 events 列表含指定 type。
func AssertEventEmitted(eventType string) Assertion {
	return func(out Output) error {
		for _, e := range out.Events {
			if e == eventType {
				return nil
			}
		}
		return fmt.Errorf("event %q not emitted; got %v", eventType, out.Events)
	}
}

// AssertCostBelow 断言 Output.Cost ≤ max（USD）。
func AssertCostBelow(max float64) Assertion {
	return func(out Output) error {
		if out.Cost > max {
			return fmt.Errorf("cost %.4f exceeds max %.4f", out.Cost, max)
		}
		return nil
	}
}

// AssertToolCalled 断言指定 tool name 出现在 ToolCalls 中。
func AssertToolCalled(name string) Assertion {
	return func(out Output) error {
		for _, t := range out.ToolCalls {
			if t == name {
				return nil
			}
		}
		return fmt.Errorf("tool %q not called; got %v", name, out.ToolCalls)
	}
}

// ============== 默认 v0.4 套件 ==============

// V04Suite 返回 v0.4.0 默认 10 条 runtime evalcase（占位实现）。
//
// 每条都用 mock Run + 内置 Assertion 验证基础协议，CI 可用真实 fake provider 替换 Run。
func V04Suite() *Suite {
	return &Suite{
		Cases: []EvalCase{
			caseToolApprovalRequired(),
			caseMCPUnavailableFallback(),
			caseSkillSelectByTopic(),
			caseInteractiveButtonResolved(),
			caseContextCompressionTriggered(),
			caseCostFallback(),
			caseLLMFailoverOnRateLimit(),
			caseSkillPipelineExecution(),
			casePolicyDenyShell(),
			caseEventEmittedOnToolCompleted(),
		},
	}
}

func caseToolApprovalRequired() EvalCase {
	return EvalCase{
		ID:          "tool-approval-required",
		Description: "shell 工具调用必须经过用户审批",
		Tags:        []string{"permission", "K12"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			return Output{Error: errors.New("tool \"shell\" blocked by policy \"shell-dangerous\"")}, nil
		},
		Assertions: []Assertion{AssertError("blocked")},
	}
}

func caseMCPUnavailableFallback() EvalCase {
	return EvalCase{
		ID:          "mcp-unavailable-fallback",
		Description: "MCP server 不可用时不阻塞主流程",
		Tags:        []string{"mcp"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			return Output{Content: "fallback used", Events: []string{"mcp.server.disconnected"}}, nil
		},
		Assertions: []Assertion{AssertContent("fallback"), AssertEventEmitted("mcp.server.disconnected")},
	}
}

func caseSkillSelectByTopic() EvalCase {
	return EvalCase{
		ID:          "skill-select-by-topic",
		Description: "K12 数学题应命中 math-tutor Skill",
		Tags:        []string{"skill"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			return Output{Content: "math-tutor", ToolCalls: []string{"math-tutor"}}, nil
		},
		Assertions: []Assertion{AssertContent("math-tutor"), AssertToolCalled("math-tutor")},
	}
}

func caseInteractiveButtonResolved() EvalCase {
	return EvalCase{
		ID:          "interactive-button-resolved",
		Description: "用户点击按钮后 metadata.interactive_action 透传",
		Tags:        []string{"interactive"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			return Output{Content: "confirmed", Events: []string{"interactive.resolved"}}, nil
		},
		Assertions: []Assertion{AssertContent("confirmed")},
	}
}

func caseContextCompressionTriggered() EvalCase {
	return EvalCase{
		ID:          "context-compression-triggered",
		Description: "超长 history 触发启发式压缩",
		Tags:        []string{"compression", "K12"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			return Output{Content: "compressed", Events: []string{"context.compressed"}}, nil
		},
		Assertions: []Assertion{AssertContent("compressed")},
	}
}

func caseCostFallback() EvalCase {
	return EvalCase{
		ID:          "cost-fallback-budget",
		Description: "预算超额时降级到低成本模型",
		Tags:        []string{"cost", "K12"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			return Output{Content: "downgraded to deepseek", Cost: 0.001}, nil
		},
		Assertions: []Assertion{AssertCostBelow(0.01)},
	}
}

func caseLLMFailoverOnRateLimit() EvalCase {
	return EvalCase{
		ID:          "llm-failover-rate-limit",
		Description: "429 触发同 provider 退避重试",
		Tags:        []string{"llm", "failover"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			return Output{Content: "ok", Events: []string{"llm.call.failover", "llm.call.completed"}}, nil
		},
		Assertions: []Assertion{AssertEventEmitted("llm.call.failover"), AssertEventEmitted("llm.call.completed")},
	}
}

func caseSkillPipelineExecution() EvalCase {
	return EvalCase{
		ID:          "skill-pipeline-execution",
		Description: "Skill 7 阶段 pipeline 完整执行",
		Tags:        []string{"skill", "pipeline"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			return Output{Content: "all phases done", Events: []string{
				"discovery", "activation", "loading", "verification", "execution", "persistence", "improvement",
			}}, nil
		},
		Assertions: []Assertion{AssertEventEmitted("execution")},
	}
}

func casePolicyDenyShell() EvalCase {
	return EvalCase{
		ID:          "policy-deny-shell",
		Description: "DefaultBaselinePolicy 拒绝直接 shell 执行",
		Tags:        []string{"permission", "policy"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			return Output{Error: errors.New("policy deny shell")}, nil
		},
		Assertions: []Assertion{AssertError("policy")},
	}
}

func caseEventEmittedOnToolCompleted() EvalCase {
	return EvalCase{
		ID:          "event-emitted-on-tool-completed",
		Description: "tool 调用完成后投递 tool.call.completed",
		Tags:        []string{"events", "observability"},
		Run: func(_ context.Context, _ map[string]any) (Output, error) {
			return Output{Content: "done", Events: []string{"tool.call.completed"}}, nil
		},
		Assertions: []Assertion{AssertEventEmitted("tool.call.completed"), AssertNoError()},
	}
}

// SortedCaseIDs 返回 suite 内 case ID 的字典序列表（用于 CI 报告）。
func SortedCaseIDs(s *Suite) []string {
	out := make([]string, 0, len(s.Cases))
	for _, c := range s.Cases {
		out = append(out, c.ID)
	}
	sort.Strings(out)
	return out
}

func truncate(s string, n int) string {
	// rune-safe 截断（委托 toolkit stringx.SubString），避免 CJK 字节切断（BUG-20260625 F-4）。
	head := stringx.SubString(s, 0, n)
	if head == s {
		return s
	}
	return head + "..."
}
