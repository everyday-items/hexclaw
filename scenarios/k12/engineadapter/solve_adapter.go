// Package engineadapter 把 hexclaw engine 的能力适配成 K12 用例层的 port 接口。
//
// 这是 clean-architecture 的 adapter 层：用例层依赖抽象 port（Solver/Grader/...），
// 真实现由本包提供，桥接到 engine/solve.go 等既有代码。composition root（wire.go）把它们注入用例。
package engineadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
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

// CauseSummaryGenerateFunc 是「记一条错题」的轻量错因摘要闭包。必须与变式出题分开，避免
// “必须出题”的系统提示覆盖“只输出错因”的用户提示。
type CauseSummaryGenerateFunc func(ctx context.Context, subject, prompt, grade string) (string, error)

// TutoringTipsReviewGenerateFunc is the bounded explanation closure used when
// textbook evidence is unavailable. It is isolated from exercise generation.
type TutoringTipsReviewGenerateFunc func(ctx context.Context, subject, prompt, grade string) (string, error)

// SolveAdapter 用 engine 的 solve skill 实现用例层的 Solver + Grader 两个 port。
type SolveAdapter struct {
	exec                   SolveExecutor
	causeSummaryGen        CauseSummaryGenerateFunc       // 轻量错因摘要；nil 时留空由用户填写
	tutoringTipsReviewGen  TutoringTipsReviewGenerateFunc // nil means an honest static degradation
	parentTeachingGuideGen ParentTeachingGuideGenerateFunc
	// parentTeachingSkillLoader 读取建档锁定的教学方法 Skill；盘上版本不可用时由
	// parent_teaching_guide.go 降级到同版本内嵌快照。
	parentTeachingSkillLoader SkillContentLoader
	workFeedbackGen           WorkFeedbackGenerateFunc // 作品点评生成（work_feedback.go）；nil 时诚实报错
	// workFeedbackVision 美术作品观察式点评的视觉闭包（work_feedback.go）：复用识题链的
	// VisionFunc 原语（原图 bytes + 提示词 → 视觉模型文本）；nil 时美术点评诚实报错。
	workFeedbackVision VisionFunc
	// workFeedbackSkillLoader 盘上 marketplace skill 内容加载闭包（work_feedback.go）：
	// 点评方法论基座「盘上→内嵌→硬编码」链的第一级；nil 时直接从内嵌快照起链。
	workFeedbackSkillLoader SkillContentLoader
}

// SolveAdapterOption 装配可选能力（保 NewSolveAdapter 单参数向后兼容）。
type SolveAdapterOption func(*SolveAdapter)

// WithCauseSummaryGen 注入专用错因摘要闭包。
func WithCauseSummaryGen(fn CauseSummaryGenerateFunc) SolveAdapterOption {
	return func(a *SolveAdapter) { a.causeSummaryGen = fn }
}

// WithTutoringTipsReviewGen injects the dedicated explanation closure.
func WithTutoringTipsReviewGen(fn TutoringTipsReviewGenerateFunc) SolveAdapterOption {
	return func(a *SolveAdapter) { a.tutoringTipsReviewGen = fn }
}

// SetCauseSummaryGen 事后注入专用错因摘要闭包。
func (a *SolveAdapter) SetCauseSummaryGen(fn CauseSummaryGenerateFunc) { a.causeSummaryGen = fn }

// SetTutoringTipsReviewGen injects the explanation closure after composition.
func (a *SolveAdapter) SetTutoringTipsReviewGen(fn TutoringTipsReviewGenerateFunc) {
	a.tutoringTipsReviewGen = fn
}

// NewSolveAdapter 创建 adapter。s 通常是 engine.NewSolveSkill(...) 的产物。
func NewSolveAdapter(s SolveExecutor, opts ...SolveAdapterOption) *SolveAdapter {
	a := &SolveAdapter{exec: s}
	for _, o := range opts {
		o(a)
	}
	return a
}

var (
	_ usecase.Solver                      = (*SolveAdapter)(nil)
	_ usecase.Grader                      = (*SolveAdapter)(nil)
	_ usecase.VerifiedSolutionGrader      = (*SolveAdapter)(nil)
	_ usecase.CauseSummarizer             = (*SolveAdapter)(nil)
	_ usecase.TutoringTipsReviewGenerator = (*SolveAdapter)(nil)
)

func (a *SolveAdapter) UsesGradingPhysicalCalls() bool {
	capable, ok := a.exec.(interface{ SupportsSubAgentCallInterceptor() bool })
	return ok && capable.SupportsSubAgentCallInterceptor()
}

