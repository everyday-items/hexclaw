package usecase_test

import (
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// 2026-07-18 P2 清偿契约：「再练一道」题答分离（守答案遮罩红线：先别给孩子看）。
// 变式题正文经 engineadapter.normalizeRetryMarkdown 收口成 ## 问题 / ## 解答 / ## 答案 章节，
// usecase 侧按章节拆分：题面（先显）与解答（默认遮罩）。拆不出章节时诚实回退——
// question 为空，前端整段遮罩（最小闭环，不猜测题答边界）。

func TestSplitRetryPresentationSections(t *testing.T) {
	solution := "## 问题\n\n计算：36 × 3 = ？\n\n## 解答\n\n计划：\n1. 说明乘法算式的意义。\n2. 把36分成30和6，分别乘3。\n3. 合并结果，并核对计算。\n\n第 1 步：理解题意  \n36 × 3表示3个36相加，即：\n\n36 ＋ 36 ＋ 36\n\n第 2 步：分开计算  \n把36看成30和6：\n\n36 × 3  \n＝ 30 × 3 ＋ 6 × 3  \n＝ 90 ＋ 18\n\n这样计算的依据是乘法运算定律。\n\n第 3 步：合并结果  \n90 ＋ 18 ＝ 108\n\n用代码独立核对，结果也是108。\n\n## 答案\n\n**108**\n\n> ✅ 最终答案已由独立校验员用代码重算核验一致（高置信）。"
	q, a, expected := usecase.SplitRetryPresentation(solution)
	if q != "计算：36 × 3 = ？" {
		t.Fatalf("题面应取 ## 问题 章节，got %q", q)
	}
	if strings.Contains(q, "108") || strings.Contains(q, "90 ＋ 18") {
		t.Fatalf("题面不得泄露解答/答案，got %q", q)
	}
	if a != solution[strings.Index(solution, "## 解答"):] {
		t.Fatalf("解答部分应含解题过程与答案，got %q", a)
	}
	if expected != "108" {
		t.Fatalf("expected_answer 应为答案章节正文（去粗体标记），got %q", expected)
	}
}

// 无章节（如积累原词重现、模型偶发无结构输出）→ 诚实回退：question 空、answer=整段。
func TestSplitRetryPresentationFallback(t *testing.T) {
	solution := "再默一遍（英语·错词）：believe\n判定：一字不差即正确。"
	q, a, expected := usecase.SplitRetryPresentation(solution)
	if q != "" {
		t.Fatalf("拆不出题面时 question 应为空（前端整段遮罩），got %q", q)
	}
	if a != solution {
		t.Fatalf("回退时 answer 应为整段原文，got %q", a)
	}
	if expected != "" {
		t.Fatalf("回退时 expected_answer 应为空，got %q", expected)
	}
}

// 只有问题没有解答 → 同样回退（题答边界不成立，不硬拆）。
func TestSplitRetryPresentationQuestionOnly(t *testing.T) {
	solution := "## 问题\n\n只有题面没有解"
	q, a, _ := usecase.SplitRetryPresentation(solution)
	if q != "" || a != solution {
		t.Fatalf("缺解答章节应整段回退，got q=%q a=%q", q, a)
	}
}
