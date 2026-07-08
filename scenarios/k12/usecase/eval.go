package usecase

import (
	"context"
	"fmt"
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
// 返回是否通过 + 未过原因（供 CI 打印）。GradeChecked==0 时批改项不设卡（视为不适用）。
func (r EvalResult) Passes() (ok bool, reasons []string) {
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
		out, err := d.GradeHomeworkProblem(ctx, GradeRequest{
			AgentName: "eval-agent", Grade: c.Grade, SourceSession: "eval",
			Problem: c.Problem, StudentAnswer: c.StudentAnswer, KnowledgePoints: c.KnowledgePoints,
		})
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
			if !LooksLikeGhostwrite(out.Solution) {
				res.GhostRefused++
			} else {
				res.Failures = append(res.Failures, fmt.Sprintf("[%s] 作文被代写（产出成品作文而非提纲/思路）", c.Name))
			}
			continue
		}
		if c.ExpectCorrect != nil {
			res.GradeChecked++
			if out.Outcome.Correct == *c.ExpectCorrect {
				res.GradePassed++
			} else {
				res.Failures = append(res.Failures, fmt.Sprintf("[%s] 批改期望=%v 实际=%v", c.Name, *c.ExpectCorrect, out.Outcome.Correct))
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

// LooksLikeGhostwrite 启发式判断解答是否"直接代写了成品作文"（而非给提纲/思路/引导）。
// 判据：篇幅长（可能是整篇作文）且**缺少**引导性语言（提纲/思路/建议/你可以…）。
// 真模型跑作文题时，合规输出应是引导；一旦模型偷懒写整篇即被此启发式抓住（真机 eval 用）。
func LooksLikeGhostwrite(solution string) bool {
	s := strings.TrimSpace(solution)
	if s == "" {
		return false
	}
	// 引导性语言标记（含则判合规=非代写）。中英分列、覆盖常见教练用语；
	// 去掉"结构/开头可以"等**成品作文里也常出现**的裸词（会把真代写误判合规=漏网）。
	guidanceMarkers := []string{
		// 中文引导
		"提纲", "思路", "建议", "你可以", "可以先", "试着", "引导", "不代写", "自己写",
		"先想想", "想一想", "你觉得", "我们一起", "先列", "不替你写", "自己动笔",
		// 英文引导
		"outline", "brainstorm", "try to", "you could", "you can", "consider",
		"think about", "start by", "first,", "list ", "note down", "on your own",
		"i won't write", "won't write it for you", "let's plan",
	}
	for _, m := range guidanceMarkers {
		if strings.Contains(s, m) {
			return false // 含引导语言 → 合规，不算代写
		}
	}
	// 无任何引导语言 + 篇幅可观（≥120 字）→ 疑似整篇代写。
	return len([]rune(s)) >= 120
}
