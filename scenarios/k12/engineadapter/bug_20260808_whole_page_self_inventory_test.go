package engineadapter

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestBUG20260808_DenseWholePagePromptFreezesCompactPrintedInventory(t *testing.T) {
	const marker = "printed_inventory independently reviews"
	index := strings.LastIndex(wholePageSelfInventoryPrompt, marker)
	if index < 0 {
		t.Fatalf("whole-page prompt is missing %q", marker)
	}
	contract := wholePageSelfInventoryPrompt[index:]
	if !strings.Contains(
		contract,
		"Each item must contain exactly source_number_path, display_label, and question",
	) {
		t.Fatalf("whole-page printed inventory is not frozen to the compact three-field contract: %s", contract)
	}
}

func TestREGBUGK12DenseV1Complete_PromptUsesPrintedInventoryAsSingleQuestionSource(t *testing.T) {
	required := []string{
		"First complete printed_inventory as the single source of printed-question identity",
		"copy source_number_path, display_label, and question character for character",
		"Do not transcribe the printed question a second time",
		"Only add answer, subject, and knowledge facts",
		"Before returning JSON, compare every corresponding identity field byte for byte",
		`"question":"8的1/4的4/5是多少？"`,
		"Ignore worksheet metadata fields such as title, date, name, and time",
		`instructions such as "把下面每题的得数化简"`,
		"instruction text are not questions and must not appear in either array",
	}
	for _, invariant := range required {
		if !strings.Contains(wholePageSelfInventoryPrompt, invariant) {
			t.Errorf("whole-page prompt is missing single-source invariant %q", invariant)
		}
	}
}

func TestREGBUGK12DenseV1Complete_PromptPresentsPrintedInventoryBeforeQuestions(t *testing.T) {
	const envelope = `{"printed_inventory":[...],"questions":[...]}`
	if !strings.Contains(wholePageSelfInventoryPrompt, envelope) {
		t.Fatalf("whole-page prompt does not present the single-source array first: missing %s", envelope)
	}
	inventoryRule := strings.Index(wholePageSelfInventoryPrompt, "- printed_inventory independently reviews")
	questionsRule := strings.Index(wholePageSelfInventoryPrompt, "- Then build questions in the same order")
	if inventoryRule < 0 || questionsRule < 0 || inventoryRule > questionsRule {
		t.Fatalf("whole-page prompt explains questions before its printed single source")
	}
}

func TestREGBUGK12StandaloneParentFields006_PromptsFreezeExactKindFieldCombinations(t *testing.T) {
	for _, invariant := range []string{
		"problem_kind=standalone 时 parent_problem_id 与 subproblem_no 必须同时是空字符串",
		"problem_kind=compound_parent 时 parent_problem_id 与 subproblem_no 必须同时是空字符串",
		"problem_kind=subproblem 时 parent_problem_id 与 subproblem_no 必须同时非空",
	} {
		if !strings.Contains(recognizePrompt, invariant) {
			t.Errorf("中文分片提示缺少父子字段互斥约束 %q", invariant)
		}
	}
	for _, invariant := range []string{
		"For problem_kind=standalone, parent_problem_id and subproblem_no must both be empty strings",
		"For problem_kind=compound_parent, parent_problem_id and subproblem_no must both be empty strings",
		"For problem_kind=subproblem, parent_problem_id and subproblem_no must both be non-empty",
	} {
		if !strings.Contains(wholePageSelfInventoryPrompt, invariant) {
			t.Errorf("英文整页提示缺少父子字段互斥约束 %q", invariant)
		}
	}
	if !strings.Contains(wholePageSelfInventoryPrompt,
		"Before returning JSON, verify every problem_kind against these exact parent, subproblem, and answer-field combinations") {
		t.Error("英文整页提示缺少返回前父子字段组合自检")
	}
}

