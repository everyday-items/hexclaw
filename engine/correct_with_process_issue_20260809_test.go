package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestREGBUGK12ProcessEvidenceComplete004Parseability(t *testing.T) {
	complete := "CORRECT: no\nFINAL_ANSWER_CORRECT: yes\nWRONG_STEP: 42=18×2\nMISCONCEPTION: 等式两边不相等\nGUIDANCE: 分别重算两组和"
	if !gradingParseable(complete) {
		t.Fatal("complete process-issue response was rejected")
	}
	for name, response := range map[string]string{
		"missing wrong step":    "CORRECT: no\nFINAL_ANSWER_CORRECT: yes\nWRONG_STEP:\nMISCONCEPTION: 等式两边不相等\nGUIDANCE: 分别重算两组和",
		"missing misconception": "CORRECT: no\nFINAL_ANSWER_CORRECT: yes\nWRONG_STEP: 42=18×2\nMISCONCEPTION:\nGUIDANCE: 分别重算两组和",
		"missing both":          "CORRECT: no\nFINAL_ANSWER_CORRECT: yes\nWRONG_STEP:\nMISCONCEPTION:\nGUIDANCE: 分别重算两组和",
	} {
		t.Run(name, func(t *testing.T) {
			if gradingParseable(response) {
				t.Fatal("incomplete process-issue response bypassed fresh-context validation")
			}
		})
	}
	fullyCorrect := "CORRECT: yes\nFINAL_ANSWER_CORRECT: yes\nWRONG_STEP:\nMISCONCEPTION:\nGUIDANCE: 做得很好"
	if !gradingParseable(fullyCorrect) {
		t.Fatal("fully correct response unexpectedly requires process-error evidence")
	}
}

func TestREGBUGK12ProcessEvidenceComplete004UsesSingleFreshContextRetry(t *testing.T) {
	problem := "从六个数中划去一个数，使其中三个数的和为另外两个数和的2倍。"
	verified := "划去29后，5+12+23=40，6+14=20，40=20×2。答案：29"
	student := "划去29；5+23+14=42；6+12=18；42=18×2"

	t.Run("complete retry is accepted", func(t *testing.T) {
		calls := 0
		var specs []SubAgentSpec
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		outerDeadline, _ := ctx.Deadline()
		solver := NewSolveSkill(func(callCtx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
			calls++
			specs = append(specs, spec)
			callDeadline, hasDeadline := callCtx.Deadline()
			if !hasDeadline || !callDeadline.Equal(outerDeadline) {
				t.Fatalf("fresh-context retry replaced the caller deadline: got=%v want=%v",
					callDeadline, outerDeadline)
			}
			if calls == 1 {
				return SubAgentResult{Output: "CORRECT: no\nFINAL_ANSWER_CORRECT: yes\nWRONG_STEP:\nMISCONCEPTION:\nGUIDANCE: 重算两组和"}, nil
			}
			return SubAgentResult{Output: "CORRECT: no\nFINAL_ANSWER_CORRECT: yes\nWRONG_STEP: 5+23+14=42\nMISCONCEPTION: 加法计算错误\nGUIDANCE: 分别重算两组和"}, nil
		}, NewSubAgentRegistry(""))
		result, err := solver.GradeVerified(ctx, problem, verified, student)
		if err != nil {
			t.Fatal(err)
		}
		if len(specs) != 2 || specs[1].RunID != specs[0].RunID+"-retry" ||
			specs[1].Task != specs[0].Task+strictFormatReminder {
			t.Fatalf("fresh-context retry identity drifted: specs=%#v", specs)
		}
		first, retry := specs[0], specs[1]
		first.RunID, first.Task = "", ""
		retry.RunID, retry.Task = "", ""
		if !reflect.DeepEqual(first, retry) {
			t.Fatalf("fresh-context retry changed route/tool semantics: first=%#v retry=%#v", first, retry)
		}
		if calls != 2 || result.Metadata["grade_final_answer_correct"] != "true" ||
			result.Metadata["grade_wrong_step"] == "" || result.Metadata["grade_misconception"] == "" {
			t.Fatalf("fresh-context retry did not preserve complete process evidence: calls=%d metadata=%#v",
				calls, result.Metadata)
		}
	})

	t.Run("incomplete retry stays untrusted without a third call", func(t *testing.T) {
		calls := 0
		solver := NewSolveSkill(func(context.Context, SubAgentSpec) (SubAgentResult, error) {
			calls++
			return SubAgentResult{Output: "CORRECT: no\nFINAL_ANSWER_CORRECT: yes\nWRONG_STEP:\nMISCONCEPTION:\nGUIDANCE: 重算两组和"}, nil
		}, NewSubAgentRegistry(""))
		result, err := solver.GradeVerified(context.Background(), problem, verified, student)
		if err != nil {
			t.Fatal(err)
		}
		if calls != 2 || result.Metadata["grade_wrong_step"] != "" ||
			result.Metadata["grade_misconception"] != "" {
			t.Fatalf("incomplete retry invented process evidence: calls=%d metadata=%#v",
				calls, result.Metadata)
		}
	})

	t.Run("unprovable incomplete retry stays untrusted without a third call", func(t *testing.T) {
		calls := 0
		solver := NewSolveSkill(func(context.Context, SubAgentSpec) (SubAgentResult, error) {
			calls++
			return SubAgentResult{Output: "CORRECT: no\nFINAL_ANSWER_CORRECT: yes\nWRONG_STEP:\nMISCONCEPTION:\nGUIDANCE: 重算两组和"}, nil
		}, NewSubAgentRegistry(""))
		result, err := solver.GradeVerified(context.Background(), problem, verified, "划去29，分组过程看不清")
		if err != nil {
			t.Fatal(err)
		}
		if calls != 2 || result.Metadata["grade_wrong_step"] != "" ||
			result.Metadata["grade_misconception"] != "" {
			t.Fatalf("无法程序证明的过程不得猜补: calls=%d metadata=%#v", calls, result.Metadata)
		}
	})
}
