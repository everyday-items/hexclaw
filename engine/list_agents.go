package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/skill"
)

// agentLister 是 list_agents 依赖的最小接口（*router.Dispatcher 满足它），便于测试注入假实现。
type agentLister interface {
	ListAgents() []agentrouter.AgentConfig
}

// ListAgentsSkill 让主 Agent 自省「有哪些专门 Agent 可派」（评审 P1 / #7 能力路由）。
//
// 动机：spawn/orchestrate 的 agent 名是 LLM 现编的——编了个不存在的"专家"只会静默退到默认 Agent。
// 给它一个自省工具，先看清都有谁、各擅长什么，再决定派给谁，避免"盲派"。
type ListAgentsSkill struct {
	lister agentLister
}

// NewListAgentsSkill 创建 list_agents 工具。
func NewListAgentsSkill(lister agentLister) *ListAgentsSkill {
	return &ListAgentsSkill{lister: lister}
}

func (s *ListAgentsSkill) Name() string { return "list_agents" }
func (s *ListAgentsSkill) Description() string {
	return "List the specialized agents available to dispatch to, with their descriptions"
}
func (s *ListAgentsSkill) Match(_ string) bool { return false }

func (s *ListAgentsSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("list_agents",
		"List the specialized agents you can dispatch to via spawn_agent / orchestrate / transfer_to_agent, with their names and what each is good at. Call this BEFORE dispatching so you route to a real agent instead of inventing a name that silently falls back to the default.",
		&llm.Schema{Type: "object", Properties: map[string]*llm.Schema{}})
}

func (s *ListAgentsSkill) Execute(_ context.Context, _ map[string]any) (*skill.Result, error) {
	if s.lister == nil {
		return &skill.Result{Content: "未配置 Agent 路由。"}, nil
	}
	agents := s.lister.ListAgents()
	if len(agents) == 0 {
		return &skill.Result{Content: "当前没有配置专门的 Agent；spawn/orchestrate 里写的角色名会落到默认 Agent 执行（按角色名提示其专长即可）。"}, nil
	}
	var b strings.Builder
	b.WriteString("可派发的专门 Agent：\n")
	for _, a := range agents {
		name := a.Name
		if a.DisplayName != "" && a.DisplayName != a.Name {
			name = fmt.Sprintf("%s（%s）", a.Name, a.DisplayName)
		}
		desc := strings.TrimSpace(a.Description)
		if desc == "" {
			desc = "（无描述）"
		}
		fmt.Fprintf(&b, "- %s：%s\n", name, desc)
	}
	b.WriteString("\n派发时 agent 名请用上面列出的；列表外的名字会退到默认 Agent。")
	return &skill.Result{Content: b.String()}, nil
}