func (a *SolveAdapter) withGradingPhysicalCallInterceptor(ctx context.Context) context.Context {
	if !usecase.HasGradingPhysicalCallExecutor(ctx) {
		return ctx
	}
	// Only the durable K12 item path explicitly selects its frozen stage
	// deadline as the authority. Ordinary engine callers retain their existing
	// 3m/5m anti-hang guards even when they happen to carry a deadline.
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		ctx = engine.WithAuthoritativeCallerDeadline(ctx)
	}
	return engine.WithSubAgentCallInterceptor(ctx, engine.SubAgentCallInterceptorFunc(
		func(
			callCtx context.Context,
			spec engine.SubAgentSpec,
			next engine.SubAgentExecFunc,
		) (engine.SubAgentResult, error) {
			var operation k12.GradingItemOperation
			switch strings.ToLower(strings.TrimSpace(spec.Agent)) {
			case "solver":
				operation = k12.GradingItemOperationSolveGenerate
			case "verifier":
				operation = k12.GradingItemOperationSolveVerify
			case "grader":
				operation = k12.GradingItemOperationGrade
			default:
				return next(callCtx, spec)
			}
			requestRaw, err := json.Marshal(struct {
				Agent  string `json:"agent"`
				Task   string `json:"task"`
				Source string `json:"source"`
				Mode   string `json:"mode"`
			}{spec.Agent, spec.Task, spec.Source, spec.Mode})
			if err != nil {
				return engine.SubAgentResult{}, err
			}
			digestRaw := sha256.Sum256(requestRaw)
			physical, err := usecase.ExecuteGradingPhysicalCall(
				callCtx,
				usecase.GradingPhysicalCallSpec{
					Operation: operation, RequestDigest: hex.EncodeToString(digestRaw[:]),
				},
				func(providerCtx context.Context) (string, error) {
					result, callErr := next(providerCtx, spec)
					if callErr != nil {
						return "", callErr
					}
					raw, marshalErr := json.Marshal(result)
					return string(raw), marshalErr
				},
			)
			if err != nil {
				return engine.SubAgentResult{}, err
			}
			var result engine.SubAgentResult
			if err := json.Unmarshal([]byte(physical.Payload), &result); err != nil {
				return engine.SubAgentResult{}, fmt.Errorf("solve adapter: decode physical call result: %w", err)
			}
			return result, nil
		},
	))
}

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
	problem = gradingProblemWithGrounding(ctx, problem)
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
	if usecase.HasGradingPhysicalCallExecutor(ctx) {
		// Durable grading requires an explicit solver + verifier pair. This also
		// prevents deterministic fast paths from producing a fake provider ledger.
		args["self_consistency"] = 1
		ctx = a.withGradingPhysicalCallInterceptor(ctx)
	}
	res, err := a.exec.Execute(ctx, args)
	if err != nil {
		return usecase.SolveResult{}, providerResponseError(err)
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

// GenerateTutoringTipsReview uses one reasoning completion for a grade-bounded
// explanation. It never enters the adversarial grading pipeline.
func (a *SolveAdapter) GenerateTutoringTipsReview(ctx context.Context, subject, knowledgePoint, grade string) (string, error) {
	if a.tutoringTipsReviewGen == nil {
		return "", nil
	}
	prompt := "为家长生成一段中小学知识点回顾。使用简洁 Markdown；数学公式必须使用 $...$ 或 $$...$$ 的 LaTeX，" +
		"不要用空格/换行拼分数。只讲核心概念、孩子常见卡点和一句引导话术，控制在120字内；" +
		"不要出题、不要假称引用教材、不要输出未经提供的课本原文。知识点：" + knowledgePoint
	out, err := a.tutoringTipsReviewGen(ctx, subject, prompt, grade)
	if err != nil {
		return "", err
	}
	return tutoringTipsMarkdown(out), nil
}

// GenerateGroundedTutoringTipsReview turns textbook evidence into concise
// parent-facing Markdown without adding unsupported facts.
func (a *SolveAdapter) GenerateGroundedTutoringTipsReview(ctx context.Context, subject, knowledgePoint, grade, evidence string) (string, error) {
	if a.tutoringTipsReviewGen == nil {
		return "", nil
	}
	prompt := "请把下面教材证据整理成家长可直接照着讲的知识点回顾。输出简洁 Markdown，包含核心概念、常见卡点、" +
		"一句引导话术；数学公式必须使用 $...$ 或 $$...$$ 的 LaTeX，不要用空格/换行拼分数；不得输出文档ID、相关度、" +
		"检索参考编号，也不得补写证据中没有的事实。控制在180字内。\n知识点：" + knowledgePoint + "\n教材证据：\n" + evidence
	out, err := a.tutoringTipsReviewGen(ctx, subject, prompt, grade)
	if err != nil {
		return "", err
	}
	return tutoringTipsMarkdown(out), nil
}

func tutoringTipsMarkdown(content string) string {
	if i := strings.Index(content, reportSentinel); i >= 0 {
		content = content[:i]
	}
	return strings.TrimSpace(content)
}

// Grade 实现 usecase.Grader：solve skill 的 grading 模式内部会重新解题得 ground truth，
// 结构化批改结果从 Metadata 读（solve.go 已同步输出，免解析 Content 文本）。
func (a *SolveAdapter) Grade(ctx context.Context, problem, studentAnswer, _ string) (usecase.GradeOutcome, error) {
	return a.GradeSubject(ctx, "", problem, studentAnswer, "")
}

// GradeSubject 与 Grade 相同，并把显式学科传给 grading 模式。
func (a *SolveAdapter) GradeSubject(ctx context.Context, subject, problem, studentAnswer, _ string) (usecase.GradeOutcome, error) {
	problem = gradingProblemWithGrounding(ctx, problem)
	args := map[string]any{"problem": problem, "student_answer": studentAnswer}
	if subject != "" {
		args["subject"] = subject
	}
	ctx = a.withGradingPhysicalCallInterceptor(ctx)
	res, err := a.exec.Execute(ctx, args)
	return gradeOutcomeFromResult(res, err)
}

// GradeVerified 复用用例层刚得到的已验证解法。生产 SolveSkill 实现内部快口时只派 grader；
// 测试/第三方旧 executor 没实现时安全回退原完整批改链，兼容性优先。
func (a *SolveAdapter) GradeVerified(ctx context.Context, subject, problem, studentAnswer, verifiedSolution string) (usecase.GradeOutcome, error) {
	if exec, ok := a.exec.(VerifiedGradeExecutor); ok {
		problem = gradingProblemWithGrounding(ctx, problem)
		ctx = a.withGradingPhysicalCallInterceptor(ctx)
		res, err := exec.GradeVerified(ctx, problem, verifiedSolution, studentAnswer)
		return gradeOutcomeFromResult(res, err)
	}
	return a.GradeSubject(ctx, subject, problem, studentAnswer, verifiedSolution)
}

// gradingProblemWithGrounding 只把编排层已经核验的教材正文加入模型输入；
// 持久回执、文档标识和 revision 摘要均不进入提示词。
func gradingProblemWithGrounding(ctx context.Context, problem string) string {
	evidence, ok := usecase.GradingGroundingForProvider(ctx)
	if !ok {
		return problem
	}
	return strings.TrimSpace(problem) +
		"\n\nVerified textbook evidence (use it only to constrain the solution and grading; it is not the student's answer; do not expose internal source identifiers). Respond in Chinese:\n" +
		strings.TrimSpace(evidence)
}

func gradeOutcomeFromResult(res *skill.Result, err error) (usecase.GradeOutcome, error) {
	if err != nil {
		return usecase.GradeOutcome{}, providerResponseError(err)
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
	var finalAnswerCorrect *bool
	if rawFinal, exists := m["grade_final_answer_correct"]; exists {
		parsedFinal, parseErr := strconv.ParseBool(rawFinal)
		if parseErr != nil {
			return usecase.GradeOutcome{}, fmt.Errorf(
				"solve adapter: invalid grade_final_answer_correct %q: %w",
				rawFinal,
				parseErr,
			)
		}
		finalAnswerCorrect = &parsedFinal
	}
	// 判定统一 Verdict 五值（§4.5 布尔删除）：engine 侧 grade_correct 布尔在 adapter 边界
	// 一次性收敛为 agree/disagree，领域层不再出现布尔判定。
	verdict := usecase.VerdictDisagree
	if correct {
		verdict = usecase.VerdictAgree
	}
	return usecase.GradeOutcome{
		Verdict:            verdict,
		FinalAnswerCorrect: finalAnswerCorrect,
		// 错步/错因也可能含模型 LaTeX（\times/\frac/\(…\)）——桌面直接展示，统一归一为 Unicode。
		// 口径裁决（K12-INV-019 终局对账 2026-07-18，存储规范形）：批改产物**入库前**在本
		// adapter 边界 Normalize 为 Unicode 规范形态是正确口径——存储即规范形，下游 IM/导出/
		// 桌面全部拿到干净 Unicode；channel.LaTeXToUnicode 出口兜底是第二道防线。
		// 本 Normalize 保留勿删（守卫测试 inv019_canonical_store_test.go）。
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

// stripReports 是 K12 解答文本出站的单一归一化 chokepoint（solve/grade 讲解/再练/辅导要点/错因归纳都过它）：
//  1. 剥掉解题文本尾部的子 Agent 回执围栏，只留教学解题正文；
//  2. 数学 LaTeX → Unicode（adapter.NormalizeMathText：\times→× / \frac{a}{b}→a/b / \text{cm}^3→cm³ /
//     剥 \(…\)\[…\]$…$）。治本 BUG-20260713：桌面 API 路径不经 IM egress，此前原样漏 LaTeX 给前端。
//     幂等——IM egress 出站再归一化一次无害。
//
// 口径裁决（K12-INV-019 终局对账 2026-07-18，存储规范形）：canonical_answer 与 solution
// **入库前**经此处 Normalize 为 Unicode 规范形态是正确口径（usecase.pipeline 判错入库
// CanonicalAnswer=sr.Solution 即已规范形）——存储即规范形，下游 IM/导出/桌面全部拿到干净
// Unicode，导出侧因此**不做**二次转换（原样保留即 INV-019「导出保留 canonical 数学公式」）；
// channel.LaTeXToUnicode 出口兜底是第二道防线。本 Normalize 保留勿删
// （守卫测试 inv019_canonical_store_test.go / math_normalize_20260713_test.go）。
func stripReports(content string) string {
	if i := strings.Index(content, reportSentinel); i >= 0 {
		content = content[:i]
	}
	return adapter.NormalizeMathText(strings.TrimSpace(content))
}
