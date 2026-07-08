package engine

// BUG-20260708 D4（真机 qwen3.5:9b 取证）：桌面「进入辅导」pin 了 role=<注册 agent 名>，但该 agent 的
// system_prompt 人设从不生效——tutor 仍自称默认助理「小蟹」。
//
// 根因链：
//   ① 桌面 body.role → api/server.go:1055 msg.Metadata["role"]（非 pinned_agent）；
//   ② buildStreamMessages 的 roleName = metadata["role"]，非空 → 走 factory.GetRole(roleName)；
//   ③ GetRole 只查 factory.roles（**内置角色 map**），注册 agent（如 k12-tutor-xxx）不在其中 → ok=false
//      → sysContent 停在默认小蟹；agent 的 system_prompt（存在 agentRouter）从没被查。
//
// 修复：roleName 命中 factory 内置角色失败时，用 agentRouter.GetAgent(roleName) 兜底取其 SystemPrompt 人设。

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

// UT-D4-001: pin role=<注册 agent 名（非内置工厂角色）> 时，应用该 agent 的 system_prompt 人设（不回落小蟹）。
func TestBugD4_RoleNamedRegisteredAgentUsesSystemPrompt(t *testing.T) {
	eng := newEngineWithProviders(t,
		map[string]hexagon.Provider{"test": mockllm.NewLLMProvider("test")},
		map[string]config.LLMProviderConfig{"test": {Model: "mock-model"}},
		"test",
	)
	// 注册一个**唯一名**的 agent（不撞任何内置工厂角色），复刻 K12 tutor 场景：只在 agentRouter，不在 factory.roles
	dispatcher := agentrouter.New()
	const tutorName = "k12-tutor-KKE5v8zQ"
	if err := dispatcher.Register(agentrouter.AgentConfig{
		Name:         tutorName,
		SystemPrompt: "你是小明的五年级上辅导老师，不是通用助手小蟹。被问身份时明确回答你是小明的辅导老师。",
	}); err != nil {
		t.Fatalf("注册 tutor agent 失败: %v", err)
	}
	eng.SetAgentRouter(dispatcher)

	// 复刻桌面：role 落 metadata["role"]（非 pinned_agent），roleName=metadata["role"]
	meta := map[string]string{"role": tutorName}
	msgs := eng.buildStreamMessages(context.Background(), tutorName, nil, "", "你是谁", meta, nil)

	if len(msgs) == 0 || msgs[0].Role != "system" {
		t.Fatalf("首条应为 system 消息，实际 %+v", msgs)
	}
	if !strings.Contains(msgs[0].Content, "你是小明的辅导老师") {
		t.Fatalf("pin 注册 agent 时必须应用其 system_prompt 人设（防回落小蟹），实际 sysContent=%q", msgs[0].Content)
	}
	// 不得回落默认小蟹 SOUL——查其独有特征句（tutor 人设里不会有），而非查"小蟹"二字
	// （tutor 人设自己写了"不是通用助手小蟹"，含该二字属正常）。
	if strings.Contains(msgs[0].Content, "长在你电脑里") || strings.Contains(msgs[0].Content, "钳子硬") {
		t.Fatalf("不应回落默认助理小蟹 SOUL，实际 sysContent=%q", msgs[0].Content)
	}
}