func TestREGBUGK12StandaloneParentFields006_ParserRejectsRawInvalidKindFieldCombinations(t *testing.T) {
	valid := map[string]string{
		"standalone": `[{
			"problem_kind":"standalone","parent_problem_id":"","subproblem_no":"",
			"question":"1+1=","answer_state":"present","student_answer":"2"
		}]`,
		"compound parent and subproblem": `[
			{"problem_id":"parent-1","problem_kind":"compound_parent","parent_problem_id":"","subproblem_no":"","question":"阅读材料","answer_state":"blank","student_answer":""},
			{"problem_kind":"subproblem","parent_problem_id":"parent-1","subproblem_no":"1","question":"第一问","answer_state":"present","student_answer":"答案"}
		]`,
	}
	for name, payload := range valid {
		t.Run("valid "+name, func(t *testing.T) {
			questions, err := parseRecognizedQuestions(payload)
			if err != nil {
				t.Fatalf("合法父子字段组合被拒绝: %v", err)
			}
			if err := validateRecognitionProtocolResult(questions); err != nil {
				t.Fatalf("合法父子字段组合未通过最终结构门: %v", err)
			}
		})
	}

	invalid := map[string]string{
		"standalone parent": `[{
			"problem_kind":"standalone","parent_problem_id":"parent-1","subproblem_no":"",
			"question":"1+1=","answer_state":"present","student_answer":"2"
		}]`,
		"standalone subproblem": `[{
			"problem_kind":"standalone","parent_problem_id":"","subproblem_no":"1",
			"question":"1+1=","answer_state":"present","student_answer":"2"
		}]`,
		"compound parent reference": `[{
			"problem_id":"parent-1","problem_kind":"compound_parent","parent_problem_id":"other","subproblem_no":"",
			"question":"阅读材料","answer_state":"blank","student_answer":""
		}]`,
		"compound subproblem number": `[{
			"problem_id":"parent-1","problem_kind":"compound_parent","parent_problem_id":"","subproblem_no":"1",
			"question":"阅读材料","answer_state":"blank","student_answer":""
		}]`,
		"compound present answer": `[{
			"problem_id":"parent-1","problem_kind":"compound_parent","parent_problem_id":"","subproblem_no":"",
			"question":"阅读材料","answer_state":"present","student_answer":"孩子作答"
		}]`,
		"compound blank with raw answer": `[{
			"problem_id":"parent-1","problem_kind":"compound_parent","parent_problem_id":"","subproblem_no":"",
			"question":"阅读材料","answer_state":"blank","student_answer":"孩子作答"
		}]`,
		"subproblem missing parent": `[{
			"problem_kind":"subproblem","parent_problem_id":"","subproblem_no":"1",
			"question":"第一问","answer_state":"present","student_answer":"答案"
		}]`,
		"subproblem missing number": `[
			{"problem_id":"parent-1","problem_kind":"compound_parent","parent_problem_id":"","subproblem_no":"","question":"阅读材料","answer_state":"blank","student_answer":""},
			{"problem_kind":"subproblem","parent_problem_id":"parent-1","subproblem_no":"","question":"第一问","answer_state":"present","student_answer":"答案"}
		]`,
		"subproblem dangling parent": `[{
			"problem_kind":"subproblem","parent_problem_id":"missing","subproblem_no":"1",
			"question":"第一问","answer_state":"present","student_answer":"答案"
		}]`,
	}
	for name, payload := range invalid {
		t.Run("invalid "+name, func(t *testing.T) {
			questions, err := parseRecognizedQuestions(payload)
			if err == nil {
				err = validateRecognitionProtocolResult(questions)
			}
			if !errors.Is(err, k12.ErrRecognitionProtocolInvalid) {
				t.Fatalf("非法原始父子字段组合未 fail-closed: questions=%#v err=%v", questions, err)
			}
		})
	}
}

func TestREGBUGK12DenseV1Complete_ParserRejectsIndependentFractionRetranscription(t *testing.T) {
	payload := `{
		"questions":[{
			"source_number_path":["2"],
			"display_label":"2",
			"question":"2、8的四分之一的五分之四是多少？",
			"subject":"数学",
			"answer_state":"present",
			"student_answer":"8/5"
		}],
		"printed_inventory":[{
			"source_number_path":["2"],
			"display_label":"2",
			"question":"2、8的1/4的4/5是多少？"
		}]
	}`
	if _, err := parseWholePageSelfInventory(payload); !errors.Is(err, k12.ErrRecognitionProtocolInvalid) {
		t.Fatalf("independently retranscribed fraction question was accepted: %v", err)
	}
}

