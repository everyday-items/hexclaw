// Package llmcall 是 ai-core/gateway/llmcall 的薄 re-export wrapper。
//
// hexclaw 在 ai-core 升级 / 重新 publish 前透过本地包过渡使用统一 LLM Gateway。
// 待 ai-core 新版本发布并被 hexclaw bump 后，本包可移除，所有调用直接 import
// `github.com/hexagon-codes/ai-core/gateway/llmcall`。
//
// 当前实现：完全委托给 ai-core 包。本包不引入额外行为。
package llmcall

import (
	upstream "github.com/hexagon-codes/ai-core/gateway/llmcall"
)

// 类型 alias — wire compatibility（用户代码 `llmcall.Progress{...}` 无需改）。
type (
	ProgressStage = upstream.ProgressStage
	Progress      = upstream.Progress
	ProgressFunc  = upstream.ProgressFunc
	Request       = upstream.Request
	Response      = upstream.Response
)

// 常量 re-export
const (
	StageStarting  = upstream.StageStarting
	StageRetrying  = upstream.StageRetrying
	StageCompleted = upstream.StageCompleted
)

// 函数 re-export
var (
	CallWithProgress = upstream.CallWithProgress
	Call             = upstream.Call
)
