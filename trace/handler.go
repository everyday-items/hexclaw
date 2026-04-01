package trace

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// Collector 是 LogCollector 需要实现的接口（避免循环依赖 trace → api）。
type Collector interface {
	AddTrace(level, source, message, traceID string, fields map[string]any)
}

// CollectorHandler 是 slog.Handler 的实现，将结构化日志写入 LogCollector。
//
// 职责：
//   - 把 slog 的 Attr 转换为 LogCollector 的 fields map
//   - 从 attrs 中提取 trace_id 填入 LogEntry.TraceID
//   - source 默认为 "trace"，可通过 WithSource 覆盖
type CollectorHandler struct {
	collector Collector
	attrs     []slog.Attr
	groups    []string
	level     slog.Leveler
}

// NewCollectorHandler 创建桥接 slog → LogCollector 的 Handler。
func NewCollectorHandler(c Collector, level slog.Leveler) *CollectorHandler {
	if level == nil {
		level = slog.LevelInfo
	}
	return &CollectorHandler{collector: c, level: level}
}

func (h *CollectorHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *CollectorHandler) Handle(_ context.Context, r slog.Record) error {
	fields := make(map[string]any, r.NumAttrs()+len(h.attrs))
	var traceID string
	source := "trace"

	prefix := ""
	if len(h.groups) > 0 {
		prefix = strings.Join(h.groups, ".") + "."
	}

	collect := func(key string, val any) {
		switch key {
		case "trace_id":
			traceID = fmt.Sprintf("%v", val)
		case "source":
			source = fmt.Sprintf("%v", val)
		default:
			fields[key] = val
		}
	}

	// 先写 handler 级别的 attrs（With 传入的）
	for _, a := range h.attrs {
		flattenAttr(a, prefix, collect)
	}

	// 再写 record 级别的 attrs
	r.Attrs(func(a slog.Attr) bool {
		flattenAttr(a, prefix, collect)
		return true
	})

	level := strings.ToLower(r.Level.String())

	if len(fields) == 0 {
		fields = nil
	}
	h.collector.AddTrace(level, source, r.Message, traceID, fields)
	return nil
}

// flattenAttr 递归展平 slog.Attr，正确处理 inline Group attrs
func flattenAttr(a slog.Attr, prefix string, fn func(key string, val any)) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p = prefix + a.Key + "."
		}
		for _, ga := range a.Value.Group() {
			flattenAttr(ga, p, fn)
		}
		return
	}
	if a.Key == "" {
		return // 忽略空 key（slog 规范：空 key 的 attr 应被丢弃）
	}
	fn(prefix+a.Key, resolveValue(a.Value))
}

func (h *CollectorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &CollectorHandler{
		collector: h.collector,
		attrs:     append(cloneAttrs(h.attrs), attrs...),
		groups:    h.groups,
		level:     h.level,
	}
}

func (h *CollectorHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &CollectorHandler{
		collector: h.collector,
		attrs:     cloneAttrs(h.attrs),
		groups:    append(append([]string{}, h.groups...), name),
		level:     h.level,
	}
}

func cloneAttrs(src []slog.Attr) []slog.Attr {
	if len(src) == 0 {
		return nil
	}
	dst := make([]slog.Attr, len(src))
	copy(dst, src)
	return dst
}

func resolveValue(v slog.Value) any {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().String()
	default:
		return fmt.Sprintf("%v", v.Any())
	}
}
