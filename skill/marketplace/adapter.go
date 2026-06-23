package marketplace

import (
	"context"

	"github.com/hexagon-codes/ai-core/llm"
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
func (a *markdownSkillAdapter) Match(content string) bool { return a.s.Match(content) }

// LoadContent 转发底层 MarkdownSkill 的正文加载，使引擎能把"用户显式挂载的 persona 类
// Markdown 技能"正文注入 system prompt（engine.buildMountedSkillsPrompt 通过窄接口
// `interface{ LoadContent() (string, error) }` 断言获取）。
//
// 不转发会导致挂载的 Markdown persona 技能（如"前女友"）正文永远注入不进上下文，模型
// 以通用助手身份回答（bug 2026-06-23：挂"前女友"问"你在哪"答"我是虚拟AI助手"）。
func (a *markdownSkillAdapter) LoadContent() (string, error) { return a.s.LoadContent() }

// ToolDefinition 返回 Markdown 技能的 LLM 工具定义
//
// 基于元数据中的名称和描述生成，不包含参数 Schema（Markdown 技能由 Prompt 驱动）
func (a *markdownSkillAdapter) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(a.s.Name(), a.s.Description(), &llm.Schema{
		Type: "object",
		Properties: map[string]*llm.Schema{
			"query": {Type: "string", Description: "用户输入内容"},
		},
		Required: []string{"query"},
	})
}

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
