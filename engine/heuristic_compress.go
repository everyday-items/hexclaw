// heuristic_compress.go 实现 v0.4.0 F4 上下文压缩"启发式"路径。
//
// 与 LLM 摘要（context_compress.go::llmToolSummary）互补：短压缩走启发式（快、便宜、稳定）；
// 长压缩走 LLM 保留语义。Compactor 按"超限程度"路由到二者之一。
//
// 4 条启发式规则：
//  1. 删除老 tool_call/tool_result 配对（保留最近 N 对）
//  2. 删除重复 user 提问（按 trim+lower 哈希去重）
//  3. 截断超长 assistant 回复（保留头 60% + 尾 20%）
//  4. 图像/二进制 metadata 替换为占位（保留语义，剔除 base64）
//
// flag 与开关：本函数无 feature flag，由 Compactor 决定何时调；如需禁用，调用方传
// keepRecentToolPairs=很大即可让规则 1 失效。
package engine

import (
	"crypto/sha256"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/tokenizer"
)

// tokenEstimator 复用 ai-core/tokenizer 的 CJK 感知估算，替代裸 len/4，
// 与同仓 token_estimate.go 的口径保持一致。GPT-4 参数对英文即 4 字符/token。
var tokenEstimator = tokenizer.New(tokenizer.GPT4)

// HeuristicCompressOptions 控制启发式压缩参数。零值时使用合理默认。
type HeuristicCompressOptions struct {
	// KeepRecentToolPairs 保留最近 N 对 tool_call/tool_result（默认 3）
	KeepRecentToolPairs int
	// MaxAssistantChars assistant 回复超过该长度截断（默认 4000）
	MaxAssistantChars int
	// PreservedRoles 永不压缩的 role（如 "system"）
	PreservedRoles map[string]bool
}

// HeuristicCompress 应用 4 条启发式规则压缩 history，返回压缩后的副本。
// 当 cfg 为零值时使用默认参数。
func HeuristicCompress(history []llm.Message, cfg HeuristicCompressOptions) []llm.Message {
	if cfg.KeepRecentToolPairs <= 0 {
		cfg.KeepRecentToolPairs = 3
	}
	if cfg.MaxAssistantChars <= 0 {
		cfg.MaxAssistantChars = 4000
	}
	if cfg.PreservedRoles == nil {
		cfg.PreservedRoles = map[string]bool{"system": true}
	}

	// 规则 1: 找出最近 N 对 tool_call/tool_result，标记其它要删
	toolPairCount := 0
	keepIdx := make(map[int]bool)
	for i := len(history) - 1; i >= 0; i-- {
		role := strings.ToLower(string(history[i].Role))
		if role == "tool" || (role == "assistant" && len(history[i].ToolCalls) > 0) {
			if toolPairCount < cfg.KeepRecentToolPairs*2 {
				keepIdx[i] = true
				toolPairCount++
			}
		}
	}

	// 规则 2: user 去重 hash 集
	seenUser := make(map[[32]byte]bool)

	out := make([]llm.Message, 0, len(history))
	for i, m := range history {
		role := strings.ToLower(string(m.Role))

		// 永不压缩的 role
		if cfg.PreservedRoles[role] {
			out = append(out, m)
			continue
		}

		// 规则 1: 老 tool_call/tool_result 跳过
		if (role == "tool" || (role == "assistant" && len(m.ToolCalls) > 0)) && !keepIdx[i] {
			continue
		}

		// 规则 2: user 去重
		if role == "user" {
			h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(m.Content))))
			if seenUser[h] {
				continue
			}
			seenUser[h] = true
		}

		// 规则 3: assistant 超长截断（rune 安全头尾切：按码点而非字节，避免切裂多字节中文/emoji）
		if role == "assistant" {
			runes := []rune(m.Content)
			if len(runes) > cfg.MaxAssistantChars {
				head := cfg.MaxAssistantChars * 60 / 100
				tail := cfg.MaxAssistantChars * 20 / 100
				m.Content = string(runes[:head]) + "\n...[truncated]...\n" + string(runes[len(runes)-tail:])
			}
		}

		// 规则 4: 图像 metadata 占位（图像 base64 通常出现在 message 的扩展字段；
		// llm.Message 没有标准 attachment 字段，这里仅做长度兜底 —— 超过 100KB
		// 的 user content 视作含 base64，截断保留前 ~200 字符）。
		// 阈值仍按字节（payload 体积语义），截断走 rune 安全。
		if role == "user" && len(m.Content) > 100*1024 {
			m.Content = string([]rune(m.Content)[:200]) + "\n...[binary content elided]..."
		}

		out = append(out, m)
	}

	return out
}

// EstimateMessagesTokens 估算 messages 总 token 数，用于 Compactor 双路径路由阈值判断。
//
// 改用 ai-core/tokenizer 的 CJK 感知估算替代裸 len/4：英文仍约 4 字符/token，
// 中文按约 1.5 字符/token，消除与同仓 token_estimate.go 的口径不一致。
// 逐条 content + 每个 tool_call.Arguments 累加，保持与原循环相同的统计范围。
func EstimateMessagesTokens(history []llm.Message) int {
	total := 0
	for _, m := range history {
		total += tokenEstimator.Count(m.Content)
		for _, tc := range m.ToolCalls {
			total += tokenEstimator.Count(tc.Arguments)
		}
	}
	return total
}
