// Package engineadapter 把 hexclaw engine 的能力适配成 K12 用例层的 port 接口。
//
// 这是 clean-architecture 的 adapter 层：用例层依赖抽象 port（Solver/Grader/...），
// 真实现由本包提供，桥接到 engine/solve.go 等既有代码。composition root（wire.go）把它们注入用例。
package engineadapter

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
)

// reportSentinel 是 engine 追加在解题文本尾部的子 Agent 回执围栏（engine/subagent_report.go）。
const reportSentinel = "\n\n```hexclaw-subagents"

// SolveExecutor 是 adapter 依赖的最小接口——engine.SolveSkill 天然满足（Execute 签名一致）。
// 用接口而非具体类型，让 adapter 可脱离 engine 单测。
type SolveExecutor interface {
	Execute(ctx context.Context, args map[string]any) (*skill.Result, error)
}

// RetryGenerateFunc 是「再练一道」的轻量出题闭包（composition root 注入，BUG-20260712）。
// 内部走**单个 reasoning 子 Agent**出题+解答——不派 verifier / 不 self-consistency / 不重出，
// 返回出题正文（含回执围栏，adapter 侧剥离）。nil 时 GenerateSimilar 安全回退全链。
type RetryGenerateFunc func(ctx context.Context, subject, prompt, grade string) (string, error)

// SolveAdapter 用 engine 的 solve skill 实现用例层的 Solver + Grader 两个 port。
type SolveAdapter struct {
	exec     SolveExecutor
	retryGen RetryGenerateFunc // 轻量「再练一道」出题；nil 时回退全链
}

// SolveAdapterOption 装配可选能力（保 NewSolveAdapter 单参数向后兼容）。
type SolveAdapterOption func(*SolveAdapter)

// WithRetryGen 注入轻量出题闭包，让「再练一道」走单次 reasoning、不落全对抗验算链。
func WithRetryGen(fn RetryGenerateFunc) SolveAdapterOption {
	return func(a *SolveAdapter) { a.retryGen = fn }
}

// SetRetryGen 事后注入轻量出题闭包（composition root 在 Deps.Solver 建好后回填）。
func (a *SolveAdapter) SetRetryGen(fn RetryGenerateFunc) { a.retryGen = fn }

// NewSolveAdapter 创建 adapter。s 通常是 engine.NewSolveSkill(...) 的产物。
func NewSolveAdapter(s SolveExecutor, opts ...SolveAdapterOption) *SolveAdapter {
	a := &SolveAdapter{exec: s}
	for _, o := range opts {
		o(a)
	}
	return a
}

var (
	_ usecase.Solver          = (*SolveAdapter)(nil)
	_ usecase.Grader          = (*SolveAdapter)(nil)
	_ usecase.RetryGenerator  = (*SolveAdapter)(nil)
	_ usecase.CauseSummarizer = (*SolveAdapter)(nil)
)

// Solve 实现 usecase.Solver：调 solve skill 解题验算（透传 grade + constraint 约束年级边界），
// 把 Metadata 里的 verdict + evidence 映射成证据对象。
func (a *SolveAdapter) Solve(ctx context.Context, problem, grade, constraint string) (usecase.SolveResult, error) {
	return a.SolveSubject(ctx, "", problem, grade, constraint)
}

// SolveSubject 与 Solve 相同，并把显式学科传给 solve skill，避免非数学 eval/批改落入默认数学路由。
func (a *SolveAdapter) SolveSubject(ctx context.Context, subject, problem, grade, constraint string) (usecase.SolveResult, error) {
	// K12 作业辅导：正确性优先于延迟——显式传 self_consistency=1 关掉 solve 的「简单题跳过验算」triage
	// （engine/solve.go 的 skipVerify：trivial 纯算术直接作答、verdict=skipped→本层归一 unverifiable）。
	// 显式传参即视为调用方接管 triage，可执行题（如 4.5×2=）照常走 verifier code_exec 精算，
	// 拿到 agree + numeric_exec 强证据（badge=已程序验算），而非规划式 unverifiable（BUG-20260712 真机取证）。
	args := map[string]any{"problem": problem, "self_consistency": 1}
	if subject != "" {
		args["subject"] = subject
	}
	if grade != "" {
		args["grade"] = grade
	}
	if constraint != "" {
		args["constraint"] = constraint
	}
	res, err := a.exec.Execute(ctx, args)
	if err != nil {
		return usecase.SolveResult{}, err
	}
	if res == nil {
		return usecase.SolveResult{}, fmt.Errorf("solve adapter: empty solve result")
	}
	return usecase.SolveResult{
		Solution: stripReports(res.Content),
		Evidence: evidenceFromMeta(res.Metadata),
	}, nil
}

// GenerateSimilar 实现 usecase.RetryGenerator（BUG-20260712 治本「再练一道」轻量出题）：
// 走单次 reasoning 出题闭包（单个子 Agent，不派 verifier / 不 self-consistency / 不重出），
// 真机从全对抗链 68s 降到单次调用量级。练习变式题容错高：**不跑 code_exec 程序验算**，
// 故 verdict=unverifiable、不给强徽章（信任红线：绝不把未验算的练习题冒充「已程序验算」）。
// 未注入 retryGen 闭包时安全回退全链（SolveSubject），保证正确性不塌。
func (a *SolveAdapter) GenerateSimilar(ctx context.Context, subject, prompt, grade string) (usecase.SolveResult, error) {
	if a.retryGen == nil {
		return a.SolveSubject(ctx, subject, prompt, grade, "")
	}
	out, err := a.retryGen(ctx, subject, prompt, grade)
	if err != nil {
		return usecase.SolveResult{}, err
	}
	return usecase.SolveResult{
		Solution: stripReports(out),
		Evidence: usecase.SolveEvidence{Verdict: usecase.VerdictUnverifiable, EvidenceType: usecase.EvidenceNone},
	}, nil
}

