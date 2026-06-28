package engine

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// SolveSkill 是 K12 作业辅导的「带独立验证的解题」工具（评审 P0，对标 OpenClaw/Hermes/ITS 三方
// 调研一致结论：正确性的第一杠杆是工具增强验证）。
//
// 流程：Solver 解题（self_consistency 多采样多数表决，或 method_diversity 跑 2 个不同方法）→
// Verifier 用 code_exec 写代码**独立重算**（fresh-context、只许 code_exec）→ 比对：
//   - 各解法一致 + 核验一致     → 高置信，输出教学版解题 + ✅
//   - 解法之间分歧             → 用 code_exec 独立核验充当**裁决者**：判出正确解法、点明另一解法错处
//   - 解法一致但核验不一致      → ⚠️ 诚实并列两答 + 请复核（绝不自信采信）
//   - 不可验证(非计算题)        → 输出解题 + 提示人工复核
//
// 为何不是裸 LLM 二次意见：数学/物理的算术幻觉是 K12 头号错因，靠模型"再想一遍"治不了；让另一个
// Agent **执行代码**算出 ground truth 才能根治。method_diversity 跨「方法」而非跨「温度」，能抓到
// 自洽采样抓不到的系统性方法错误。
type SolveSkill struct {
	executeFunc SubAgentExecFunc
	registry    *SubAgentRegistry
}

// NewSolveSkill 创建 solve 工具。
func NewSolveSkill(execFn SubAgentExecFunc, reg *SubAgentRegistry) *SolveSkill {
	return &SolveSkill{executeFunc: execFn, registry: reg}
}

func (o *SolveSkill) Name() string { return "solve" }
func (o *SolveSkill) Description() string {
	return "Solve a homework/exam problem and return a step-by-step solution whose final answer has been independently verified by executing code."
}
func (o *SolveSkill) Match(_ string) bool { return false }

func (o *SolveSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("solve",
		"Solve a K12 homework/exam problem with a step-by-step teaching explanation whose final answer is INDEPENDENTLY VERIFIED by a separate agent that runs code (code_exec) to recompute it. Use for math/physics/chemistry word problems or any problem where correctness matters (don't bother for trivial single-step arithmetic a student can check by hand — just answer those directly). Effort auto-scales to difficulty; returns the worked solution plus a verification verdict, and on disagreement honestly surfaces both answers instead of guessing.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"problem":          {Type: "string", Description: "The full problem statement to solve."},
				"subject":          {Type: "string", Description: "Optional subject (math/physics/chemistry/english/chinese) to tune the teaching style."},
				"self_consistency": {Type: "integer", Description: "Optional number of independent same-method solver samples to majority-vote (default 1; >1 boosts accuracy on hard problems)."},
				"method_diversity": {Type: "boolean", Description: "Optional. When true, solve with TWO deliberately different methods and cross-check them (catches systematic method errors; the code verifier breaks ties). Great for problems with multiple solution paths."},
				"student_answer":   {Type: "string", Description: "Optional. The student's submitted answer (or worked steps). When present, GRADES it: solves to get the correct answer, then identifies whether the student is right and, if wrong, WHICH step erred + the misconception + a guiding hint (without just handing over the full answer)."},
			},
			Required: []string{"problem"},
		})
}

// 子 Agent 角色名 + 工具名（verifier 被限定为只有 code_exec）。
const (
	solverAgentName   = "solver"
	verifierAgentName = "verifier"
	graderAgentName   = "grader"
	codeExecToolName  = "code_exec"
)

// maxSelfConsistency 限制自洽采样的解题次数上限（兜住延迟/成本）。
var maxSelfConsistency = 5

// verifyVerdict 是 verifier 的判定。
type verifyVerdict int

const (
	verdictUnverifiable verifyVerdict = iota // 非计算题 / 无法判定 / verifier 不可用
	verdictAgree
	verdictDisagree
)

// solverSolution 是一份解题产出。
type solverSolution struct {
	output string
	answer string // 抽出的最终答案（已归一化）
}

// answerGroup 是同一答案的一组解题（多数表决 / 分歧分组用）。
type answerGroup struct {
	answer string
	sols   []solverSolution
}

