package engine

import (
	"context"
	"reflect"
	"testing"
)

// 内部快口只允许追加 grader；若再次出现 solver/verifier，整卷批改延迟会重新翻倍。
func TestBUG20260714_GradeVerifiedRunsOnlyGrader(t *testing.T) {
	var agents []string
	exec := func(_ context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		agents = append(agents, spec.Agent)
		return SubAgentResult{Output: "CORRECT: yes\nFINAL_ANSWER_CORRECT: yes\nWRONG_STEP:\nMISCONCEPTION:\nGUIDANCE: 很好"}, nil
	}
	s := NewSolveSkill(exec, NewSubAgentRegistry(""))
	res, err := s.GradeVerified(context.Background(), "每支笔3.8元，买3支一共多少钱？", "解：3.8×3=11.4\n答案：11.4", "11.4")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(agents, []string{"grader"}) {
		t.Fatalf("agents=%v, want only grader", agents)
	}
	if res.Metadata["grade_correct"] != "true" || res.Metadata["solve_mode"] != "grading_verified_reuse" {
		t.Fatalf("unexpected metadata: %#v", res.Metadata)
	}
}

// 纯四则算式已经由本机精确求值器得到 ground truth，批改数值答案时不得再为一个相等性判断
// 调用云端/本地 grader；当前云端资源不可用时，整卷会因此每两题多等数分钟。
func TestBUG20260714_GradeVerifiedArithmeticComparesLocally(t *testing.T) {
	for _, tt := range []struct {
		name          string
		studentAnswer string
		wantCorrect   string
	}{
		{name: "decimal", studentAnswer: "11.4", wantCorrect: "true"},
		{name: "equivalent fraction", studentAnswer: "114/10", wantCorrect: "true"},
		{name: "full equation", studentAnswer: "3.8×3=11.4", wantCorrect: "true"},
		{name: "wrong", studentAnswer: "12", wantCorrect: "false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			s := NewSolveSkill(func(_ context.Context, _ SubAgentSpec) (SubAgentResult, error) {
				calls++
				return SubAgentResult{Output: "CORRECT: yes"}, nil
			}, NewSubAgentRegistry(""))
			res, err := s.GradeVerified(context.Background(), "3.8×3=?", "解：3.8×3=11.4\n答案：11.4", tt.studentAnswer)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatalf("纯算式数值批改不应调用 grader，calls=%d", calls)
			}
			if got := res.Metadata["grade_correct"]; got != tt.wantCorrect {
				t.Fatalf("grade_correct=%q, want %q", got, tt.wantCorrect)
			}
			if got := res.Metadata["solve_mode"]; got != "grading_deterministic_arithmetic" {
				t.Fatalf("solve_mode=%q", got)
			}
		})
	}
}

func TestGradeVerifiedArithmeticTreatsMixedNumberAsWholePlusProperFraction(t *testing.T) {
	for _, tt := range []struct {
		name, studentAnswer, wantCorrect string
	}{
		{name: "space separated mixed number", studentAnswer: "6 2/7", wantCorrect: "true"},
		{name: "Chinese mixed-number separator", studentAnswer: "6又2/7", wantCorrect: "true"},
		{name: "wrong mixed number", studentAnswer: "6 1/7", wantCorrect: "false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			s := NewSolveSkill(func(_ context.Context, _ SubAgentSpec) (SubAgentResult, error) {
				calls++
				return SubAgentResult{Output: "CORRECT: no"}, nil
			}, NewSubAgentRegistry(""))
			res, err := s.GradeVerified(context.Background(), "7-5/7=?", "答案：44/7", tt.studentAnswer)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatalf("mixed-number arithmetic should be compared exactly without a grader, calls=%d", calls)
			}
			if got := res.Metadata["grade_correct"]; got != tt.wantCorrect {
				t.Fatalf("grade_correct=%q, want %q; metadata=%#v", got, tt.wantCorrect, res.Metadata)
			}
			if tt.name == "space separated mixed number" {
				student := `8\times\frac{1}{4}\times\frac{4}{5}=2\times\frac{4}{5}=\frac{8}{5}=1\frac{3}{5}；\quad 答：是1\frac{3}{5}。`
				if value, ok := arithmeticAnswerValue(student); !ok || value != "1.6" {
					t.Errorf("complete mixed-number answer value=%q,%v; want 1.6,true", value, ok)
				}
				res, err = s.GradeVerified(context.Background(), "8的1/4的4/5是多少？", "答案：1.6", student)
				if err != nil {
					t.Fatal(err)
				}
				if calls != 0 || res.Metadata["grade_correct"] != "true" || res.Metadata["grade_final_answer_correct"] != "true" {
					t.Errorf("complete mixed-number work must remain locally correct: calls=%d metadata=%#v", calls, res.Metadata)
				}
			}
		})
	}
}

