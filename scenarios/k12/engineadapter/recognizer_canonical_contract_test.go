package engineadapter

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestParseRecognizedQuestions_PreservesRawAndCanonicalLatexSeparately(t *testing.T) {
	rawQuestion := `  计算 \frac{3}{5}+\frac{1}{5}=  `
	rawAnswer := ` \frac{3}{5} `
	questions, err := parseRecognizedQuestions(`[{` +
		`"problem_id":"p-1","problem_kind":"standalone",` +
		`"question":"  计算 \\frac{3}{5}+\\frac{1}{5}=  ",` +
		`"canonical_markdown":"$\\frac{3}{5}+\\frac{1}{5}=$",` +
		`"subject":"数学","knowledge_points":["分数加法"],` +
		`"answer_state":"present","student_answer":" \\frac{3}{5} ",` +
		`"answer_canonical_markdown":"$\\frac{3}{5}$",` +
		`"recognition_confidence":0.99,"ocr_signals":["fraction"]}]`)
	if err != nil {
		t.Fatalf("parseRecognizedQuestions: %v", err)
	}
	if len(questions) != 1 {
		t.Fatalf("questions = %#v", questions)
	}
	q := questions[0]
	if q.RawTranscription != rawQuestion || q.AnswerRawTranscription != rawAnswer {
		t.Fatalf("raw bytes changed: question=%q answer=%q", q.RawTranscription, q.AnswerRawTranscription)
	}
	if q.CanonicalMarkdown != `$\frac{3}{5}+\frac{1}{5}=$` || q.AnswerCanonicalMarkdown != `$\frac{3}{5}$` {
		t.Fatalf("canonical LaTeX lost: %#v", q)
	}
	if q.ProblemID != "p-1" || q.ProblemKind != usecase.ProblemKindStandalone {
		t.Fatalf("problem structure lost: %#v", q)
	}
	if q.RecognitionConfidence == nil || *q.RecognitionConfidence != .99 {
		t.Fatalf("confidence lost: %#v", q.RecognitionConfidence)
	}
}

func TestMergeRecognizedEvidence_RetainsConflictingObservationsForConfirmation(t *testing.T) {
	left := usecase.RecognizedQuestion{
		Question: "5/7-1/5=", RawTranscription: "5/7-1/5=", Subject: "数学",
	}
	right := usecase.RecognizedQuestion{
		Question: "5-1/5=", RawTranscription: "5-1/5=", Subject: "数学",
	}
	merged := mergeRecognizedEvidence(left, right)
	merged = usecase.EvaluateOCRConfirmationRisk(merged)
	if len(merged.EvidenceTranscriptions) != 2 {
		t.Fatalf("all independent observations must remain auditable: %#v", merged.EvidenceTranscriptions)
	}
	foundConflict := false
	for _, reason := range merged.ConfirmationReasons {
		foundConflict = foundConflict || reason == usecase.OCRRiskEvidenceConflict
	}
	if !foundConflict {
		t.Fatalf("conflicting observations must force confirmation: %v", merged.ConfirmationReasons)
	}
}
