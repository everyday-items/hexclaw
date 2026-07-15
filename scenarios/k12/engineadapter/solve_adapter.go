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

	"github.com/hexagon-codes/hexclaw/adapter"
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

// VerifiedGradeExecutor 是 engine.SolveSkill 的内部快口：正确解已在上一步验证时，只派 grader。
// 不放进 LLM tool schema，外部模型不能伪造“已验证”输入。
type VerifiedGradeExecutor interface {
	GradeVerified(ctx context.Context, problem, verifiedSolution, studentAnswer string) (*skill.Result, error)
}

// RetryGenerateFunc 是「再练一道」的轻量出题闭包（composition root 注入，BUG-20260712）。
// 内部走**单个 reasoning 子 Agent**出题+解答——不派 verifier / 不 self-consistency / 不重出，
// 返回出题正文（含回执围栏，adapter 侧剥离）。nil 时 GenerateSimilar 安全回退全链。
type RetryGenerateFunc func(ctx context.Context, subject, prompt, grade string) (string, error)

// CauseSummaryGenerateFunc 是「记一条错题」的轻量错因摘要闭包。必须与变式出题分开，避免
// “必须出题”的系统提示覆盖“只输出错因”的用户提示。
type CauseSummaryGenerateFunc func(ctx context.Context, subject, prompt, grade string) (string, error)

// PrepReviewGenerateFunc 是教材未命中时的轻量知识点回顾闭包。与变式出题分开，避免出题系统提示
// 污染“不要出题、不要伪造教材引用”的备课回顾任务。
type PrepReviewGenerateFunc func(ctx context.Context, subject, prompt, grade string) (string, error)

// SolveAdapter 用 engine 的 solve skill 实现用例层的 Solver + Grader 两个 port。
type SolveAdapter struct {
	exec            SolveExecutor
	retryGen        RetryGenerateFunc        // 轻量「再练一道」出题；nil 时回退全链
	causeSummaryGen CauseSummaryGenerateFunc // 轻量错因摘要；nil 时留空由用户填写
	prepReviewGen   PrepReviewGenerateFunc   // 轻量备课回顾；nil 时由用例层诚实降级
}

// SolveAdapterOption 装配可选能力（保 NewSolveAdapter 单参数向后兼容）。
type SolveAdapterOption func(*SolveAdapter)

// WithRetryGen 注入轻量出题闭包，让「再练一道」走单次 reasoning、不落全对抗验算链。
func WithRetryGen(fn RetryGenerateFunc) SolveAdapterOption {
	return func(a *SolveAdapter) { a.retryGen = fn }
}

// WithCauseSummaryGen 注入专用错因摘要闭包。
func WithCauseSummaryGen(fn CauseSummaryGenerateFunc) SolveAdapterOption {
	return func(a *SolveAdapter) { a.causeSummaryGen = fn }
}

// WithPrepReviewGen 注入专用备课回顾闭包。
func WithPrepReviewGen(fn PrepReviewGenerateFunc) SolveAdapterOption {
	return func(a *SolveAdapter) { a.prepReviewGen = fn }
}

// SetRetryGen 事后注入轻量出题闭包（composition root 在 Deps.Solver 建好后回填）。
func (a *SolveAdapter) SetRetryGen(fn RetryGenerateFunc) { a.retryGen = fn }

// SetCauseSummaryGen 事后注入专用错因摘要闭包。
func (a *SolveAdapter) SetCauseSummaryGen(fn CauseSummaryGenerateFunc) { a.causeSummaryGen = fn }

// SetPrepReviewGen 事后注入专用备课回顾闭包。
func (a *SolveAdapter) SetPrepReviewGen(fn PrepReviewGenerateFunc) { a.prepReviewGen = fn }

