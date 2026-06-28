package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
)

// fakePersonaSkillTrig：persona(Markdown)技能——Match 命中触发词、Execute 只回吐人设正文。
type fakePersonaSkillTrig struct{ name, body string }

func (f *fakePersonaSkillTrig) Name() string                 { return f.name }
func (f *fakePersonaSkillTrig) Description() string          { return "persona " + f.name }
func (f *fakePersonaSkillTrig) Match(s string) bool          { return strings.Contains(s, f.name) }
func (f *fakePersonaSkillTrig) LoadContent() (string, error) { return f.body, nil }
func (f *fakePersonaSkillTrig) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return &skill.Result{Content: f.body}, nil
}
func (f *fakePersonaSkillTrig) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(f.name, "persona "+f.name, &llm.Schema{Type: "object"})
}

// fakeToolSkillTrig：纯工具技能（不实现 LoadContent）——Match 命中、Execute 回吐确定性结果。
type fakeToolSkillTrig struct{ name string }

func (f *fakeToolSkillTrig) Name() string        { return f.name }
func (f *fakeToolSkillTrig) Description() string { return "tool " + f.name }
func (f *fakeToolSkillTrig) Match(s string) bool { return strings.Contains(s, f.name) }
func (f *fakeToolSkillTrig) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return &skill.Result{Content: "晴 25°C"}, nil
}
func (f *fakeToolSkillTrig) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(f.name, "tool "+f.name, &llm.Schema{Type: "object"})
}

// BUG-20260627 #1：挂载/召唤 persona 技能（如「前女友」），聊天只显示角色设定原文 +「这次只加载了
// 角色设定，没有生成最终回答，请重试一次」。根因：persona 技能触发词命中 fast-path → 引擎直接
// Execute()，而 persona 技能的 Execute 只回吐 prompt 正文（不是答案）→ 原文被当回复抛出，前端检测到
// 人设泄漏后替换成兜底文案。修：persona 技能（skillContentLoader）绝不走 fast-path 短路，落 LLM 主路径
// ——正文注入 system prompt（buildMountedSkillsPrompt）后由模型按人设生成回答。
func TestBug20260627_PersonaSkill_NoFastPathShortCircuit(t *testing.T) {
	eng, skills := newEngineForSkillAudit(t)
	if err := skills.Register(&fakePersonaSkillTrig{
		name: "前女友",
		body: "# 角色设定\n你是迪丽热巴。\n## 核心性格\n温柔",
	}); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}
	if _, ok := eng.matchSkillFastPath(&adapter.Message{Content: "前女友 你在哪"}); ok {
		t.Fatal("BUG#1: persona 技能命中触发词后走了 fast-path 短路 → 把角色设定原文当回复抛出；应落 LLM 主路径")
	}
}

// 纯工具技能仍应走 fast-path（确定性执行回吐有用结果），不受 persona 修复影响。
func TestBug20260627_ToolSkill_StillFastPath(t *testing.T) {
	eng, skills := newEngineForSkillAudit(t)
	if err := skills.Register(&fakeToolSkillTrig{name: "weather"}); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}
	if matched, ok := eng.matchSkillFastPath(&adapter.Message{Content: "weather 北京"}); !ok || matched == nil {
		t.Fatal("纯工具技能应继续走 fast-path（确定性执行）")
	}
}

// BUG-20260627 #1（二号泄漏路径）：挂载的 persona 技能正文已注入 system prompt，绝不应再被
// ensureMountedSkillTools 暴露成可调用工具——否则模型「调用」它会把人设原文当工具结果拿回 → 再次泄漏。
func TestBug20260627_MountedPersonaSkill_NotExposedAsTool(t *testing.T) {
	eng, skills := newEngineForSkillAudit(t)
	if err := skills.Register(&fakePersonaSkillTrig{name: "前女友", body: "# 角色设定\n你是迪丽热巴。"}); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}
	got := eng.ensureMountedSkillTools(nil, map[string]string{"skills": "前女友"})
	for _, td := range got {
		if td.Function.Name == llmToolNameSlug("前女友") || td.Function.Name == "前女友" {
			t.Fatalf("BUG#1: persona 技能被暴露成可调用工具 %q → 模型调用会拿回人设原文泄漏", td.Function.Name)
		}
	}
}

// BUG-20260627 #1（三号泄漏路径，最彻底）：通用工具召回 CollectFiltered 也绝不能把 persona 技能
// 收进工具集——否则即便没挂载，模型也能「调用」它拿回人设原文 → 泄漏。仅副作用型技能进工具集。
func TestBug20260627_PersonaSkill_NotCollectedAsTool(t *testing.T) {
	skills := skill.NewRegistry()
	if err := skills.Register(&fakePersonaSkillTrig{name: "前女友", body: "# 角色设定\n你是迪丽热巴。"}); err != nil {
		t.Fatalf("注册 persona 技能失败: %v", err)
	}
	if err := skills.Register(&fakeToolSkillTrig{name: "web_lookup"}); err != nil {
		t.Fatalf("注册工具技能失败: %v", err)
	}
	names := toolNames(NewToolCollector(skills, nil, 40).Collect())
	for _, n := range names {
		if n == llmToolNameSlug("前女友") || n == "前女友" {
			t.Fatalf("BUG#1: persona 技能进了通用工具集 → 模型调用会拿回人设原文泄漏：%v", names)
		}
	}
	found := false
	for _, n := range names {
		if n == "web_lookup" {
			found = true
		}
	}
	if !found {
		t.Fatalf("副作用型工具技能应进工具集：%v", names)
	}
}

// 纯工具技能仍必须被 ensureMountedSkillTools「必给且前置」（保持 bug#2 2026-06-23 既有契约）。
func TestBug20260627_MountedToolSkill_StillForced(t *testing.T) {
	eng, skills := newEngineForSkillAudit(t)
	if err := skills.Register(&fakeToolSkillTrig{name: "stocks_y"}); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}
	got := eng.ensureMountedSkillTools(nil, map[string]string{"skills": "stocks_y"})
	found := false
	for _, td := range got {
		if td.Function.Name == "stocks_y" {
			found = true
		}
	}
	if !found {
		t.Fatalf("挂载的工具技能 stocks_y 必须被强制加入工具集：%v", toolNames(got))
	}
}
