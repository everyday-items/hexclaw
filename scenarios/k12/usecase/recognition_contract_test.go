package usecase

import (
	"encoding/json"
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
			name: "clear high confidence fraction auto freezes",
			q: RecognizedQuestion{
				Question: "0.25＋11/15＋4/15＋3/4", Subject: "数学",
				RecognitionConfidence:  float64Ptr(0.99),
				OCRSignals:             []string{"fraction"},
				EvidenceTranscriptions: []string{"0.25", "11/15", "4/15", "3/4"},
				AnswerState:            AnswerStatePresent, StudentAnswer: "=0.25+1+0.75\n=2",
				AnswerEvidenceTranscriptions: []string{"=0.25+1+0.75", "=2"},
			},
		},
		{
			name: "clear high confidence decimal and unit auto freeze",
			q: RecognizedQuestion{
				Question: "长 12.5 cm", Subject: "数学",
				RecognitionConfidence: float64Ptr(0.99),
				OCRSignals:            []string{"decimal_point", "unit"},
			},
		},
		{
			name: "clear high confidence negative sign auto freezes",
			q: RecognizedQuestion{
				Question: "x=-3", Subject: "数学",
				RecognitionConfidence: float64Ptr(0.99),
				OCRSignals:            []string{"negative_sign"},
			},
		},
		{
			name: "stale format only reasons are discarded on reevaluation",
			q: RecognizedQuestion{
				Question: "15.02－6.8－1.02", Subject: "数学",
				RecognitionConfidence: float64Ptr(0.99),
				AnswerState:           AnswerStatePresent, StudentAnswer: "14－6.8＝7.2",
				AnswerEvidenceTranscriptions: []string{"＝14－6.8", "＝7.2"},
				ConfirmationReasons: []OCRRiskReason{
					OCRRiskFraction,
					OCRRiskDecimalPoint,
					OCRRiskNegativeSign,
					OCRRiskUnit,
				},
			},
		},
		{
			name: "stale format reason does not mask real uncertainty",
			q: RecognizedQuestion{
				Question: "4÷0.5=", Subject: "数学",
				RecognitionConfidence: float64Ptr(0.99),
				ConfirmationReasons: []OCRRiskReason{
					OCRRiskDecimalPoint,
					OCRRiskEvidenceConflict,
				},
			},
			want: []OCRRiskReason{OCRRiskEvidenceConflict},
		},
		{
			name: "explicit erasure signal is high risk",
			q: RecognizedQuestion{
				Question: "7+8=", Subject: "数学",
				RecognitionConfidence: float64Ptr(0.99), OCRSignals: []string{"erasure"},
			},
			want: []OCRRiskReason{OCRRiskErasure},
		},
		{
			name: "independent observations disagree",
			q: RecognizedQuestion{
				Question: "7+8=", Subject: "数学",
				RecognitionConfidence:  float64Ptr(0.99),
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
			name: "explicit occlusion signal lowers an overstated confidence",
			q: RecognizedQuestion{
				Question: "5−1/5=", Subject: "数学",
				RecognitionConfidence: float64Ptr(0.97),
				OCRSignals:            []string{"题目左侧有其他印刷文字及手写痕迹重叠，但算式清晰可辨"},
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
			if tt.name == "independent observations disagree" {
				roles := RecognizedQuestion{
					RawTranscription:       "在下列六个数：5、6、12、14、23、29中，划去数（ ）后，能使其中3个数的和为另外2个数和的2倍。",
					AnswerRawTranscription: "划去：29\n因为：5+23+14=42\n6+12=18\n42=18×2",
					AnswerState:            AnswerStatePresent, Subject: "数学", RecognitionConfidence: float64Ptr(0.99),
					AnswerEvidenceTranscriptions: []string{"划去：29", "因为：5+23+14=42", "6+12=18", "42=18×2"},
				}
				roles.CanonicalMarkdown = roles.RawTranscription
				roles.AnswerCanonicalMarkdown = roles.AnswerRawTranscription
				roles.EvidenceTranscriptions = []string{"印刷体：" + roles.RawTranscription, "手写：划去：29；因为：5+23+14=42；6+12=18；42=18×2。"}
				matched := EvaluateOCRConfirmationRisk(roles)
				if matched.ConfirmationRequired {
					t.Errorf("full printed question and handwritten answer must match their normalized roles: %v", matched.ConfirmationReasons)
				}
				if matched.RawTranscription != roles.RawTranscription || matched.AnswerRawTranscription != roles.AnswerRawTranscription ||
					matched.CanonicalMarkdown != roles.CanonicalMarkdown || matched.AnswerCanonicalMarkdown != roles.AnswerCanonicalMarkdown ||
					!reflect.DeepEqual(matched.EvidenceTranscriptions, roles.EvidenceTranscriptions) ||
					!reflect.DeepEqual(matched.AnswerEvidenceTranscriptions, roles.AnswerEvidenceTranscriptions) {
					t.Error("role comparison must preserve all raw evidence bytes")
				}
				changed := roles
				changed.EvidenceTranscriptions = append(append([]string{}, roles.EvidenceTranscriptions...), roles.EvidenceTranscriptions[0])
				if !EvaluateOCRConfirmationRisk(changed).ConfirmationRequired {
					t.Error("a third evidence entry must not be accepted as the two-role envelope")
				}
				changed.EvidenceTranscriptions = []string{roles.EvidenceTranscriptions[0], roles.EvidenceTranscriptions[0]}
				if !EvaluateOCRConfirmationRisk(changed).ConfirmationRequired {
					t.Error("duplicate evidence roles must remain conflicting")
				}
				changed.EvidenceTranscriptions = []string{"不是：" + roles.RawTranscription, roles.EvidenceTranscriptions[1]}
				if !EvaluateOCRConfirmationRisk(changed).ConfirmationRequired {
					t.Error("an unapproved or negated prefix must remain conflicting")
				}
				changed.EvidenceTranscriptions = []string{roles.EvidenceTranscriptions[0] + "另有条件", roles.EvidenceTranscriptions[1]}
				if !EvaluateOCRConfirmationRisk(changed).ConfirmationRequired {
					t.Error("additional question content must remain conflicting")
				}
				changed.EvidenceTranscriptions = []string{roles.EvidenceTranscriptions[0], strings.Replace(roles.EvidenceTranscriptions[1], "42=18×2", "42=21×2", 1)}
				if !EvaluateOCRConfirmationRisk(changed).ConfirmationRequired {
					t.Error("a different handwritten value must remain conflicting")
				}
				changed.EvidenceTranscriptions = []string{roles.EvidenceTranscriptions[0], strings.Replace(roles.EvidenceTranscriptions[1], "42=18×2", "42=18+2", 1)}
				if !EvaluateOCRConfirmationRisk(changed).ConfirmationRequired {
					t.Error("a different handwritten operator must remain conflicting")
				}
				changed = roles
				changed.AnswerEvidenceTranscriptions = []string{"划去：29", "因为：5+23+14=42", "6+12=18", "42=21×2"}
				if !EvaluateOCRConfirmationRisk(changed).ConfirmationRequired {
					t.Error("matching roles must not bypass conflicting answer evidence")
				}
				changed = roles
				changed.OCRSignals = []string{"evidence_conflict"}
				if !EvaluateOCRConfirmationRisk(changed).ConfirmationRequired {
					t.Error("matching roles must not bypass an independent OCR conflict signal")
				}
			}
			if tt.name == "clear high confidence fraction auto freezes" {
				changed := tt.q
				changed.EvidenceTranscriptions = []string{"0.25", "11/15", "7/15", "3/4"}
				if !EvaluateOCRConfirmationRisk(changed).ConfirmationRequired {
					t.Fatal("changed numeric evidence must remain conflicting")
				}
				changed.EvidenceTranscriptions = []string{"0.25", "4/15", "11/15", "3/4"}
				if !EvaluateOCRConfirmationRisk(changed).ConfirmationRequired {
					t.Fatal("reordered evidence must remain conflicting")
				}
				changed.EvidenceTranscriptions = []string{tt.q.Question, "0.25－11/15＋4/15＋3/4"}
				if !EvaluateOCRConfirmationRisk(changed).ConfirmationRequired {
					t.Fatal("different full-expression operators must remain conflicting")
				}
				if evidenceTranscriptionsConflict(`= 8.7 × (17.4 − 7.4)\n= 8.7 × 10\n= 87`,
					[]string{"= 8.7 × (17.4 − 7.4)", "= 8.7 × 10", "= 87"}, "") {
					t.Error("literal newline serialization must not conflict with separate decimal steps")
				}
				if evidenceTranscriptionsConflict(`= 15.02 − 1.02 − 6.8\n= 14 − 6.8\n= 7.2`,
					[]string{"= 15.02 − 1.02 − 6.8", "= 14 − 6.8", "= 7.2"}, "") {
					t.Error("literal newline serialization must preserve the full subtraction chain")
				}
				if evidenceTranscriptionsConflict(`= 0.25 + (\frac{11}{15} + \frac{4}{15}) + \frac{3}{4}\n= 0.25 + \frac{15}{15} + \frac{3}{4}\n= 0.25 + 1 + 0.75\n= 2`,
					[]string{"= 0.25 + (11/15 + 4/15) + 3/4", "= 0.25 + 15/15 + 3/4", "= 0.25 + 1 + 0.75", "= 2"}, "") {
					t.Error("literal newlines and numeric fraction notation must compare equally")
				}
				if evidenceTranscriptionsConflict("24 ÷ \\(\\frac{3}{8}\\) = 24 × \\(\\frac{8}{3}\\) = 64\n答：这个数是64。",
					[]string{"24 ÷ 3/8 = 24 × 8/3 = 64", "答：这个数是64。"}, "") {
					t.Error("LaTeX wrappers must not conflict with the same visible fraction chain")
				}
				if evidenceTranscriptionsConflict("8 × \\(\\frac{1}{4}\\) × \\(\\frac{4}{5}\\) = 2 × \\(\\frac{4}{5}\\) = \\(\\frac{8}{5}\\) = 1\\(\\frac{3}{5}\\)\n答：是1\\(\\frac{3}{5}\\)。",
					[]string{"8 × 1/4 × 4/5 = 2 × 4/5 = 8/5 = 1 3/5", "答：是1 3/5。"}, "") {
					t.Error("mixed-number notation must retain the integer and fractional parts")
				}
				if !evidenceTranscriptionsConflict(`1\frac{3}{5}=\frac{8}{5}`, []string{"13/5", "=8/5"}, "") {
					t.Error("mixed number 1 3/5 must not become fraction 13/5")
				}
				if !evidenceTranscriptionsConflict("1/(2+3)=0.2", []string{"1/2+3", "=0.2"}, "") {
					t.Error("grouping parentheses must not be discarded")
				}
				if evidenceTranscriptionsConflict(`1\neq2`, []string{"1", "≠2"}, "") {
					t.Error("LaTeX inequality must not be decoded as a newline")
				}
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

func TestBUG20260726_D_SourceNumberPathSurvivesCanonicalDigestAndDurableRoundTrip(t *testing.T) {
	var input []RecognizedQuestion
	if err := json.Unmarshal([]byte(`[{
		"problem_kind":"standalone",
		"source_number_path":["三","1"],
		"display_label":"三、1",
		"question":"24÷8=",
		"canonical_markdown":"24\\div8=",
		"subject":"数学",
		"answer_state":"present",
		"student_answer":"3",
		"answer_canonical_markdown":"3"
	}]`), &input); err != nil {
		t.Fatal(err)
	}
	normalized, err := NormalizeRecognizedProblems("submission-numbering", input)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := RecognizedQuestionsProblemAttemptSnapshot(
		"mingming", "submission-numbering", normalized, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RecognizedQuestionsFromProblemAttemptSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(restored[0])
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wire["source_number_path"], []any{"三", "1"}) ||
		wire["display_label"] != "三、1" {
		t.Fatalf("BUG-20260726-D durable round-trip dropped source numbering: %s", encoded)
	}

	var changedInput []RecognizedQuestion
	if err := json.Unmarshal([]byte(`[{
		"problem_kind":"standalone",
		"source_number_path":["三","2"],
		"display_label":"三、2",
		"question":"24÷8=",
		"canonical_markdown":"24\\div8=",
		"subject":"数学",
		"answer_state":"present",
		"student_answer":"3",
		"answer_canonical_markdown":"3"
	}]`), &changedInput); err != nil {
		t.Fatal(err)
	}
	changed, err := NormalizeRecognizedProblems("submission-numbering", changedInput)
	if err != nil {
		t.Fatal(err)
	}
	if CanonicalRecognizedQuestionsDigest(normalized) ==
		CanonicalRecognizedQuestionsDigest(changed) {
		t.Fatal("BUG-20260726-D original number path must participate in canonical digest")
	}
}

func TestDD041_UnnumberedSectionItemsReceiveOnlyServerDerivedSystemOrder(t *testing.T) {
	questions, err := NormalizeRecognizedProblems("submission-dd041", []RecognizedQuestion{
		{
			ProblemKind:        ProblemKindStandalone,
			SourceSectionPath:  []string{"一"},
			SourceSectionLabel: "一、直接写得数",
			Question:           "4÷0.5=",
			Subject:            "数学",
			AnswerState:        AnswerStatePresent,
			StudentAnswer:      "8",
		},
		{
			ProblemKind:        ProblemKindStandalone,
			SourceSectionPath:  []string{"一"},
			SourceSectionLabel: "一、直接写得数",
			Question:           "10×0.01=",
			Subject:            "数学",
			AnswerState:        AnswerStatePresent,
			StudentAnswer:      "0.1",
		},
		{
			ProblemKind:        ProblemKindStandalone,
			SourceNumberPath:   []string{"三", "1"},
			DisplayLabel:       "三、1",
			SourceSectionPath:  []string{"三"},
			SourceSectionLabel: "三、列式计算",
			Question:           "3/8 是 24",
			Subject:            "数学",
			AnswerState:        AnswerStateBlank,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range []struct {
		sourcePath    []string
		sectionPath   []string
		sectionLabel  string
		systemOrdinal int
		systemLabel   string
	}{
		{nil, []string{"一"}, "一、直接写得数", 1, "第 1 题（系统序号）"},
		{nil, []string{"一"}, "一、直接写得数", 2, "第 2 题（系统序号）"},
		{[]string{"三", "1"}, []string{"三"}, "三、列式计算", 0, ""},
	} {
		got := questions[index]
		if !reflect.DeepEqual(got.SourceNumberPath, want.sourcePath) ||
			!reflect.DeepEqual(got.SourceSectionPath, want.sectionPath) ||
			got.SourceSectionLabel != want.sectionLabel ||
			got.SystemSectionOrdinal != want.systemOrdinal ||
			got.SystemDisplayLabel != want.systemLabel {
			t.Fatalf("DD-041 facts at %d = %#v, want source=%#v section=%#v/%q system=%d/%q", index, got, want.sourcePath, want.sectionPath, want.sectionLabel, want.systemOrdinal, want.systemLabel)
		}
	}

	snapshot, err := RecognizedQuestionsProblemAttemptSnapshot("mingming", "submission-dd041", questions, 100)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RecognizedQuestionsFromProblemAttemptSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, questions) {
		t.Fatalf("DD-041 durable round-trip changed source/system facts:\n got=%#v\nwant=%#v", restored, questions)
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

func TestRecognizedQuestionsFromProblemAttemptSnapshot_DropsHistoricalFormatOnlyReasons(t *testing.T) {
	questions, err := NormalizeRecognizedProblems("submission-stale-format", []RecognizedQuestion{{
		Question: "4÷0.5=", Subject: "数学",
		StudentAnswer: "8", AnswerState: AnswerStatePresent,
		RecognitionConfidence: float64Ptr(0.99),
	}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := RecognizedQuestionsProblemAttemptSnapshot(
		"mingming", "submission-stale-format", questions, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	// Persisted shape produced by the previous policy.
	snapshot.Problems[0].ConfirmationRequired = true
	snapshot.Problems[0].ConfirmationReasons = []string{string(OCRRiskDecimalPoint)}

	restored, err := RecognizedQuestionsFromProblemAttemptSnapshot(snapshot)
	if err != nil {
		t.Fatalf("restore historical Problem/Attempt snapshot: %v", err)
	}
	if restored[0].ConfirmationRequired || len(restored[0].ConfirmationReasons) != 0 {
		t.Fatalf("historical format-only reason survived re-evaluation: %#v", restored[0])
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
