package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/security"
)

// ToolCallInfo holds metadata about a tool call for hooks
type ToolCallInfo struct {
	Name      string
	Arguments map[string]any
	Source    string // "skill" | "mcp" | "chain"
}

// ToolCallResult holds the result of a tool call for hooks.
//
// v0.4.0 H1 当 feature flag tool.lifecycle.v2 启用时，StartedAt 与 Duration
// 由 ToolExecutor 自动填充，供 Audit / metrics hook 读取；flag 关闭时这两个字段
// 为零值，调用方按 IsZero() 判定即可，向后兼容。
type ToolCallResult struct {
	Content   string
	Error     error
	StartedAt time.Time     // v0.4.0 H1: tool 调用开始时刻；flag OFF 时为零值
	Duration  time.Duration // v0.4.0 H1: tool 调用耗时；flag OFF 时为 0
}

// BeforeToolHook is called before tool execution.
// Return non-nil error to abort the tool call.
type BeforeToolHook interface {
	BeforeToolCall(ctx context.Context, call *ToolCallInfo) error
}

// AfterToolHook is called after tool execution.
// Can modify the result (e.g., truncation, sanitization).
type AfterToolHook interface {
	AfterToolCall(ctx context.Context, call *ToolCallInfo, result *ToolCallResult)
}

// ============== Built-in Hooks ==============

// AuditHook logs all tool calls for audit trail.
// D24 will add SQLite persistence (gap 1.1 in 19D).
type AuditHook struct{}

// Priority 把 AuditHook 排在 chain 最末（最大数字），让其它 hook 先做完
// 修改/截断/审批，audit 看到最终生效的状态。flag tool.lifecycle.v2 OFF 时不参与排序。
func (h *AuditHook) Priority() int { return 100 }

func (h *AuditHook) BeforeToolCall(ctx context.Context, call *ToolCallInfo) error {
	trace.L(ctx).Info("工具调用", "tool", call.Name, "source", call.Source)
	return nil
}

func (h *AuditHook) AfterToolCall(ctx context.Context, call *ToolCallInfo, result *ToolCallResult) {
	status := "success"
	if result.Error != nil {
		status = "error"
	}
	trace.L(ctx).Info("工具结果", "tool", call.Name, "status", status, "content_len", len(result.Content))
}

// TruncateHook truncates long tool results.
// Strategy: keep first 60% + last 20%, remove middle 20%.
type TruncateHook struct {
	MaxChars int // default 8000
}

// Priority 把 Truncate 排在 Sanitize 之前（小数字先），让 Sanitize 看到已截断
// 内容；继续早于 Audit。
func (h *TruncateHook) Priority() int { return 50 }

func (h *TruncateHook) AfterToolCall(_ context.Context, _ *ToolCallInfo, result *ToolCallResult) {
	if result.Error != nil {
		return
	}
	maxChars := h.MaxChars
	if maxChars <= 0 {
		maxChars = 8000
	}
	// 按 rune 切分，避免在多字节中文/emoji 中间切裂产生 U+FFFD（�）。
	runes := []rune(result.Content)
	if len(runes) <= maxChars {
		return
	}
	headLen := maxChars * 60 / 100
	tailLen := maxChars * 20 / 100
	head := string(runes[:headLen])
	tail := string(runes[len(runes)-tailLen:])
	truncated := len(runes) - headLen - tailLen
	result.Content = fmt.Sprintf("%s\n\n...[truncated %d characters]...\n\n%s", head, truncated, tail)
}

// SanitizeHook cleans tool output to defend against indirect prompt injection.
// Should be the LAST AfterToolHook in the chain (after Truncate).
type SanitizeHook struct{}

// Priority 排 Truncate 之后、Audit 之前。
func (h *SanitizeHook) Priority() int { return 80 }

func (h *SanitizeHook) AfterToolCall(_ context.Context, call *ToolCallInfo, result *ToolCallResult) {
	if result.Error != nil || result.Content == "" {
		return
	}
	// Only sanitize external sources (browser, MCP) — not internal skills
	if call.Source == "mcp" || call.Name == "browser" {
		result.Content = security.SanitizeToolOutput(result.Content, 0)
	}
}
