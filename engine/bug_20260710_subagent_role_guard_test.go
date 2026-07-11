package engine

// BUG-20260710-C1（/review-go 审查 Critical）：guardExplicitRoleExists 只看
// metadata["role"] 键名、不看来源。内部子 Agent 派发（solve/spawn/orchestrate）
// 统一经 ApplySpecToMessage 无条件写 role=spec.Agent，而 solver/verifier/grader
// 既不在工厂内置角色也不在 agentRouter——guard 把整个子 Agent 体系连坐杀死：
// solve 每次运行三个子 Agent 全部报「智能体不存在」→ 收空解 →「未能解出本题」。
//
// 期望不变量（本测试钉死）：系统派发（isSystemDispatch=true，source 由 Go 侧
// 固定写入、客户端伪造不了）的 role 是内部子角色标签，不适用 fail-loud guard；
// 顶层用户显式 role 查无此人仍必须 fail-loud（由 bug_20260710_ghost_agent_role_test
// 锁住，两条不变量互为反向）。

import (
	"context"
	"strings"
	"testing"

	hexagon "github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
)

func TestBUG20260710_C1_SubAgentDispatchNotKilledByRoleGuard(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			return &hexagon.CompletionResponse{Content: "42"}, nil
		})
	eng := newEngineWithProviderAndSkills(t, provider, skill.NewRegistry())

	cases := []struct {
		agent  string
		source string
	}{
		{"solver", "solve"},      // SolveSkill 固定子角色（solve.go）
		{"verifier", "solve"},    // 同上——P0 独立验证命门
		{"grader", "solve"},      // 批改子角色
		{"math-expert", "spawn"}, // spawn/orchestrate 由 LLM 起的任意子角色名
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			msg := &adapter.Message{
				ID:       "sub-" + tc.agent,
				Platform: adapter.PlatformAPI,
				UserID:   "system",
				Content:  "1+1 等于几",
			}
			ApplySpecToMessage(msg, SubAgentSpec{
				RunID:  "run-" + tc.agent,
				Agent:  tc.agent,
				Task:   "1+1 等于几",
				Source: tc.source,
				Depth:  1,
			})
			reply, err := eng.Process(context.Background(), msg)
			if err != nil {
				t.Fatalf("BUG 复现：系统派发的子 Agent(role=%q, source=%q) 被 role guard 误杀: %v",
					tc.agent, tc.source, err)
			}
			if reply == nil || reply.Content == "" {
				t.Fatalf("子 Agent(role=%q) 应正常产出回复，got reply=%+v", tc.agent, reply)
			}
		})
	}
}

// 反向不变量兜底：guard 修复（豁免系统派发）后，顶层用户消息的幽灵 role 仍必须
// fail-loud——直接在 guard 单元层钉住两态，防止豁免写宽。
func TestBUG20260710_C1_TopLevelGhostRoleStillFailsLoud(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			return &hexagon.CompletionResponse{Content: "我是小蟹"}, nil
		})
	eng := newEngineWithProviderAndSkills(t, provider, skill.NewRegistry())
	msg := &adapter.Message{
		ID:       "top-ghost",
		Platform: adapter.PlatformAPI,
		UserID:   "u1",
		Content:  "介绍下你",
		Metadata: map[string]string{"role": "k12-tutor-DELETED00"}, // 无 source=非系统派发
	}
	_, err := eng.Process(context.Background(), msg)
	if err == nil {
		t.Fatal("顶层用户消息的幽灵 role 必须 fail-loud，got err=nil")
	}
	if want := "不存在"; !strings.Contains(err.Error(), want) {
		t.Fatalf("错误应含 %q，got: %v", want, err)
	}
}