func (o *SolveSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	problem, _ := args["problem"].(string)
	if strings.TrimSpace(problem) == "" {
		return nil, fmt.Errorf("problem is required")
	}
	subject, _ := args["subject"].(string)
	methodDiversity, _ := args["method_diversity"].(bool)
	samples := clampSamples(intArg(args["self_consistency"], 1))
	studentAnswer, _ := args["student_answer"].(string)
	gradingMode := strings.TrimSpace(studentAnswer) != ""

	// P0.5 复杂度 triage：调用方未显式指定力度时，按题目复杂度自适应——难题自动上 method_diversity
	// 加交叉校验，简单题（单步算术）直接作答、省去校验子 Agent 的额外一次调用（桌面延迟优先，
	// "默认轻、按需重"）。显式传 method_diversity/self_consistency 即视为调用方接管、不再自动 triage。
	auto := !hasArg(args, "method_diversity") && !hasArg(args, "self_consistency")
	complexity := assessComplexity(problem)
	if auto && complexity == complexityHard {
		methodDiversity = true
	}
	skipVerify := auto && complexity == complexityTrivial && !gradingMode // 批改需先得正确答案，不跳校验

	// P2 防卡死：整次 solve 套总墙钟（solver 采样/多法 + verifier）。
	runCtx, cancel := context.WithTimeout(ctx, orchestrateMaxWall)
	defer cancel()

	solveRunID := "solve-" + idgen.NanoID()
	o.registry.Start(&SubAgentRunRecord{
		ID: solveRunID, Agent: "solve", Role: roleForDepth(spawnDepthFromContext(ctx)),
		Depth: spawnDepthFromContext(ctx), Mode: "run", Status: subAgentStatusRunning,
	})
	childCtx := withCurrentRunID(runCtx, solveRunID)

	// 1) Solver 阶段：method_diversity → 2 个不同方法；否则 self_consistency 同法多采样。
	var sols []solverSolution
	for _, sp := range o.solverSpecs(problem, subject, methodDiversity, samples) {
		if childCtx.Err() != nil {
			break
		}
		out, _ := o.runSolveAgent(childCtx, sp)
		if strings.TrimSpace(out) != "" {
			sols = append(sols, solverSolution{output: out, answer: extractFinalAnswer(out)})
		}
	}
	if len(sols) == 0 {
		o.registry.Finish(solveRunID, subAgentStatusError, "", "solver produced no solution", "")
		return &skill.Result{Content: "未能解出本题，请补充题目信息或换一种问法。"}, nil
	}
	groups := groupByAnswer(sols)
	primary := groups[0]

	// P0.5：简单题直接作答——略过校验子 Agent 的额外调用（延迟优先）。
	if skipVerify {
		o.registry.Finish(solveRunID, subAgentStatusOK, "trivial:"+primary.answer, "", "")
		return &skill.Result{
			Content:  primary.sols[0].output + encodeSubAgentReports([]SubAgentReport{{Agent: solverAgentName, Status: subAgentStatusOK}}),
			Metadata: map[string]string{"solve_run_id": solveRunID, "solve_verdict": "skipped"},
		}, nil
	}

	// 2) Verifier 阶段：code_exec 独立重算（fresh-context、只许 code_exec）。
	verdict, computed := o.verify(childCtx, problem, primary.answer)

	o.registry.Finish(solveRunID, subAgentStatusOK,
		fmt.Sprintf("verdict=%d answer=%q groups=%d", verdict, primary.answer, len(groups)), "", "")

	// P1.5 批改模式：有 student_answer → 拿到正确答案后，派 grader 对比学生答案、找错步 + 误区 + 引导。
	if gradingMode {
		groundTruth := primary.answer
		if verdict == verdictAgree && computed != "" {
			groundTruth = computed
		}
		assess := o.grade(childCtx, problem, primary.sols[0].output, groundTruth, studentAnswer)
		reports := []SubAgentReport{
			{Agent: solverAgentName, Status: subAgentStatusOK},
			newVerifierReport(verdict),
			{Agent: graderAgentName, Status: subAgentStatusOK},
		}
		return &skill.Result{
			Content:  formatGrading(studentAnswer, groundTruth, assess) + encodeSubAgentReports(reports),
			Metadata: map[string]string{"solve_run_id": solveRunID, "solve_mode": "grading"},
		}, nil
	}

	// 3) 组装教学结果 + 置信徽标 + 结构化回执。
	content := formatSolve(groups, verdict, computed, len(sols), methodDiversity)
	reports := []SubAgentReport{
		{Agent: solverAgentName, Status: subAgentStatusOK},
		newVerifierReport(verdict),
	}
	return &skill.Result{
		Content:  content + encodeSubAgentReports(reports),
		Metadata: map[string]string{"solve_run_id": solveRunID, "solve_verdict": verdictString(verdict)},
	}, nil
}

