package engineadapter

import (
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