func TestBUG20260714_GradeVerifiedElementaryWordProblemComparesLocally(t *testing.T) {
	calls := 0
	s := NewSolveSkill(func(_ context.Context, _ SubAgentSpec) (SubAgentResult, error) {
		calls++
		return SubAgentResult{Output: "CORRECT: yes"}, nil
	}, NewSubAgentRegistry(""))
	problem := "一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，一共产鱼多少千克？"
	verified := "宽：300÷[2×(2+1)]=50米；长：100米；面积：5000平方米。\n答案：11250千克"
	res, err := s.GradeVerified(context.Background(), problem, verified, "300÷6=50m 100×2.25=225kg 答：225kg")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("已确定性求解的小学应用题不应再调用 grader，calls=%d", calls)
	}
	if res.Metadata["grade_correct"] != "false" || res.Metadata["grade_ground_truth"] != "11250千克" {
		t.Fatalf("unexpected metadata: %#v", res.Metadata)
	}
}

func TestBUG20260714_GradeVerifiedElementaryWordChecksUnitAndWork(t *testing.T) {
	problem := "一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，一共产鱼多少千克？"
	verified := "宽：300÷[2×(2+1)]=50米；长：100米；面积：5000平方米。\n答案：11250千克"
	for _, tt := range []struct {
		name, studentAnswer, wantCorrect string
	}{
		{name: "compatible unit and correct work", studentAnswer: "300÷[2×(2+1)]=50米；50×2=100米；50×100=5000平方米；5000×2.25=11250千克；答：11250kg", wantCorrect: "true"},
		{name: "wrong unit", studentAnswer: "答：11250克", wantCorrect: "false"},
		{name: "wrong intermediate step cannot be hidden by final answer", studentAnswer: "300÷6=60米；答：11250千克", wantCorrect: "false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			s := NewSolveSkill(func(_ context.Context, _ SubAgentSpec) (SubAgentResult, error) {
				calls++
				return SubAgentResult{Output: "CORRECT: yes"}, nil
			}, NewSubAgentRegistry(""))
			res, err := s.GradeVerified(context.Background(), problem, verified, tt.studentAnswer)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatalf("confident typed deterministic comparison should not call grader, calls=%d", calls)
			}
			if got := res.Metadata["grade_correct"]; got != tt.wantCorrect {
				t.Fatalf("grade_correct=%q, want %q; metadata=%#v", got, tt.wantCorrect, res.Metadata)
			}
		})
	}
}

func TestBUG20260714_GradeVerifiedDoesNotOverrideConflictingVerifiedAnswer(t *testing.T) {
	calls := 0
	s := NewSolveSkill(func(_ context.Context, spec SubAgentSpec) (SubAgentResult, error) {
		calls++
		if spec.Agent != graderAgentName {
			t.Fatalf("unexpected agent %q", spec.Agent)
		}
		return SubAgentResult{Output: "CORRECT: no\nFINAL_ANSWER_CORRECT: no\nWRONG_STEP: 答案不一致\nMISCONCEPTION: 数量关系\nGUIDANCE: 复核题目"}, nil
	}, NewSubAgentRegistry(""))
	problem := "一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，一共产鱼多少千克？"
	res, err := s.GradeVerified(context.Background(), problem, "答案：999千克", "答：11250千克")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || res.Metadata["solve_mode"] != "grading_verified_reuse" || res.Metadata["grade_ground_truth"] != "999千克" {
		t.Fatalf("conflicting verified answer must fall back to grader: calls=%d metadata=%#v", calls, res.Metadata)
	}
}

func TestBUG20260714_ParseCorrectGradeDoesNotConsumeFollowingLabels(t *testing.T) {
	a := parseGrading("CORRECT: yes\nWRONG_STEP:\nMISCONCEPTION:\nGUIDANCE: 很好", "2", "2")
	if !a.correct || a.wrongStep != "" || a.misconception != "" || a.guidance != "很好" {
		t.Fatalf("empty grading labels crossed line boundaries: %+v", a)
	}
}
