package engine

import (
	"context"
	"fmt"
	"log"

	"github.com/hexagon-codes/hexclaw/security"
)

// ToolCallInfo holds metadata about a tool call for hooks
type ToolCallInfo struct {
	Name      string
	Arguments map[string]any
	Source    string // "skill" | "mcp" | "chain"
}

// ToolCallResult holds the result of a tool call for hooks
type ToolCallResult struct {
	Content string
	Error   error
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

func (h *AuditHook) BeforeToolCall(_ context.Context, call *ToolCallInfo) error {
	log.Printf("[audit] tool_call: name=%s source=%s", call.Name, call.Source)
	return nil
}

func (h *AuditHook) AfterToolCall(_ context.Context, call *ToolCallInfo, result *ToolCallResult) {
	status := "success"
	if result.Error != nil {
		status = "error"
	}
	log.Printf("[audit] tool_result: name=%s status=%s content_len=%d", call.Name, status, len(result.Content))
}

// TruncateHook truncates long tool results.
// Strategy: keep first 60% + last 20%, remove middle 20%.
type TruncateHook struct {
	MaxChars int // default 8000
}

func (h *TruncateHook) AfterToolCall(_ context.Context, _ *ToolCallInfo, result *ToolCallResult) {
	if result.Error != nil {
		return
	}
	maxChars := h.MaxChars
	if maxChars <= 0 {
		maxChars = 8000
	}
	if len(result.Content) <= maxChars {
		return
	}
	headLen := maxChars * 60 / 100
	tailLen := maxChars * 20 / 100
	head := result.Content[:headLen]
	tail := result.Content[len(result.Content)-tailLen:]
	truncated := len(result.Content) - headLen - tailLen
	result.Content = fmt.Sprintf("%s\n\n...[truncated %d characters]...\n\n%s", head, truncated, tail)
}

// SanitizeHook cleans tool output to defend against indirect prompt injection.
// Should be the LAST AfterToolHook in the chain (after Truncate).
type SanitizeHook struct{}

func (h *SanitizeHook) AfterToolCall(_ context.Context, call *ToolCallInfo, result *ToolCallResult) {
	if result.Error != nil || result.Content == "" {
		return
	}
	// Only sanitize external sources (browser, MCP) — not internal skills
	if call.Source == "mcp" || call.Name == "browser" {
		result.Content = security.SanitizeToolOutput(result.Content, 0)
	}
}
