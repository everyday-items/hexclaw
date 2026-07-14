package engine

import (
	"context"
	"strings"
	"testing"
)

func TestSolveElementaryWordProblem(t *testing.T) {
	tests := []struct {
		name, problem, answer string
	}{
		{
			name:    "fraction inverse",
			problem: "一个数的3/8是24，求这个数？",
			answer:  "64",
		},
		{
			name:    "successive fractions",
			problem: "8的1/4的4/5是多少？",
			answer:  "1.6",
		},
		{
			name:    "rectangle perimeter and yield",
			problem: "一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，一共产鱼多少千克？",
			answer:  "11250",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worked, answer, ok := solveElementaryWordProblem(tt.problem)
			if !ok || answer != tt.answer {
				t.Fatalf("solveElementaryWordProblem() = %q,%v; want %q,true", answer, ok, tt.answer)
			}
			if !strings.Contains(worked, tt.answer) || !strings.Contains(worked, "答案") {
				t.Fatalf("worked solution missing answer: %q", worked)
			}
		})
	}
}

func TestSolveElementaryWordProblemRejectsUnknownText(t *testing.T) {
	for _, problem := range []string{
		"小明有一些苹果，送人后还剩多少？",
		"证明三角形内角和是180度",
		"一个数的3/0是24，求这个数？",
		"一个周长是300米的鱼塘，一共产鱼多少千克？",
		"一个数的3/8是24，求3/8是多少？",
		"一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，面积是多少平方米？",
		"一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，宽是多少米？",
	} {
		if _, _, ok := solveElementaryWordProblem(problem); ok {
			t.Fatalf("unsupported problem must not be guessed: %q", problem)
		}
	}
}

func TestSolveElementaryWordProblemUsesGradeAppropriateIntegerSteps(t *testing.T) {
	worked, _, ok := solveElementaryWordProblem("一个数的3/8是24，求这个数？")
	if !ok {
		t.Fatal("inverse fraction problem was not solved")
	}
	if strings.Contains(worked, "÷(3/8)") || !strings.Contains(worked, "24÷3×8") {
		t.Fatalf("inverse solution uses an out-of-grade fraction division: %q", worked)
	}

	worked, _, ok = solveElementaryWordProblem("8的1/4的4/5是多少？")
	if !ok {
		t.Fatal("successive fraction problem was not solved")
	}
	if strings.Contains(worked, "×1/4") || !strings.Contains(worked, "8÷4×1÷5×4") {
		t.Fatalf("successive-fraction solution uses an out-of-grade fraction multiplication: %q", worked)
	}
}

func TestSolveElementaryWordProblemOutsideConstraintReturnsImmediately(t *testing.T) {
	calls := 0
	s := NewSolveSkill(func(context.Context, SubAgentSpec) (SubAgentResult, error) {
		calls++
		return SubAgentResult{Output: "不应调用模型"}, nil
	}, NewSubAgentRegistry(""))
	res, err := s.Execute(context.Background(), map[string]any{
		"problem":    "一个数的3/8是24，求这个数？",
		"grade":      "五年级上",
		"constraint": "小数乘法、小数除法、多边形的面积",
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("已知题型超出约束时不应掉进慢模型链，calls=%d", calls)
	}
	if res.Metadata["solve_verdict"] != "out_of_scope" || res.Metadata["solve_out_of_scope_kp"] != "分数的意义和性质" {
		t.Fatalf("unexpected metadata: %#v", res.Metadata)
	}
}
