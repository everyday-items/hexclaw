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

// SolveAdapter 用 engine 的 solve skill 实现用例层的 Solver + Grader 两个 port。
type SolveAdapter struct {
	exec SolveExecutor
}

// NewSolveAdapter 创建 adapter。s 通常是 engine.NewSolveSkill(...) 的产物。
func NewSolveAdapter(s SolveExecutor) *SolveAdapter { return &SolveAdapter{exec: s} }

var (
	_ usecase.Solver = (*SolveAdapter)(nil)
	_ usecase.Grader = (*SolveAdapter)(nil)
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
