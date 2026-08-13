package engineadapter

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/toolkit/util/logger"
)

func TestRecognizerParseErrorsDoNotLeakModelResponse(t *testing.T) {
	t.Parallel()
	parsers := []struct {
		name        string
		stableStage string
		parse       func(string) error
	}{
		{
			name:        "recognized questions",
			stableStage: "解析识题结果失败",
			parse: func(raw string) error {
				_, err := parseRecognizedQuestions(raw)
				return err
			},
		},
		{
			name:        "printed inventory",
			stableStage: "解析印刷题清单失败",
			parse: func(raw string) error {
				_, err := parsePrintedQuestionInventory(raw)
				return err
			},
		},
	}
	payloads := []struct {
		name       string
		canary     string
		raw        string
		jsonDetail string
	}{
		{
			name:       "syntax mismatch",
			canary:     "DD036-PRIVATE-SYNTAX-3e2a91",
			raw:        `[{"question":"DD036-PRIVATE-SYNTAX-3e2a91"`,
			jsonDetail: "unexpected end of JSON input",
		},
		{
			name:       "type mismatch",
			canary:     "DD036-PRIVATE-TYPE-a4167c",
			raw:        `[{"question":"DD036-PRIVATE-TYPE-a4167c","recognition_confidence":{"private":1}}]`,
			jsonDetail: "cannot unmarshal object",
		},
	}
	for _, parser := range parsers {
		parser := parser
		t.Run(parser.name, func(t *testing.T) {
			t.Parallel()
			for _, payload := range payloads {
				payload := payload
				t.Run(payload.name, func(t *testing.T) {
					err := parser.parse(payload.raw)
					if err == nil {
						t.Fatal("invalid model response must fail")
					}
					if !errors.Is(err, k12.ErrRecognitionProtocolInvalid) {
						t.Fatalf("parse error category drifted: %v", err)
					}
					if strings.Contains(err.Error(), payload.canary) {
						t.Fatalf("parse error leaked model response: %v", err)
					}
					if strings.Contains(err.Error(), payload.jsonDetail) {
						t.Fatalf("parse error leaked encoding/json diagnostic: %v", err)
					}
					if !strings.Contains(err.Error(), parser.stableStage) {
						t.Fatalf(
							"parse error lost stable stage %q: %v",
							parser.stableStage,
							err,
						)
					}
				})
			}
		})
	}
}

func TestRecognizerProtocolErrorsDoNotLeakModelControlValues(t *testing.T) {
	tests := []struct {
		name    string
		canary  string
		field   string
		payload string
	}{
		{
			name:   "ambiguous model problem id",
			canary: "DD036-PRIVATE-MODELREF-73f8b6",
			field:  "problem_id",
			payload: `[
				{
					"problem_id":"DD036-PRIVATE-MODELREF-73f8b6",
					"problem_kind":"compound_parent",
					"question":"共同材料甲",
					"answer_state":"blank"
				},
				{
					"problem_id":"DD036-PRIVATE-MODELREF-73f8b6",
					"problem_kind":"compound_parent",
					"question":"共同材料乙",
					"answer_state":"blank"
				}
			]`,
		},
		{
			name:   "unsupported problem kind",
			canary: "DD036-PRIVATE-KIND-b8472a",
			field:  "problem_kind",
			payload: `[{
				"problem_id":"model-problem-1",
				"problem_kind":"DD036-PRIVATE-KIND-b8472a",
				"question":"1+1=",
				"answer_state":"present",
				"student_answer":"2"
			}]`,
		},
		{
			name:   "dangling parent reference",
			canary: "DD036-PRIVATE-PARENT-e09c41",
			field:  "parent_problem_id",
			payload: `[{
				"problem_id":"model-child-1",
				"problem_kind":"subproblem",
				"parent_problem_id":"DD036-PRIVATE-PARENT-e09c41",
				"subproblem_no":"1",
				"question":"1+1=",
				"answer_state":"present",
				"student_answer":"2"
			}]`,
		},
		{
			name:   "duplicate subproblem number",
			canary: "DD036-PRIVATE-SUBNO-4c93de",
			field:  "subproblem_no",
			payload: `[
				{
					"problem_id":"model-parent-1",
					"problem_kind":"compound_parent",
					"question":"共同材料",
					"answer_state":"blank"
				},
				{
					"problem_id":"model-child-1",
					"problem_kind":"subproblem",
					"parent_problem_id":"model-parent-1",
					"subproblem_no":"DD036-PRIVATE-SUBNO-4c93de",
					"question":"第一问",
					"answer_state":"present",
					"student_answer":"甲"
				},
				{
					"problem_id":"model-child-2",
					"problem_kind":"subproblem",
					"parent_problem_id":"model-parent-1",
					"subproblem_no":"DD036-PRIVATE-SUBNO-4c93de",
					"question":"第二问",
					"answer_state":"present",
					"student_answer":"乙"
				}
			]`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recognizer := NewRecognizerAdapter(
				func(context.Context, []byte, string) (string, error) {
					return test.payload, nil
				},
			)
			_, err := recognizer.Recognize(
				context.Background(),
				[]byte("non-dense-privacy-fixture"),
			)
			if err == nil {
				t.Fatal("invalid model control value must fail protocol validation")
			}
			if !errors.Is(err, k12.ErrRecognitionProtocolInvalid) {
				t.Fatalf("protocol error category drifted: %v", err)
			}
			if strings.Contains(err.Error(), test.canary) {
				t.Fatalf(
					"protocol error leaked private model control value %q: %v",
					test.canary,
					err,
				)
			}
			if !strings.Contains(err.Error(), test.field) {
				t.Fatalf(
					"protocol error lost stable field name %q: %v",
					test.field,
					err,
				)
			}
		})
	}
}