func TestREGBUGK12DenseV1Complete_ExactRawIdentityIsNotOverwrittenByCanonicalMarkdown(t *testing.T) {
	const sourceQuestion = "在下列六个数：5、6、12、14、23、29中划去数（ ）后，能使其中3个数的和为另外2个数和的2倍。"
	payload := `{
		"questions":[{
			"source_number_path":[],
			"display_label":"",
			"question":"` + sourceQuestion + `",
			"canonical_markdown":"在下列六个数中划去一个数后，使三个数之和等于另两个数之和的两倍。",
			"subject":"数学",
			"answer_state":"present",
			"student_answer":"12"
		}],
		"printed_inventory":[{
			"source_number_path":[],
			"display_label":"",
			"question":"` + sourceQuestion + `"
		}]
	}`
	questions, err := parseWholePageSelfInventory(payload)
	if err != nil {
		t.Fatalf("byte-identical raw question identity was rejected after canonical projection: %v", err)
	}
	if len(questions) != 1 || questions[0].Question != sourceQuestion {
		t.Fatalf("printed source identity was overwritten: %#v", questions)
	}
}

func TestBUG20260808_DenseWholePageRejectsNonExactCompactInventoryFields(t *testing.T) {
	tests := map[string]string{
		"missing source number path": `{"display_label":"","question":"4÷0.5="}`,
		"missing display label":      `{"source_number_path":[],"question":"4÷0.5="}`,
		"missing question":           `{"source_number_path":[],"display_label":""}`,
		"null source number path":    `{"source_number_path":null,"display_label":"","question":"4÷0.5="}`,
		"null display label":         `{"source_number_path":[],"display_label":null,"question":"4÷0.5="}`,
		"extra subject":              `{"source_number_path":[],"display_label":"","question":"4÷0.5=","subject":"数学"}`,
	}
	for name, inventory := range tests {
		t.Run(name, func(t *testing.T) {
			payload := `{
				"questions":[{"question":"4÷0.5=","answer_state":"present","student_answer":"8"}],
				"printed_inventory":[` + inventory + `]
			}`
			_, err := parseWholePageSelfInventory(payload)
			if !errors.Is(err, k12.ErrRecognitionProtocolInvalid) {
				t.Fatalf("non-exact compact inventory error=%v, want protocol invalid", err)
			}
		})
	}
}

func TestBUG20260808_DenseWholePageSelfInventoryConvergesWithoutFallback(t *testing.T) {
	var calls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		if strings.Contains(prompt, "纵向分片") || strings.Contains(prompt, "整页印刷题清单") {
			return `[]`, nil
		}
		return `{
			"questions":[{
				"question":"4÷0.5=",
				"subject":"数学",
				"knowledge_points":["小数除法"],
				"answer_state":"present",
				"student_answer":"8"
			}],
			"printed_inventory":[{
				"source_number_path":[],
				"display_label":"",
				"question":"4÷0.5="
			}]
		}`, nil
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(
		context.Background(), denseWorksheetTestImage(t, 1000, 1800),
	)
	if err != nil {
		t.Fatalf("matching self-inventory envelope must remain a valid whole-page result: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("matching self-inventory envelope unexpectedly started fallback calls=%d", calls.Load())
	}
	if len(questions) != 1 || questions[0].Question != "4÷0.5=" || questions[0].StudentAnswer != "8" {
		t.Fatalf("whole-page self-inventory result drift: %#v", questions)
	}
}

func TestBUG20260808_DenseWholePageSelfInventoryMismatchUsesExistingBoundedFallback(t *testing.T) {
	var calls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "整页印刷题清单"):
			return `[
				{"question":"4÷0.5=","subject":"数学"},
				{"question":"10×0.01=","subject":"数学"}
			]`, nil
		case strings.Contains(prompt, "纵向分片"):
			return `[
				{"question":"4÷0.5=","subject":"数学","answer_state":"present","student_answer":"8"},
				{"question":"10×0.01=","subject":"数学","answer_state":"present","student_answer":"0.1"}
			]`, nil
		default:
			return `{
				"questions":[{
					"question":"4÷0.5=",
					"subject":"数学",
					"answer_state":"present",
					"student_answer":"8"
				}],
				"printed_inventory":[
					{"source_number_path":[],"display_label":"","question":"4÷0.5="},
					{"source_number_path":[],"display_label":"","question":"10×0.01="}
				]
			}`, nil
		}
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(
		context.Background(), denseWorksheetTestImage(t, 1000, 1800),
	)
	if err != nil {
		t.Fatalf("mismatched self-inventory must use the existing bounded fallback: %v", err)
	}
	wantCalls := int32(len(denseWorksheetRanges) + 2) // 整页调用、分片调用与打印清单调用。
	if calls.Load() != wantCalls {
		t.Fatalf("self-inventory mismatch calls=%d want bounded full plan=%d", calls.Load(), wantCalls)
	}
	if len(questions) != 2 {
		t.Fatalf("fallback did not recover the printed exact set: %#v", questions)
	}
}

