// thinking.go 把 thinking/reasoning 标签剥离从 engine 包下移到 adapter 包。
//
// v0.4.0 E2 收口：所有 IM 适配器 SendStream 出口必须在最终发送前调用 StripThinking，
// 防止 `<think>...</think>` 直接泄漏给终端用户（部分场景敏感）。
//
// 架构原因：engine 已依赖 adapter，因此 thinking 剥离的纯字符串处理放到 adapter 更底层，
// 让所有 IM 适配器同包内直接调用，不产生循环依赖。engine.StripAllThinking 保留为 wrapper
// 兼容老调用方。
package adapter

import (
	"regexp"
	"strings"
)

// thinkingTagPattern 匹配 <think>/<thinking>/<reasoning> 配对块（含多行）。
//   - (?s) 让 . 能匹配换行
//   - 非贪婪 .*? 避免跨标签吞噬
var thinkingTagPattern = regexp.MustCompile(`(?s)<think>.*?</think>|<thinking>.*?</thinking>|<reasoning>.*?</reasoning>`)

// thinkingOpenTailPattern 匹配未闭合的残段（流式拆包时可能只传半截标签）
var thinkingOpenTailPattern = regexp.MustCompile(`(?s)<think>.*$|<thinking>.*$|<reasoning>.*$`)

// StripThinking 移除 content 中所有 <think>/<thinking>/<reasoning> 配对块。
//
//   - "回答 <think>推理</think> 结束" → "回答  结束"
//   - 多段嵌入：全部去掉
//   - 未闭合残段（"回答 <think>还没说完"）会一并截掉（流式安全）
//
// 输入 nil-safe；保留正文连续空行（避免代码块 / Markdown 段落分隔被误压），
// 只对首尾做 TrimSpace。
func StripThinking(content string) string {
	if content == "" {
		return ""
	}
	result := thinkingTagPattern.ReplaceAllString(content, "")
	result = thinkingOpenTailPattern.ReplaceAllString(result, "")
	return strings.TrimSpace(result)
}
