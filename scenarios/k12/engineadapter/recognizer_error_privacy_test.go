package engineadapter

import (
	"strings"
	"testing"
)

func TestRecognizerParseErrorsDoNotLeakModelResponse(t *testing.T) {
	t.Parallel()
	const secret = "child-answer-secret-90731"
	tests := []struct {
		name  string
		parse func(string) error
	}{
		{
			name: "recognized questions",
			parse: func(raw string) error {
				_, err := parseRecognizedQuestions(raw)
				return err
			},
		},
		{
			name: "printed inventory",
			parse: func(raw string) error {
				_, err := parsePrintedQuestionInventory(raw)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.parse("not-json-" + secret)
			if err == nil {
				t.Fatal("invalid model response must fail")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("parse error leaked model response: %v", err)
			}
		})
	}
}
