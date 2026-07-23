package usecase

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func float64Ptr(v float64) *float64 { return &v }

func TestEvaluateOCRConfirmationRisk_Table(t *testing.T) {
	tests := []struct {
		name string
		q    RecognizedQuestion
		want []OCRRiskReason
	}{
		{
			name: "normal text does not require an item confirmation",
			q: RecognizedQuestion{
				Question: "计算一共有多少人", Subject: "数学",
				RecognitionConfidence: float64Ptr(0.99),
			},
		},
		{
			name: "high confidence cannot bypass fraction risk",
			q: RecognizedQuestion{
				Question: `计算 \frac{3}{5}+\frac{1}{5}`, Subject: "数学",
				RecognitionConfidence: float64Ptr(0.99),
			},
			want: []OCRRiskReason{OCRRiskFraction},
		},
		{
			name: "unit is high risk",
			q:    RecognizedQuestion{Question: "长 12.5 cm", Subject: "数学"},
			want: []OCRRiskReason{OCRRiskDecimalPoint, OCRRiskUnit},
		},
		{
			name: "negative sign is high risk",
			q:    RecognizedQuestion{Question: "x=-3", Subject: "数学"},
			want: []OCRRiskReason{OCRRiskNegativeSign},
		},
		{
			name: "explicit erasure signal is high risk",
			q:    RecognizedQuestion{Question: "7+8=", Subject: "数学", OCRSignals: []string{"erasure"}},
			want: []OCRRiskReason{OCRRiskErasure},
		},
		{
			name: "independent observations disagree",
			q: RecognizedQuestion{
				Question: "7+8=", Subject: "数学",
				EvidenceTranscriptions: []string{"15", "18", "15"},
			},
			want: []OCRRiskReason{OCRRiskEvidenceConflict},
		},
		{
			name: "low confidence remains a reason",
			q: RecognizedQuestion{
				Question: "7+8=", Subject: "数学",
				RecognitionConfidence: float64Ptr(0.64),
			},
			want: []OCRRiskReason{OCRRiskLowConfidence},
		},
		{
			name: "undetermined subject needs item confirmation",
			q: RecognizedQuestion{
				Question: "分析这道题", RecognitionConfidence: float64Ptr(0.99),
			},
			want: []OCRRiskReason{OCRRiskSubjectUndetermined},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateOCRConfirmationRisk(tt.q)
			if !reflect.DeepEqual(got.ConfirmationReasons, tt.want) {
				t.Fatalf("reasons = %v, want %v", got.ConfirmationReasons, tt.want)
			}
			if got.ConfirmationRequired != (len(tt.want) > 0) {
				t.Fatalf("confirmation_required = %v, want %v", got.ConfirmationRequired, len(tt.want) > 0)
			}
		})
	}
}

func TestRecognizedQuestion_RawCanonicalAreIndependentAndFallbackIsCopyable(t *testing.T) {
	raw := "  视觉原文 \\frac{1}{2}  "
	q := NormalizeRecognizedQuestion(RecognizedQuestion{
		Question:                "legacy projection",
		RawTranscription:        raw,
		CanonicalMarkdown:       `求 $\frac{1}{2}$`,
		StudentAnswer:           "0.5",
		AnswerRawTranscription:  "  0.5  ",
		AnswerCanonicalMarkdown: "$0.5$",
	})
	if q.RawTranscription != raw || q.AnswerRawTranscription != "  0.5  " {
		t.Fatalf("raw transcription bytes must be retained: %#v", q)
	}
	if q.CanonicalMarkdown != `求 $\frac{1}{2}$` || q.AnswerCanonicalMarkdown != "$0.5$" {
		t.Fatalf("canonical facts unexpectedly rewritten: %#v", q)
	}
	if got := CanonicalPlainTextFallback(`\frac{1}{2}`); got != "(1)/(2)" {
		t.Fatalf("fraction fallback = %q, want parenthesized numerator/denominator", got)
	}

	invalid := NormalizeRecognizedQuestion(RecognizedQuestion{
		RawTranscription:  "原始可复制题干",
		CanonicalMarkdown: `坏公式 $\frac{1}{2$`,
	})
	if CanonicalMarkdownValid(invalid.CanonicalMarkdown) {
		t.Fatal("unbalanced LaTeX must be invalid")
	}
	if got := RecognizedQuestionDisplayText(invalid); got != "原始可复制题干" {
		t.Fatalf("invalid canonical must fall back to raw text, got %q", got)
	}
	if strings.TrimSpace(RecognizedQuestionDisplayText(invalid)) == "" {
		t.Fatal("parse failure must never produce blank display")
	}
}

