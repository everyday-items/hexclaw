package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// metaSkill 实现 MetaProvider，用于验证 Tier 2 元信息渲染。
type metaSkill struct {
	name string
	desc string
	meta skill.SkillMetaInfo
}

func (s *metaSkill) Name() string                                                { return s.name }
func (s *metaSkill) Description() string                                         { return s.desc }
func (s *metaSkill) Match(string) bool                                           { return false }
func (s *metaSkill) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return nil, errors.New("metaSkill.Execute should not be called by skill_view")
}
func (s *metaSkill) ToolDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{Type: "function", Function: llm.ToolFunctionDef{Name: s.name}}
}
func (s *metaSkill) SkillMeta() skill.SkillMetaInfo { return s.meta }

// loadableSkill 实现 ContentLoader，验证 Tier 3 加载路径。
type loadableSkill struct {
	metaSkill
	content string
	err     error
}

func (s *loadableSkill) LoadContent() (string, error) { return s.content, s.err }

func TestSkillView_NotFound_ReturnsHint(t *testing.T) {
	reg := skill.NewRegistry()
	view := NewSkillViewSkill(reg)

	res, err := view.Execute(context.Background(), map[string]any{"skill_name": "missing"})
	if err != nil {
		t.Fatalf("expected no error for missing skill, got %v", err)
	}
	if !strings.Contains(res.Content, "not found") {
		t.Errorf("content should hint not-found; got %q", res.Content)
	}
	if res.Metadata["found"] != "false" {
		t.Errorf("metadata.found should be 'false'; got %q", res.Metadata["found"])
	}
}

func TestSkillView_MissingSkillName_Errors(t *testing.T) {
	reg := skill.NewRegistry()
	view := NewSkillViewSkill(reg)
	_, err := view.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error when skill_name missing")
	}
}

func TestSkillView_NilRegistry_Errors(t *testing.T) {
	view := NewSkillViewSkill(nil)
	_, err := view.Execute(context.Background(), map[string]any{"skill_name": "x"})
	if err == nil {
		t.Fatal("expected error when registry nil")
	}
}

func TestSkillView_Tier2_RendersMetaInfo(t *testing.T) {
	reg := skill.NewRegistry()
	s := &metaSkill{
		name: "math-tutor",
		desc: "辅导小学数学",
		meta: skill.SkillMetaInfo{
			Name:          "math-tutor",
			Description:   "辅导小学数学",
			Triggers:      []string{"求导", "math"},
			Tags:          []string{"k12", "math"},
			When:          []string{"subject=math"},
			NotWhen:       []string{"mode=react"},
			Tools:         []string{"calculator"},
			Requires:      []string{"knowledge-base"},
			PreferredMode: "tot",
		},
	}
	if err := reg.Register(s); err != nil {
		t.Fatal(err)
	}

	view := NewSkillViewSkill(reg)
	res, err := view.Execute(context.Background(), map[string]any{"skill_name": "math-tutor"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	c := res.Content
	for _, want := range []string{
		"# Skill: math-tutor",
		"辅导小学数学",
		"Triggers: 求导, math",
		"Tags: k12, math",
		"When: subject=math",
		"Not When: mode=react",
		"Required Tools: calculator",
		"Required Capabilities: knowledge-base",
		"Preferred Mode: tot",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("expected content to contain %q\n--- got ---\n%s", want, c)
		}
	}
	if res.Metadata["tier"] != "2" {
		t.Errorf("expected tier=2 (no ContentLoader), got %q", res.Metadata["tier"])
	}
}

func TestSkillView_Tier3_LoadsFullContent(t *testing.T) {
	reg := skill.NewRegistry()
	s := &loadableSkill{
		metaSkill: metaSkill{
			name: "essay-grader",
			desc: "作文批改",
			meta: skill.SkillMetaInfo{Name: "essay-grader", Description: "作文批改"},
		},
		content: "你是作文批改助手...\n规则：\n1. 检查语法\n2. 给出建议",
	}
	if err := reg.Register(s); err != nil {
		t.Fatal(err)
	}

	view := NewSkillViewSkill(reg)
	res, err := view.Execute(context.Background(), map[string]any{"skill_name": "essay-grader"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !strings.Contains(res.Content, "## Full Content") {
		t.Errorf("expected Tier 3 content header; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "你是作文批改助手") {
		t.Errorf("expected raw content; got %q", res.Content)
	}
	if res.Metadata["tier"] != "3" {
		t.Errorf("expected tier=3, got %q", res.Metadata["tier"])
	}
}

func TestSkillView_Tier3_LoadErrorFallsBackToTier2(t *testing.T) {
	reg := skill.NewRegistry()
	s := &loadableSkill{
		metaSkill: metaSkill{
			name: "broken",
			desc: "broken description",
			meta: skill.SkillMetaInfo{Name: "broken", Description: "broken description"},
		},
		err: errors.New("disk I/O error"),
	}
	if err := reg.Register(s); err != nil {
		t.Fatal(err)
	}

	view := NewSkillViewSkill(reg)
	res, err := view.Execute(context.Background(), map[string]any{"skill_name": "broken"})
	if err != nil {
		t.Fatalf("expected fallback to Tier 2 on load error, got hard error: %v", err)
	}
	if strings.Contains(res.Content, "## Full Content") {
		t.Errorf("Tier 3 header should not appear when content load fails")
	}
	if res.Metadata["tier"] != "2" {
		t.Errorf("expected tier=2 on load error; got %q", res.Metadata["tier"])
	}
}

func TestSkillView_ToolDefinitionShape(t *testing.T) {
	view := NewSkillViewSkill(skill.NewRegistry())
	def := view.ToolDefinition()
	if def.Function.Name != "skill_view" {
		t.Errorf("name: got %q", def.Function.Name)
	}
	if def.Function.Parameters == nil {
		t.Fatal("parameters should not be nil")
	}
	if _, ok := def.Function.Parameters.Properties["skill_name"]; !ok {
		t.Error("skill_name property missing")
	}
	required := def.Function.Parameters.Required
	if len(required) != 1 || required[0] != "skill_name" {
		t.Errorf("required should be [skill_name]; got %v", required)
	}
}

func TestSkillView_DoesNotMatchAnyContent(t *testing.T) {
	view := NewSkillViewSkill(skill.NewRegistry())
	if view.Match("anything") {
		t.Error("Match should always be false (LLM-only invocation)")
	}
}
