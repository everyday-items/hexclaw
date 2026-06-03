// cron_intent 实现 D2.2 Layer 3 引导 prompt：
//
// 当用户在 chat 输入 cron-like 但不完整的描述（"每天做点东西"）
// 且没走 Layer 1 fast-path / Layer 2 LLM JSON parse 时，本层兜底：
//   - 检测意图（关键词扫描，零成本）
//   - 注入"反问澄清"引导 system prompt
//   - 强制 req.Tools=nil（从协议层根除 tool_use_id 链路 400 bug）
//
// 这是 belt-and-suspenders：前端 classifyCronIntent + cron parse endpoint
// 已经覆盖 99% 路径，本层兜剩下的 1%（手动 chat / 第三方 IM 转发）。
package engine

import (
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
)

// cronIntentKeywords cron-like 触发词（覆盖中英常见说法）。
//
// 任一关键词命中即视为 cron-like。误报代价低（多一条反问提示），漏报代价高（触发 400）。
var cronIntentKeywords = []string{
	"定时", "每天", "每周", "每月", "每年", "每小时", "每分钟", "每秒",
	"提醒", "周期", "周期性", "schedule", "cron", "remind",
	"daily", "hourly", "weekly", "monthly",
	"每隔", "每过",
}

// detectCronIntent 返回 (isCronLike, lookupMissing)。
//
// lookupMissing=true 表示意图明显但关键字段（时间或动作）缺一。
// 这种情况要走"引导反问"而不是直接进 LLM tool-calling 路径。
func detectCronIntent(text string) (bool, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false, false
	}
	hit := false
	for _, kw := range cronIntentKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			hit = true
			break
		}
	}
	if !hit {
		return false, false
	}
	// 简化判定："感觉是 cron"但字符数 < 16 通常缺一半信息
	missing := len([]rune(lower)) < 16
	return true, missing
}

// cronGuidanceSystemPrompt 引导 LLM 反问澄清，绝不调工具。
const cronGuidanceSystemPrompt = `用户的描述看起来是想要创建一个定时任务，但信息可能不完整。

你的任务：
1. **不要调用任何工具**，直接用自然语言回复用户
2. 礼貌反问用户补全：什么时候执行？要做什么？
3. 给一两个示例帮助用户："比如：每天 8 点采集新闻头条"
4. 回复要简短（≤ 80 字）

绝对不要：
- 调用 mcp / filesystem / search 等任何工具
- 假装已经创建任务
- 编造执行结果`

// applyCronIntentGuidance 在 LLM 请求层注入引导 prompt + 清空 tools。
//
// 调用时机：buildCompletionRequest 之后、provider.Complete 之前。
// 输入 req 会被原地修改。
func applyCronIntentGuidance(req *llm.CompletionRequest) {
	if req == nil {
		return
	}
	// 关键 1：tools=nil 从协议层根除 tool_use_id 链路 400
	req.Tools = nil
	// 关键 2：metadata 打 cron_context，hexagon runner 二次守卫
	if req.Metadata == nil {
		req.Metadata = make(map[string]any)
	}
	req.Metadata["cron_context"] = true
	// 关键 3：在 system 消息前/拼接引导 prompt
	guidance := llm.Message{Role: "system", Content: cronGuidanceSystemPrompt}
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		// 已有 system → prepend guidance（短行，不被淹没）
		merged := cronGuidanceSystemPrompt + "\n\n" + req.Messages[0].Content
		req.Messages[0].Content = merged
	} else {
		req.Messages = append([]llm.Message{guidance}, req.Messages...)
	}
}