// NewSolveAdapter 创建 adapter。s 通常是 engine.NewSolveSkill(...) 的产物。
func NewSolveAdapter(s SolveExecutor, opts ...SolveAdapterOption) *SolveAdapter {
	a := &SolveAdapter{exec: s}
	for _, o := range opts {
		o(a)
	}
	return a
}

var (
	_ usecase.Solver                 = (*SolveAdapter)(nil)
	_ usecase.Grader                 = (*SolveAdapter)(nil)
	_ usecase.VerifiedSolutionGrader = (*SolveAdapter)(nil)
	_ usecase.RetryGenerator         = (*SolveAdapter)(nil)
	_ usecase.CauseSummarizer        = (*SolveAdapter)(nil)
	_ usecase.PrepReviewGenerator    = (*SolveAdapter)(nil)
)

// Solve 实现 usecase.Solver：调 solve skill 解题验算（透传 grade + constraint 约束年级边界），
// 把 Metadata 里的 verdict + evidence 映射成证据对象。
func (a *SolveAdapter) Solve(ctx context.Context, problem, grade, constraint string) (usecase.SolveResult, error) {
	return a.SolveSubject(ctx, "", problem, grade, constraint)
}

// SolveSubject 与 Solve 相同，并把显式学科传给 solve skill，避免非数学 eval/批改落入默认数学路由。
func (a *SolveAdapter) SolveSubject(ctx context.Context, subject, problem, grade, constraint string) (usecase.SolveResult, error) {
	// 不注入 self_consistency 默认值：由 solve 自适应 triage。纯数字四则算式走本机精确求值器并给
	// numeric_exec 强证据；复杂题仍走 solver + verifier。只有真正由调用方显式指定校验力度时，
	// engine 才禁用快速路径。
	args := map[string]any{"problem": problem}
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
		Solution:     normalizeSolveMarkdown(stripReports(res.Content)),
		Evidence:     evidenceFromMeta(res.Metadata),
		OutOfScopeKP: res.Metadata["solve_out_of_scope_kp"],
	}, nil
}

// GenerateSimilar 实现 usecase.RetryGenerator（BUG-20260712 治本「再练一道」轻量出题）：
// 走单次 reasoning 出题闭包（单个子 Agent，不派 verifier / 不 self-consistency / 不重出），
// 真机从全对抗链 68s 降到单次调用量级。练习变式题容错高：**不跑 code_exec 程序验算**，
// 故 verdict=unverifiable、不给强徽章（信任红线：绝不把未验算的练习题冒充「已程序验算」）。
// 未注入 retryGen 闭包时安全回退全链（SolveSubject），保证正确性不塌。
func (a *SolveAdapter) GenerateSimilar(ctx context.Context, subject, prompt, grade string) (usecase.SolveResult, error) {
	if a.retryGen == nil {
		sr, err := a.SolveSubject(ctx, subject, prompt, grade, "")
		if err == nil {
			sr.Solution = normalizeRetryMarkdown(sr.Solution)
		}
		return sr, err
	}
	out, err := a.retryGen(ctx, subject, prompt, grade)
	if err != nil {
		return usecase.SolveResult{}, err
	}
	return usecase.SolveResult{
		Solution: normalizeRetryMarkdown(stripReports(out)),
		Evidence: usecase.SolveEvidence{Verdict: usecase.VerdictUnverifiable, EvidenceType: usecase.EvidenceNone},
	}, nil
}

// SummarizeCause 实现 usecase.CauseSummarizer（BUG-20260712「记一条错题」轻量错因归纳）：
// 使用专用单次 reasoning 闭包归纳一句话错因，**不派 verifier / 不 self-consistency /
// 不 code_exec**——家长记的是已知错题，无需判对错。
// 未注入闭包时返回空串（错因留空由用户填），绝不回退全对抗验算链（否则又变回 1-2 分钟）。
func (a *SolveAdapter) SummarizeCause(ctx context.Context, subject, question, studentAnswer, grade string) (string, error) {
	if a.causeSummaryGen == nil {
		return "", nil
	}
	prompt := "这道题孩子做错了。用一句话（≤20 字）归纳错因本身，只输出错因、不要解题、不要复述题目。\n题目：" + question
	if strings.TrimSpace(studentAnswer) != "" {
		prompt += "\n孩子的答案/错处：" + studentAnswer
	}
	out, err := a.causeSummaryGen(ctx, subject, prompt, grade)
	if err != nil {
		return "", err
	}
	return stripReports(out), nil
}

