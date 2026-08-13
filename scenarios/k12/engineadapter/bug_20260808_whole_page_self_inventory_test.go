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
