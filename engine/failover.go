package engine

import (
	"context"
	"errors"
	"strings"
)

// FailoverReason 错误分类：不同分类对应不同的降级/重试策略。
//
// 设计原则：
//   - 按 HTTP 状态码 + error 特征分类
//   - 每类有明确的处理策略（见 FailoverAction）
//   - 未识别 → FailUnknown（默认重试 1 次，再失败上报）
//
// 为什么要分类：
// 以前统一用 `isTransientError` 布尔判定，所有错误都走"退避重试"，但：
//   - 402（余额不足）重试无意义 → 应换凭证
//   - 400（context 超长）重试无意义 → 应压缩重试
//   - 401（无效 Key）重试无意义 → 应直接报错
//   - 404（模型不存在）重试无意义 → 应直接报错
//
// 按原因分类后才能做精确降级。
type FailoverReason int

const (
	FailNone           FailoverReason = iota // 无错误
	FailRateLimit                            // 429 - 指数退避重试
	FailQuotaExceeded                        // 402 - 换凭证
	FailContextTooLong                       // 400 + context length - 压缩后重试
	FailProviderDown                         // 503/504/超时 - 切 Provider
	FailInvalidKey                           // 401/403 - 报错给用户
	FailModelNotFound                        // 404 - 报错
	FailUnknown                              // 其他 - 重试 1 次
)

// String 便于日志输出和测试断言
func (r FailoverReason) String() string {
	switch r {
	case FailNone:
		return "none"
	case FailRateLimit:
		return "rate_limit"
	case FailQuotaExceeded:
		return "quota_exceeded"
	case FailContextTooLong:
		return "context_too_long"
	case FailProviderDown:
		return "provider_down"
	case FailInvalidKey:
		return "invalid_key"
	case FailModelNotFound:
		return "model_not_found"
	default:
		return "unknown"
	}
}

// ClassifyError 把原始 error + HTTP 状态码 + 响应体分类为 FailoverReason。
//
// 输入：
//   - err: 原始错误（可能是网络超时、JSON 解析失败、上游返回的 HTTP error 等）
//   - httpStatus: 上游 HTTP 响应码（没有就传 0）
//   - body: 响应体文本（用于识别 context length / quota 等细节，没有就传空串）
//
// 优先级：httpStatus 精确分类 → err 消息匹配（上游库可能没暴露 status code）→ FailUnknown
func ClassifyError(err error, httpStatus int, body string) FailoverReason {
	if err == nil && httpStatus == 0 {
		return FailNone
	}

	// 1. HTTP 状态码优先分类
	switch httpStatus {
	case 429:
		return FailRateLimit
	case 402:
		return FailQuotaExceeded
	case 400:
		if isContextLengthError(body) {
			return FailContextTooLong
		}
		return FailUnknown
	case 401, 403:
		return FailInvalidKey
	case 404:
		return FailModelNotFound
	case 500, 502, 503, 504:
		return FailProviderDown
	}

	// 2. err 上下文/超时 → ProviderDown
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return FailProviderDown
		}

		// 3. error 消息模式匹配（上游库可能没暴露 httpStatus）
		// 优先英文关键词（国际化 API 标准），中文关键词仅作为国内 Provider 的兜底
		msg := strings.ToLower(err.Error())
		switch {
		case strings.Contains(msg, "429") || strings.Contains(msg, "too many requests") ||
			strings.Contains(msg, "rate limit") || strings.Contains(msg, "请求过于频繁"):
			return FailRateLimit
		case strings.Contains(msg, "402") || strings.Contains(msg, "insufficient") ||
			strings.Contains(msg, "quota") || strings.Contains(msg, "balance") ||
			strings.Contains(msg, "credit") || strings.Contains(msg, "billing") ||
			strings.Contains(msg, "余额") || strings.Contains(msg, "欠费"):
			return FailQuotaExceeded
		case isContextLengthError(msg):
			return FailContextTooLong
		case strings.Contains(msg, "401") || strings.Contains(msg, "invalid api key") || strings.Contains(msg, "unauthorized"):
			return FailInvalidKey
		case strings.Contains(msg, "404") || strings.Contains(msg, "model not found") || strings.Contains(msg, "does not exist"):
			return FailModelNotFound
		case strings.Contains(msg, "503") || strings.Contains(msg, "502") || strings.Contains(msg, "504") ||
			strings.Contains(msg, "service unavailable") || strings.Contains(msg, "bad gateway") ||
			strings.Contains(msg, "timeout") || strings.Contains(msg, "connection") ||
			strings.Contains(msg, "deadline") || strings.Contains(msg, "temporary") ||
			strings.Contains(msg, "transient"):
			return FailProviderDown
		}
	}

	return FailUnknown
}

// isContextLengthError 判定上下文超长错误。
// 各家 Provider 返回的 message 不统一，这里做模糊匹配。
func isContextLengthError(text string) bool {
	s := strings.ToLower(text)
	// 常见提示词
	markers := []string{
		"context length", "context_length",
		"maximum context", "max context",
		"context window",
		"context limit",
		"too many tokens", "token limit",
		"exceeds the model",
	}
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// FailoverAction 描述分类后应该采取的行动。
// engine 层根据这个决定如何重试 / 切 Provider / 上报。
type FailoverAction struct {
	Retry            bool   // 是否值得重试
	SwitchProvider   bool   // 是否应该换 Provider
	SwitchCredential bool   // 是否应该换凭证
	CompressContext  bool   // 是否应该压缩 context 再试
	BackoffSeconds   int    // 重试前等待秒数（0 = 立即）
	MaxRetries       int    // 最大重试次数
	UserFacing       string // 给用户看的简短原因（中文）
}

// HandleFailover 查表：FailoverReason → 推荐 Action
func HandleFailover(reason FailoverReason) FailoverAction {
	switch reason {
	case FailRateLimit:
		return FailoverAction{
			Retry:          true,
			BackoffSeconds: 4,
			MaxRetries:     3,
			UserFacing:     "服务繁忙，稍后重试",
		}
	case FailQuotaExceeded:
		return FailoverAction{
			Retry:            true,
			SwitchCredential: true,
			MaxRetries:       1,
			UserFacing:       "账号余额不足或达到配额，尝试切换凭证",
		}
	case FailContextTooLong:
		return FailoverAction{
			Retry:           true,
			CompressContext: true,
			MaxRetries:      1,
			UserFacing:      "对话过长，已压缩后重试",
		}
	case FailProviderDown:
		return FailoverAction{
			Retry:          true,
			SwitchProvider: true,
			MaxRetries:     2,
			UserFacing:     "模型服务暂时不可用，切换其他 Provider",
		}
	case FailInvalidKey:
		return FailoverAction{
			Retry:      false,
			UserFacing: "API 凭证无效，请检查设置",
		}
	case FailModelNotFound:
		return FailoverAction{
			Retry:      false,
			UserFacing: "指定模型不存在或已下线",
		}
	case FailUnknown:
		return FailoverAction{
			Retry:      true,
			MaxRetries: 1,
			UserFacing: "发生未知错误，已自动重试一次",
		}
	default:
		return FailoverAction{}
	}
}
