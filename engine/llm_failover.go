package engine

// BUG-20260711 云端 provider 自动回退分类器。
//
// 当默认 provider 硬失败（额度耗尽 429 / 5xx / 服务不可用 / 超时 / 连接失败）时，整条云端
// 就此不可用——即便用户还配了其它健康 provider（如智谱 glm-4.5）。本文件提供“provider 级
// 不可用”的判定，供 react.go 两处 provider 调用点（completeWithTools 非流式 / processStreamRuntime
// 流式）在“非用户显式 pin”时熔断当前 provider + 自动换下一个健康 provider 重试。
//
// 与 llm_degrade.go 的分工：
//   - isToolUnsupportedError（llm_degrade.go）→ “模型不支持工具调用”，走**去工具重试**降级，
//     不换 provider。故本判定显式**排除**它（换 provider 也解决不了工具能力问题）。
//   - friendlyLLMError（llm_degrade.go）→ 回退全失败后的最终兜底，把技术错误翻译成友好中文。
//
// 鉴权类（401/403/api key）**暂不纳入**：换 provider 也可能好，但更可能是用户 key 配置问题，
// 贸然改派会掩盖真实原因、误判用户意图。先保守留在 friendlyLLMError 兜底，留此注释待议。

import (
	"strings"
	"time"
)

// providerFailoverTTL 是回退时把失败 provider 标记为短期熔断的时长。够跨过一次限流窗口，
// 又不至于长期把用户默认 provider 打黑（额度/限流多为分钟级恢复）。
const providerFailoverTTL = 60 * time.Second

// providerUnavailableNeedles 覆盖“provider 级硬失败”的小写子串特征：额度/限流、5xx、
// 服务不可用、超时、连接失败。命中即认为“换个健康 provider 可能就好”。
var providerUnavailableNeedles = []string{
	"429",
	"rate limit",
	"too many requests",
	"quota",
	"free-models-per-day",
	"insufficient",
	"500",
	"502",
	"503",
	"internal server error",
	"service unavailable",
	"connection refused",
	"unavailable",
	"context deadline",
	"timeout",
	"i/o timeout",
	// BUG-20260712：本地 CPU 巨型 prompt 触发 ResponseHeaderTimeout 时，HTTP 客户端以
	// context 取消中止读 header，错误呈 "context canceled"。这属 provider 侧超时取消（非
	// 用户主动取消），应能触发回退到云端 provider——否则本地慢/超时即整条链断在本地。
	"context canceled",
	"canceled",
}

// isProviderUnavailableError 判定错误是否为“当前 provider 级不可用、换一个健康 provider 可能就好”。
// 显式排除“模型不支持工具调用”——那条走去工具降级，不是换 provider。
func isProviderUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	if isToolUnsupportedError(err) {
		return false
	}
	return containsAnyFold(err, providerUnavailableNeedles)
}

// mapKeys 返回 set 的所有键（顺序无关），供回退 exclude 集合传给 router.Fallback。
func mapKeys(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	return names
}

// causeReason 把错误压成一行短原因，供熔断日志/审计（原始堆栈只进 trace，不外泄用户）。
func causeReason(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	const max = 200
	if len(msg) > max {
		msg = msg[:max]
	}
	return msg
}
