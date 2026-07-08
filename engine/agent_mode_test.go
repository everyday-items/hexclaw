package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// metaSkillStub 实现 skill.Skill + skill.MetaProvider 用于 ResolveModeWithSkillHint 测试
type metaSkillStub struct {
	name string
	desc string
	meta skill.SkillMetaInfo
}

func (s *metaSkillStub) Name() string        { return s.name }
func (s *metaSkillStub) Description() string { return s.desc }
func (s *metaSkillStub) Match(string) bool   { return false }
func (s *metaSkillStub) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return nil, nil
}
func (s *metaSkillStub) ToolDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{Type: "function", Function: llm.ToolFunctionDef{Name: s.name}}
}
func (s *metaSkillStub) SkillMeta() skill.SkillMetaInfo { return s.meta }

func TestDecodeMode(t *testing.T) {
	cases := map[string]AgentMode{
		"":               ModeAuto,
		"unknown":        ModeAuto,
		"react":          ModeReAct,
		"REACT":          ModeReAct,
		" plan-execute ": ModePlanExecute,
		"reflection":     ModeReflection,
		"tot":            ModeToT,
		"TOT":            ModeToT,
		"self-reflect":   ModeSelfReflect,
		"mem-augmented":  ModeMemAugmented,
		"debate":         ModeDebate,
		"auto":           ModeAuto,
	}
	for raw, want := range cases {
		if got := DecodeMode(raw); got != want {
			t.Errorf("DecodeMode(%q): got=%s want=%s", raw, got, want)
		}
	}
}

func TestAutoRoute(t *testing.T) {
	// 清债 P5：K12 领域词（复习/备考/我孩子/以前错过/之前那道/错题本）不再 engine 硬编码，
	// 由场景包经 matcher 注入。此处模拟 K12 pack 注入，验证 engine 在原路由位置正确消费。
	SetModeKeywordMatcher(func(mode AgentMode, text string) bool {
		kw := map[AgentMode][]string{
			ModePlanExecute:  {"复习", "备考"},
			ModeMemAugmented: {"我孩子", "以前错过", "之前那道", "错题本"},
		}
		for _, k := range kw[mode] {
			if strings.Contains(text, k) {
				return true
			}
		}
		return false
	})
	defer SetModeKeywordMatcher(nil)

	cases := []struct {
		text string
		want AgentMode
	}{
		// debate 优先级最高
		{"两个答案到底哪个对", ModeDebate},
		{"帮我评分这篇作文", ModeDebate},
		// ToT 多解
		{"这道题有几种方法", ModeToT},
		{"还有别的解法吗", ModeToT},
		// memory 个性化
		{"我孩子以前错过类似的题", ModeMemAugmented},
		{"看我家错题本里这道题", ModeMemAugmented},
		// 数学 → plan-execute
		{"1 + 1 等于几", ModePlanExecute},
		{"求解方程 x^2 + 2x - 3 = 0", ModePlanExecute},
		{"$\\frac{a}{b}$ 怎么化简", ModePlanExecute},
		// 规划类 → plan-execute
		{"帮我安排下周的复习计划", ModePlanExecute},
		{"一个月备考语文怎么规划", ModePlanExecute},
		// 判题 → reflection
		{"这个答案对不对", ModeReflection},
		{"帮我批改这道题", ModeReflection},
		// 兜底 → react
		{"你好", ModeReAct},
		{"今天天气怎么样", ModeReAct},
	}
	for _, c := range cases {
		if got := AutoRoute(c.text); got != c.want {
			t.Errorf("AutoRoute(%q): got=%s want=%s", c.text, got, c.want)
		}
	}
}

func TestResolveMode(t *testing.T) {
	// 显式指定不走 auto
	if got := ResolveMode("reflection", "今天天气怎么样"); got != ModeReflection {
		t.Errorf("显式 reflection 应保留；got=%s", got)
	}
	if got := ResolveMode("tot", "你好"); got != ModeToT {
		t.Errorf("显式 tot 应保留；got=%s", got)
	}
	if got := ResolveMode("debate", "你好"); got != ModeDebate {
		t.Errorf("显式 debate 应保留；got=%s", got)
	}
	// auto 会做启发式
	if got := ResolveMode("auto", "1+1=几"); got != ModePlanExecute {
		t.Errorf("auto 在数学题应转 plan-execute；got=%s", got)
	}
	if got := ResolveMode("auto", "这道题有几种方法"); got != ModeToT {
		t.Errorf("auto 在多解题应转 tot；got=%s", got)
	}
	// 空字符串 → auto → 兜底
	if got := ResolveMode("", "你好"); got != ModeReAct {
		t.Errorf("空 mode 应 auto→react 兜底；got=%s", got)
	}
}

func TestResolveModeWithSkillHint(t *testing.T) {
	r := skill.NewRegistry()
	// 注册一个声明 preferred_mode=tot 的 Skill，trigger 含 "几何"
	_ = r.Register(&metaSkillStub{
		name: "geometry-tutor",
		desc: "几何题求解",
		meta: skill.SkillMetaInfo{
			Name:          "geometry-tutor",
			Triggers:      []string{"几何"},
			PreferredMode: "tot",
		},
	})
	// 显式指定 reflection 不被 hint 覆盖（优先级最高）
	if got := ResolveModeWithSkillHint("reflection", "几何题怎么做", r); got != ModeReflection {
		t.Errorf("显式 reflection 应保留；got=%s", got)
	}
	// auto + Top-1 命中 preferred_mode → 用 Skill 声明
	if got := ResolveModeWithSkillHint("auto", "几何题求外接圆", r); got != ModeToT {
		t.Errorf("auto + Skill PreferredMode=tot 应得 tot；got=%s", got)
	}
	// auto + 无 Skill 命中 → 走 AutoRoute（数学题命中 plan-execute）
	if got := ResolveModeWithSkillHint("auto", "1+1=几", r); got != ModePlanExecute {
		t.Errorf("auto + 无 hint 应走 AutoRoute；got=%s", got)
	}
	// nil registry → 退化到 ResolveMode
	if got := ResolveModeWithSkillHint("auto", "你好", nil); got != ModeReAct {
		t.Errorf("nil registry 应退化兜底 react；got=%s", got)
	}
	// 空 query → 退化（无法召回 Skill）
	if got := ResolveModeWithSkillHint("auto", "", r); got != ModeReAct {
		t.Errorf("空 query 应走 AutoRoute 兜底；got=%s", got)
	}
}

func TestModePromptPrefix(t *testing.T) {
	if p := modePromptPrefix(ModePlanExecute); !contains(p, "计划") {
		t.Error("plan-execute prefix 应提示计划")
	}
	if p := modePromptPrefix(ModeReflection); !contains(p, "自查") {
		t.Error("reflection prefix 应提示自查")
	}
	if p := modePromptPrefix(ModeToT); !contains(p, "思路 A") {
		t.Error("tot prefix 应提示多思路")
	}
	if p := modePromptPrefix(ModeSelfReflect); !contains(p, "反思") {
		t.Error("self-reflect prefix 应提示反思")
	}
	if p := modePromptPrefix(ModeMemAugmented); !contains(p, "档案") {
		t.Error("mem-augmented prefix 应提示档案")
	}
	if p := modePromptPrefix(ModeDebate); !contains(p, "正方") {
		t.Error("debate prefix 应提示正反方")
	}
	if p := modePromptPrefix(ModeReAct); p != "" {
		t.Error("react 不应追加 prefix（保持默认行为）")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
