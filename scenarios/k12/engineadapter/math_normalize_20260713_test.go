package engineadapter

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill"
)

// BUG-20260713（真机取证·桌面 K12 错题「再练一道」弹层）：解答正文把 LaTeX 原文
// 原样漏给桌面前端——用户看到 `\( V = l \times w \times h \)`、`\[ V = 6 \, \text{cm} \times ... \]`、
// `\text{cm}^3`，数学符号没渲染。桌面 API 路径不像 IM egress 会过 NormalizeMathText。
//
// 治本：把数学归一化下沉到 engineadapter 的单一出站口（stripReports/GradeSubject 元数据），
// 让 solve / grade 讲解 / 再练 / prep-card / 错因 全部拿到干净 Unicode。
//
// RED（fix 前）：Solution/ErrorCause 原样含 \( \times \text{ \frac $ → 断言失败。
// GREEN（fix 后）：不含任何 LaTeX 痕迹、含 Unicode ×÷ 上下标。
func assertNoLatex(t *testing.T, field, s string) {
	t.Helper()
	for _, bad := range []string{`\(`, `\)`, `\[`, `\]`, `\times`, `\div`, `\frac`, `\text{`, `\,`, "$"} {
		if strings.Contains(s, bad) {
			t.Errorf("%s 仍含 LaTeX 痕迹 %q\n  全文: %q", field, bad, s)
		}
	}
}

func TestSolveAdapter_Solve_NormalizesLatexToUnicode(t *testing.T) {
	a := NewSolveAdapter(&fakeExec{
		solveResult: &skill.Result{
			Content: `长方体体积 \( V = l \times w \times h \)：\n` +
				`\[ V = 6 \, \text{cm} \times 6 \, \text{cm} \times 6 \, \text{cm} \]\n` +
				`答案：216 \text{cm}^3`,
			Metadata: map[string]string{"solve_verdict": "agree", "solve_evidence": "numeric_exec"},
		},
	})
	sr, err := a.Solve(context.Background(), "求正方体体积", "五年级上", "体积")
	if err != nil {
		t.Fatal(err)
	}
	assertNoLatex(t, "solve.Solution", sr.Solution)
	if !strings.Contains(sr.Solution, "×") {
		t.Errorf("应含 Unicode 乘号 ×, got %q", sr.Solution)
	}
	if !strings.Contains(sr.Solution, "cm³") {
		t.Errorf("cm^3 应转 Unicode 上标 cm³, got %q", sr.Solution)
	}
}

func TestSolveAdapter_GenerateSimilar_NormalizesLatex(t *testing.T) {
	a := NewSolveAdapter(&countingExec{}, WithRetryGen(func(_ context.Context, _, _, _ string) (string, error) {
		return `再练：$3.9 \times 3 = ?$，用 \frac{1}{2} 想一想。`, nil
	}))
	sr, err := a.GenerateSimilar(context.Background(), "数学", "出一道", "五年级上")
	if err != nil {
		t.Fatal(err)
	}
	assertNoLatex(t, "retry.Solution", sr.Solution)
	if !strings.Contains(sr.Solution, "×") || !strings.Contains(sr.Solution, "1/2") {
		t.Errorf("应含 Unicode × 与 1/2, got %q", sr.Solution)
	}
}

func TestSolveAdapter_Grade_NormalizesErrorCause(t *testing.T) {
	a := NewSolveAdapter(&fakeExec{
		gradeResult: &skill.Result{
			Metadata: map[string]string{
				"grade_correct":       "false",
				"grade_wrong_step":    `第二步 \( 3.8 \times 3 \) 误算`,
				"grade_misconception": `把 \frac{1}{2} 当成了 0.2`,
			},
		},
	})
	out, err := a.Grade(context.Background(), "3.8×3=?", "10.4", "")
	if err != nil {
		t.Fatal(err)
	}
	assertNoLatex(t, "grade.WrongStep", out.WrongStep)
	assertNoLatex(t, "grade.ErrorCause", out.ErrorCause)
	if !strings.Contains(out.WrongStep, "×") || !strings.Contains(out.ErrorCause, "1/2") {
		t.Errorf("错步/错因应转 Unicode: step=%q cause=%q", out.WrongStep, out.ErrorCause)
	}
}
