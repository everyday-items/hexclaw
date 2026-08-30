package usecase

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestBUGK12DingTalkPhotoMarkdownLocalization20260825(t *testing.T) {
	entries := make([]gradingFinalEntry, 0, 16)
	for ordinal := 1; ordinal <= 14; ordinal++ {
		ordinalText := strconv.Itoa(ordinal)
		entries = append(entries, gradingFinalEntry{
			question: RecognizedQuestion{DisplayLabel: "第 " + ordinalText + " 题"},
			assessment: &k12.GradingAssessmentItem{
				Status:       k12.GradingAssessmentCorrect,
				ResultJSON:   `{"Status":"correct","internal":"must-not-leak"}`,
				ResultDigest: "sha256:correct-" + ordinalText,
			},
		})
	}
	entries = append(entries,
		gradingProcessIssueEntryForMarkdownTest(
			"第 15 题",
			"11250",
			"300 ÷ 2 ÷ 2 = 50",
			"该步算术不成立：300 ÷ 2 = 150，150 ÷ 2 = 75，不是 50；当前书写过程不能支持最终答案。",
			"先让孩子只验这一行的两次除法，再从这一步重新核对后续算式；不要用已经正确的最终答案倒推过程。",
		),
		gradingProcessIssueEntryForMarkdownTest(
			"第 16 题",
			"29",
			"42 = 18 × 2",
			"等号两边不相等，且原过程中的算式相互矛盾，无法作为最终答案 29 的可复核证据。",
			"逐行做等号检查，先算出 18 × 2 = 36 并标出冲突，再请孩子从上一条可信算式重新写到最终答案。",
		),
	)

	beforeJSON := make([]string, len(entries))
	beforeDigest := make([]string, len(entries))
	for index, entry := range entries {
		beforeJSON[index] = entry.assessment.ResultJSON
		beforeDigest[index] = entry.assessment.ResultDigest
	}

	markdown := renderCanonicalGradingFinal(entries, &TutoringTips{Sections: []TutoringTipsSection{
		{Title: "旧概览", Content: "No reliable explanation was generated this time."},
		{Title: "旧学情", Content: "problem-internal-history"},
		{Title: "旧逐题讲解", Content: "problem-internal-guide"},
	}})
	for _, want := range []string{
		"## 批改摘要",
		"**共 16 题 · 14 题正确 · 2 题过程需关注**",
		"过程问题表示最终答案正确，但书写过程需要核对，不记为错题。",
		"## 需关注的题",
		"### 第 15 题",
		"**原始作答：** 11250",
		"**正确答案：** 11250",
		"**批改状态：** ⚠ 过程问题（最终答案正确，不记为错题）",
		"**错误步骤：** 300 ÷ 2 ÷ 2 = 50",
		"**原因：** 该步算术不成立：300 ÷ 2 = 150，150 ÷ 2 = 75，不是 50；当前书写过程不能支持最终答案。",
		"### 家长怎么讲",
		"先让孩子只验这一行的两次除法，再从这一步重新核对后续算式；不要用已经正确的最终答案倒推过程。",
		"### 第 16 题",
		"**错误步骤：** 42 = 18 × 2",
		"**原因：** 等号两边不相等，且原过程中的算式相互矛盾，无法作为最终答案 29 的可复核证据。",
		"逐行做等号检查，先算出 18 × 2 = 36 并标出冲突，再请孩子从上一条可信算式重新写到最终答案。",
		"## 已答对的题",
		"其余 14 题已答对。",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("canonical parent markdown lacks %q:\n%s", want, markdown)
		}
	}
	for _, forbidden := range []string{
		"Grading summary",
		"Grading status",
		"Process note",
		"How the parent can explain it",
		"correct_with_process_issue",
		"```json",
		`"Status"`,
		`"internal"`,
		"2 道错题",
		"# 这份作业的辅导要点",
		"No reliable explanation",
		"problem-internal",
		"\n### 第 1 题\n",
	} {
		if strings.Contains(markdown, forbidden) {
			t.Fatalf("canonical parent markdown leaked %q:\n%s", forbidden, markdown)
		}
	}

	for index, entry := range entries {
		if entry.assessment.ResultJSON != beforeJSON[index] ||
			entry.assessment.ResultDigest != beforeDigest[index] {
			t.Fatalf("render mutated durable item %d", index+1)
		}
	}
}

func gradingProcessIssueEntryForMarkdownTest(
	label string,
	answer string,
	wrongStep string,
	cause string,
	parentExplanation string,
) gradingFinalEntry {
	item := PhotoGradeItem{
		Recognized: RecognizedQuestion{
			DisplayLabel:  label,
			Question:      label,
			StudentAnswer: answer,
			ProblemID:     strings.ReplaceAll(label, " ", "-"),
		},
		Status: PhotoCorrectWithProcessIssue,
		Grade: GradeResult{Outcome: GradeOutcome{
			Verdict:            VerdictDisagree,
			WrongStep:          wrongStep,
			ErrorCause:         cause,
			FinalAnswerCorrect: gradingBoolForMarkdownTest(true),
		}},
		ParentGuide: &ParentTeachingGuide{
			Answer:                 answer,
			FullSolutionSteps:      []string{"核对每一步等式"},
			GradeLevelMethod:       "逐步计算并检查等号两边",
			LikelyMistakes:         []string{"只看最终答案，漏看中间过程"},
			ParentTeachingSequence: []string{parentExplanation},
			FollowUpQuestions:      []string{"这一步等号两边相等吗？"},
			CheckingMethod:         "逐式代回检查",
		},
	}
	resultJSON, _ := json.Marshal(gradingAssessmentCanonicalResult(item))
	return gradingFinalEntry{
		question: item.Recognized,
		assessment: &k12.GradingAssessmentItem{
			Status:       k12.GradingAssessmentProcessIssue,
			ResultJSON:   string(resultJSON),
			ResultDigest: "sha256:" + strings.ReplaceAll(label, " ", "-"),
		},
	}
}

func gradingBoolForMarkdownTest(value bool) *bool {
	return &value
}