func TestDenseWorksheetFallbackLogDoesNotLeakInvalidWholePageControlValue(
	t *testing.T,
) {
	previous := logger.Default()
	t.Cleanup(func() { logger.SetDefault(previous) })

	tests := []struct {
		name            string
		canary          string
		payload         string
		stableField     string
		forbiddenDetail string
	}{
		{
			name:        "invalid model control value",
			canary:      "DD036-PRIVATE-DENSE-KIND-9a624d",
			stableField: "problem_kind",
			payload: `{"questions":[{
				"problem_id":"model-problem-dense",
				"problem_kind":"DD036-PRIVATE-DENSE-KIND-9a624d",
				"question":"2+2=",
				"answer_state":"present",
				"student_answer":"4"
			}],"printed_inventory":[{"source_number_path":[],"display_label":"","question":"2+2="}]}`,
		},
		{
			name:            "syntax mismatch",
			canary:          "DD036-PRIVATE-DENSE-SYNTAX-1027bf",
			stableField:     "failed to parse whole-page self-inventory result",
			forbiddenDetail: "unexpected end of JSON input",
			payload:         `[{"question":"DD036-PRIVATE-DENSE-SYNTAX-1027bf"`,
		},
		{
			name:            "type mismatch",
			canary:          "DD036-PRIVATE-DENSE-TYPE-f9d34e",
			stableField:     "解析识题结果失败",
			forbiddenDetail: "cannot unmarshal object",
			payload:         `{"questions":[{"question":"DD036-PRIVATE-DENSE-TYPE-f9d34e","recognition_confidence":{"private":1}}],"printed_inventory":[{"source_number_path":[],"display_label":"","question":"DD036-PRIVATE-DENSE-TYPE-f9d34e"}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var captured bytes.Buffer
			logger.SetDefault(
				logger.NewWithHandler(
					slog.NewTextHandler(&captured, &slog.HandlerOptions{
						Level: slog.LevelDebug,
					}),
				),
			)

			calls := 0
			recognizer := NewRecognizerAdapter(
				func(context.Context, []byte, string) (string, error) {
					calls++
					if calls == 1 {
						return test.payload, nil
					}
					return `[]`, nil
				},
			)
			if _, err := recognizer.Recognize(
				context.Background(),
				denseWorksheetTestImage(t, 1000, 1800),
			); err != nil {
				t.Fatalf("dense fallback fixture: %v", err)
			}

			logText := captured.String()
			if !strings.Contains(logText, "Whole-page structured-result validation failed") {
				t.Fatalf("dense protocol fallback warning was not observable: %s", logText)
			}
			if strings.Contains(logText, test.canary) {
				t.Fatalf("dense protocol fallback log leaked private model value: %s", logText)
			}
			if test.forbiddenDetail != "" &&
				strings.Contains(logText, test.forbiddenDetail) {
				t.Fatalf("dense fallback log leaked encoding/json diagnostic: %s", logText)
			}
			if !strings.Contains(logText, test.stableField) {
				t.Fatalf(
					"dense protocol fallback log lost stable field/stage %q: %s",
					test.stableField,
					logText,
				)
			}
		})
	}
}
