package builtin

import (
	"context"

	"github.com/everyday-items/hexclaw/skill"
)

// SummarySkill 文本摘要 Skill（占位实现）
//
// 摘要需要 LLM 能力，当前版本将请求转回主路径由 LLM 处理。
type SummarySkill struct{}

// NewSummarySkill 创建摘要 Skill
func NewSummarySkill() *SummarySkill {
	return &SummarySkill{}
}

func (s *SummarySkill) Name() string        { return "summary" }
func (s *SummarySkill) Description() string { return "对文本内容进行摘要概括" }

// Match 匹配摘要关键词
//
// 返回 false，让摘要请求走 LLM 主路径。
func (s *SummarySkill) Match(_ string) bool {
	return false
}

// Execute 执行摘要
//
// 当前为占位实现，摘要功能通过 LLM 主路径完成。
func (s *SummarySkill) Execute(_ context.Context, _ map[string]any) (*skill.Result, error) {
	return &skill.Result{
		Content: "摘要功能将通过 AI 助手完成。请直接发送需要摘要的文本，我会帮你生成摘要。",
	}, nil
}
