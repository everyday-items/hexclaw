package engine

import (
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

// toolNameSet 收集工具集里出现的工具名，便于断言。
func toolNameSet(tools []llm.ToolDefinition) map[string]bool {
	out := make(map[string]bool, len(tools))
	for _, t := range tools {
		out[t.Function.Name] = true
	}
	return out
}

// BUG-20260704 残留漏点：挂载 persona 时 buildTurnContext 已抑制 KB [参考知识] + 跨会话召回两路
// 自动注入，但模型仍可**主动调用** knowledge_search / session_search 把同样的无关跨会话/KB 旧内容
// 当工具结果拉回，绕过抑制、压过人设（「帮手」型人设尤甚）。
//
// 契约：显式挂载 persona 技能时，从工具集移除这两个内部检索工具（模型主动调用的对应物），
// 与自动注入抑制对称；web_search/weather 等外部信息工具不受影响（persona 仍可合法上网）。
func TestBug20260704_MountedPersona_StripsInternalRetrievalTools(t *testing.T) {
	eng, skills := newEngineForSkillAudit(t)
	if err := skills.Register(&fakePersonaSkill{name: "示例人设", body: "你是一个示例人格助手"}); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}

	tools := []llm.ToolDefinition{
		llm.NewToolDefinition("knowledge_search", "KB 检索", &llm.Schema{Type: "object"}),
		llm.NewToolDefinition("session_search", "会话深召回", &llm.Schema{Type: "object"}),
		llm.NewToolDefinition("web_search", "联网检索", &llm.Schema{Type: "object"}),
		llm.NewToolDefinition("weather", "天气", &llm.Schema{Type: "object"}),
	}

	// 对照组（未挂载 persona）：四个工具全部保留——证明断言有牙。
	base := eng.filterInternalRetrievalToolsForPersona(tools, map[string]string{})
	if got := toolNameSet(base); !got["knowledge_search"] || !got["session_search"] || !got["web_search"] || !got["weather"] {
		t.Fatalf("对照失效：未挂载 persona 时不应剥离任何工具，got=%v", got)
	}

	// 挂载 persona：内部检索两工具必须被剥离，外部信息工具保留。
	got := toolNameSet(eng.filterInternalRetrievalToolsForPersona(tools, map[string]string{"skills": "示例人设"}))
	if got["knowledge_search"] {
		t.Error("BUG-20260704: 挂载 persona 时 knowledge_search 未被剥离，模型可主动拉回 KB 旧内容压过人设")
	}
	if got["session_search"] {
		t.Error("BUG-20260704: 挂载 persona 时 session_search 未被剥离，模型可主动拉回跨会话旧内容压过人设")
	}
	if !got["web_search"] {
		t.Error("web_search 是外部信息工具，不应被剥离（persona 仍可合法上网检索）")
	}
	if !got["weather"] {
		t.Error("weather 是外部信息工具，不应被剥离")
	}
}

// 显式挂载检索工具优先于抑制：用户既挂 persona 又显式挂 knowledge_search 时，
// 显式挂载=「必给」契约（bug#2）胜出，该工具保留；未显式挂的 session_search 仍剥离。
func TestBug20260704_ExplicitlyMountedRetrievalToolSurvivesPersona(t *testing.T) {
	eng, skills := newEngineForSkillAudit(t)
	if err := skills.Register(&fakePersonaSkill{name: "示例人设", body: "你是一个示例人格助手"}); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}

	tools := []llm.ToolDefinition{
		llm.NewToolDefinition("knowledge_search", "KB 检索", &llm.Schema{Type: "object"}),
		llm.NewToolDefinition("session_search", "会话深召回", &llm.Schema{Type: "object"}),
	}

	// 同时挂载 persona 与 knowledge_search：前者触发抑制门，后者被显式挂载须保留。
	got := toolNameSet(eng.filterInternalRetrievalToolsForPersona(
		tools, map[string]string{"skills": "示例人设,knowledge_search"}))
	if !got["knowledge_search"] {
		t.Error("显式挂载的 knowledge_search 应保留（bug#2 显式挂载=必给契约胜过 persona 抑制）")
	}
	if got["session_search"] {
		t.Error("未显式挂载的 session_search 在 persona 挂载时仍应被剥离")
	}
}
