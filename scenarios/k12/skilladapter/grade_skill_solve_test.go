package skilladapter_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/skilladapter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
)

// TestGradeSkill_StudentAnswerOptional IM 侧契约：k12_grade 的 student_answer 改为可选
// （Required 只剩 problem）。RED（治本前）：Required 含 student_answer → 空白题 LLM 被迫编答案。
func TestGradeSkill_StudentAnswerOptional(t *testing.T) {
	sk := skilladapter.NewGradeSkill(usecase.Deps{})
	req := sk.ToolDefinition().Function.Parameters.Required
	for _, r := range req {
		if r == "student_answer" {
			t.Fatalf("student_answer 不应再必填（空白题要能不填走解题）: required=%v", req)
		}
	}
	hasProblem := false
	for _, r := range req {
		if r == "problem" {
			hasProblem = true
		}
	}
	if !hasProblem {
		t.Errorf("problem 应仍必填: required=%v", req)
	}
}

// TestGradeSkill_BlankSolves 空白题（不传 student_answer）→ 解题分叉，返回解法、不入错题本。
func TestGradeSkill_BlankSolves(t *testing.T) {
	deps := newDeps(t, usecase.GradeOutcome{}) // grader 不会被调用
	sk := skilladapter.NewGradeSkill(deps)
	ctx := skill.WithRoutedAgent(context.Background(), "mingming")

	res, err := sk.Execute(ctx, map[string]any{"problem": "3.8 × 3 = ?"}) // 无 student_answer
	if err != nil {
		t.Fatalf("空白题不应报错: %v", err)
	}
	if !strings.Contains(res.Content, "还没作答") {
		t.Errorf("空白题应走解题口径, got %q", res.Content)
	}
	if res.Metadata["k12_record_created"] == "true" {
		t.Error("空白题不应入错题本")
	}
}
