package engine

import (
	"context"
	"strings"
	"testing"
)

func TestSolveLinearEquation_RealBlankWorksheet(t *testing.T) {
	tests := []struct {
		problem string
		answer  string
	}{
		{"解方程：\n\n\\[2x+5=15\\]", "5"},
		{"4X+3×0.7=6.5", "1.1"},
		{"0.75X-0.95×4=8.5", "16.4"},
		{"2x÷2.8=8.2", "11.48"},
		{"2.7+4x=12.7", "2.5"},
		{"6x+15×7=141", "6"},
	}
	for _, tt := range tests {
		t.Run(tt.problem, func(t *testing.T) {
			worked, got, ok := solveLinearEquation(tt.problem)
			if !ok || got != tt.answer {
				t.Fatalf("solveLinearEquation(%q) = %q,%v want %q,true", tt.problem, got, ok, tt.answer)
			}
			for _, want := range []string{"等式的性质", "验算", "答案：x = " + tt.answer} {
				if !strings.Contains(worked, want) {
					t.Errorf("worked solution missing %q:\n%s", want, worked)
				}
			}
		})
	}
}

func TestSolveLinearEquation_ExactFractionsAndBothSides(t *testing.T) {
	tests := []struct {
		problem string
		answer  string
	}{
		{"3x=1", "1/3"},
		{"2×(x+1)=8", "3"},
		{"2x+3=x+8", "5"},
		{"10=2x", "5"},
		{"-(x-2)=5", "-3"},
	}
	for _, tt := range tests {
		t.Run(tt.problem, func(t *testing.T) {
			_, got, ok := solveLinearEquation(tt.problem)
			if !ok || got != tt.answer {
				t.Fatalf("solveLinearEquation(%q) = %q,%v want %q,true", tt.problem, got, ok, tt.answer)
			}
		})
	}
}

func TestSolveLinearEquation_FailsClosed(t *testing.T) {
	for _, problem := range []string{
		"解方程：\\[x+1=2",                 // 不完整数学包装
		"y+1=2",                        // another variable
		"解方程：\\[x*x=1\\]",              // 非线性
		"x×x=1",                        // nonlinear, Unicode operator
		"(x+1)(x-1)=0",                 // implicit nonlinear multiplication
		"x*0*x=0",                      // syntactically nonlinear even though it simplifies
		"1/x=2",                        // division by a variable
		"2/(x-x)=1",                    // disguised variable denominator
		"x=x",                          // infinitely many solutions
		"x=x+1",                        // no solution
		"2+2=4",                        // no variable
		"解方程：\\[x+1=2\\]\n\\[x+2=3\\]", // 多题
		"x^2=1",                        // operator outside whitelist
		"解方程：\\[x+1米=2米\\]",            // 单位不是纯方程语法
		"x+()=1",                       // malformed expression
		"",                             // empty
	} {
		t.Run(problem, func(t *testing.T) {
			if _, _, ok := solveLinearEquation(problem); ok {
				t.Fatalf("unsafe/ambiguous equation must not hit deterministic path: %q", problem)
			}
		})
	}
}

func TestSolveLinearEquation_ExecuteFastPath(t *testing.T) {
	modelCalls := 0
	s := NewSolveSkill(func(_ context.Context, _ SubAgentSpec) (SubAgentResult, error) {
		modelCalls++
		return SubAgentResult{}, nil
	}, nil)
	res, err := s.Execute(context.Background(), map[string]any{
		"problem":    "解方程：\n\n\\[2x+5=15\\]",
		"grade":      "六年级上",
		"constraint": "小数加减法、小数乘法、小数除法、简易方程、解方程",
	})
	if err != nil {
		t.Fatal(err)
	}
	if modelCalls != 0 {
		t.Fatalf("deterministic equation must not call model, calls=%d", modelCalls)
	}
	if res.Metadata["solve_mode"] != "deterministic_linear_equation" ||
		res.Metadata["solve_verdict"] != "agree" ||
		res.Metadata["solve_evidence"] != "numeric_exec" ||
		res.Metadata["solve_computed"] != "5" {
		t.Fatalf("unexpected deterministic metadata: %+v", res.Metadata)
	}
}

func TestSolveLinearEquation_FastPathOnlyInAutoNonGradingMode(t *testing.T) {
	tests := []struct {
		name  string
		extra map[string]any
	}{
		{name: "explicit verification effort", extra: map[string]any{"self_consistency": 1}},
		{name: "grading", extra: map[string]any{"student_answer": "0.01"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modelCalls := 0
			s := NewSolveSkill(func(_ context.Context, spec SubAgentSpec) (SubAgentResult, error) {
				modelCalls++
				switch spec.Agent {
				case solverAgentName:
					return SubAgentResult{Output: "4x + 66.1 = 66.14\n答案：0.01"}, nil
				case verifierAgentName:
					return SubAgentResult{Output: "VERDICT: AGREE\nCOMPUTED: 0.01\n说明：一致。"}, nil
				case graderAgentName:
					return SubAgentResult{Output: "CORRECT: true\nFINAL_ANSWER_CORRECT: true\nWRONG_STEP:\nMISCONCEPTION:\nGUIDANCE: 继续保持。"}, nil
				default:
					return SubAgentResult{}, nil
				}
			}, nil)
			args := map[string]any{"problem": "75.9-9.8+4X=66.14"}
			for key, value := range tt.extra {
				args[key] = value
			}
			res, err := s.Execute(context.Background(), args)
			if err != nil {
				t.Fatal(err)
			}
			if modelCalls == 0 {
				t.Fatal("explicit verification/grading must retain the full model workflow")
			}
			if res.Metadata["solve_mode"] == "deterministic_linear_equation" {
				t.Fatalf("fast path escaped its auto&&!grading boundary: %+v", res.Metadata)
			}
		})
	}
}

func TestLinearEquationRespectsCurriculumConstraint(t *testing.T) {
	if linearEquationAllowedByConstraint("解方程：\n\n\\[2x+5=15\\]", "整数加法") {
		t.Fatal("constraint without equation knowledge must not bypass the normal scope check")
	}
	if linearEquationAllowedByConstraint("4.5x=9", "简易方程、解方程") {
		t.Fatal("constraint without decimal arithmetic must not bypass the arithmetic scope check")
	}
	if !linearEquationAllowedByConstraint("4.5x=9", "小数乘法、四则运算、简易方程") {
		t.Fatal("explicit decimal arithmetic + elementary equation scope should allow deterministic path")
	}
}
