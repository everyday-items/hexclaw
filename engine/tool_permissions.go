package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// ToolPermissions manages per-tool allow/deny rules.
//
// Rules use glob patterns matching tool names:
//
//	allow: ["search", "weather", "translate"]
//	deny:  ["mcp:*:exec_*", "shell", "code"]
//
// Evaluation order: deny takes precedence over allow.
// If no rules match, the tool is allowed by default.
type ToolPermissions struct {
	mu       sync.RWMutex
	allow    []string // glob patterns
	deny     []string // glob patterns
	sessions map[string]*sessionOverride
}

type sessionOverride struct {
	disabled map[string]bool // tool names disabled for this session
}

// NewToolPermissions creates a permission checker with global rules.
func NewToolPermissions(allow, deny []string) *ToolPermissions {
	return &ToolPermissions{
		allow:    allow,
		deny:     deny,
		sessions: make(map[string]*sessionOverride),
	}
}

// Check returns nil if the tool is permitted, error otherwise.
func (p *ToolPermissions) Check(toolName string, sessionID string) error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// Session-level override (highest priority)
	if sessionID != "" {
		if so, ok := p.sessions[sessionID]; ok {
			if so.disabled[toolName] {
				return fmt.Errorf("tool %q is disabled for this session", toolName)
			}
		}
	}

	// Deny rules (deny takes precedence)
	for _, pattern := range p.deny {
		if matchGlob(pattern, toolName) {
			return fmt.Errorf("tool %q is denied by rule: %s", toolName, pattern)
		}
	}

	// If allow rules exist, tool must match at least one
	if len(p.allow) > 0 {
		for _, pattern := range p.allow {
			if matchGlob(pattern, toolName) {
				return nil
			}
		}
		return fmt.Errorf("tool %q not in allow list", toolName)
	}

	return nil // No rules → allowed
}

// DisableForSession disables a tool for a specific session.
func (p *ToolPermissions) DisableForSession(sessionID, toolName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions[sessionID] == nil {
		p.sessions[sessionID] = &sessionOverride{disabled: make(map[string]bool)}
	}
	p.sessions[sessionID].disabled[toolName] = true
}

// ClearSession removes session-level overrides.
func (p *ToolPermissions) ClearSession(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, sessionID)
}

func matchGlob(pattern, name string) bool {
	if pattern == name {
		return true
	}
	// Use filepath.Match for glob matching (supports * and ?)
	if ok, _ := filepath.Match(pattern, name); ok {
		return true
	}
	// Handle "mcp:*:exec_*" style patterns with : separator
	if strings.Contains(pattern, ":") || strings.Contains(name, ":") {
		if ok, _ := filepath.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

// ToolPermissionHook is a BeforeToolHook that enforces per-tool permissions.
type ToolPermissionHook struct {
	perms *ToolPermissions
}

// NewToolPermissionHook creates a hook that enforces tool permissions.
func NewToolPermissionHook(perms *ToolPermissions) *ToolPermissionHook {
	return &ToolPermissionHook{perms: perms}
}

func (h *ToolPermissionHook) BeforeToolCall(ctx context.Context, call *ToolCallInfo) error {
	sessionID, _ := ctx.Value("session_id").(string)
	return h.perms.Check(call.Name, sessionID)
}