func TestCanonicalRecognizedQuestionsDigest_IgnoresRawButChangesWithCanonical(t *testing.T) {
	base, err := NormalizeRecognizedProblems("job-digest", []RecognizedQuestion{{
		RawTranscription: "OCR 版本 A", CanonicalMarkdown: `$\frac{1}{2}$`,
		AnswerState: AnswerStateBlank, Subject: "数学",
	}})
	if err != nil {
		t.Fatal(err)
	}
	sameCanonical := cloneRecognizedQuestions(base)
	sameCanonical[0].RawTranscription = "OCR 版本 B"
	if got, want := CanonicalRecognizedQuestionsDigest(sameCanonical), CanonicalRecognizedQuestionsDigest(base); got != want {
		t.Fatalf("raw observation must not alter canonical digest: got=%s want=%s", got, want)
	}
	changedCanonical := cloneRecognizedQuestions(base)
	changedCanonical[0].CanonicalMarkdown = `$\frac{2}{3}$`
	if CanonicalRecognizedQuestionsDigest(changedCanonical) == CanonicalRecognizedQuestionsDigest(base) {
		t.Fatal("canonical correction must alter digest")
	}
}

func TestNormalizeRecognizedProblems_CompoundParentAndStableChildren(t *testing.T) {
	input := []RecognizedQuestion{
		{ProblemID: "model-parent", ProblemKind: ProblemKindCompoundParent, Question: "阅读材料并回答", Subject: "语文"},
		{ProblemKind: ProblemKindSubproblem, ParentProblemID: "model-parent", SubproblemNo: "1", Question: "概括第一段", Subject: "语文", StudentAnswer: "写景"},
		{ProblemKind: ProblemKindSubproblem, ParentProblemID: "model-parent", SubproblemNo: "2", Question: "说明作用", Subject: "语文", StudentAnswer: "承上启下"},
	}
	first, err := NormalizeRecognizedProblems("job-stable", input)
	if err != nil {
		t.Fatalf("NormalizeRecognizedProblems: %v", err)
	}
	second, err := NormalizeRecognizedProblems("job-stable", input)
	if err != nil {
		t.Fatalf("second normalization: %v", err)
	}
	for i := range first {
		if first[i].ProblemID == "" || first[i].ProblemID != second[i].ProblemID {
			t.Fatalf("problem id must be stable: first=%q second=%q", first[i].ProblemID, second[i].ProblemID)
		}
	}
	if first[1].ParentProblemID != first[0].ProblemID || first[2].ParentProblemID != first[0].ProblemID {
		t.Fatalf("children must reference normalized parent id: %#v", first)
	}
	if first[1].ProblemID == first[2].ProblemID || first[1].SubproblemNo == first[2].SubproblemNo {
		t.Fatal("siblings need independent stable id and subproblem number")
	}
	if first[0].AttemptID != "" || first[1].AttemptID == "" || first[1].AttemptID == first[2].AttemptID {
		t.Fatalf("parent must not own Attempt; siblings need independent attempts: %#v", first)
	}
	if first[0].PageAssetID == "" || first[1].PageAssetID != first[0].PageAssetID || first[2].PageAssetID != first[0].PageAssetID {
		t.Fatalf("compound family must stay on one page asset: %#v", first)
	}
	frozen := FreezeRecognizedQuestionInputDigests(first)
	if frozen[0].InputDigest != "" || frozen[1].InputDigest == "" || frozen[1].InputDigest == frozen[2].InputDigest {
		t.Fatalf("only children get independent canonical input digests: %#v", frozen)
	}

	assessable := RecognizedQuestionsForAssessment(first)
	if len(assessable) != 2 {
		t.Fatalf("compound parent must not create Attempt/Assessment, got %d items", len(assessable))
	}
	for i, q := range assessable {
		if q.ProblemKind != ProblemKindSubproblem || !strings.Contains(q.Question, "阅读材料并回答") {
			t.Fatalf("child %d must be graded with common stem composed once in its input: %#v", i, q)
		}
		if normalizedAgain := NormalizeRecognizedQuestion(q); !strings.Contains(normalizedAgain.Question, "阅读材料并回答") {
			t.Fatalf("assessment projection must survive port normalization: %#v", normalizedAgain)
		}
	}
}