// solverSpecs 按模式产出本次要跑的 solver spec 列表。
func (o *SolveSkill) solverSpecs(problem, subject string, methodDiversity bool, samples int) []SubAgentSpec {
	if methodDiversity {
		specs := make([]SubAgentSpec, 0, len(solverMethods))
		for _, m := range solverMethods {
			specs = append(specs, solverSpec(problem, subject, m))
		}
		return specs
	}
	specs := make([]SubAgentSpec, 0, samples)
	for i := 0; i < samples; i++ {
		specs = append(specs, solverSpec(problem, subject, ""))
	}
	return specs
}

// runSolveAgent 跑一个子 Agent（solver/verifier/grader）并登记注册表，返回其输出。
func (o *SolveSkill) runSolveAgent(ctx context.Context, spec SubAgentSpec) (string, error) {
	if o.executeFunc == nil {
		return "", fmt.Errorf("executor not available")
	}
	o.registry.Start(newSubAgentRecord(ctx, spec))
	// 受信内部授权（设计⑤根治）：仅对真 solve 派生盖不可伪造的 grant 到子 ctx，permission 据它放行
	// 沙箱 code_exec。grant 经 executeFunc→eng.Process→工具执行透传到 permission 闸。
	execCtx := ctx
	if spec.Source == solveDispatchSource {
		execCtx = withSolveGrant(ctx)
	}
	res, err := runSubAgentWithRetry(execCtx, o.executeFunc, spec, defaultSubAgentTimeout)
	status, errStr := subAgentStatusOK, ""
	if err != nil {
		status, errStr = subAgentStatusError, err.Error()
	}
	o.registry.Finish(spec.RunID, status, res.Output, errStr, res.SessionID)
	return res.Output, err
}

// verify 用 code_exec 独立重算。verifier 不可用/出错/不可解析 → unverifiable（不阻断）。
func (o *SolveSkill) verify(ctx context.Context, problem, candidate string) (verifyVerdict, string) {
	if o.executeFunc == nil || ctx.Err() != nil {
		return verdictUnverifiable, ""
	}
	out := o.runValidated(ctx, verifierSpec(problem, candidate), verdictParseable)
	if strings.TrimSpace(out) == "" {
		return verdictUnverifiable, ""
	}
	verdict, computed := parseVerdict(out)
	// 交叉校验硬化：verifier 真算出一个数（computed）时，以「算出的数 vs 候选」的客观比对为准，
	// 纠正模型口头判定词与自己算出的数自相矛盾的两种危险情形——
	//   • BUG-D（真模型实测 Qwen2.5-72B 算对 computed=2550 却说 DISAGREE）：computed≡候选却判 DISAGREE → 纠为 AGREE。
	//   • ① false-AGREE（K12 最坏失效）：computed 与候选**可确信不等**却判 AGREE → 降为 DISAGREE，错答案绝不当「已验证」。
	// 用数值/分数/集合容差等值，不靠脆字符串比对：既认出 0.5≡1/2、'12和8'≡'8和12'，又只在「能解析且确不等」
	// 时才下调 AGREE——带单位/等价文字形式解析不了 → 不武断（信模型对「42支==42」这类等价的判断）。
	if computed != "" {
		switch {
		case verdict == verdictDisagree && answersEqual(computed, candidate):
			verdict = verdictAgree
		case verdict == verdictAgree && answersDefinitelyDiffer(computed, candidate):
			verdict = verdictDisagree
		}
	}
	return verdict, computed
}

// strictFormatReminder 在校验重试时附加，逼模型只吐固定格式。
const strictFormatReminder = "\n\n⚠️ 上一次输出未按要求的固定格式。请**严格只**逐行输出上面要求的字段（每行一个），不要任何多余文字、解释、寒暄或代码围栏。"

// runValidated 跑结构化子 Agent 并按 ok() 校验其输出；首次解析失败 → 附加严格格式提醒、fresh 重派一次。
// 对标 Hermes JSON-mode 的「校验 + 失败重提示」：让 verdict/批改在弱/本地模型上也能被稳定解析，
// 不静默退化成 unverifiable/兜底。仅首次解析失败才多花一次调用（按需付费；首次成功零额外成本）。
func (o *SolveSkill) runValidated(ctx context.Context, spec SubAgentSpec, ok func(string) bool) string {
	out, err := o.runSolveAgent(ctx, spec)
	if err == nil && ok(out) {
		return out
	}
	if ctx.Err() != nil {
		return out
	}
	retry := spec
	retry.RunID = spec.RunID + "-retry"
	retry.Task = spec.Task + strictFormatReminder
	if out2, err2 := o.runSolveAgent(ctx, retry); err2 == nil && ok(out2) {
		return out2
	}
	return out // 两次都没解析干净：交给宽容解析 + 兜底，绝不更差。
}

