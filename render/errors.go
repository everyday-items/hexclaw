package render

import "fmt"

// 错误码（前端按 Code 决定 toast 文案 + 是否显示"安装引擎"按钮）。
//
// 这是稳定契约，前端 i18n key 直接用这些字符串。
const (
	CodeEngineMissing       = "ENGINE_MISSING"       // 引擎二进制不可用（PDF engine 缺失最常见）
	CodeFormatUnsupported   = "FORMAT_UNSUPPORTED"   // 后端不识别的 format
	CodeInputTooLarge       = "INPUT_TOO_LARGE"      // 请求体过大（> 50 MB 总，或 > 5 MB 纯 markdown）
	CodeInvalidInput        = "INVALID_INPUT"        // 输入语义违规（file:// 图片 / 绝对路径 / 远程图过大等）
	CodeOutputTooLarge      = "OUTPUT_TOO_LARGE"     // 渲染产物 > 100 MB
	CodeTimeout             = "TIMEOUT"              // 超时（pandoc 30s / typst 60s）
	CodeRenderFailed        = "RENDER_FAILED"        // 引擎返回非 0
	CodeConcurrencyRejected = "CONCURRENCY_REJECTED" // 队列已满
)

// RenderError 渲染错误。Code 字段稳定，Detail 截断 stderr 不超过 1 KB。
type RenderError struct {
	Code   string `json:"code"`
	Format Format `json:"format,omitempty"`
	Engine string `json:"engine,omitempty"` // pandoc | typst | tectonic | xelatex
	Detail string `json:"detail,omitempty"`
}

func (e *RenderError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s [%s/%s]: %s", e.Code, e.Format, e.Engine, e.Detail)
	}
	return fmt.Sprintf("%s [%s/%s]", e.Code, e.Format, e.Engine)
}

// HTTPStatus 返回错误码对应的 HTTP 状态码。
func (e *RenderError) HTTPStatus() int {
	switch e.Code {
	case CodeFormatUnsupported, CodeInputTooLarge, CodeInvalidInput:
		return 400
	case CodeEngineMissing:
		return 503
	case CodeTimeout:
		return 504
	case CodeOutputTooLarge:
		return 413
	case CodeConcurrencyRejected:
		return 429
	case CodeRenderFailed:
		return 500
	default:
		return 500
	}
}

// truncateStderr 把 stderr 截到不超过 1 KB（错误模型契约）。
// rune 安全：按码点切，绝不在多字节中文/emoji 中间切裂出 U+FFFD。
func truncateStderr(s string) string {
	const max = 1024
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "...[truncated]"
}