// GeneratePrepReview 在教材未命中时复用裸 reasoning completion 生成按年级约束的知识点回顾。
// 不回退 SolveExecutor：备课讲法不需要全对抗验算链；无闭包时返回空，让用例层诚实标静态降级。
func (a *SolveAdapter) GeneratePrepReview(ctx context.Context, subject, knowledgePoint, grade string) (string, error) {
	if a.prepReviewGen == nil {
		return "", nil
	}
	prompt := "为家长生成一段中小学知识点回顾。只讲核心概念、孩子常见卡点和一句引导话术，控制在120字内；" +
		"不要出题、不要假称引用教材、不要输出未经提供的课本原文。知识点：" + knowledgePoint
	out, err := a.prepReviewGen(ctx, subject, prompt, grade)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stripReports(out)), nil
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
	return gradeOutcomeFromResult(res, err)
}

// GradeVerified 复用用例层刚得到的已验证解法。生产 SolveSkill 实现内部快口时只派 grader；
// 测试/第三方旧 executor 没实现时安全回退原完整批改链，兼容性优先。
func (a *SolveAdapter) GradeVerified(ctx context.Context, subject, problem, studentAnswer, verifiedSolution string) (usecase.GradeOutcome, error) {
	if exec, ok := a.exec.(VerifiedGradeExecutor); ok {
		res, err := exec.GradeVerified(ctx, problem, verifiedSolution, studentAnswer)
		return gradeOutcomeFromResult(res, err)
	}
	return a.GradeSubject(ctx, subject, problem, studentAnswer, verifiedSolution)
}

func gradeOutcomeFromResult(res *skill.Result, err error) (usecase.GradeOutcome, error) {
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
		Correct: correct,
		// 错步/错因也可能含模型 LaTeX（\times/\frac/\(…\)）——桌面直接展示，统一归一为 Unicode。
		WrongStep:  adapter.NormalizeMathText(m["grade_wrong_step"]),
		ErrorCause: adapter.NormalizeMathText(m["grade_misconception"]),
		// KnowledgePoint 由识题/课标决定，不来自 grader；用例层从识题结果回填。
	}, nil
}

// evidenceFromMeta 把 solve 的 Metadata 映射成证据对象。
//
// 关键（信任红线，§5.3.2/§8.1）：强徽章「已程序验算」只在 solve 通过 code_exec 或本机
// 确定性算式求值器得到数值 ground truth 时才给——由 `solve_evidence=numeric_exec` 决定，
// **不再对任意 agree 都标强证据**。solve_evidence=model（仅模型口头 agree）→ 弱证据 heuristic。
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

// stripReports 是 K12 解答文本出站的单一归一化 chokepoint（solve/grade 讲解/再练/prep-card/错因归纳都过它）：
//  1. 剥掉解题文本尾部的子 Agent 回执围栏，只留教学解题正文；
//  2. 数学 LaTeX → Unicode（adapter.NormalizeMathText：\times→× / \frac{a}{b}→a/b / \text{cm}^3→cm³ /
//     剥 \(…\)\[…\]$…$）。治本 BUG-20260713：桌面 API 路径不经 IM egress，此前原样漏 LaTeX 给前端。
//     幂等——IM egress 出站再归一化一次无害。
func stripReports(content string) string {
	if i := strings.Index(content, reportSentinel); i >= 0 {
		content = content[:i]
	}
	return adapter.NormalizeMathText(strings.TrimSpace(content))
}