// verdictParseable 报告 verifier 输出是否含可识别判定。
func verdictParseable(out string) bool {
	up := strings.ToUpper(out)
	return strings.Contains(up, "AGREE") || strings.Contains(up, "DISAGREE") || strings.Contains(up, "UNVERIFIABLE")
}

// gradingParseable 报告 grader 输出是否含 CORRECT 行。
func gradingParseable(out string) bool { return gradeCorrectRe.MatchString(out) }

// gradeAssessment 是 grader 对学生答案的批改结论。
type gradeAssessment struct {
	correct       bool
	wrongStep     string
	misconception string
	guidance      string
}

// grade 派一个 grader 子 Agent 对比学生答案与正确解。解析失败 → 回退按答案文本直接比对。
func (o *SolveSkill) grade(ctx context.Context, problem, solution, groundTruth, studentAnswer string) gradeAssessment {
	if o.executeFunc == nil || ctx.Err() != nil {
		return gradeAssessment{correct: normalizeAnswer(studentAnswer) == normalizeAnswer(groundTruth)}
	}
	out := o.runValidated(ctx, graderSpec(problem, solution, groundTruth, studentAnswer), gradingParseable)
	if strings.TrimSpace(out) == "" {
		return gradeAssessment{correct: normalizeAnswer(studentAnswer) == normalizeAnswer(groundTruth)}
	}
	return parseGrading(out, studentAnswer, groundTruth)
}

// graderSpec 构造批改子 Agent：可用 code_exec 核对学生算术；fresh-context、受信来源、leaf。
func graderSpec(problem, solution, groundTruth, studentAnswer string) SubAgentSpec {
	return SubAgentSpec{
		RunID:     "grader-" + idgen.NanoID(),
		Agent:     graderAgentName,
		Task:      buildGraderPrompt(problem, solution, groundTruth, studentAnswer),
		ToolAllow: []string{codeExecToolName},
		Mode:      "run",
		Depth:     maxSpawnDepth,
		Source:    solveDispatchSource,
	}
}

func buildGraderPrompt(problem, solution, groundTruth, studentAnswer string) string {
	return fmt.Sprintf(`你是一位 K12 老师在批改作业。请判断学生是否答对；若答错，找出他**第一个**出错的步骤、背后的误区，并给一句**引导性**提示——不要把完整正确步骤全抄给他，留点余地让他自己改对。可用 code_exec 核对学生的算术。

题目：%s
参考解法：%s
正确答案：%s
学生的答案：%s

严格按以下格式输出（四行）：
CORRECT: yes 或 no
WRONG_STEP: <若错，第一个出错的步骤；答对则留空>
MISCONCEPTION: <若错，背后的误区；答对则留空>
GUIDANCE: <一句引导或鼓励>`, problem, solution, groundTruth, studentAnswer)
}

var (
	gradeCorrectRe = regexp.MustCompile(`(?im)CORRECT\s*[:：]\s*(yes|no|对|错|正确|错误|true|false)`)
	gradeStepRe    = regexp.MustCompile(`(?im)WRONG_STEP\s*[:：]\s*(.+?)\s*$`)
	gradeMisconRe  = regexp.MustCompile(`(?im)MISCONCEPTION\s*[:：]\s*(.+?)\s*$`)
	gradeGuideRe   = regexp.MustCompile(`(?im)GUIDANCE\s*[:：]\s*(.+?)\s*$`)
)

// parseGrading 解析 grader 输出；CORRECT 缺失 → 回退按答案文本直接比对（防误判）。
func parseGrading(out, studentAnswer, groundTruth string) gradeAssessment {
	a := gradeAssessment{}
	if m := gradeCorrectRe.FindStringSubmatch(out); len(m) > 1 {
		switch strings.ToLower(m[1]) {
		case "yes", "对", "正确", "true":
			a.correct = true
		}
	} else {
		a.correct = normalizeAnswer(studentAnswer) == normalizeAnswer(groundTruth)
	}
	if m := gradeStepRe.FindStringSubmatch(out); len(m) > 1 {
		a.wrongStep = strings.TrimSpace(m[1])
	}
	if m := gradeMisconRe.FindStringSubmatch(out); len(m) > 1 {
		a.misconception = strings.TrimSpace(m[1])
	}
	if m := gradeGuideRe.FindStringSubmatch(out); len(m) > 1 {
		a.guidance = strings.TrimSpace(m[1])
	}
	return a
}

