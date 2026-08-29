package main

import (
	"encoding/json"
	"strings"
	"testing"

	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestK12FinalArtifactIMProjectionLocalizesLegacyEnglishMarkdown(t *testing.T) {
	finalAnswerCorrect := true
	legacyItem := k12usecase.PhotoGradeItem{
		Recognized: k12usecase.RecognizedQuestion{
			ProblemID:         "legacy-problem-15",
			AttemptID:         "legacy-attempt-15",
			InputDigest:       "sha256:legacy-input-15",
			ConfirmedVersion:  1,
			CanonicalMarkdown: "300 ÷ 2 ÷ 2 = 50",
			Question:          "300 ÷ 2 ÷ 2 = ?",
			StudentAnswer:     "50",
		},
		Status: k12usecase.PhotoCorrectWithProcessIssue,
		Grade: k12usecase.GradeResult{Outcome: k12usecase.GradeOutcome{
			FinalAnswerCorrect: &finalAnswerCorrect,
			WrongStep:          "300 ÷ 2 ÷ 2 = 50",
			ErrorCause:         "连续除法计算错误",
		}},
		ParentGuide: &k12usecase.ParentTeachingGuide{
			Answer:                 "50",
			FullSolutionSteps:      []string{"先算 300 ÷ 2 = 150，再算 150 ÷ 2 = 75"},
			GradeLevelMethod:       "按顺序计算每一步",
			LikelyMistakes:         []string{"忽略中间结果"},
			ParentTeachingSequence: []string{"先核对第一步"},
			FollowUpQuestions:      []string{"第一步算出的结果是多少？"},
			CheckingMethod:         "逐步验算",
		},
	}
	resultJSON, err := json.Marshal(legacyItem)
	if err != nil {
		t.Fatal(err)
	}

	legacy := "# 作业批改结果\n\n## Grading summary\n\nThis run determined **16** questions: **14 correct / 2 with process issues / 0 requiring correction**.\n\n> A process issue has a correct final answer and is not recorded as wrong.\n\n## 第 15 题\n\n300 ÷ 2 ÷ 2 = 50\n\n**Grading status:** ⚠ Process issue (final answer correct; not recorded as wrong) · `correct_with_process_issue`\n\n**Process note:** 300 ÷ 2 ÷ 2 = 50\n\n**Cause:** 连续除法计算错误\n\n### How the parent can explain it\n\n```json\n" + string(resultJSON) + "\n```"

	got := k12FinalArtifactIMMarkdown(legacy)
	for _, want := range []string{
		"## 批改摘要",
		"**14 道正确 / 2 道过程问题**",
		"过程问题表示最终答案正确，但书写过程需要核对，不记为错题。",
		"**批改状态：** ⚠ 过程问题（最终答案正确，不记为错题）",
		"**错误步骤：** 300 ÷ 2 ÷ 2 = 50",
		"**原因：** 连续除法计算错误",
		"### 家长怎么讲",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("localized DingTalk Markdown lacks %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"Grading summary",
		"This run determined",
		"process issues",
		"requiring correction",
		"A process issue has a correct final answer",
		"Grading status",
		"Process issue (final answer correct; not recorded as wrong)",
		"correct_with_process_issue",
		"Process note",
		"How the parent can explain it",
		"```json",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("legacy internal English leaked into DingTalk Markdown %q:\n%s", forbidden, got)
		}
	}
}