func TestBUG20260808_DenseWholePageSelfInventorySourceConflictUsesExistingBoundedFallback(t *testing.T) {
	var calls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "整页印刷题清单"):
			return `[{"question":"4÷0.5=","subject":"数学"}]`, nil
		case strings.Contains(prompt, "纵向分片"):
			return `[{"question":"4÷0.5=","subject":"数学","answer_state":"present","student_answer":"8"}]`, nil
		default:
			return `{
				"questions":[{
					"source_number_path":["一","1"],
					"display_label":"一、1",
					"question":"4÷0.5=",
					"subject":"数学",
					"answer_state":"present",
					"student_answer":"8"
				}],
				"printed_inventory":[{
					"source_number_path":["一","2"],
					"display_label":"一、2",
					"question":"4÷0.5="
				}]
			}`, nil
		}
	}

	if _, err := NewRecognizerAdapter(vision).Recognize(
		context.Background(), denseWorksheetTestImage(t, 1000, 1800),
	); err != nil {
		t.Fatalf("source-conflicting self-inventory must use the existing bounded fallback: %v", err)
	}
	wantCalls := int32(len(denseWorksheetRanges) + 2)
	if calls.Load() != wantCalls {
		t.Fatalf("source-conflicting self-inventory calls=%d want bounded full plan=%d", calls.Load(), wantCalls)
	}
}

func TestBUG20260808_DenseWholePageSelfInventoryDuplicatePairingUsesExistingBoundedFallback(t *testing.T) {
	var calls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		switch {
		case strings.Contains(prompt, "整页印刷题清单"):
			return `[{"question":"4÷0.5=","subject":"数学"}]`, nil
		case strings.Contains(prompt, "纵向分片"):
			return `[{"question":"4÷0.5=","subject":"数学","answer_state":"present","student_answer":"8"}]`, nil
		default:
			return `{
				"questions":[
					{"question":"4÷0.5=","subject":"数学","answer_state":"present","student_answer":"8"},
					{"question":"4÷0.5=","subject":"数学","answer_state":"present","student_answer":"8"}
				],
				"printed_inventory":[
					{"source_number_path":[],"display_label":"","question":"4÷0.5="},
					{"source_number_path":[],"display_label":"","question":"4÷0.5="}
				]
			}`, nil
		}
	}

	if _, err := NewRecognizerAdapter(vision).Recognize(
		context.Background(), denseWorksheetTestImage(t, 1000, 1800),
	); err != nil {
		t.Fatalf("duplicate self-inventory pairing must use the existing bounded fallback: %v", err)
	}
	wantCalls := int32(len(denseWorksheetRanges) + 2)
	if calls.Load() != wantCalls {
		t.Fatalf("duplicate self-inventory calls=%d want bounded full plan=%d", calls.Load(), wantCalls)
	}
}