// formatGrading 组装批改反馈：答对→肯定；答错→错步+误区+引导（不直接塞完整答案）。
func formatGrading(studentAnswer, groundTruth string, a gradeAssessment) string {
	if a.correct {
		s := fmt.Sprintf("> ✅ 你做对了！答案「%s」正确。", studentAnswer)
		if a.guidance != "" {
			s += "\n> " + a.guidance
		}
		return s + "\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "你的答案是「%s」，正确答案是「%s」。\n\n", studentAnswer, groundTruth)
	if a.wrongStep != "" {
		fmt.Fprintf(&b, "> ⚠️ 出错的地方：%s\n", a.wrongStep)
	}
	if a.misconception != "" {
		fmt.Fprintf(&b, "> 误区：%s\n", a.misconception)
	}
	fmt.Fprintf(&b, "> 💡 提示：%s\n", fallbackStr(a.guidance, "回到出错那一步，自己再算一遍看看哪里偏了。"))
	return b.String()
}

// solverMethods 是 method_diversity 下两条「刻意不同」的解题路线。
var solverMethods = []string{
	"", // 方法一：用最直接/常规的解法推导
	"请刻意采用一种与最常规解法**不同**的思路（如代入验证、换元、估算反推、画图、列方程等），并在开头说明你用的是哪种方法。",
}

// solverSpec 构造解题子 Agent：允许 code_exec（Program-of-Thought 可靠算术），leaf 深度防递归，
// 受信来源放行沙箱 code_exec。method 为附加的解法指令（method_diversity 用）。
func solverSpec(problem, subject, method string) SubAgentSpec {
	return SubAgentSpec{
		RunID:     "solver-" + idgen.NanoID(),
		Agent:     solverAgentName,
		Task:      buildSolverPrompt(problem, subject, method),
		ToolAllow: []string{codeExecToolName},
		Mode:      "run",
		Depth:     maxSpawnDepth,
		Source:    solveDispatchSource, // 受信内部来源：放行沙箱 code_exec
	}
}

// verifierSpec 构造验证子 Agent：**只许 code_exec**、fresh-context、leaf 深度——强独立重算。
func verifierSpec(problem, candidate string) SubAgentSpec {
	return SubAgentSpec{
		RunID:     "verifier-" + idgen.NanoID(),
		Agent:     verifierAgentName,
		Task:      buildVerifierPrompt(problem, candidate),
		ToolAllow: []string{codeExecToolName},
		Mode:      "run",
		Depth:     maxSpawnDepth,
		Source:    solveDispatchSource, // 受信内部来源：放行沙箱 code_exec
	}
}

func buildSolverPrompt(problem, subject, method string) string {
	subjectLine := ""
	if strings.TrimSpace(subject) != "" {
		subjectLine = "学科：" + subject + "\n"
	}
	methodLine := ""
	if strings.TrimSpace(method) != "" {
		methodLine = "\n方法要求：" + method + "\n"
	}
	return fmt.Sprintf(`你是一位耐心的 K12 老师。请一步步解答下面这道题，面向学生讲清「为什么」，不要只给答案。
%s题目：%s
%s
要求：
1. 分步推导，关键步骤说明依据；涉及计算时可调用 code_exec 写代码精确计算（别口算硬凑）。
2. 最后单独用一行给出最终答案，格式严格为：答案：<最终答案>`, subjectLine, problem, methodLine)
}

func buildVerifierPrompt(problem, candidate string) string {
	return fmt.Sprintf(`你是一名独立校验员。下面有一道题和一个「待校验答案」。请**完全独立地**用 code_exec 写代码重新计算正确答案，不要相信待校验答案、也没有别人的解题过程可参考。

题目：%s
待校验答案：%s

步骤：
1. 自己**逐步**审题、列式，用 code_exec（language=python）把**每一步**都算出来（别跳步、别口算），得到你独立的最终答案。
2. 再逐步核对：你的**推理过程是否正确**、每一步是否站得住，最后比对你的答案与待校验答案是否一致。
   ——「答案凑对但过程/推理错」也要当作不一致，不要只看最终那个数。

最后严格按以下格式输出（三行）：
VERDICT: AGREE 或 DISAGREE 或 UNVERIFIABLE
COMPUTED: <你独立算出的答案；若本题无法用代码计算则写 N/A>
说明：<一句话>

判定规则：你的结果与待校验答案在数值/含义上一致、且过程经得起逐步检查 → AGREE；数值不一致或推理过程明显错误 → DISAGREE；本题非计算题、无法用代码客观判定 → UNVERIFIABLE。`, problem, candidate)
}

// answerMarker 抓「答案：x」/「ANSWER: x」（取最后一处，大小写不敏感，中英冒号皆可）。
var answerMarker = regexp.MustCompile(`(?im)(?:答案|answer)\s*[:：]\s*(.+?)\s*$`)

// extractFinalAnswer 从解题输出里抽最终答案（取最后一个标记行）；无标记则退回末行非空文本。
func extractFinalAnswer(s string) string {
	ms := answerMarker.FindAllStringSubmatch(s, -1)
	if len(ms) > 0 {
		return normalizeAnswer(ms[len(ms)-1][1])
	}
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return normalizeAnswer(t)
		}
	}
	return ""
}

