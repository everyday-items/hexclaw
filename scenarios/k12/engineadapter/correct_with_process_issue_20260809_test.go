package engineadapter

import (
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill"
)

func TestREGK12CorrectWithProcessIssue20260809001AdapterPreservesTriState(t *testing.T) {
	t.Run("explicit true is preserved with process evidence", func(t *testing.T) {
		outcome, err := gradeOutcomeFromResult(&skill.Result{Metadata: map[string]string{
			"grade_correct":              "false",
			"grade_final_answer_correct": "true",
			"grade_wrong_step":           "300÷2÷2=50",
			"grade_misconception":        "连续除法计算错误",
		}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		field := reflect.ValueOf(outcome).FieldByName("FinalAnswerCorrect")
		if !field.IsValid() || field.Kind() != reflect.Pointer || field.IsNil() || !field.Elem().Bool() {
			t.Fatalf("adapter dropped explicit final-answer true: %#v", outcome)
		}
	})

	t.Run("legacy metadata remains unknown rather than fabricated false", func(t *testing.T) {
		outcome, err := gradeOutcomeFromResult(&skill.Result{Metadata: map[string]string{
			"grade_correct":       "false",
			"grade_wrong_step":    "错步",
			"grade_misconception": "错因",
		}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		field := reflect.ValueOf(outcome).FieldByName("FinalAnswerCorrect")
		if !field.IsValid() || field.Kind() != reflect.Pointer || !field.IsNil() {
			t.Fatalf("legacy metadata must preserve unknown tri-state: %#v", outcome)
		}
	})

	t.Run("malformed explicit fact fails closed", func(t *testing.T) {
		_, err := gradeOutcomeFromResult(&skill.Result{Metadata: map[string]string{
			"grade_correct":              "false",
			"grade_final_answer_correct": "probably",
		}}, nil)
		if err == nil {
			t.Fatal("malformed grade_final_answer_correct was silently ignored")
		}
	})
}
