package usecase

import (
	"strings"
	"testing"
)

func TestDingTalkPhotoMarkdownUsesCanonicalChineseProcessIssueTerms(t *testing.T) {
	finalAnswerCorrect := true
	got := photoGradeMarkdown(PhotoGradeResult{
		Mode: PhotoModeGrade,
		Items: []PhotoGradeItem{{
			Recognized: RecognizedQuestion{Question: "300 ÷ 2 ÷ 2 = ?", StudentAnswer: "50"},
			Status:     PhotoCorrectWithProcessIssue,
			Grade: GradeResult{Outcome: GradeOutcome{
				FinalAnswerCorrect: &finalAnswerCorrect,
				WrongStep:          "300 ÷ 2 ÷ 2 = 50",
				ErrorCause:         "连续除法计算错误",
			}},
			ParentGuide: &ParentTeachingGuide{
				Answer:                 "50",
				FullSolutionSteps:      []string{"先算 300 ÷ 2 = 150，再算 150 ÷ 2 = 75"},
				GradeLevelMethod:       "按顺序计算每一步",
				LikelyMistakes:         []string{"忽略中间结果"},
				ParentTeachingSequence: []string{"先核对第一步"},
				FollowUpQuestions:      []string{"第一步算出的结果是多少？"},
				CheckingMethod:         "逐步验算",
			},
		}},
	})

	for _, want := range []string{
		"- 共识别 **1** 道题",
		"**1** 道过程问题",
		"### ⚠️ 过程问题（1）",
		"过程问题表示最终答案正确，但书写过程需要核对，不记为错题。",
		"- **题目：** 300 ÷ 2 ÷ 2 = ?",
		"- **你的作答：** 50",
		"- **错误步骤：** 300 ÷ 2 ÷ 2 = 50",
		"- **原因：** 连续除法计算错误",
		"##### 家长怎么讲",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("photo Markdown lacks canonical Chinese term %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"questions recognized",
		"with process issues",
		"requiring correction",
		"Process issues",
		"The final answer is correct",
		"Question:",
		"Your answer:",
		"Process note:",
		"Cause:",
		"How the parent can explain it",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("photo Markdown leaked internal English term %q:\n%s", forbidden, got)
		}
	}
}