// SummarizeCause 实现 usecase.CauseSummarizer（BUG-20260712「记一条错题」轻量错因归纳）：
// 复用已注入的单次 reasoning 出题闭包（retryGen，单个子 Agent），只让它归纳一句话错因，
// **不派 verifier / 不 self-consistency / 不 code_exec**——家长记的是已知错题，无需判对错。
// 未注入闭包时返回空串（错因留空由用户填），绝不回退全对抗验算链（否则又变回 1-2 分钟）。
func (a *SolveAdapter) SummarizeCause(ctx context.Context, subject, question, studentAnswer, grade string) (string, error) {
	if a.retryGen == nil {
		return "", nil
	}
	prompt := "这道题孩子做错了。用一句话（≤20 字）归纳错因本身，只输出错因、不要解题、不要复述题目。\n题目：" + question
	if strings.TrimSpace(studentAnswer) != "" {
		prompt += "\n孩子的答案/错处：" + studentAnswer
	}
	out, err := a.retryGen(ctx, subject, prompt, grade)
	if err != nil {
		return "", err
	}
	return stripReports(out), nil
}

// Grade 实现 usecase.Grader：solve skill 的 grading 模式内部会重新解题得 ground truth，
// 结构化批改结果从 Metadata 读（solve.go 已同步输出，免解析 Content 文本）。
func (a *SolveAdapter) Grade(ctx context.Context, problem, studentAnswer, _ string) (usecase.GradeOutcome, error) {
	return a.GradeSubject(ctx, "", problem, studentAnswer, "")
}

// GradeSubject 与 Grade 相同，并把显式学科传给 grading 模式。
func (a *SolveAdapter) GradeSubject(ctx context.Context, subject, problem, studentAnswer, _ string) (usecase.GradeOutcome, error) {
	args := map[string]any{"problem": problem, "student_answer": studentAnswer}
	if subject != "" {
		args["subject"] = subject
	}
	res, err := a.exec.Execute(ctx, args)
	if err != nil {
		return usecase.GradeOutcome{}, err
	}
	if res == nil {
		return usecase.GradeOutcome{}, fmt.Errorf("solve adapter: empty grading result")
	}
	m := res.Metadata
	raw, ok := m["grade_correct"]
	if !ok {
		return usecase.GradeOutcome{}, fmt.Errorf("solve adapter: grading metadata missing grade_correct")
	}
	correct, err := strconv.ParseBool(raw)
	if err != nil {
		return usecase.GradeOutcome{}, fmt.Errorf("solve adapter: invalid grade_correct %q: %w", raw, err)
	}
	return usecase.GradeOutcome{
		Correct:    correct,
		WrongStep:  m["grade_wrong_step"],
		ErrorCause: m["grade_misconception"],
		// KnowledgePoint 由识题/课标决定，不来自 grader；用例层从识题结果回填。
	}, nil
}

// evidenceFromMeta 把 solve 的 Metadata 映射成证据对象。
//
// 关键（信任红线，§5.3.2/§8.1）：强徽章「已程序验算」只在 solve 真跑 code_exec 得到数值
// ground truth 时才给——由 solve 下发的 `solve_evidence=numeric_exec` 决定，**不再对任意
// agree 都标强证据**。solve_evidence=model（仅模型口头 agree）→ 弱证据 heuristic。
func evidenceFromMeta(m map[string]string) usecase.SolveEvidence {
	verdict := usecase.Verdict(m["solve_verdict"])
	ev := usecase.SolveEvidence{Verdict: verdict}
	// 归一非标准 verdict（如 "skipped"）为 unverifiable。
	switch verdict {
	case usecase.VerdictAgree, usecase.VerdictDisagree, usecase.VerdictUnverifiable, usecase.VerdictOutOfScope:
	default:
		ev.Verdict = usecase.VerdictUnverifiable
	}
	// 证据强弱按 solve 下发的 solve_evidence 定，与 verdict 解耦。
	switch {
	case ev.Verdict == usecase.VerdictAgree && m["solve_evidence"] == "numeric_exec":
		ev.EvidenceType = usecase.EvidenceNumericExec // 强：code_exec 客观重算相等
	case ev.Verdict == usecase.VerdictAgree || ev.Verdict == usecase.VerdictDisagree:
		ev.EvidenceType = usecase.EvidenceHeuristic // 弱：模型口头判定，未程序验算
	default:
		ev.EvidenceType = usecase.EvidenceNone
	}
	return ev
}

// stripReports 剥掉解题文本尾部的子 Agent 回执围栏，只留教学解题正文。
func stripReports(content string) string {
	if i := strings.Index(content, reportSentinel); i >= 0 {
		return strings.TrimSpace(content[:i])
	}
	return strings.TrimSpace(content)
}
