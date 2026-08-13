package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestREGK12CorrectWithProcessIssue20260809001EnginePreservesFinalAnswerFact(t *testing.T) {
	t.Run("deterministic word problem separates final answer from invalid work", func(t *testing.T) {
		problem := "一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，一共产鱼多少千克？"
		verified := "宽：300÷[2×(2+1)]=50米；长：100米；面积：5000平方米。\n答案：11250千克"
		solver := NewSolveSkill(func(context.Context, SubAgentSpec) (SubAgentResult, error) {
			t.Fatal("deterministic grading must not call a model")
			return SubAgentResult{}, nil
		}, NewSubAgentRegistry(""))

		result, err := solver.GradeVerified(
			context.Background(),
			problem,
			verified,
			"300÷2÷2=50米；50×2=100米；50×100=5000平方米；答：11250千克",
		)
		if err != nil {
			t.Fatal(err)
		}
		if got := result.Metadata["grade_correct"]; got != "false" {
			t.Fatalf("overall grade=%q, want false for invalid work", got)
		}
		if got := result.Metadata["grade_final_answer_correct"]; got != "true" {
			t.Fatalf("final-answer fact=%q, want true; metadata=%#v", got, result.Metadata)
		}
		if !strings.Contains(result.Content, "The final answer is correct") {
			t.Fatalf("process-issue feedback must not say the final answer is wrong: %q", result.Content)
		}
	})

	t.Run("model contract and parser carry the independent final answer fact", func(t *testing.T) {
		prompt := buildGraderPrompt("题目", "参考解法", "11250", "过程有误，答11250")
		if !strings.Contains(prompt, "FINAL_ANSWER_CORRECT: yes or no") {
			t.Fatalf("grader prompt lacks typed final-answer field: %q", prompt)
		}

		assessment := parseGrading(
			"CORRECT: no\nFINAL_ANSWER_CORRECT: yes\nWRONG_STEP: 300÷2÷2=50\nMISCONCEPTION: 连续除法计算错误\nGUIDANCE: 逐步重算",
			"过程有误，答11250",
			"11250",
		)
		field := reflect.ValueOf(assessment).FieldByName("finalAnswerCorrect")
		if !field.IsValid() || field.Kind() != reflect.Bool || !field.Bool() {
			t.Fatalf("parsed assessment lost explicit final-answer correctness: %#v", assessment)
		}
		if assessment.correct || assessment.wrongStep == "" || assessment.misconception == "" {
			t.Fatalf("process issue must retain overall-false and explicit evidence: %#v", assessment)
		}
	})

	t.Run("final-answer label cannot impersonate the overall correct line", func(t *testing.T) {
		assessment := parseGrading(
			"FINAL_ANSWER_CORRECT: yes\nWRONG_STEP: 300÷2÷2=50\nMISCONCEPTION: 连续除法计算错误\nGUIDANCE: 逐步重算",
			"过程有误，答11250",
			"11250",
		)
		if assessment.correct {
			t.Fatalf("FINAL_ANSWER_CORRECT must not match the overall CORRECT parser: %#v", assessment)
		}
	})

	t.Run("invalid work with a wrong final answer stays final-answer false", func(t *testing.T) {
		problem := "一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，一共产鱼多少千克？"
		verified := "宽：300÷[2×(2+1)]=50米；长：100米；面积：5000平方米。\n答案：11250千克"
		solver := NewSolveSkill(nil, NewSubAgentRegistry(""))
		result, err := solver.GradeVerified(
			context.Background(),
			problem,
			verified,
			"300÷2÷2=50米；50×100=5000平方米；答：225千克",
		)
		if err != nil {
			t.Fatal(err)
		}
		if got := result.Metadata["grade_final_answer_correct"]; got != "false" {
			t.Fatalf("wrong final answer was misclassified: got=%q metadata=%#v", got, result.Metadata)
		}
	})
}
