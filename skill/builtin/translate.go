package builtin

import (
	"context"
	"strings"

	"github.com/everyday-items/hexclaw/skill"
)

// TranslateSkill 翻译 Skill（占位实现）
//
// 翻译需要 LLM 能力，当前版本将请求转回主路径由 LLM 处理。
// TODO: 后续可接入专用翻译 API（DeepL、Google Translate 等）
type TranslateSkill struct{}

// NewTranslateSkill 创建翻译 Skill
func NewTranslateSkill() *TranslateSkill {
	return &TranslateSkill{}
}

func (s *TranslateSkill) Name() string        { return "translate" }
func (s *TranslateSkill) Description() string { return "翻译文本内容，支持中英互译" }

// Match 匹配翻译关键词
//
// 注意：当前实现 Match 返回 false，让翻译请求走 LLM 主路径。
// 这样 LLM 可以更准确地理解翻译意图和语种。
func (s *TranslateSkill) Match(content string) bool {
	lower := strings.ToLower(content)
	keywords := []string{"翻译", "translate", "英译中", "中译英"}
	for _, kw := range keywords {
		if strings.HasPrefix(lower, kw) {
			// 返回 false 让 LLM 处理（翻译质量更高）
			return false
		}
	}
	return false
}

// Execute 执行翻译
//
// 当前为占位实现，翻译功能通过 LLM 主路径完成。
func (s *TranslateSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	query, _ := args["query"].(string)
	content := "翻译功能将通过 AI 助手完成。请直接说「翻译：" + query + "」，我会帮你翻译。"
	return &skill.Result{Content: content}, nil
}
