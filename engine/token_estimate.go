package engine

import (
	"strings"

	"github.com/hexagon-codes/ai-core/tokenizer"
	"github.com/hexagon-codes/hexagon"
)

// providerToTokenizerModel 将 engine 层的 provider 名称映射到 tokenizer 支持的 Model。
// 未匹配的 provider 返回空字符串，调用方使用 fallback 逻辑。
func providerToTokenizerModel(provider string) tokenizer.Model {
	switch strings.ToLower(provider) {
	case "openai":
		return tokenizer.GPT4o
	case "anthropic":
		return tokenizer.Claude3
	case "google":
		return tokenizer.Gemini
	case "deepseek":
		return tokenizer.DeepSeek
	case "qwen", "dashscope":
		return tokenizer.Qwen
	default:
		return "" // 走 fallback
	}
}

// estimateResponseUsage 当 LLM 返回的 Usage 为空时（部分 provider 不返回 token 统计），
// 使用 ai-core tokenizer 估算 prompt + completion 的 token 数。
//
// 这是估算值（±10%），仅作为 budget 控制和成本记录的降级方案。
// 若 provider 返回了真实 usage，不应调用此函数。
func estimateResponseUsage(provider string, messages []hexagon.Message, completionContent string) (promptTokens, completionTokens, totalTokens int) {
	model := providerToTokenizerModel(provider)

	var counter *tokenizer.Counter
	if model != "" {
		counter = tokenizer.New(model)
	} else {
		// 未知 provider：使用 GPT-4 参数作为通用 fallback
		counter = tokenizer.New(tokenizer.GPT4)
	}

	// 估算 prompt tokens: 转换 hexagon.Message → tokenizer.Message
	tokMsgs := make([]tokenizer.Message, 0, len(messages))
	for _, m := range messages {
		tokMsgs = append(tokMsgs, tokenizer.Message{
			Role:    string(m.Role),
			Content: m.Content,
			Name:    m.Name,
		})
	}
	promptTokens = counter.CountMessages(tokMsgs)

	// 估算 completion tokens
	completionTokens = counter.Count(completionContent)

	totalTokens = promptTokens + completionTokens
	return
}
