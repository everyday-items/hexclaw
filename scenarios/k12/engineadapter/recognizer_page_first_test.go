package engineadapter

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestDenseWorksheet_ValidWholePageRecognitionUsesOnePhysicalRequest(t *testing.T) {
	var calls atomic.Int32
	vision := func(_ context.Context, _ []byte, prompt string) (string, error) {
		calls.Add(1)
		if strings.Contains(prompt, "纵向分片") || strings.Contains(prompt, "整页印刷题清单") {
			t.Fatalf("valid whole-page result must not fan out: %.120s", prompt)
		}
		return `[
			{"question":"1/8+1/4=","subject":"数学","answer_state":"present","student_answer":"3/8","recognition_confidence":0.99},
			{"question":"3.25+0.75=","subject":"数学","answer_state":"blank","student_answer":"","recognition_confidence":0.98}
		]`, nil
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(
		context.Background(), denseWorksheetTestImage(t, 1000, 1800),
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(questions) != 2 {
		t.Fatalf("whole-page requests=%d questions=%d, want 1/2", calls.Load(), len(questions))
	}
	if questions[0].AnswerState != usecase.AnswerStatePresent || questions[1].AnswerState != usecase.AnswerStateBlank {
		t.Fatalf("whole-page structured facts changed: %#v", questions)
	}
}

func TestDenseWorksheet_ValidEmptyWholePageDoesNotTriggerSixRequestFanout(t *testing.T) {
	var calls atomic.Int32
	vision := func(context.Context, []byte, string) (string, error) {
		calls.Add(1)
		return `[]`, nil
	}
	questions, err := NewRecognizerAdapter(vision).Recognize(
		context.Background(), denseWorksheetTestImage(t, 1000, 1800),
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(questions) != 0 {
		t.Fatalf("valid blank page requests=%d questions=%d, want 1/0", calls.Load(), len(questions))
	}
}
