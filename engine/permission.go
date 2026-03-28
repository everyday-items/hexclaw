package engine

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// PermissionRequest is sent to the frontend for user approval.
type PermissionRequest struct {
	ID        string         `json:"id"`
	ToolName  string         `json:"tool_name"`
	Arguments map[string]any `json:"arguments"`
	Risk      string         `json:"risk"` // "safe" | "sensitive" | "dangerous"
	Reason    string         `json:"reason"`
}

// PermissionResponse is the user's decision.
type PermissionResponse struct {
	RequestID string `json:"request_id"`
	Approved  bool   `json:"approved"`
	Remember  bool   `json:"remember"` // "always allow this tool" for session
}

// PermissionSender pushes approval requests to the frontend.
// Implemented by WebAdapter or CLI adapter.
type PermissionSender interface {
	SendPermissionRequest(ctx context.Context, sessionID string, req *PermissionRequest) error
}

// PermissionHub manages pending approval requests and their responses.
type PermissionHub struct {
	mu       sync.Mutex
	pending  map[string]chan PermissionResponse // requestID → response channel
	allowed  map[string]map[string]bool         // sessionID → set of always-allowed tool names
	sender   PermissionSender
	timeout  time.Duration
}

// NewPermissionHub creates a permission hub.
func NewPermissionHub(timeout time.Duration) *PermissionHub {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &PermissionHub{
		pending: make(map[string]chan PermissionResponse),
		allowed: make(map[string]map[string]bool),
		timeout: timeout,
	}
}

// SetSender sets the adapter that can push messages to the frontend.
func (h *PermissionHub) SetSender(s PermissionSender) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sender = s
}

// ClearSession removes all remembered permissions for a session.
// Should be called when a session is deleted or user disconnects.
func (h *PermissionHub) ClearSession(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.allowed, sessionID)
}

// RequestApproval sends an approval request and blocks until the user responds or timeout.
func (h *PermissionHub) RequestApproval(ctx context.Context, sessionID string, req *PermissionRequest) (bool, error) {
	// Check if this tool is already allowed for this session
	h.mu.Lock()
	if tools, ok := h.allowed[sessionID]; ok && tools[req.ToolName] {
		h.mu.Unlock()
		return true, nil
	}

	if h.sender == nil {
		h.mu.Unlock()
		// No frontend connected — use default policy (deny)
		log.Printf("[permission] no sender available, denying %s", req.ToolName)
		return false, nil
	}

	ch := make(chan PermissionResponse, 1)
	h.pending[req.ID] = ch
	sender := h.sender
	h.mu.Unlock()

	// Send request to frontend
	if err := sender.SendPermissionRequest(ctx, sessionID, req); err != nil {
		h.mu.Lock()
		delete(h.pending, req.ID)
		h.mu.Unlock()
		return false, fmt.Errorf("failed to send permission request: %w", err)
	}

	// Wait for response
	select {
	case resp := <-ch:
		if resp.Remember && resp.Approved {
			h.mu.Lock()
			if h.allowed[sessionID] == nil {
				h.allowed[sessionID] = make(map[string]bool)
			}
			h.allowed[sessionID][req.ToolName] = true
			h.mu.Unlock()
		}
		return resp.Approved, nil
	case <-time.After(h.timeout):
		h.mu.Lock()
		delete(h.pending, req.ID)
		h.mu.Unlock()
		return false, fmt.Errorf("permission request timed out after %v", h.timeout)
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.pending, req.ID)
		h.mu.Unlock()
		return false, ctx.Err()
	}
}

// HandleResponse is called when the frontend sends back an approval decision.
func (h *PermissionHub) HandleResponse(resp PermissionResponse) {
	h.mu.Lock()
	ch, ok := h.pending[resp.RequestID]
	if ok {
		delete(h.pending, resp.RequestID)
	}
	h.mu.Unlock()

	if ok {
		ch <- resp
	}
}

// PermissionHook is a BeforeToolHook that asks for user approval on sensitive/dangerous tools.
type PermissionHook struct {
	hub             *PermissionHub
	dangerousTools  map[string]bool // tools that always require approval
	sensitiveTools  map[string]bool // tools that require approval on first use
}

// NewPermissionHook creates a permission hook.
func NewPermissionHook(hub *PermissionHub) *PermissionHook {
	return &PermissionHook{
		hub: hub,
		dangerousTools: map[string]bool{
			"shell":     true,
			"code":      true,
			"code_exec": true,
		},
		sensitiveTools: map[string]bool{
			"browser":            true,
			"create_skill":       true,
			"manage_mcp_server":  true,
		},
	}
}

func (h *PermissionHook) BeforeToolCall(ctx context.Context, call *ToolCallInfo) error {
	risk := h.classifyRisk(call.Name)
	if risk == "safe" {
		return nil
	}

	sessionID, _ := ctx.Value("session_id").(string)
	if sessionID == "" {
		// No session context — deny dangerous, allow sensitive
		if risk == "dangerous" {
			return fmt.Errorf("tool %q requires approval but no session context", call.Name)
		}
		return nil
	}

	reqID := fmt.Sprintf("perm-%s-%d", call.Name, time.Now().UnixNano())
	req := &PermissionRequest{
		ID:        reqID,
		ToolName:  call.Name,
		Arguments: call.Arguments,
		Risk:      risk,
		Reason:    fmt.Sprintf("Agent wants to execute %s(%s)", call.Name, summarizeArgs(call.Arguments)),
	}

	approved, err := h.hub.RequestApproval(ctx, sessionID, req)
	if err != nil {
		log.Printf("[permission] approval error for %s: %v", call.Name, err)
		return fmt.Errorf("tool %q: approval failed: %w", call.Name, err)
	}
	if !approved {
		return fmt.Errorf("tool %q: user denied execution", call.Name)
	}
	return nil
}

func (h *PermissionHook) classifyRisk(toolName string) string {
	if h.dangerousTools[toolName] {
		return "dangerous"
	}
	if h.sensitiveTools[toolName] {
		return "sensitive"
	}
	return "safe"
}

func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 50 {
			s = s[:47] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
	}
	return strings.Join(parts, ", ")
}
