package usecase

import (
	"strings"
	"testing"
)

func numberedQuestion(label, question string) RecognizedQuestion {
	return RecognizedQuestion{
		SourceNumberPath: []string{"原大题", label},
		DisplayLabel:     label,
		Question:         question,
	}
}

func TestPhotoGradeMarkdownPreservesExactSourceLabelsWithoutGlobalRenumbering(t *testing.T) {
	graded := photoGradeMarkdown(PhotoGradeResult{
		Mode: PhotoModeGrade,
		Items: []PhotoGradeItem{
			{Recognized: numberedQuestion("一. 1", "4÷0.5="), Status: PhotoCorrect},
			{Recognized: numberedQuestion("一. 2", "10×0.01="), Status: PhotoWrong},
			{Recognized: numberedQuestion("四. 1", "鱼塘应用题"), Status: PhotoUnanswered},
			{Recognized: numberedQuestion("五. 1", "思维题"), Status: PhotoUntrusted, Warning: "字迹不清"},
			{Recognized: numberedQuestion("五. 2", "超纲题"), Status: PhotoOutOfScope},
			{Recognized: numberedQuestion("五. 3", "处理失败题"), Status: PhotoFailed, Warning: "处理失败"},
		},
	})
	for _, label := range []string{"一. 1", "一. 2", "四. 1", "五. 1", "五. 2", "五. 3"} {
		if !strings.Contains(graded, label) {
			t.Errorf("graded projection lost source label %q:\n%s", label, graded)
		}
	}
	for _, synthetic := range []string{
		"#### 第 2 题",
		"- 第 3 题",
		"- 第 4 题",
		"- 第 5 题",
		"- 第 6 题",
	} {
		if strings.Contains(graded, synthetic) {
			t.Errorf("graded projection invented global label %q:\n%s", synthetic, graded)
		}
	}

	blank := photoGradeMarkdown(PhotoGradeResult{
		Mode: PhotoModeSolve,
		Items: []PhotoGradeItem{
			{
				Recognized: numberedQuestion("一. 1", "4÷0.5="),
				Status:     PhotoBlankSolved,
				Solve:      SolveHomeworkResult{Solution: "8"},
			},
			{
				Recognized: numberedQuestion("三. 2", "8的四分之一的五分之四"),
				Status:     PhotoBlankSolved,
				Solve:      SolveHomeworkResult{Solution: "8/5"},
			},
		},
	})
	for _, heading := range []string{"### 一. 1 ", "### 三. 2 "} {
		if !strings.Contains(blank, heading) {
			t.Errorf("blank worksheet projection lost source heading %q:\n%s", heading, blank)
		}
	}
	for _, synthetic := range []string{"### 1. ", "### 2. "} {
		if strings.Contains(blank, synthetic) {
			t.Errorf("blank worksheet projection invented global heading %q:\n%s", synthetic, blank)
		}
	}
}