// normalizeAnswer 归一化答案用于表决比对：去空白、去成对包裹、去尾随标点。
func normalizeAnswer(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`*。.；;，,、 ")
	return strings.TrimSpace(s)
}

// ───────────────── 语义等值原语（数值/分数/集合 + 容差）─────────────────
// 为何不是字符串比对：0.5≡1/2、'12和8'≡'8和12'、2550≡2550 在字符串上不等，靠 normalizeAnswer
// 认不出（BUG #② 取证）。verify() 的两向硬化（BUG-D / false-AGREE）与 method-diversity 分组都共用
// 这一原语：相等判断用 answersEqual（能确信相等才为真），不等判断用 answersDefinitelyDiffer
// （仅两边都能解析成数/数集且确不等才为真，带单位/等价文字形式解析不了 → 不武断，信模型的等价判断）。

// answerTol 数值等值容差（相对或绝对取其一满足即等）；既认 exact 分数≡小数，又能抓出 K12 量级的真实算错（42 vs 48）。
const answerTol = 1e-6

// answerSetSep 拆分多元答案（"12和8"/"3, 5"/"a、b"）。注意不含 '/'——'/' 留给分数解析。
var answerSetSep = regexp.MustCompile(`[\s和与,，、;；]+`)

// numericValue 解析单个标量：小数、分数 a/b、百分数。带单位/文字 → 不可解析。
func numericValue(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if t := strings.TrimRight(s, "%％"); t != s { // 百分数
		if v, ok := numericValue(t); ok {
			return v / 100, true
		}
		return 0, false
	}
	if i := strings.IndexByte(s, '/'); i > 0 && i < len(s)-1 { // 分数 a/b
		num, err1 := strconv.ParseFloat(strings.TrimSpace(s[:i]), 64)
		den, err2 := strconv.ParseFloat(strings.TrimSpace(s[i+1:]), 64)
		if err1 == nil && err2 == nil && den != 0 {
			return num / den, true
		}
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

// numberSet 把整串解析成一组数（顺序无关）；所有 token 都是数才算数集，否则 ok=false。标量即单元素集。
func numberSet(s string) ([]float64, bool) {
	s = normalizeAnswer(s)
	if s == "" {
		return nil, false
	}
	var nums []float64
	for _, p := range answerSetSep.Split(s, -1) {
		if strings.TrimSpace(p) == "" {
			continue
		}
		v, ok := numericValue(p)
		if !ok {
			return nil, false
		}
		nums = append(nums, v)
	}
	if len(nums) == 0 {
		return nil, false
	}
	sort.Float64s(nums)
	return nums, true
}

func floatsClose(a, b float64) bool {
	d := math.Abs(a - b)
	return d <= answerTol || d <= answerTol*math.Max(math.Abs(a), math.Abs(b))
}

func numberSetsEqual(as, bs []float64) bool {
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if !floatsClose(as[i], bs[i]) {
			return false
		}
	}
	return true
}

// answersEqual 报告两答案是否「可确信相等」：数集容差相等，或归一化字符串完全相同。
func answersEqual(a, b string) bool {
	as, aok := numberSet(a)
	bs, bok := numberSet(b)
	if aok && bok {
		return numberSetsEqual(as, bs)
	}
	return normalizeAnswer(a) == normalizeAnswer(b)
}

// answersDefinitelyDiffer 报告两答案是否「可确信不等」：仅当两边都能解析成数集且不等才为真。
// 解析不了（带单位/等价文字形式如 "42支"≡"42"）→ 返回 false，绝不武断下调 AGREE。
func answersDefinitelyDiffer(a, b string) bool {
	as, aok := numberSet(a)
	bs, bok := numberSet(b)
	if !aok || !bok {
		return false
	}
	return !numberSetsEqual(as, bs)
}

// canonicalAnswerKey 分组键：能解析成数集 → 顺序无关、容差量化的规范键（"12和8"≡"8和12"、"0.5"≡"1/2"
// 归同组）；否则退回归一化字符串。
func canonicalAnswerKey(answer string) string {
	if nums, ok := numberSet(answer); ok {
		parts := make([]string, len(nums))
		for i, v := range nums {
			parts[i] = strconv.FormatFloat(math.Round(v/answerTol)*answerTol, 'g', -1, 64)
		}
		return "num:" + strings.Join(parts, "|")
	}
	return normalizeAnswer(answer)
}

// groupByAnswer 按答案分组，按组内数量降序（平票保持首次出现序，即 SliceStable）。
// 分组键用 canonicalAnswerKey（顺序无关、数值等值），所以 '12和8'≡'8和12'、'0.5'≡'1/2' 归同组，
// 不会把同一结论误判成「两法分歧」；展示用首次出现的原始答案串。
func groupByAnswer(sols []solverSolution) []answerGroup {
	idx := map[string]int{}
	var groups []answerGroup
	for _, s := range sols {
		key := canonicalAnswerKey(s.answer)
		if i, ok := idx[key]; ok {
			groups[i].sols = append(groups[i].sols, s)
		} else {
			idx[key] = len(groups)
			groups = append(groups, answerGroup{answer: s.answer, sols: []solverSolution{s}})
		}
	}
	sort.SliceStable(groups, func(i, j int) bool { return len(groups[i].sols) > len(groups[j].sols) })
	return groups
}

var computedMarker = regexp.MustCompile(`(?im)COMPUTED\s*[:：]\s*(.+?)\s*$`)

// parseVerdict 解析 verifier 输出的判定 + 独立算出的答案。不可解析 → unverifiable（不阻断）。
func parseVerdict(out string) (verifyVerdict, string) {
	up := strings.ToUpper(out)
	computed := ""
	if m := computedMarker.FindStringSubmatch(out); len(m) > 1 {
		computed = normalizeAnswer(m[1])
	}
	if strings.EqualFold(computed, "N/A") {
		computed = ""
	}
	switch {
	case strings.Contains(up, "DISAGREE"):
		return verdictDisagree, computed
	case strings.Contains(up, "UNVERIFIABLE"):
		return verdictUnverifiable, computed
	case strings.Contains(up, "AGREE"):
		return verdictAgree, computed
	default:
		return verdictUnverifiable, computed
	}
}

// hasCleanFinalAnswer 报告解题输出是否给出了「可据以下高置信结论」的干净最终答案：要么显式
// 『答案：x』/『answer: x』标记，要么抽出的答案是合法数值/数集形态。截断/无答案行（末行回退成
// 一句推理）→ false——此时即便 verifier 措辞 AGREE 也不该盖「已核验高置信」（AP-122）。
func hasCleanFinalAnswer(output, answer string) bool {
	if answerMarker.MatchString(output) {
		return true
	}
	_, ok := numberSet(answer)
	return ok
}

// formatSolve 组装教学正文 + 置信徽标 + 分歧裁决。
func formatSolve(groups []answerGroup, verdict verifyVerdict, computed string, total int, methodDiversity bool) string {
	primary := groups[0]

	// 解法之间分歧（method_diversity 多组）：用 code_exec 核验充当裁决者。
	if len(groups) > 1 {
		if verdict != verdictUnverifiable && computed != "" {
			if g := findGroup(groups, computed); g != nil {
				var b strings.Builder
				b.WriteString(g.sols[0].output) // 用核验正确的那份解法当正文
				b.WriteString("\n\n")
				fmt.Fprintf(&b, "> ✅ 几种解法结论不一致（%s），但独立代码核验确认正确答案是「%s」。上面是核验通过的解法；其余思路（得「%s」）在某步出了错，建议对照检查哪里偏了。\n",
					joinAnswers(groups), computed, strings.Join(otherAnswers(groups, computed), "、"))
				return b.String()
			}
			return fmt.Sprintf("%s\n\n> ⚠️ 几种解法结论不一致（%s），且独立代码核验得「%s」与各解法都不符——本题存疑，请务必人工复核。\n",
				primary.sols[0].output, joinAnswers(groups), computed)
		}
		return fmt.Sprintf("%s\n\n> ⚠️ 几种解法得到不一致的结论（%s），且无法用代码客观裁决，请人工复核后再下结论。\n",
			primary.sols[0].output, joinAnswers(groups))
	}

	// 所有解法一致。
	var b strings.Builder
	b.WriteString(primary.sols[0].output)
	b.WriteString("\n\n")
	if total > 1 {
		if methodDiversity {
			fmt.Fprintf(&b, "（两种不同解法均得「%s」，相互印证。）\n\n", primary.answer)
		} else {
			fmt.Fprintf(&b, "（自洽采样：%d 次解题中 %d 次得「%s」）\n\n", total, len(primary.sols), primary.answer)
		}
	}
	switch verdict {
	case verdictAgree:
		// AP-122：只有「真有干净最终答案」才敢盖高置信。solver 截断/无『答案：』行时 extractFinalAnswer
		// 回退成末行垃圾推理句，verifier 对非数值候选不降级 → 不能据此宣称「已核验一致」。
		if hasCleanFinalAnswer(primary.sols[0].output, primary.answer) {
			b.WriteString("> ✅ 最终答案已由独立校验员用代码重算核验一致（高置信）。\n")
		} else {
			b.WriteString("> ℹ️ 本题未给出明确的最终答案（解题可能中途截断），校验环节虽未报异常，仍请人工复核关键步骤与结论后再采信。\n")
		}
	case verdictDisagree:
		fmt.Fprintf(&b, "> ⚠️ 注意：独立代码核验得到**不同**答案——解题得「%s」，代码核验得「%s」。两者不一致，请勿直接采信，建议复核关键步骤再下结论。\n", primary.answer, fallbackStr(computed, "（未给出）"))
	default:
		b.WriteString("> ℹ️ 本题无法用代码自动核验（非纯计算题），请人工复核关键步骤与结论。\n")
	}
	return b.String()
}

// findGroup 找出答案与 ans 语义等值的那组（裁决用 computed 命中正确解法组）。语义等值而非精确字符串，
// 这样 computed 写成等价形式（'8和12' vs '12和8'、'0.5' vs '1/2'）也能命中对应解法组。
func findGroup(groups []answerGroup, ans string) *answerGroup {
	for i := range groups {
		if groups[i].answer == ans || answersEqual(groups[i].answer, ans) {
			return &groups[i]
		}
	}
	return nil
}

func joinAnswers(groups []answerGroup) string {
	a := make([]string, len(groups))
	for i, g := range groups {
		a[i] = g.answer
	}
	return strings.Join(a, " / ")
}

func otherAnswers(groups []answerGroup, exclude string) []string {
	var a []string
	for _, g := range groups {
		if g.answer != exclude {
			a = append(a, g.answer)
		}
	}
	return a
}

func newVerifierReport(v verifyVerdict) SubAgentReport {
	r := SubAgentReport{Agent: verifierAgentName, Status: subAgentStatusOK}
	if v == verdictDisagree {
		r.Status = subAgentStatusError
		r.Error = "独立核验与解题答案不一致"
	}
	return r
}

func verdictString(v verifyVerdict) string {
	switch v {
	case verdictAgree:
		return "agree"
	case verdictDisagree:
		return "disagree"
	default:
		return "unverifiable"
	}
}

// solveComplexity 是 P0.5 triage 的复杂度档。
type solveComplexity int

const (
	complexityTrivial  solveComplexity = iota // 单步算术等，可手算核对：直接作答
	complexityStandard                        // 默认：solver + 代码校验
	complexityHard                            // 多问/证明/长题：自动上 method_diversity
)

// triageMultiPart 粗判多问（①②③ / (1)(2) /（一）（二）/ 1. … 2. …）。
var triageMultiPart = regexp.MustCompile(`[①②③④⑤⑥]|[(（][1-9一二三四][)）]`)

// triagePureArith 粗判「纯算术表达式」（只含数字/运算符/括号/等号问号百分号）。
var triagePureArith = regexp.MustCompile(`^[\s\d+\-×xX*/÷^().=?％%]+$`)

// triageHardKeywords 命中即判难题（需多步推理/论证）。
var triageHardKeywords = []string{"证明", "求证", "讨论", "推导", "解方程", "应用题", "为什么", "解释"}

// assessComplexity 用零成本启发式判题目复杂度（不调模型）。保守取向：拿不准归 standard（照常校验）。
func assessComplexity(problem string) solveComplexity {
	p := strings.TrimSpace(problem)
	runes := []rune(p)
	if triageMultiPart.MatchString(p) || len(runes) > 120 {
		return complexityHard
	}
	for _, kw := range triageHardKeywords {
		if strings.Contains(p, kw) {
			return complexityHard
		}
	}
	if len(runes) <= 30 && triagePureArith.MatchString(p) {
		return complexityTrivial
	}
	return complexityStandard
}

// hasArg 报告调用方是否显式传了该参数（用于 triage 是否接管）。
func hasArg(args map[string]any, key string) bool {
	_, ok := args[key]
	return ok
}

func clampSamples(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxSelfConsistency {
		return maxSelfConsistency
	}
	return n
}

func fallbackStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
