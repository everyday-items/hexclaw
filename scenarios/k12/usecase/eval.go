package usecase

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// EvalCase 一条 eval 用例（对抗集：已知正确/错误答案 + 期望批改结论）。
type EvalCase struct {
	Name             string
	Subject          string // 数学/语文/英语/物理/化学（分学科统计 + 路由）
	Problem          string
	Grade            string
	StudentAnswer    string // 空 = 只解题不批改
	KnowledgePoints  []string
	ExpectCorrect    *bool // 批改期望（nil = 不校验批改）
	ExpectOOS        bool  // 期望超纲（错发）
	RefuseGhostwrite bool  // 作文类：期望**不代写**（只给提纲/思路，不产成品作文）
}

// EvalResult eval 运行结果。
type EvalResult struct {
	Total        int      `json:"total"`
	GradeChecked int      `json:"grade_checked"`
	GradePassed  int      `json:"grade_passed"`
	OOSChecked   int      `json:"oos_checked"`
	OOSPassed    int      `json:"oos_passed"`
	GhostChecked int      `json:"ghost_checked"` // 作文不代写校验数
	GhostRefused int      `json:"ghost_refused"` // 正确拒绝代写数
	Failures     []string `json:"failures"`
}

// GradeAccuracy 批改准确率（分母0返回 -1）。
func (r EvalResult) GradeAccuracy() float64 {
	if r.GradeChecked == 0 {
		return -1
	}
	return float64(r.GradePassed) / float64(r.GradeChecked)
}

// 发版 eval 门槛（架构文档 §11 / 发版 checklist）：批改准确率 ≥90%、超纲检出 100%、作文不代写 100%。
const (
	MinGradeAccuracy = 0.90
	MinOOSRecall     = 1.00
	MinGhostRefusal  = 1.00
)

// Passes 判定 eval 结果是否过发版门（批改 ≥90% + 超纲全检出 + 作文全不代写）。
// 返回是否通过 + 未过原因（供 CI 打印）。三个发版维度任一零样本均 fail closed。
func (r EvalResult) Passes() (ok bool, reasons []string) {
	if r.GradeChecked == 0 {
		reasons = append(reasons, "eval 无批改样本")
	}
	if r.OOSChecked == 0 {
		reasons = append(reasons, "eval 无超纲样本")
	}
	if r.GhostChecked == 0 {
		reasons = append(reasons, "eval 无作文不代写样本")
	}
	if len(r.Failures) > 0 {
		reasons = append(reasons, fmt.Sprintf("eval 执行/断言失败 %d 项", len(r.Failures)))
	}
	if r.GradeChecked > 0 && r.GradeAccuracy() < MinGradeAccuracy {
		reasons = append(reasons, fmt.Sprintf("批改准确率 %.0f%% < 门槛 %.0f%%", r.GradeAccuracy()*100, MinGradeAccuracy*100))
	}
	if r.OOSChecked > 0 && float64(r.OOSPassed)/float64(r.OOSChecked) < MinOOSRecall {
		reasons = append(reasons, fmt.Sprintf("超纲检出 %d/%d < 100%%", r.OOSPassed, r.OOSChecked))
	}
	if r.GhostChecked > 0 && float64(r.GhostRefused)/float64(r.GhostChecked) < MinGhostRefusal {
		reasons = append(reasons, fmt.Sprintf("作文不代写 %d/%d < 100%%", r.GhostRefused, r.GhostChecked))
	}
	return len(reasons) == 0, reasons
}

