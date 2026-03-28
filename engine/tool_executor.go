package engine

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/skill"
)

// ToolExecutor executes tool calls with hook chain support.
//
// Resolution order: Skill registry → MCP Manager.
// Hook chain: beforeHooks → execute → afterHooks.
type ToolExecutor struct {
	skills      *skill.DefaultRegistry
	mcpMgr      *mcp.Manager
	beforeHooks []BeforeToolHook
	afterHooks  []AfterToolHook
}

// NewToolExecutor creates a new executor.
func NewToolExecutor(skills *skill.DefaultRegistry, mcpMgr *mcp.Manager) *ToolExecutor {
	return &ToolExecutor{skills: skills, mcpMgr: mcpMgr}
}

// AddHook adds a hook that implements BeforeToolHook and/or AfterToolHook.
func (e *ToolExecutor) AddHook(h any) {
	if bh, ok := h.(BeforeToolHook); ok {
		e.beforeHooks = append(e.beforeHooks, bh)
	}
	if ah, ok := h.(AfterToolHook); ok {
		e.afterHooks = append(e.afterHooks, ah)
	}
}

// Execute runs a tool call through the hook chain.
//
//  1. Find the tool (Skill first, then MCP)
//  2. Run beforeHooks (any can abort by returning error)
//  3. Execute the tool
//  4. Run afterHooks (can modify result, e.g. truncation)
func (e *ToolExecutor) Execute(ctx context.Context, toolName string, args map[string]any) (string, error) {
	call := &ToolCallInfo{
		Name:      toolName,
		Arguments: args,
	}

	// 1. Try Skill registry
	if e.skills != nil {
		if s, ok := e.skills.Get(toolName); ok {
			call.Source = "skill"
			return e.executeWithHooks(ctx, call, func(ctx context.Context) (string, error) {
				result, err := s.Execute(ctx, args)
				if err != nil {
					return "", err
				}
				return result.Content, nil
			})
		}
	}

	// 2. Try MCP Manager
	if e.mcpMgr != nil {
		call.Source = "mcp"
		return e.executeWithHooks(ctx, call, func(ctx context.Context) (string, error) {
			return e.mcpMgr.CallTool(ctx, toolName, args)
		})
	}

	return "", fmt.Errorf("tool '%s' not found in skills or MCP servers", toolName)
}

// HasTool checks whether a tool exists in any source.
func (e *ToolExecutor) HasTool(toolName string) bool {
	if e.skills != nil {
		if _, ok := e.skills.Get(toolName); ok {
			return true
		}
	}
	if e.mcpMgr != nil {
		for _, info := range e.mcpMgr.ListToolInfos() {
			if info.Name == toolName {
				return true
			}
		}
	}
	return false
}

func (e *ToolExecutor) executeWithHooks(ctx context.Context, call *ToolCallInfo, exec func(context.Context) (string, error)) (string, error) {
	// Before hooks
	for _, h := range e.beforeHooks {
		if err := h.BeforeToolCall(ctx, call); err != nil {
			return "", fmt.Errorf("tool call blocked by hook: %w", err)
		}
	}

	// Execute
	content, err := exec(ctx)
	result := &ToolCallResult{Content: content, Error: err}

	// After hooks (run even on error for audit)
	for _, h := range e.afterHooks {
		h.AfterToolCall(ctx, call, result)
	}

	return result.Content, result.Error
}
