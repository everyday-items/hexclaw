package engineadapter

import (
	"errors"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
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

func TestBUG20260726_D_RecognizerPreservesSourceNumberPathAndDisplayLabel(t *testing.T) {
	questions, err := parseRecognizedQuestions(`[{
		"problem_id":"p-3-1",
		"problem_kind":"standalone",
		"source_number_path":["三","1"],
		"display_label":"三、1",
		"question":"24÷8=",
		"canonical_markdown":"24\\div8=",
		"subject":"数学",
		"knowledge_points":["整数除法"],
		"answer_state":"present",
		"student_answer":"3",
		"answer_canonical_markdown":"3",
		"recognition_confidence":0.99
	}]`)
	if err != nil {
		t.Fatalf("parseRecognizedQuestions: %v", err)
	}
	if len(questions) != 1 {
		t.Fatalf("questions=%#v", questions)
	}
	value := reflect.ValueOf(questions[0])
	pathField := value.FieldByName("SourceNumberPath")
	labelField := value.FieldByName("DisplayLabel")
	if !pathField.IsValid() || !labelField.IsValid() {
		t.Fatalf("BUG-20260726-D recognized question dropped source_number_path/display_label: %#v", questions[0])
	}
	path, ok := pathField.Interface().([]string)
	if !ok || !reflect.DeepEqual(path, []string{"三", "1"}) || labelField.String() != "三、1" {
		t.Fatalf("BUG-20260726-D source number drift: path=%#v label=%q", pathField.Interface(), labelField.String())
	}
}

// REG-SOURCE-NUMBER-UNIQUE-20260801-001: a syntactically valid model array
// cannot erase child-number evidence by assigning one heading label to several
// independently answerable questions. This must be a protocol failure so the
// existing bounded DD-036 fallback, rather than ordinal invention, decides
// whether a more faithful recognition is available.
func TestRecognitionProtocolRejectsDuplicateNonEmptySourceNumberEvidence(t *testing.T) {
	for _, testCase := range []struct {
		name string
		raw  string
	}{
		{
			name: "duplicate source number path",
			raw: `[
				{"problem_kind":"standalone","source_number_path":["一"],"display_label":"一","question":"4÷0.5=","subject":"数学"},
				{"problem_kind":"standalone","source_number_path":["一"],"display_label":"一、2","question":"10×0.01=","subject":"数学"}
			]`,
		},
		{
			name: "duplicate display label",
			raw: `[
				{"problem_kind":"standalone","source_number_path":["一","1"],"display_label":"一、1","question":"4÷0.5=","subject":"数学"},
				{"problem_kind":"standalone","source_number_path":["一","2"],"display_label":"一、1","question":"10×0.01=","subject":"数学"}
			]`,
		},
		{
			name: "partial source-number evidence",
			raw: `[
				{"problem_kind":"standalone","source_number_path":["一"],"display_label":"一","question":"4÷0.5=","subject":"数学"},
				{"problem_kind":"standalone","source_number_path":[],"display_label":"","question":"10×0.01=","subject":"数学"}
			]`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			questions, err := parseRecognizedQuestions(testCase.raw)
			if err != nil {
				t.Fatalf("parse syntactically valid model result: %v", err)
			}
			err = validateRecognitionProtocolResult(questions)
			if !errors.Is(err, k12.ErrRecognitionProtocolInvalid) {
				t.Fatalf("duplicate source evidence err=%v, want ErrRecognitionProtocolInvalid", err)
			}
		})
	}

	questions, err := parseRecognizedQuestions(`[
		{"problem_kind":"standalone","source_number_path":[],"display_label":"","question":"未标号第一题","subject":"数学"},
		{"problem_kind":"standalone","source_number_path":[],"display_label":"","question":"未标号第二题","subject":"数学"}
	]`)
	if err != nil {
		t.Fatalf("parse legitimate unnumbered questions: %v", err)
	}
	if err := validateRecognitionProtocolResult(questions); err != nil {
		t.Fatalf("legitimate unnumbered questions were rejected: %v", err)
	}
}
