package engine

import (
	"context"
	"strings"

	"github.com/hexagon-codes/hexclaw/memory"
)

// 增量 G③：用户画像蒸馏的 LLM 实现（实现 memory.ProfileSynthesizer）。
// 把零碎事实交给当前路由 LLM 合成稳定画像；prompt **强制只综合、不杜撰**，低温（CompleteOnce=0.3）。
// 与机械反思（零 LLM）并存不替换：本步只读事实、另写 Pinned 画像条，绝不改写既有事实正文。

// profileSynthMaxTokens 给推理模型留足额度：reasoning 模型把思考放 reasoning_content，
// 额度太低会在思考阶段耗尽 → content 空（见 AP-098）。2000 留出思考 + 画像正文空间。
const profileSynthMaxTokens = 2000

const profileSynthSystemPrompt = `你是一个用户画像蒸馏器。把零碎的事实合成为一段稳定、紧凑的中文画像，**只描述当前软件使用者本人**。

严格规则：
- 只综合下面列出的【已知事实】，绝不杜撰、不推测、不添加任何未列出的信息。
- **只刻画当前使用者本人**。事实里若提到**其他具名人物**（第三方，如某人姓名 + 其头衔/简介/特质——
  往往是使用者让你记住的资料或彩蛋），**忽略这些人物事实**，绝不把他们的身份、头衔、特质安到使用者
  头上（如"某某是公司创始人"、"某某偏爱X"这类描述别人的句子，不得写进画像）。
  分不清某条事实说的是使用者还是第三方时，宁可略去该条。
- 若与【现有画像】有时序矛盾（如"要去X"已变为"已去X"、住址/状态变更），以最新事实为准更新表述。
- 输出一段或几条短句，紧凑可读；不要解释说明、不要前后缀、不要 markdown 标题。
- 若已知事实不足以形成有意义的画像，直接输出空。`

// llmProfileSynthesizer 用 ReActEngine 的无状态 completion 合成画像。
type llmProfileSynthesizer struct {
	eng *ReActEngine
}

// NewProfileSynthesizer 返回基于引擎当前路由 LLM 的画像蒸馏器（供 main.go 接入 StartProfileDistillation）。
func NewProfileSynthesizer(eng *ReActEngine) memory.ProfileSynthesizer {
	return &llmProfileSynthesizer{eng: eng}
}

func (s *llmProfileSynthesizer) Synthesize(ctx context.Context, facts []string, prevProfile string) (string, error) {
	if len(facts) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("【已知事实】\n")
	for _, f := range facts {
		b.WriteString("- ")
		b.WriteString(f)
		b.WriteByte('\n')
	}
	if p := strings.TrimSpace(prevProfile); p != "" {
		b.WriteString("\n【现有画像】\n")
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteString("\n请输出更新后的用户画像：")
	return s.eng.CompleteOnce(ctx, profileSynthSystemPrompt, b.String(), profileSynthMaxTokens)
}