func TestNormalizeRecognizedProblems_ServerMintsIdentityFromUntrustedModelReferences(t *testing.T) {
	input := []RecognizedQuestion{
		{
			ProblemID: "../../model-duplicate", AttemptID: "model-attempt",
			ConfirmedVersion: 91, InputDigest: "model-digest",
			Question: "1+1=", Subject: "数学",
		},
		{
			ProblemID: "../../model-duplicate", AttemptID: "model-attempt",
			ConfirmedVersion: 92, InputDigest: "model-digest",
			Question: "2+2=", Subject: "数学",
		},
		{
			ProblemID: "\x00malformed", AttemptID: "\x00attempt",
			ConfirmedVersion: 93, InputDigest: "model-digest",
			Question: "3+3=", Subject: "数学",
		},
	}

	first, err := NormalizeRecognizedProblems("submission-server-owned", input)
	if err != nil {
		t.Fatalf("NormalizeRecognizedProblems: %v", err)
	}
	second, err := NormalizeRecognizedProblems("submission-server-owned", input)
	if err != nil {
		t.Fatalf("second normalization: %v", err)
	}

	problemIDs := map[string]struct{}{}
	attemptIDs := map[string]struct{}{}
	for i, question := range first {
		if !strings.HasPrefix(question.ProblemID, "problem-") || question.ProblemID == input[i].ProblemID {
			t.Fatalf("problem %d kept model-owned identity: got=%q model=%q", i, question.ProblemID, input[i].ProblemID)
		}
		if !strings.HasPrefix(question.AttemptID, "attempt-") || question.AttemptID == input[i].AttemptID {
			t.Fatalf("problem %d kept model-owned attempt: got=%q model=%q", i, question.AttemptID, input[i].AttemptID)
		}
		if question.ConfirmedVersion != 0 || question.InputDigest != "" {
			t.Fatalf("problem %d kept model-owned confirmation facts: version=%d digest=%q", i, question.ConfirmedVersion, question.InputDigest)
		}
		if question.ProblemID != second[i].ProblemID || question.AttemptID != second[i].AttemptID {
			t.Fatalf("server identity is not deterministic at %d: first=%#v second=%#v", i, question, second[i])
		}
		if _, duplicate := problemIDs[question.ProblemID]; duplicate {
			t.Fatalf("server minted duplicate problem_id %q", question.ProblemID)
		}
		problemIDs[question.ProblemID] = struct{}{}
		if _, duplicate := attemptIDs[question.AttemptID]; duplicate {
			t.Fatalf("server minted duplicate attempt_id %q", question.AttemptID)
		}
		attemptIDs[question.AttemptID] = struct{}{}
	}
}

func TestNormalizeRecognizedProblems_MapsOpaqueCompoundReferencesToServerIdentity(t *testing.T) {
	input := []RecognizedQuestion{
		{ProblemID: "../model parent/\x00", ProblemKind: ProblemKindCompoundParent, Question: "公共题干", Subject: "语文"},
		{ProblemID: "duplicate-child", ProblemKind: ProblemKindSubproblem, ParentProblemID: " ../model parent/\x00 ", SubproblemNo: " (1) ", Question: "第一问", StudentAnswer: "甲", Subject: "语文"},
		{ProblemID: "duplicate-child", ProblemKind: ProblemKindSubproblem, ParentProblemID: "../model parent/\x00", SubproblemNo: "(2)", Question: "第二问", StudentAnswer: "乙", Subject: "语文"},
	}

	got, err := NormalizeRecognizedProblems("submission-compound", input)
	if err != nil {
		t.Fatalf("NormalizeRecognizedProblems: %v", err)
	}
	if got[0].ProblemID == input[0].ProblemID || !strings.HasPrefix(got[0].ProblemID, "problem-") {
		t.Fatalf("compound parent identity must be server-owned: %#v", got[0])
	}
	for _, child := range got[1:] {
		if child.ParentProblemID != got[0].ProblemID {
			t.Fatalf("child did not map opaque model reference to server parent: child=%#v parent=%#v", child, got[0])
		}
		if child.ProblemID == "duplicate-child" || child.AttemptID == "" {
			t.Fatalf("child identity must be server-owned: %#v", child)
		}
	}
	if got[1].ProblemID == got[2].ProblemID || got[1].AttemptID == got[2].AttemptID {
		t.Fatalf("duplicate model child references must not merge siblings: %#v", got)
	}
}

