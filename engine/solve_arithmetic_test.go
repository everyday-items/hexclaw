package engine

import (
	"strings"
	"testing"
)

func TestSolveTrivialArithmetic(t *testing.T) {
	tests := []struct {
		problem string
		answer  string
	}{
		{"4.5×2=", "9"},
		{"10×0.01=?", "0.1"},
		{"4÷0.5", "8"},
		{"(2+3)×4", "20"},
		{"1/3 + 1/6", "0.5"},
		{"-1.5+2", "0.5"},
	}
	for _, tt := range tests {
		t.Run(tt.problem, func(t *testing.T) {
			worked, got, ok := solveTrivialArithmetic(tt.problem)
			if !ok || got != tt.answer {
				t.Fatalf("solveTrivialArithmetic(%q) = %q,%v want %q,true", tt.problem, got, ok, tt.answer)
			}
			if !strings.Contains(worked, "答案："+tt.answer) {
				t.Fatalf("worked solution missing answer: %q", worked)
			}
		})
	}
}

func TestSolveTrivialArithmeticRejectsNonArithmetic(t *testing.T) {
	for _, problem := range []string{
		"小明有 4 个苹果，又买 2 个，一共有多少？",
		"x+2=3",
		"2^3",
		"1÷0",
		"2+2=4",
		"",
	} {
		t.Run(problem, func(t *testing.T) {
			if _, _, ok := solveTrivialArithmetic(problem); ok {
				t.Fatalf("非纯求值题不应命中确定性快路: %q", problem)
			}
		})
	}
}

func TestTrivialArithmeticRespectsCurriculumConstraint(t *testing.T) {
	if trivialArithmeticAllowedByConstraint("4.5×2=", "整数加法") {
		t.Fatal("约束未允许小数/乘法时不得绕过学段校验")
	}
	if !trivialArithmeticAllowedByConstraint("4.5×2=", "小数乘法、四则运算") {
		t.Fatal("明确允许小数乘法时应开放确定性快路")
	}
}
