package engine

import (
	"regexp"
	"strings"
)

// thinkingTagPattern 匹配 <think>/<thinking>/<reasoning> 配对块（含多行）。
//   - (?s) 让 . 能匹配换行
//   - 非贪婪 .*? 避免跨标签吞噬
//   - 对三种标签各出一条分支
var thinkingTagPattern = regexp.MustCompile(`(?s)<think>.*?</think>|<thinking>.*?</thinking>|<reasoning>.*?</reasoning>`)

// 匹配未闭合的残段（流式拆包时可能只传半截标签）
var thinkingOpenTailPattern = regexp.MustCompile(`(?s)<think>.*$|<thinking>.*$|<reasoning>.*$`)

// StripAllThinking 移除 content 中所有 <think>/<thinking>/<reasoning> 配对块。
//
// 相比已有的 extractThinkTags（只识别开头标签），本函数处理任意位置的嵌入：
//   - "回答 <think>推理</think> 结束" → "回答  结束"
//   - 多段嵌入：全部去掉
//   - 未闭合残段（"回答 <think>还没说完"）会一并截掉（流式安全）
//
// 用于：
//   - IM 适配器 SendStream 最终消息的输出前清洗
//   - WebSocket 下发给桌面端的 finalContent
//   - 任何面向用户的 text 输出路径
//
// 输入 nil-safe；保留原字符串间的正常空白，**不压缩正文中的连续空行**（v0.3.12 G3 修复：
// 避免代码块 / YAML / Python 多行字面量被误压），只在开头/结尾做 TrimSpace。
func StripAllThinking(content string) string {
	if content == "" {
		return ""
	}
	// 1. 优先移除配对的完整块
	result := thinkingTagPattern.ReplaceAllString(content, "")
	// 2. 兜底：流式分包时可能看到未闭合的 "<think>..." 尾巴
	result = thinkingOpenTailPattern.ReplaceAllString(result, "")
	// 3. 仅首尾 trim；正文连续换行保留（代码块 / Markdown 段落分隔语义）
	return strings.TrimSpace(result)
}
