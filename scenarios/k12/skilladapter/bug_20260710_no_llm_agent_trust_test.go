package skilladapter_test

// BUG-20260710-H1 反面半区（/review-go 审查 High，安全面）：grade/review skill 在
// ctx 无已路由 Agent 时回退采信 LLM 传的 args["agent"]——而两个 skill 的
// ToolDefinition schema 均未声明 agent 参数、描述里还写着 "do NOT pass an agent
// id"。LLM 幻觉式传入未声明参数即可把批改记录/错题写进任意其他孩子的命名空间，
// 击穿多孩隔离（records 层以 agent_name 为隔离硬边界）。
//
// 期望不变量：实例 scope 只能来自 ctx（引擎 stamp 的已路由 Agent，同
// authUserCtxKey 纪律）；ctx 缺失时**无论 args 是否带 agent**都必须 fail-loud，
// 绝不采信 LLM 参数。

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/skilladapter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
)

func TestBUG20260710_H1_GradeSkillMustNotTrustLLMAgentArg(t *testing.T) {
	deps := newDeps(t, usecase.GradeOutcome{Correct: true})
	s := skilladapter.NewGradeSkill(deps)

	// ctx 无 routed agent + LLM 幻觉传 agent 参数 → 必须报错拒绝，不得写库。
	_, err := s.Execute(context.Background(), map[string]any{
		"agent":          "mingming", // schema 未声明的幻觉参数
		"problem":        "3.8×3",
		"student_answer": "11.4",
	})
	if err == nil {
		t.Fatal("BUG 复现：ctx 无已路由 Agent 时采信了 LLM 传的 agent 参数——多孩隔离被打穿（want fail-loud error）")
	}
	if !strings.Contains(err.Error(), "无法确定辅导实例") {
		t.Fatalf("应报「无法确定辅导实例」，got: %v", err)
	}
}

func TestBUG20260710_H1_ReviewSkillMustNotTrustLLMAgentArg(t *testing.T) {
	deps := newDeps(t, usecase.GradeOutcome{Correct: true})
	s := skilladapter.NewReviewSkill(deps)

	_, err := s.Execute(context.Background(), map[string]any{
		"agent":  "mingming",
		"action": "queue",
	})
	if err == nil {
		t.Fatal("BUG 复现：review skill 在 ctx 无已路由 Agent 时采信了 LLM 传的 agent 参数（want fail-loud error）")
	}
	if !strings.Contains(err.Error(), "无法确定辅导实例") {
		t.Fatalf("应报「无法确定辅导实例」，got: %v", err)
	}
}

// 正路径不受影响：ctx 带已路由 Agent 时照常工作（防修复写宽成"全拒"）。
func TestBUG20260710_H1_GradeSkillCtxScopeStillWorks(t *testing.T) {
	deps := newDeps(t, usecase.GradeOutcome{Correct: true})
	s := skilladapter.NewGradeSkill(deps)

	ctx := skill.WithRoutedAgent(context.Background(), "mingming")
	res, err := s.Execute(ctx, map[string]any{
		"problem":        "3.8×3",
		"student_answer": "11.4",
	})
	if err != nil {
		t.Fatalf("ctx 带 routed agent 应正常执行: %v", err)
	}
	if res == nil || res.Content == "" {
		t.Fatalf("应有批改结果, got %+v", res)
	}
}
