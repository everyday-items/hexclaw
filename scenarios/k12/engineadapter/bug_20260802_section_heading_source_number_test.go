package engineadapter

import (
	"errors"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// The adapter must use the shared DD-041 normalizer before a real model's
// heading-only token can become durable source-number evidence.
func TestBUG20260802_RecognizerDropsHeadingOnlySourceNumberBeforeProtocolValidation(t *testing.T) {
	questions, err := parseRecognizedQuestions(`[
		{
			"problem_kind":"standalone",
			"source_number_path":["五"],
			"display_label":"五",
			"source_section_path":["五"],
			"source_section_label":"五、思维题",
			"question":"在下列六个数中划去一个数后使三个数之和为另外三个数和的2倍。",
			"subject":"数学",
			"answer_state":"present",
			"student_answer":"29"
		}
	]`)
	if err != nil {
		t.Fatalf("parseRecognizedQuestions: %v", err)
	}
	if err := validateRecognitionProtocolResult(questions); err != nil {
		t.Fatalf("heading-only source token should normalize before validation: %v", err)
	}
	if len(questions) != 1 || len(questions[0].SourceNumberPath) != 0 || questions[0].DisplayLabel != "" {
		t.Fatalf("heading-only token reached adapter output: %#v", questions)
	}
	if !reflect.DeepEqual(questions[0].SourceSectionPath, []string{"五"}) ||
		questions[0].SourceSectionLabel != "五、思维题" {
		t.Fatalf("adapter changed durable section fact: %#v", questions[0])
	}

	normalized, err := usecase.NormalizeRecognizedProblems("bug-20260802-adapter", questions)
	if err != nil {
		t.Fatalf("NormalizeRecognizedProblems: %v", err)
	}
	if normalized[0].SystemSectionOrdinal != 1 ||
		normalized[0].SystemDisplayLabel != "第 1 题（系统序号）" {
		t.Fatalf("system order=%d/%q, want 1/第 1 题（系统序号）", normalized[0].SystemSectionOrdinal, normalized[0].SystemDisplayLabel)
	}
}

func TestREGK12LegacyFallbackSourceSection20260809002DropsOnlyIncompleteModelSectionPair(t *testing.T) {
	for _, tc := range []struct {
		name             string
		sourceSection    string
		wantSectionPath  []string
		wantSectionLabel string
	}{
		{
			name:             "complete_pair_is_preserved",
			sourceSection:    `"source_section_path":["三"],"source_section_label":"三、列式计算",`,
			wantSectionPath:  []string{"三"},
			wantSectionLabel: "三、列式计算",
		},
		{
			name:          "path_without_label_is_cleared",
			sourceSection: `"source_section_path":["三"],`,
		},
		{
			name:          "label_without_path_is_cleared",
			sourceSection: `"source_section_label":"三、列式计算",`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			questions, err := parseRecognizedQuestions(`[{` + tc.sourceSection + `
				"source_number_path":["三","1"],
				"display_label":"三、1",
				"question":"一个数的3/8是24，求这个数？",
				"subject":"数学",
				"answer_state":"present",
				"student_answer":"64"
			}]`)
			if err != nil {
				t.Fatalf("parseRecognizedQuestions: %v", err)
			}
			if len(questions) != 1 {
				t.Fatalf("questions=%d want=1", len(questions))
			}
			question := questions[0]
			if !reflect.DeepEqual(question.SourceSectionPath, tc.wantSectionPath) ||
				question.SourceSectionLabel != tc.wantSectionLabel {
				t.Fatalf("source section pair=%#v/%q want=%#v/%q",
					question.SourceSectionPath, question.SourceSectionLabel,
					tc.wantSectionPath, tc.wantSectionLabel)
			}
			if !reflect.DeepEqual(question.SourceNumberPath, []string{"三", "1"}) ||
				question.DisplayLabel != "三、1" ||
				question.AnswerState != usecase.AnswerStatePresent || question.StudentAnswer != "64" {
				t.Fatalf("clearing optional section pair changed required question facts: %#v", question)
			}
			if err := validateRecognitionProtocolResult(questions); err != nil {
				t.Fatalf("adapter output violated final source pair invariant: %v", err)
			}
		})
	}

	if _, err := usecase.NormalizeRecognizedProblems("adapter-bypass", []usecase.RecognizedQuestion{{
		Question:          "题目",
		SourceSectionPath: []string{"三"},
	}}); !errors.Is(err, usecase.ErrInvalidInput) {
		t.Fatalf("domain accepted incomplete source section pair outside adapter: %v", err)
	}
}