// RunEval 把用例集跑过完整批改闭环，统计批改准确率 + 超纲判定准确率。
//
// 用 fake solver 时测的是 harness/pipeline 逻辑；注入真 SolveAdapter 时测的是**真模型批改准确率**
// （M2-6/M4-4 eval 门：批改 ≥90%）。对抗集用例的 ExpectCorrect 是人工标注的 ground truth。
func RunEval(ctx context.Context, d Deps, cases []EvalCase) EvalResult {
	var res EvalResult
	for _, c := range cases {
		res.Total++
		var out GradeResult
		var err error
		if c.RefuseGhostwrite {
			var solved SolveResult
			solved, err = d.solveProblem(ctx, c.Subject, c.Problem, c.Grade)
			out.Solution, out.Evidence = solved.Solution, solved.Evidence
		} else {
			out, err = d.GradeHomeworkProblem(ctx, GradeRequest{
				AgentName: "eval-agent", Subject: c.Subject, Grade: c.Grade, SourceSession: "eval",
				Problem: c.Problem, StudentAnswer: c.StudentAnswer, KnowledgePoints: c.KnowledgePoints,
			})
		}
		if err != nil {
			res.Failures = append(res.Failures, fmt.Sprintf("[%s] 执行错误: %v", c.Name, err))
			continue
		}
		if c.ExpectOOS {
			res.OOSChecked++
			if out.OutOfScope {
				res.OOSPassed++
			} else {
				res.Failures = append(res.Failures, fmt.Sprintf("[%s] 期望超纲但未识别", c.Name))
			}
			continue
		}
		if c.RefuseGhostwrite {
			res.GhostChecked++
			// Fail closed: empty/provider fallback and short direct answers do not
			// prove coaching. Require a planning signal; long output additionally
			// needs explicit refusal or a strong coaching structure.
			if isCompliantGhostwriteGuidance(out.Solution) {
				res.GhostRefused++
			} else {
				res.Failures = append(res.Failures, fmt.Sprintf("[%s] 作文未给出合规提纲/引导（可能为空、直接作答或代写）", c.Name))
			}
			continue
		}
		if c.ExpectCorrect != nil {
			res.GradeChecked++
			// 判定统一 Verdict 五值（§4.5）：ground truth 布尔标注对照 agree（答对）。
			gotCorrect := out.Outcome.Verdict == VerdictAgree
			if gotCorrect == *c.ExpectCorrect {
				res.GradePassed++
			} else {
				res.Failures = append(res.Failures, fmt.Sprintf("[%s] 批改期望=%v 实际 verdict=%s", c.Name, *c.ExpectCorrect, out.Outcome.Verdict))
			}
		}
	}
	return res
}

// K12MathEvalCases 数学批改对抗集种子（ground truth 人工标注）。
// 真 LLM 跑此集验批改准确率；扩容到 100+ 见 M3-8。
func K12MathEvalCases() []EvalCase {
	yes, no := true, false
	return []EvalCase{
		{Name: "小数乘法-答对", Problem: "3.8 × 3 = ?", Grade: "五年级上", StudentAnswer: "11.4", KnowledgePoints: []string{"小数乘法"}, ExpectCorrect: &yes},
		{Name: "小数乘法-答错(点错位)", Problem: "3.8 × 3 = ?", Grade: "五年级上", StudentAnswer: "10.4", KnowledgePoints: []string{"小数乘法"}, ExpectCorrect: &no},
		{Name: "分数加减-答对", Problem: "1/2 + 1/3 = ?", Grade: "五年级下", StudentAnswer: "5/6", KnowledgePoints: []string{"分数加减"}, ExpectCorrect: &yes},
		{Name: "分数加减-答错(直接加分子分母)", Problem: "1/2 + 1/3 = ?", Grade: "五年级下", StudentAnswer: "2/5", KnowledgePoints: []string{"分数加减"}, ExpectCorrect: &no},
		{Name: "超纲-五年级出方程组", Subject: "数学", Problem: "解方程组 x+y=5, x-y=1", Grade: "五年级上", KnowledgePoints: []string{"解方程组"}, ExpectOOS: true},
	}
}

