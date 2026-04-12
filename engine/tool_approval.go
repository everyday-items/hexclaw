package engine

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ApprovalRequest is sent to the user when a sensitive/dangerous tool is about to execute.
type ApprovalRequest struct {
	ToolName  string         `json:"tool_name"`
	Arguments map[string]any `json:"arguments,omitempty"`
	RiskLevel string         `json:"risk_level"` // "sensitive" or "dangerous"
	SessionID string         `json:"session_id"`
}

// ApprovalResponse is the user's decision.
type ApprovalResponse struct {
	Approved    bool   `json:"approved"`
	AlwaysAllow bool   `json:"always_allow"` // remember for this session
	Reason      string `json:"reason,omitempty"`
}

// ApprovalCallback is called to ask the user for approval.
type ApprovalCallback func(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error)

// RiskClassifier classifies a tool's risk level ("safe" / "sensitive" / "dangerous").
type RiskClassifier func(toolName string) string

// ToolApprovalGate implements BeforeToolHook, gating sensitive/dangerous tool execution
// on interactive user approval via WebSocket or callback.
type ToolApprovalGate struct {
	callback   ApprovalCallback
	timeout    time.Duration
	classifier RiskClassifier

	mu            sync.RWMutex
	alwaysAllowed map[string]map[string]bool // sessionID -> toolName -> allowed
}

// Compile-time check that ToolApprovalGate implements BeforeToolHook.
var _ BeforeToolHook = (*ToolApprovalGate)(nil)

// NewToolApprovalGate creates a new approval gate.
func NewToolApprovalGate(callback ApprovalCallback, timeout time.Duration) *ToolApprovalGate {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &ToolApprovalGate{
		callback:      callback,
		timeout:       timeout,
		alwaysAllowed: make(map[string]map[string]bool),
	}
}

// SetClassifier sets a custom risk classifier (e.g. from MCP annotations).
func (g *ToolApprovalGate) SetClassifier(c RiskClassifier) {
	g.classifier = c
}

func (g *ToolApprovalGate) classifyRisk(toolName string) string {
	if g.classifier != nil {
		return g.classifier(toolName)
	}
	switch toolName {
	case "shell", "code", "code_exec":
		return "dangerous"
	case "browser", "create_skill", "manage_mcp_server", "file_edit":
		return "sensitive"
	default:
		return "safe"
	}
}

// BeforeToolCall implements BeforeToolHook.
func (g *ToolApprovalGate) BeforeToolCall(ctx context.Context, call *ToolCallInfo) error {
	risk := g.classifyRisk(call.Name)
	if risk == "safe" {
		return nil
	}

	sessionID, _ := ctx.Value(ctxKeySessionID).(string)

	// Check session-level "always allow" (skip for anonymous sessions to prevent shared state)
	if sessionID != "" && g.isAlwaysAllowed(sessionID, call.Name) {
		return nil
	}

	// Ask user with timeout
	reqCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	req := ApprovalRequest{
		ToolName:  call.Name,
		Arguments: call.Arguments,
		RiskLevel: risk,
		SessionID: sessionID,
	}

	resp, err := g.callback(reqCtx, req)
	if err != nil {
		return fmt.Errorf("tool %q approval failed: %w", call.Name, err)
	}

	if !resp.Approved {
		reason := resp.Reason
		if reason == "" {
			reason = "user denied"
		}
		return fmt.Errorf("tool %q blocked by user: %s", call.Name, reason)
	}

	if resp.AlwaysAllow && sessionID != "" {
		g.setAlwaysAllowed(sessionID, call.Name)
	}

	return nil
}

// ClearSession removes all remembered approvals for a session.
func (g *ToolApprovalGate) ClearSession(sessionID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.alwaysAllowed, sessionID)
}

func (g *ToolApprovalGate) isAlwaysAllowed(sessionID, toolName string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if m, ok := g.alwaysAllowed[sessionID]; ok {
		return m[toolName]
	}
	return false
}

func (g *ToolApprovalGate) setAlwaysAllowed(sessionID, toolName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.alwaysAllowed[sessionID] == nil {
		g.alwaysAllowed[sessionID] = make(map[string]bool)
	}
	g.alwaysAllowed[sessionID][toolName] = true
}
