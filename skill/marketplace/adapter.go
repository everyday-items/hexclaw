package marketplace

import (
	"context"

	"github.com/hexagon-codes/hexclaw/skill"
)

// markdownSkillAdapter 将 MarkdownSkill 适配为 skill.Skill（供引擎注册表使用）
type markdownSkillAdapter struct {
	s *MarkdownSkill
}

// WrapAsSkill 包装为可注册到 skill.DefaultRegistry 的 Skill
func WrapAsSkill(s *MarkdownSkill) skill.Skill {
	return &markdownSkillAdapter{s: s}
}

func (a *markdownSkillAdapter) Name() string              { return a.s.Name() }
func (a *markdownSkillAdapter) Description() string       { return a.s.Description() }
func (a *markdownSkillAdapter) Match(content string) bool   { return a.s.Match(content) }

func (a *markdownSkillAdapter) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	res, err := a.s.Execute(ctx, args)
	if err != nil {
		return nil, err
	}
	return &skill.Result{
		Content:  res.Content,
		Metadata: res.Metadata,
	}, nil
}
