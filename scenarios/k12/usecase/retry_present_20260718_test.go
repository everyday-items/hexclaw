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
	solution := "## 问题\n\n3.9 × 4 = ?\n\n## 解答\n\n1. 先按整数算：39 × 4 = 156\n2. 再点小数点\n\n## 答案\n\n**15.6**"
	q, a, expected := usecase.SplitRetryPresentation(solution)
	if !strings.Contains(q, "3.9 × 4 = ?") {
		t.Fatalf("题面应取 ## 问题 章节，got %q", q)
	}
	if strings.Contains(q, "15.6") || strings.Contains(q, "156") {
		t.Fatalf("题面不得泄露解答/答案，got %q", q)
	}
	if !strings.Contains(a, "先按整数算") || !strings.Contains(a, "15.6") {
		t.Fatalf("解答部分应含解题过程与答案，got %q", a)
	}
	if expected != "15.6" {
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
