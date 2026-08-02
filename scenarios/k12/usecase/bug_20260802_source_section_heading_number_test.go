package usecase

import (
	"reflect"
	"testing"
)

// BUG-20260801-011 / DD-041: a section heading is not a printed child number.
// A real C02 gpt-5.6-sol response returned 四/五 in both source fields although
// the frozen worksheet prints those tokens only in their section headings.
func TestBUG20260802_HeadingOnlySourceNumberBecomesSectionSystemOrdinal(t *testing.T) {
	questions, err := NormalizeRecognizedProblems("bug-20260802-heading-token", []RecognizedQuestion{
		{
			ProblemKind:        ProblemKindStandalone,
			SourceNumberPath:   []string{"四"},
			DisplayLabel:       "四",
			SourceSectionPath:  []string{"四"},
			SourceSectionLabel: "四、应用题",
			Question:           "一个周长是300米的长方形鱼塘，长是宽的2倍。",
			Subject:            "数学",
			AnswerState:        AnswerStatePresent,
			StudentAnswer:      "11250",
		},
		{
			ProblemKind:        ProblemKindStandalone,
			SourceNumberPath:   []string{"三", "1"},
			DisplayLabel:       "三、1",
			SourceSectionPath:  []string{"三"},
			SourceSectionLabel: "三、列式计算",
			Question:           "一个数的3/8是24，求这个数。",
			Subject:            "数学",
			AnswerState:        AnswerStatePresent,
			StudentAnswer:      "64",
		},
	})
	if err != nil {
		t.Fatalf("NormalizeRecognizedProblems: %v", err)
	}
	if len(questions) != 2 {
		t.Fatalf("questions=%d, want 2", len(questions))
	}

	headingOnly := questions[0]
	if len(headingOnly.SourceNumberPath) != 0 || headingOnly.DisplayLabel != "" {
		t.Fatalf("section heading leaked into source-number facts: %#v", headingOnly)
	}
	if !reflect.DeepEqual(headingOnly.SourceSectionPath, []string{"四"}) ||
		headingOnly.SourceSectionLabel != "四、应用题" {
		t.Fatalf("section fact changed while removing heading-only number: %#v", headingOnly)
	}
	if headingOnly.SystemSectionOrdinal != 1 ||
		headingOnly.SystemDisplayLabel != "第 1 题（系统序号）" {
		t.Fatalf("heading-only item system order=%d/%q, want 1/第 1 题（系统序号）", headingOnly.SystemSectionOrdinal, headingOnly.SystemDisplayLabel)
	}

	printedChild := questions[1]
	if !reflect.DeepEqual(printedChild.SourceNumberPath, []string{"三", "1"}) ||
		printedChild.DisplayLabel != "三、1" {
		t.Fatalf("printed child number was incorrectly cleared: %#v", printedChild)
	}
	if printedChild.SystemSectionOrdinal != 0 || printedChild.SystemDisplayLabel != "" {
		t.Fatalf("printed child acquired a system label: %#v", printedChild)
	}

	mismatchedHeading := NormalizeRecognizedQuestion(RecognizedQuestion{
		SourceNumberPath:   []string{"五"},
		DisplayLabel:       "五",
		SourceSectionPath:  []string{"五"},
		SourceSectionLabel: "五. 思维题",
	})
	if !reflect.DeepEqual(mismatchedHeading.SourceNumberPath, []string{"五"}) ||
		mismatchedHeading.DisplayLabel != "五" {
		t.Fatalf("non-DD-041 heading shape was silently cleared: %#v", mismatchedHeading)
	}
}
