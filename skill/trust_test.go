package skill

import (
	"context"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

// trustingSkill 实现 Skill + MetaProvider + TrustProvider 用于 trust 过滤测试。
type trustingSkill struct {
	name  string
	desc  string
	trust TrustLevel
}

func (s *trustingSkill) Name() string                                          { return s.name }
func (s *trustingSkill) Description() string                                   { return s.desc }
func (s *trustingSkill) Match(string) bool                                     { return false }
func (s *trustingSkill) Execute(context.Context, map[string]any) (*Result, error) { return nil, nil }
func (s *trustingSkill) ToolDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{Type: "function", Function: llm.ToolFunctionDef{Name: s.name}}
}
func (s *trustingSkill) SkillMeta() SkillMetaInfo {
	return SkillMetaInfo{Name: s.name, Description: s.desc}
}
func (s *trustingSkill) TrustLevel() TrustLevel { return s.trust }

func TestTrustLevel_StringForm(t *testing.T) {
	cases := map[TrustLevel]string{
		TrustBuiltin:   "builtin",
		TrustSigned:    "signed",
		TrustLocal:     "local",
		TrustUntrusted: "untrusted",
	}
	for tl, want := range cases {
		if got := tl.String(); got != want {
			t.Errorf("TrustLevel(%d).String() = %q, want %q", tl, got, want)
		}
	}
}

func TestSkillTrust_DefaultsToLocal(t *testing.T) {
	s := &fakeSkill{name: "x", meta: SkillMetaInfo{Name: "x"}}
	if got := SkillTrust(s); got != TrustLocal {
		t.Errorf("Skill 不实现 TrustProvider 时应默认 TrustLocal；got %v", got)
	}
}

func TestSkillTrust_RespectsProvider(t *testing.T) {
	s := &trustingSkill{name: "x", trust: TrustSigned}
	if got := SkillTrust(s); got != TrustSigned {
		t.Errorf("应返回 TrustSigned；got %v", got)
	}
}

func TestSelectByMinTrust_FiltersBelow(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(&trustingSkill{name: "untrusted-1", trust: TrustUntrusted})
	_ = r.Register(&trustingSkill{name: "local-1", trust: TrustLocal})
	_ = r.Register(&trustingSkill{name: "signed-1", trust: TrustSigned})
	_ = r.Register(&trustingSkill{name: "builtin-1", trust: TrustBuiltin})

	// minTrust=TrustSigned 应过滤掉 untrusted+local
	got := r.SelectByMinTrust("", Activation{}, TrustSigned, 10)
	gotNames := names(got)
	for _, name := range gotNames {
		if name == "untrusted-1" || name == "local-1" {
			t.Errorf("低于 TrustSigned 的 Skill 不应出现；got %v", gotNames)
		}
	}
	// signed + builtin 都应在
	hasSigned, hasBuiltin := false, false
	for _, n := range gotNames {
		if n == "signed-1" {
			hasSigned = true
		}
		if n == "builtin-1" {
			hasBuiltin = true
		}
	}
	if !hasSigned || !hasBuiltin {
		t.Errorf("signed/builtin 应通过过滤；got %v", gotNames)
	}
}

func TestSelectByMinTrust_KCap(t *testing.T) {
	r := NewRegistry()
	for i := 0; i < 10; i++ {
		_ = r.Register(&trustingSkill{name: "signed-" + string(rune('a'+i)), trust: TrustSigned})
	}
	got := r.SelectByMinTrust("", Activation{}, TrustSigned, 3)
	if len(got) != 3 {
		t.Errorf("k=3 应返回 3 个；got %d", len(got))
	}
}

func TestSelectByMinTrust_BuiltinNotImplementingTrustProvider(t *testing.T) {
	// fakeSkill 不实现 TrustProvider，默认 TrustLocal —— minTrust=TrustSigned 时应被排除
	r := NewRegistry()
	_ = r.Register(&fakeSkill{name: "fake", meta: SkillMetaInfo{Name: "fake"}})
	_ = r.Register(&trustingSkill{name: "signed", trust: TrustSigned})

	got := r.SelectByMinTrust("", Activation{}, TrustSigned, 10)
	for _, s := range got {
		if s.Name() == "fake" {
			t.Errorf("默认 TrustLocal 的 Skill 不应通过 TrustSigned 过滤")
		}
	}
}
