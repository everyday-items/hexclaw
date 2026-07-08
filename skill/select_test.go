package skill

import (
	"context"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

// fakeSkill 实现 Skill + MetaProvider，仅用于测试 select 逻辑。
type fakeSkill struct {
	name string
	desc string
	meta SkillMetaInfo
}

func (s *fakeSkill) Name() string                                             { return s.name }
func (s *fakeSkill) Description() string                                      { return s.desc }
func (s *fakeSkill) Match(string) bool                                        { return false }
func (s *fakeSkill) Execute(context.Context, map[string]any) (*Result, error) { return nil, nil }
func (s *fakeSkill) ToolDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Type:     "function",
		Function: llm.ToolFunctionDef{Name: s.name, Description: s.desc},
	}
}
func (s *fakeSkill) SkillMeta() SkillMetaInfo { return s.meta }

func newFake(name, desc string, triggers, tags, when, notWhen []string, prefMode string) *fakeSkill {
	return &fakeSkill{
		name: name,
		desc: desc,
		meta: SkillMetaInfo{
			Name:          name,
			Description:   desc,
			Triggers:      triggers,
			Tags:          tags,
			When:          when,
			NotWhen:       notWhen,
			PreferredMode: prefMode,
		},
	}
}

// builtinSkill 模拟内置 Skill —— 不实现 MetaProvider，验证 MissingDependencies 兜底逻辑。
type builtinSkill struct{ name string }

func (s *builtinSkill) Name() string                                             { return s.name }
func (s *builtinSkill) Description() string                                      { return "builtin" }
func (s *builtinSkill) Match(string) bool                                        { return false }
func (s *builtinSkill) Execute(context.Context, map[string]any) (*Result, error) { return nil, nil }
func (s *builtinSkill) ToolDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{Type: "function", Function: llm.ToolFunctionDef{Name: s.name}}
}

// TestMissingDependencies_AllAvailable 工具齐全时返回 nil
func TestMissingDependencies_AllAvailable(t *testing.T) {
	s := &fakeSkill{
		name: "math-tutor",
		meta: SkillMetaInfo{Name: "math-tutor", Tools: []string{"calc", "graph"}},
	}
	avail := AvailableSet(nil, "calc", "graph", "extra")
	if missing := MissingDependencies(s, avail); len(missing) != 0 {
		t.Errorf("expected no missing deps, got %v", missing)
	}
}

// TestMissingDependencies_PartialMissing 缺一个工具时返回该工具
func TestMissingDependencies_PartialMissing(t *testing.T) {
	s := &fakeSkill{
		name: "math-tutor",
		meta: SkillMetaInfo{Name: "math-tutor", Tools: []string{"calc", "graph", "ocr"}},
	}
	avail := AvailableSet(nil, "calc", "graph")
	missing := MissingDependencies(s, avail)
	if len(missing) != 1 || missing[0] != "ocr" {
		t.Errorf("expected [ocr] missing, got %v", missing)
	}
}

// TestMissingDependencies_BuiltinSkillNoDeps 不实现 MetaProvider 的 Skill 永远视作无依赖
func TestMissingDependencies_BuiltinSkillNoDeps(t *testing.T) {
	s := &builtinSkill{name: "weather"}
	if missing := MissingDependencies(s, map[string]bool{}); missing != nil {
		t.Errorf("builtin skill should have no deps, got %v", missing)
	}
}

// TestMissingDependencies_EmptyToolsList 空 Tools 列表返回 nil
func TestMissingDependencies_EmptyToolsList(t *testing.T) {
	s := &fakeSkill{name: "x", meta: SkillMetaInfo{Name: "x"}}
	if missing := MissingDependencies(s, map[string]bool{}); missing != nil {
		t.Errorf("empty tools should yield no missing, got %v", missing)
	}
}

// TestAvailableSet_BuildsFromSkillsAndExtras 构造工具集合
func TestAvailableSet_BuildsFromSkillsAndExtras(t *testing.T) {
	s1 := &fakeSkill{name: "alpha"}
	s2 := &fakeSkill{name: "beta"}
	avail := AvailableSet([]Skill{s1, s2, nil}, "extra", "")
	if !avail["alpha"] || !avail["beta"] || !avail["extra"] {
		t.Errorf("expected alpha+beta+extra available, got %v", avail)
	}
	if avail[""] {
		t.Errorf("empty extra should be ignored")
	}
}

