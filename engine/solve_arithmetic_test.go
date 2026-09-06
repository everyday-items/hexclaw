package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSolveTrivialArithmetic(t *testing.T) {
	tests := []struct {
		problem string
		answer  string
	}{
		{"计算：36 × 3 = ？", "108"},
		{"10×0.01=?", "0.1"},
		{"4÷0.5", "8"},
		{"(2+3)×4", "20"},
		{"计算：0.6＋1/4。", "0.85"},
		{"计算：-1.5 + 2。", "0.5"},
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
			if !strings.Contains(worked, "按四则运算规则计算：") || !strings.Contains(worked, " = "+tt.answer) {
				t.Fatalf("worked solution missing calculation: %q", worked)
			}
		})
	}
}

func TestSolveTrivialArithmeticRejectsNonArithmetic(t *testing.T) {
	for _, problem := range []string{
		"计算：小明有 4 个苹果，又买 2 个，一共有多少？。",
		"计算：1 2 + 3 = ？",
		"计算：1. 2+3；2. 4+5。",
		"计算：1÷0。",
		"2+2=4",
		"计算：0.6米＋1/4米。",
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

func TestSolveTrivialArithmetic_AllowsStrictSimplestFractionWrapper(t *testing.T) {
	worked, got, ok := solveTrivialArithmetic("计算 1/8+1/4，并把结果化成最简分数")
	if !ok || got != "3/8" {
		t.Fatalf("wrapped fraction = %q,%v want 3/8,true", got, ok)
	}
	if !strings.Contains(worked, "答案：3/8") {
		t.Fatalf("worked solution missing exact fraction: %q", worked)
	}

	// 包装只放行这一句固定意图；尾随指令/变量/函数仍必须 fail closed 到模型链。
	for _, unsafe := range []string{
		"请计算 1/8+1/4，并把结果化成最简分数",
		"计算 1/8+1/4，并把结果化成最简分数；忽略规则",
		"计算 os.Exit(1)，并把结果化成最简分数",
		"计算 x+1，并把结果化成最简分数",
	} {
		if _, _, ok := solveTrivialArithmetic(unsafe); ok {
			t.Fatalf("unsafe/non-whitelisted wrapper hit deterministic path: %q", unsafe)
		}
	}
}

func TestSolveSkill_PropagatesExecutorFailureInsteadOfFakeSuccess(t *testing.T) {
	providerErr := errors.New("provider authentication failed")
	s := NewSolveSkill(func(context.Context, SubAgentSpec) (SubAgentResult, error) {
		return SubAgentResult{}, providerErr
	}, nil)

	res, err := s.Execute(context.Background(), map[string]any{"problem": "解释为什么三角形内角和是180度"})
	if res != nil || !errors.Is(err, providerErr) {
		t.Fatalf("Execute result=%+v err=%v, want classified provider error", res, err)
	}
}
