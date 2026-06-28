package engine

import (
	"context"
	"strings"
	"testing"

	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

type fakeLister struct{ agents []agentrouter.AgentConfig }

func (f fakeLister) ListAgents() []agentrouter.AgentConfig { return f.agents }

// P1：list_agents 自省——列出配置的专门 Agent（名 + 描述），修"盲派不存在角色名"。
func TestListAgents_ListsConfigured(t *testing.T) {
	s := NewListAgentsSkill(fakeLister{agents: []agentrouter.AgentConfig{
		{Name: "math-tutor", DisplayName: "数学老师", Description: "中小学数学解题与讲解"},
		{Name: "english-tutor", Description: "英语语法与作文"},
	}})
	res, err := s.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("list_agents 报错：%v", err)
	}
	for _, want := range []string{"math-tutor", "数学老师", "中小学数学解题与讲解", "english-tutor", "英语语法与作文"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("应列出 %q，得：%s", want, res.Content)
		}
	}
}

// 无配置 Agent → 明确说明会退默认（而非空/误导）。
func TestListAgents_EmptyExplains(t *testing.T) {
	s := NewListAgentsSkill(fakeLister{})
	res, _ := s.Execute(context.Background(), nil)
	if !strings.Contains(res.Content, "默认") {
		t.Errorf("空列表应说明退默认 Agent，得：%s", res.Content)
	}
}