func TestSelectByQuery_TopKByName(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newFake("math-tutor", "math help", []string{"求导", "math"}, []string{"k12"}, nil, nil, ""))
	_ = r.Register(newFake("english-tutor", "english help", []string{"english"}, []string{"k12"}, nil, nil, ""))
	_ = r.Register(newFake("weather", "show weather", []string{"weather", "天气"}, nil, nil, nil, ""))

	got := r.SelectByQuery("math-tutor", 5)
	if len(got) == 0 || got[0].Name() != "math-tutor" {
		t.Errorf("name 完全匹配应排第一，got=%v", names(got))
	}
}

func TestSelectByQuery_TriggerMatch(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newFake("math-tutor", "x", []string{"求导"}, nil, nil, nil, ""))
	_ = r.Register(newFake("plain", "x", nil, nil, nil, nil, ""))

	got := r.SelectByQuery("帮我求导这道题", 5)
	if len(got) == 0 || got[0].Name() != "math-tutor" {
		t.Errorf("trigger 命中应排在前；got=%v", names(got))
	}
}

func TestSelectByQuery_K(t *testing.T) {
	r := NewRegistry()
	for i, n := range []string{"a", "b", "c"} {
		_ = r.Register(newFake(n, "match-me", []string{"hit"}, nil, nil, nil, ""))
		_ = i
	}
	got := r.SelectByQuery("hit", 2)
	if len(got) != 2 {
		t.Errorf("Top-K 截断失败，got %d", len(got))
	}
}

func TestSelectByQuery_EmptyQueryReturnsAll(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newFake("a", "", nil, nil, nil, nil, ""))
	_ = r.Register(newFake("b", "", nil, nil, nil, nil, ""))
	got := r.SelectByQuery("", 0)
	if len(got) != 2 {
		t.Errorf("空 query 应全返回；got %d", len(got))
	}
}

func TestSelectByQuery_DescriptionMatch(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newFake("solver", "求解一元二次方程", nil, nil, nil, nil, ""))
	_ = r.Register(newFake("noise", "其他能力", nil, nil, nil, nil, ""))
	got := r.SelectByQuery("一元二次方程", 5)
	if len(got) == 0 || got[0].Name() != "solver" {
		t.Errorf("desc 命中应召回；got=%v", names(got))
	}
}

func TestSelectByContext_When(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newFake("math", "x", nil, nil, []string{"subject=math"}, nil, ""))
	_ = r.Register(newFake("english", "x", nil, nil, []string{"subject=english"}, nil, ""))

	got := r.SelectByContext("", Activation{Subject: "math"}, 0)
	if len(got) != 1 || got[0].Name() != "math" {
		t.Errorf("when=subject=math 应只激活 math；got=%v", names(got))
	}
}

func TestSelectByContext_NotWhen(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newFake("any", "x", nil, nil, nil, []string{"mode=react"}, ""))
	got := r.SelectByContext("", Activation{Mode: "react"}, 0)
	if len(got) != 0 {
		t.Errorf("not_when=mode=react 应排除；got=%v", names(got))
	}
	got2 := r.SelectByContext("", Activation{Mode: "tot"}, 0)
	if len(got2) != 1 {
		t.Errorf("not_when 不命中应保留；got=%v", names(got2))
	}
}

func TestSelectByContext_WildcardCustom(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(newFake("k12", "x", nil, nil, []string{"custom.tutor=*"}, nil, ""))
	got := r.SelectByContext("", Activation{Custom: map[string]string{"tutor": "k12-math"}}, 0)
	if len(got) != 1 {
		t.Errorf("custom.* 通配应激活；got=%v", names(got))
	}
}

func TestPreferredMode(t *testing.T) {
	s := newFake("math-tutor", "", nil, nil, nil, nil, "tot")
	if PreferredMode(s) != "tot" {
		t.Errorf("PreferredMode 读取失败")
	}
}

func names(skills []Skill) []string {
	out := make([]string, len(skills))
	for i, s := range skills {
		out[i] = s.Name()
	}
	return out
}
