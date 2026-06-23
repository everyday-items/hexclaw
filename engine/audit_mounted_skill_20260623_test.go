package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// fakePersonaSkill 模拟 Markdown(persona) 技能：实现 skill.Skill + LoadContent()（正文）。
type fakePersonaSkill struct {
	name string
	body string
}

func (f *fakePersonaSkill) Name() string         { return f.name }
func (f *fakePersonaSkill) Description() string   { return "切换为人格：" + f.name }
func (f *fakePersonaSkill) Match(string) bool     { return false }
func (f *fakePersonaSkill) LoadContent() (string, error) { return f.body, nil }
func (f *fakePersonaSkill) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return &skill.Result{Content: f.body}, nil
}
func (f *fakePersonaSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(f.name, "切换为人格："+f.name, &llm.Schema{Type: "object"})
}

func newEngineForSkillAudit(t *testing.T) (*ReActEngine, skill.Registry) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {APIKey: "sk-test", Model: "test-model"},
	}
	router, _ := llmrouter.New(cfg.LLM)
	dir := t.TempDir()
	store, _ := sqlitestore.New(filepath.Join(dir, "test.db"))
	t.Cleanup(func() { store.Close() })
	store.Init(context.Background())
	skills := skill.NewRegistry()
	eng := NewReActEngine(cfg, router, store, skills)
	return eng, skills
}

// AUDIT bug#2 2026-06-23：用户在对话框挂载/召唤的 skill（如「前女友」persona）当轮不生效。
// 根因链：前端 skillNames 在 deliverMessage 处被丢弃（不入 metadata）→ 后端拿不到 → 引擎对显式挂载的
// persona 技能既不注入正文也不强制供给工具，只能靠 query 内容召回（persona 触发词常不命中）→ 首轮不识别。
// 本测试钉死引擎侧：当 metadata["skills"] 指定了某 persona 技能时，其正文必须注入 system prompt（当轮生效）。
func TestAudit_MountedPersonaSkillInjected_20260623(t *testing.T) {
	eng, skills := newEngineForSkillAudit(t)

	const marker = "你是用户的前女友，说话带点醋意与温柔，自称小染 EXGF_MARKER_9931"
	if err := skills.Register(&fakePersonaSkill{name: "前女友", body: marker}); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}

	// 模拟前端显式挂载「前女友」技能（经 metadata["skills"] 透传）
	meta := map[string]string{"skills": "前女友"}
	msgs := eng.buildStreamMessages(context.Background(), "", nil, "", "你在哪里", meta, nil)
	sys := msgs[0].Content

	if !strings.Contains(sys, marker) {
		t.Errorf("BUG#2: 显式挂载的 persona 技能正文未注入 system prompt → 当轮不生效\n实际 system:\n%s", sys)
	}
}

// fakeToolSkill 模拟纯工具技能（不实现 LoadContent）：靠工具供给生效，不注入正文。
type fakeToolSkill struct{ name string }

func (f *fakeToolSkill) Name() string       { return f.name }
func (f *fakeToolSkill) Description() string { return "工具技能 " + f.name }
func (f *fakeToolSkill) Match(string) bool   { return false }
func (f *fakeToolSkill) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return &skill.Result{Content: "ok"}, nil
}
func (f *fakeToolSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(f.name, "工具技能 "+f.name, &llm.Schema{Type: "object"})
}

// AUDIT bug#2（最优雅版）：显式挂载的【工具类】技能，其工具必须"必给且前置"——
// 与 query 召回打分无关，即使触发词不匹配正文、且 maxTools 截断也不丢（取代 query-append 打分 hack）。
func TestAudit_MountedSkillToolForcedAndCapSurvives_20260623(t *testing.T) {
	eng, skills := newEngineForSkillAudit(t)
	if err := skills.Register(&fakeToolSkill{name: "stocks_x"}); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}

	// 召回结果里全是别的工具（模拟 stocks_x 触发词不匹配正文、未被召回）
	recalled := []llm.ToolDefinition{
		llm.NewToolDefinition("web_search", "搜索", &llm.Schema{Type: "object"}),
		llm.NewToolDefinition("code_exec", "执行", &llm.Schema{Type: "object"}),
	}
	meta := map[string]string{"skills": "stocks_x"}
	got := eng.ensureMountedSkillTools(recalled, meta)

	// 1) 挂载技能的工具必须出现
	found := false
	for _, tdef := range got {
		if tdef.Function.Name == "stocks_x" {
			found = true
		}
	}
	if !found {
		t.Fatalf("BUG#2: 挂载工具技能 stocks_x 未被强制加入工具集：%v", toolNames(got))
	}
	// 2) 必须前置（index 0），这样 maxTools 截断 tools[:cap] 才保得住
	if got[0].Function.Name != "stocks_x" {
		t.Errorf("BUG#2: 挂载技能工具未前置 → maxTools 截断会丢；实际首位=%s", got[0].Function.Name)
	}
	// 3) 模拟 maxTools=1 截断后仍在
	capped := got[:1]
	if capped[0].Function.Name != "stocks_x" {
		t.Errorf("BUG#2: maxTools=1 截断后挂载技能工具丢失：%v", toolNames(capped))
	}
}

func toolNames(tools []llm.ToolDefinition) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Function.Name)
	}
	return out
}