func TestNormalizeRecognizedProblems_RejectsAmbiguousOrDanglingCompoundReferences(t *testing.T) {
	for name, questions := range map[string][]RecognizedQuestion{
		"ambiguous duplicate parent reference": {
			{ProblemID: "parent", ProblemKind: ProblemKindCompoundParent, Question: "材料一", Subject: "语文"},
			{ProblemID: "parent", ProblemKind: ProblemKindCompoundParent, Question: "材料二", Subject: "语文"},
			{ProblemKind: ProblemKindSubproblem, ParentProblemID: "parent", SubproblemNo: "1", Question: "问题", Subject: "语文"},
		},
		"dangling parent reference": {
			{ProblemID: "parent", ProblemKind: ProblemKindCompoundParent, Question: "材料", Subject: "语文"},
			{ProblemKind: ProblemKindSubproblem, ParentProblemID: "missing", SubproblemNo: "1", Question: "问题", Subject: "语文"},
		},
		"missing parent reference": {
			{ProblemID: "parent", ProblemKind: ProblemKindCompoundParent, Question: "材料", Subject: "语文"},
			{ProblemKind: ProblemKindSubproblem, SubproblemNo: "1", Question: "问题", Subject: "语文"},
		},
		"standalone contradicts parent reference": {
			{ProblemID: "parent", ProblemKind: ProblemKindCompoundParent, Question: "材料", Subject: "语文"},
			{ProblemKind: ProblemKindStandalone, ParentProblemID: "parent", Question: "问题", Subject: "语文"},
		},
		"compound parent contradicts child number": {
			{ProblemID: "parent", ProblemKind: ProblemKindCompoundParent, SubproblemNo: "1", Question: "材料", Subject: "语文"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeRecognizedProblems("submission-invalid-compound", questions); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestNormalizeRecognizedProblems_RejectsInvalidConfidenceAndGeometry(t *testing.T) {
	for name, question := range map[string]RecognizedQuestion{
		"confidence below zero":              {Question: "题目", Subject: "数学", RecognitionConfidence: float64Ptr(-0.01)},
		"confidence above one":               {Question: "题目", Subject: "数学", RecognitionConfidence: float64Ptr(1.01)},
		"geometry outside page":              {Question: "题目", StudentAnswer: "1", AnswerState: AnswerStatePresent, Subject: "数学", BBox: &BBox{X: .9, Y: .1, W: .2, H: .2}},
		"geometry with no area":              {Question: "题目", StudentAnswer: "1", AnswerState: AnswerStatePresent, Subject: "数学", BBox: &BBox{X: .1, Y: .1, W: 0, H: .2}},
		"blank cannot hide invalid geometry": {Question: "题目", AnswerState: AnswerStateBlank, Subject: "数学", BBox: &BBox{X: -.1, Y: .1, W: .2, H: .2}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeRecognizedProblems("submission-invalid-evidence", []RecognizedQuestion{question}); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestRecognizedQuestionsProblemAttemptSnapshot_ParentAndSiblingFacts(t *testing.T) {
	questions, err := NormalizeRecognizedProblems("submission-typed", []RecognizedQuestion{
		{ProblemID: "parent", ProblemKind: ProblemKindCompoundParent, Question: "公共题干", Subject: "数学"},
		{ProblemID: "child-1", ProblemKind: ProblemKindSubproblem, ParentProblemID: "parent", SubproblemNo: "1", Question: "第一问", StudentAnswer: "31", Subject: "数学"},
		{ProblemID: "child-2", ProblemKind: ProblemKindSubproblem, ParentProblemID: "parent", SubproblemNo: "2", Question: "第二问", StudentAnswer: "42", Subject: "数学"},
	})
	if err != nil {
		t.Fatal(err)
	}
	questions = FreezeRecognizedQuestionInputDigests(questions, "五年级下")
	questions[1].ConfirmedVersion = 1
	questions[2].ConfirmedVersion = 1
	questions[1].BBox = &BBox{X: .1, Y: .2, W: .3, H: .1}
	snapshot, err := RecognizedQuestionsProblemAttemptSnapshot("mingming", "submission-typed", questions, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Problems) != 3 || len(snapshot.Attempts) != 2 {
		t.Fatalf("compound parent must own no Attempt: %+v", snapshot)
	}
	if snapshot.Problems[0].ProblemKind != "compound_parent" || snapshot.Problems[1].ParentProblemID != questions[0].ProblemID {
		t.Fatalf("parent-child identity lost: %+v", snapshot.Problems)
	}
	if snapshot.Attempts[0].ProblemID != questions[1].ProblemID || snapshot.Attempts[0].AnswerRaw != "31" ||
		snapshot.Attempts[0].AnswerMarkdown != "31" || snapshot.Attempts[0].InputDigest == "" || snapshot.Attempts[0].BBox == nil {
		t.Fatalf("child-1 Attempt facts lost: %+v", snapshot.Attempts[0])
	}
	if snapshot.Attempts[1].ProblemID != questions[2].ProblemID || snapshot.Attempts[1].AttemptID == snapshot.Attempts[0].AttemptID {
		t.Fatalf("siblings must keep independent attempts: %+v", snapshot.Attempts)
	}
	restored, err := RecognizedQuestionsFromProblemAttemptSnapshot(snapshot)
	if err != nil {
		t.Fatalf("restore Problem/Attempt snapshot: %v", err)
	}
	for i := range questions {
		if restored[i].ProblemID != questions[i].ProblemID || restored[i].ParentProblemID != questions[i].ParentProblemID ||
			restored[i].AttemptID != questions[i].AttemptID || restored[i].ConfirmedVersion != questions[i].ConfirmedVersion ||
			restored[i].InputDigest != questions[i].InputDigest {
			t.Fatalf("server-owned facts changed across durable round-trip at %d:\n got=%#v\nwant=%#v", i, restored[i], questions[i])
		}
	}
}

func TestRecognizedQuestionsProblemAttemptSnapshot_UpgradesLegacyRunWithoutServerAttempt(t *testing.T) {
	snapshot, err := RecognizedQuestionsProblemAttemptSnapshot("mingming", "submission-legacy-run", []RecognizedQuestion{{
		ProblemID: "legacy-p1", AttemptID: "",
		Question: "1+1=", CanonicalMarkdown: "1+1=", Subject: "数学", AnswerState: AnswerStateBlank,
	}}, 100)
	if err != nil {
		t.Fatalf("legacy run upgrade: %v", err)
	}
	if len(snapshot.Problems) != 1 || len(snapshot.Attempts) != 1 {
		t.Fatalf("legacy run was not materialized: %+v", snapshot)
	}
	if snapshot.Problems[0].ProblemID != "legacy-p1" || snapshot.Attempts[0].ProblemID != "legacy-p1" ||
		snapshot.Attempts[0].AttemptID == "" || snapshot.Attempts[0].ConfirmedVersion != 0 || snapshot.Attempts[0].InputDigest != "" {
		t.Fatalf("legacy Problem identity must stay stable while its missing Attempt is minted: problems=%+v attempts=%+v", snapshot.Problems, snapshot.Attempts)
	}
}

func TestRecognizedQuestionsProblemAttemptSnapshot_RejectsPartiallyMintedLegacyIdentity(t *testing.T) {
	_, err := RecognizedQuestionsProblemAttemptSnapshot("mingming", "submission-partial-identity", []RecognizedQuestion{
		{
			ProblemID: "already-exposed-p1",
			Question:  "1+1=", CanonicalMarkdown: "1+1=", Subject: "数学", AnswerState: AnswerStateBlank,
		},
		{
			ProblemID: "",
			Question:  "2+2=", CanonicalMarkdown: "2+2=", Subject: "数学", AnswerState: AnswerStateBlank,
		},
	}, 100)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("partially minted identity must fail closed instead of replacing an exposed problem_id: %v", err)
	}
}

func TestMergeAnchorGeometry_MatchesStableProblemIDNotSiblingOrder(t *testing.T) {
	canonical, err := NormalizeRecognizedProblems("job-anchor", []RecognizedQuestion{
		{Question: "第一题", StudentAnswer: "一"},
		{Question: "第二题", StudentAnswer: "二"},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstBox := &BBox{X: .1, Y: .2, W: .3, H: .4}
	secondBox := &BBox{X: .5, Y: .6, W: .2, H: .1}
	anchored := []RecognizedQuestion{
		{ProblemID: canonical[1].ProblemID, BBox: secondBox},
		{ProblemID: canonical[0].ProblemID, BBox: firstBox},
	}
	got := mergeAnchorGeometry(canonical, anchored)
	if got[0].BBox == nil || *got[0].BBox != *firstBox || got[1].BBox == nil || *got[1].BBox != *secondBox {
		t.Fatalf("anchor geometry crossed sibling ids: %#v", got)
	}
}

func TestApplyGradingCorrections_RejectsUnknownOrDuplicateStableTarget(t *testing.T) {
	questions, err := NormalizeRecognizedProblems("job-correction", []RecognizedQuestion{{Question: "第一题"}})
	if err != nil {
		t.Fatal(err)
	}
	for name, corrections := range map[string][]GradingQuestionCorrection{
		"unknown id": {{ProblemID: "missing", Confirmed: true}},
		"duplicate target": {
			{ProblemID: questions[0].ProblemID, Confirmed: true},
			{ProblemID: questions[0].ProblemID, Confirmed: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := &gradingRun{questions: cloneRecognizedQuestions(questions)}
			err := applyAndValidateGradingConfirmation(run, ConfirmPhotoGradingInput{Corrections: corrections})
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
			if run.questions[0].ConfirmedVersion != 0 {
				t.Fatalf("rejected correction mutated confirmation: %#v", run.questions[0])
			}
		})
	}
}
