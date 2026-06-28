package engine

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// 子 Agent 执行路径硬化（评审 #1 重试/退避 + #2 输出注入防护 + #8 per-子超时）。
//
// 实测痛点：云端 429 / 本地超时会让子 Agent 一次失败即永久失败（无重试）；子 Agent 输出原样
// 回灌父 Agent 存在 Agent-to-Agent 提示注入面。本文件集中这两道防护 + 单子超时。

// subAgentRetryBackoff 是瞬时错误的退避序列（指数 + 固定，避免 rand 依赖）。
// 重试次数 = len(序列)；每次失败后按对应时长退避再试。
var subAgentRetryBackoff = []time.Duration{200 * time.Millisecond, 600 * time.Millisecond, 1500 * time.Millisecond}

// defaultSubAgentTimeout 是 orchestrate 中单个子 Agent 的默认超时（spawn 自带 5min；orchestrate
// 此前只有父 ctx，一个慢子拖垮整批——#8）。
const defaultSubAgentTimeout = 3 * time.Minute

// transientPhrases 是「无歧义的瞬时错误短语」（限流/超时/网络抖动/5xx 文字形态）。
var transientPhrases = []string{
	"rate limit", "rate-limit", "ratelimit", "too many requests",
	"timeout", "timed out", "deadline", "context deadline",
	"unexpected eof", "connection reset", "connection refused", "temporarily",
	"overloaded", "unavailable", "bad gateway", "gateway timeout",
	"service unavailable", "internal server error", "server error",
}

// transientStatusCodeRe 匹配独立的 HTTP 瞬时状态码（词边界，避免 5000 命中 500）。
var transientStatusCodeRe = regexp.MustCompile(`\b(429|500|502|503|504)\b`)

// statusContextMarkers：裸状态码仅在伴随这些「请求/上游」语境时才算瞬时——否则"computed 500 items"会误判（设计⑥）。
var statusContextMarkers = []string{"status", "code", "http", "api", "response", "gateway", "server", "请求", "上游", "服务"}

// isTransientErr 判定错误是否瞬时可重试。ctx 取消（用户主动停）不算瞬时——不重试。
func isTransientErr(err error) bool {
	if err == nil {
		return false
	}
	if err == context.Canceled {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "canceled") || strings.Contains(s, "cancelled") {
		return false
	}
	for _, m := range transientPhrases {
		if strings.Contains(s, m) {
			return true
		}
	}
	// 裸状态码仅在有请求/上游语境时才算瞬时（防把含数字的普通错误误判为可重试）。
	if transientStatusCodeRe.MatchString(s) {
		for _, m := range statusContextMarkers {
			if strings.Contains(s, m) {
				return true
			}
		}
	}
	return false
}

// runSubAgentWithRetry 在瞬时错误上按退避重试地执行一次子 Agent。非瞬时错误立即返回（不浪费）。
// ctx 取消时立刻停止。每次尝试套独立超时（#8），避免单子拖垮整批。
func runSubAgentWithRetry(ctx context.Context, execFn SubAgentExecFunc, spec SubAgentSpec, perTry time.Duration) (SubAgentResult, error) {
	if perTry <= 0 {
		perTry = defaultSubAgentTimeout
	}
	var lastErr error
	var lastRes SubAgentResult
	// 尝试次数 = 1（首发）+ len(backoff)（重试）。
	for attempt := 0; attempt <= len(subAgentRetryBackoff); attempt++ {
		tryCtx, cancel := context.WithTimeout(ctx, perTry)
		res, err := execFn(tryCtx, spec)
		cancel()
		if err == nil {
			return res, nil
		}
		lastErr, lastRes = err, res
		if !isTransientErr(err) {
			return res, err // 非瞬时：不重试
		}
		if attempt == len(subAgentRetryBackoff) {
			break // 重试用尽
		}
		select {
		case <-time.After(subAgentRetryBackoff[attempt]):
		case <-ctx.Done():
			return lastRes, ctx.Err()
		}
	}
	return lastRes, lastErr
}
