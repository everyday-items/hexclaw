// Package trace 提供请求级别的结构化日志追踪。
//
// 设计原则：
//   - 基于 Go 标准库 log/slog，不引入第三方日志框架
//   - 每个请求在入口生成 trace ID，通过 context 贯穿全链路
//   - 所有日志自动携带 trace_id / user_id / session 等结构化字段
//   - 通过 CollectorHandler 桥接到 LogCollector（前端日志面板 + 文件持久化）
//
// 用法:
//
//	// 入口层（WebSocket/HTTP handler）
//	logger := trace.NewRequest(traceID, userID, sessionID)
//	ctx = trace.WithLogger(ctx, logger)
//
//	// 业务层
//	trace.L(ctx).Info("LLM 调用开始", "provider", p, "model", m, "tools", n)
//	trace.L(ctx).Error("流式输出失败", "err", err, "stage", "llm_stream")
package trace

import (
	"context"
	"log/slog"

	"github.com/hexagon-codes/toolkit/lang/contextx"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

type ctxKey struct{}

// L 从 context 中获取 logger；未设置时返回 slog.Default()。
func L(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// WithLogger 将 logger 存入 context。
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// NewRequest 创建请求级 logger，携带 trace_id 和身份信息。
func NewRequest(userID, sessionID string) *slog.Logger {
	return slog.Default().With(
		"trace_id", idgen.ShortID(),
		"user_id", userID,
		"session", sessionID,
	)
}

// ID 生成一个短 trace ID。
func ID() string {
	return idgen.ShortID()
}

// Detach 返回一个新 context：
//   - 保留父 ctx 的**所有** Values（logger / trace_id / user_id / session_id / tenant 等）
//   - 不继承 cancel/deadline（异步任务不受父请求生命周期影响）
//
// 直接复用 toolkit/lang/contextx.Detach（保留所有 Values 的正确实现）。
// 历史版本（v0.3.12 早期）只保留 logger 丢了其他 Values，已修复。
//
// 用途：fire-and-forget 的 goroutine、保存钩子、远端通知等，
// 既要日志链路追得上，又不希望父请求一超时就把异步工作也取消。
func Detach(parent context.Context) context.Context {
	return contextx.Detach(parent)
}

// Go 启动一个带 recover 和 logger 链路的 goroutine。
//
// 相比裸 `go func() { fn(context.Background()) }()`：
//   - panic 会被 recover 并记录到日志（不静默吞）
//   - context.Background() 换成 Detach(parent)，保留 trace_id / session_id
//   - task 名字用于日志定位
//
// 典型场景：异步保存 / 消息推送 / 清理任务 / 通知
//
//	trace.Go(ctx, "save-session", func(ctx context.Context) {
//	    db.Save(ctx, sess)
//	})
func Go(parent context.Context, task string, fn func(ctx context.Context)) {
	ctx := Detach(parent)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				L(ctx).Error("goroutine panic recovered",
					"task", task,
					"panic", r,
				)
			}
		}()
		fn(ctx)
	}()
}

// Recover 在同步路径里包 panic 恢复 + 日志（供 Skill/MCP 调用入口使用）。
//
//	defer trace.Recover(ctx, "skill.exec")(&err)
func Recover(ctx context.Context, task string) func(errOut *error) {
	return func(errOut *error) {
		if r := recover(); r != nil {
			L(ctx).Error("panic recovered",
				"task", task,
				"panic", r,
			)
			if errOut != nil && *errOut == nil {
				*errOut = &panicError{task: task, value: r}
			}
		}
	}
}

type panicError struct {
	task  string
	value any
}

func (e *panicError) Error() string {
	return "panic in " + e.task + ": " + toString(e.value)
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return "non-string panic value"
}
