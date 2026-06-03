package api

import "net/http"

// APIError 是 hexclaw 所有 HTTP 端点的标准错误响应结构。
//
// 设计参考：gRPC google.rpc.Status / GitHub REST API / RFC 7807（Problem Details）。
//
// 客户端解析时优先看 Code（机器可读，i18n 不变），fallback 到 Message（人类可读）。
// Details 仅在 debug 模式或排查时填充，避免泄露实现细节给最终用户。
// DocsURL 可选，指向帮助文档（如 https://docs.hexclaw.com/errors/CRON_COMPILE_FAILED）。
//
// 兼容性：JSON 形态保留旧 `error` 字段（旧客户端继续读消息），新客户端读 code/message。
type APIError struct {
	Code     string `json:"code"`               // 机器可读错误码（蛇形大写）
	Message  string `json:"message"`            // 用户可读错误描述（中文）
	Details  any    `json:"details,omitempty"`  // 实现细节（仅 debug / sentry 用）
	DocsURL  string `json:"docs_url,omitempty"` // 帮助文档链接（可选）
	LegacyEr string `json:"error,omitempty"`    // 向后兼容字段（=== Message）
}

// 标准错误码常量。新增时遵循：模块_错误类型 大写蛇形。
const (
	// 通用
	CodeBadRequest     = "BAD_REQUEST"
	CodeInternalError  = "INTERNAL_ERROR"
	CodeNotFound       = "NOT_FOUND"
	CodeUnauthorized   = "UNAUTHORIZED"
	CodeRateLimited    = "RATE_LIMITED"
	CodeServiceUnavail = "SERVICE_UNAVAILABLE"

	// Cron 模块
	CodeCronDisabled       = "CRON_DISABLED"
	CodeCronInvalidSched   = "CRON_INVALID_SCHEDULE"
	CodeCronCompileFailed  = "CRON_COMPILE_FAILED"
	CodeCronValidateFailed = "CRON_VALIDATE_FAILED"
	CodeCronNotSupported   = "CRON_NOT_SUPPORTED"
	CodeCronQuotaExceeded  = "CRON_QUOTA_EXCEEDED" // 用户活跃任务数超额
	CodeCronJobNotFound    = "CRON_JOB_NOT_FOUND"
	CodeCronUnknownAction  = "CRON_UNKNOWN_ACTION"
)

// writeAPIError 把 APIError 写回 HTTP 响应（同时填 LegacyEr 兼容旧前端）。
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	err := APIError{Code: code, Message: message, LegacyEr: message}
	writeJSON(w, status, err)
}

// writeAPIErrorWithDetails 同上但附加 details（仅 debug 用）。
func writeAPIErrorWithDetails(w http.ResponseWriter, status int, code, message string, details any) {
	err := APIError{Code: code, Message: message, Details: details, LegacyEr: message}
	writeJSON(w, status, err)
}
