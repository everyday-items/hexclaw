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
