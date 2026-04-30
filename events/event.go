// Package events 实现 v0.4.0 H6 结构化事件协议（feature-flag gated）。
//
// 目标：把散落各处的 logger.Info/Warn 升级成可观测、可远程上报的结构化事件。
// 不替换现有 logger 调用 —— 而是并存：调用方在关键路径主动 Emit(ctx, evt)，
// 同时保留 logger 用于人类可读日志。
//
// 协议：每个 Event 有 Type / Severity / Source / Timestamp / Data；编码为 JSON，
// 通过 EventSink 投递（本地 JSONL 文件 / 远程 HTTP collector / 测试用 InMemory）。
//
// feature flag `events.transport.v1`：alpha 默认 OFF，flag 关闭时 Emit 是 no-op；
// flag 开启后 Emit 透传到注入到 ctx 的 Emitter 上挂的所有 Sink。
package events

import (
	"context"
	"time"
)

// Severity 事件严重级别。与 slog.Level 语义对齐，便于日志系统二次接入。
type Severity string

const (
	SeverityDebug Severity = "debug"
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Event 是单条结构化事件。所有字段都有 JSON tag，可被 Sink 直接 marshal。
//
// Data 是任意 key → value（值可序列化为 JSON）；用 map 而非 typed struct 是为了
// 兼容现有点位（trace_id、tool_name、duration_ms 等异质字段不必引入新类型）。
type Event struct {
	Type      string         `json:"type"`              // 事件类型，如 "tool.call.completed"、"llm.failover"
	Severity  Severity       `json:"severity"`          // 严重级别
	Source    string         `json:"source"`            // 事件来源模块，如 "engine.tool_executor"
	Timestamp time.Time      `json:"timestamp"`         // 发出时刻（UTC）
	TraceID   string         `json:"trace_id,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// New 构造一个 Event 并填充 Timestamp。
func New(eventType string, severity Severity) Event {
	return Event{
		Type:      eventType,
		Severity:  severity,
		Timestamp: time.Now().UTC(),
		Data:      map[string]any{},
	}
}

// With 为 Event.Data 设置一个键值对，并返回 Event 自身（方便链式构造）。
//
//	evt := events.New("tool.call.completed", events.SeverityInfo).
//	    With("tool", "shell").
//	    With("duration_ms", 142)
func (e Event) With(key string, value any) Event {
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	e.Data[key] = value
	return e
}

// WithSource 链式设置 Source。
func (e Event) WithSource(src string) Event {
	e.Source = src
	return e
}

// WithTrace 链式设置 TraceID / SessionID。
func (e Event) WithTrace(traceID, sessionID string) Event {
	e.TraceID = traceID
	e.SessionID = sessionID
	return e
}

// 校验函数 —— 保证 Sink 投递的事件至少含 Type；缺 Type 视作错误。
func (e Event) valid() bool {
	return e.Type != ""
}

// ctxEmitterKey 是 ctx 中 Emitter 实例的私有 key。
type ctxEmitterKey struct{}

// WithEmitter 在 ctx 中注入 Emitter，供调用栈底部的 Emit() 抓取。
func WithEmitter(ctx context.Context, e *Emitter) context.Context {
	if e == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxEmitterKey{}, e)
}

// emitterFromCtx 从 ctx 取出 Emitter，未注入时返回 nil（Emit 此时退化为 no-op）。
func emitterFromCtx(ctx context.Context) *Emitter {
	if ctx == nil {
		return nil
	}
	if e, ok := ctx.Value(ctxEmitterKey{}).(*Emitter); ok {
		return e
	}
	return nil
}