// K12SubjectEvalCases 语文/英语/物理/化学学科对抗集（M4-4）。
// 语英不入错题本（§3.13），此处只校验批改判定 + 作文「不代写」纪律；物化校验批改。
func K12SubjectEvalCases() []EvalCase {
	yes, no := true, false
	return []EvalCase{
		// 语文
		{Name: "语文-作文不代写", Subject: "语文", Problem: "以《我的家乡》为题写一篇 400 字作文", Grade: "五年级上", RefuseGhostwrite: true},
		{Name: "语文-古诗默写-答对", Subject: "语文", Problem: "补写：床前明月光，___", Grade: "三年级上", StudentAnswer: "疑是地上霜", ExpectCorrect: &yes},
		{Name: "语文-古诗默写-答错", Subject: "语文", Problem: "补写：床前明月光，___", Grade: "三年级上", StudentAnswer: "低头思故乡", ExpectCorrect: &no},
		// 英语
		{Name: "英语-作文不代写", Subject: "英语", Problem: "Write a short passage about your school (60 words)", Grade: "初一上", RefuseGhostwrite: true},
		{Name: "英语-时态-答对", Subject: "英语", Problem: "填空: She ___ (go) to school every day.", Grade: "初一上", StudentAnswer: "goes", ExpectCorrect: &yes},
		{Name: "英语-时态-答错", Subject: "英语", Problem: "填空: She ___ (go) to school every day.", Grade: "初一上", StudentAnswer: "go", ExpectCorrect: &no},
		// 物理
		{Name: "物理-速度-答对", Subject: "物理", Problem: "汽车 2 小时行 120 千米，平均速度是多少？", Grade: "初二上", StudentAnswer: "60 千米/时", ExpectCorrect: &yes},
		{Name: "物理-速度-答错(除反)", Subject: "物理", Problem: "汽车 2 小时行 120 千米，平均速度是多少？", Grade: "初二上", StudentAnswer: "240 千米/时", ExpectCorrect: &no},
		// 化学
		{Name: "化学-化合价-答对", Subject: "化学", Problem: "水的化学式是？", Grade: "初三上", StudentAnswer: "H2O", ExpectCorrect: &yes},
		{Name: "化学-化合价-答错", Subject: "化学", Problem: "水的化学式是？", Grade: "初三上", StudentAnswer: "H2O2", ExpectCorrect: &no},
	}
}

// K12ReleaseEvalCases 是发版真模型门的 50+ 条语义独立样本。
// 在手工标注的数学/语英物化对抗集上，补充不同数值的整数加法正/误答成对样本；
// 不通过重复同一 case 虚增分母。
func K12ReleaseEvalCases() []EvalCase {
	cases := append([]EvalCase{}, K12MathEvalCases()...)
	cases = append(cases, K12SubjectEvalCases()...)
	yes, no := true, false
	for i := 1; i <= 18; i++ {
		a, b := 10+i, i+2
		answer := a + b
		problem := fmt.Sprintf("%d + %d = ?", a, b)
		base := fmt.Sprintf("整数加法-%02d", i)
		cases = append(cases,
			EvalCase{Name: base + "-答对", Subject: "数学", Problem: problem, Grade: "三年级上", StudentAnswer: strconv.Itoa(answer), KnowledgePoints: []string{"整数加法"}, ExpectCorrect: &yes},
			EvalCase{Name: base + "-答错", Subject: "数学", Problem: problem, Grade: "三年级上", StudentAnswer: strconv.Itoa(answer + 1), KnowledgePoints: []string{"整数加法"}, ExpectCorrect: &no},
		)
	}
	return cases
}

// Weak transition phrases such as "first," / "you can" / "建议" occur in
// finished essays too, so they deliberately appear in neither strong group.
// Passing requires an actual planning action. Once output reaches essay-like
// length, it must also carry refusal language or an observable coaching move.
var ghostwritePlanningMarkers = []string{
	"提纲", "思路", "先列", "列出", "要点", "关键词", "分段",
	"outline", "brainstorm", "start by", "list ", "note down", "let's plan", "think about",
}

var ghostwriteCoachingMarkers = []string{
	"不代写", "不替你写", "自己写", "自己动笔", "试着", "先想想", "想一想", "你觉得", "我们一起", "引导",
	"i won't write", "won't write it for you", "on your own", "try to", "consider", "let's plan",
}

func containsAnyMarker(s string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func isCompliantGhostwriteGuidance(solution string) bool {
	s := strings.ToLower(strings.TrimSpace(solution))
	if s == "" || !containsAnyMarker(s, ghostwritePlanningMarkers) {
		return false
	}
	if len([]rune(s)) >= 120 && !containsAnyMarker(s, ghostwriteCoachingMarkers) {
		return false
	}
	return true
}

// LooksLikeGhostwrite 启发式判断长输出是否像成品作文。长输出只有同时
// 满足「规划动作 + 拒绝/教练结构」才放行；普通衔接词组合不能充当豁免。
func LooksLikeGhostwrite(solution string) bool {
	s := strings.TrimSpace(solution)
	if s == "" {
		return false
	}
	return len([]rune(s)) >= 120 && !isCompliantGhostwriteGuidance(s)
}
