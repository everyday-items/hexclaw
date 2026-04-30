package engine

import (
	"github.com/hexagon-codes/hexclaw/adapter"
)

// StripAllThinking 移除 content 中所有 <think>/<thinking>/<reasoning> 配对块。
//
// v0.4.0 E2：实际逻辑下移到 adapter 包（让 IM 适配器同包直接调用，避免循环依赖），
// 本函数保留为 wrapper 兼容老调用方（engine 内部 + 历史 imports）。
//
// 相比已有的 extractThinkTags（只识别开头标签），本函数处理任意位置的嵌入：
//   - "回答 <think>推理</think> 结束" → "回答  结束"
//   - 多段嵌入：全部去掉
//   - 未闭合残段（"回答 <think>还没说完"）会一并截掉（流式安全）
//
// 输入 nil-safe；保留原字符串间的正常空白，**不压缩正文中的连续空行**（v0.3.12 G3 修复：
// 避免代码块 / YAML / Python 多行字面量被误压），只在开头/结尾做 TrimSpace。
func StripAllThinking(content string) string {
	return adapter.StripThinking(content)
}
