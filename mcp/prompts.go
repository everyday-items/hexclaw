package mcp

import (
	"context"
	"fmt"
)

// Prompt represents an MCP Prompt — a parameterized template that servers expose.
type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptArgument defines a parameter accepted by a Prompt.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// PromptMessage is a resolved prompt message returned by prompts/get.
type PromptMessage struct {
	Role    string        `json:"role"`
	Content PromptContent `json:"content"`
}

// PromptContent holds the content of a prompt message.
type PromptContent struct {
	Type        string `json:"type"`                   // "text" or "resource"
	Text        string `json:"text,omitempty"`         // for type="text"
	ResourceURI string `json:"resource_uri,omitempty"` // for type="resource"
}

// GetPromptResult is the response from prompts/get.
type GetPromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// PromptInfo is lightweight metadata for API/frontend display.
type PromptInfo struct {
	Server      string `json:"server"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ArgCount    int    `json:"arg_count"`
}

// ListPrompts returns prompts from all connected MCP servers.
// Current SDK integration is placeholder — will be enriched when hexagon SDK
// adds prompts/list support.
func (m *Manager) ListPrompts(ctx context.Context) []Prompt {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var prompts []Prompt
	for _, server := range m.servers {
		if !server.connected {
			continue
		}
		// Placeholder: hexagon SDK currently only exposes Tools.
		// When session-level prompts/list is available, call it here.
		_ = server
	}
	return prompts
}

// GetPrompt resolves a prompt template with the given arguments.
func (m *Manager) GetPrompt(ctx context.Context, name string, args map[string]string) (*GetPromptResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, server := range m.servers {
		if !server.connected {
			continue
		}
		// Placeholder: call prompts/get when SDK supports it.
		_ = server
	}
	return nil, fmt.Errorf("prompt %q not found", name)
}

// AllPromptInfos returns lightweight info for all prompts across connected servers.
func (m *Manager) AllPromptInfos(ctx context.Context) []PromptInfo {
	m.mu.RLock()
	names := make([]string, 0, len(m.servers))
	for name, srv := range m.servers {
		if srv.connected {
			names = append(names, name)
		}
	}
	m.mu.RUnlock()

	var infos []PromptInfo
	// When ListPrompts gains per-server support, iterate names here.
	// For now, use the aggregate list and leave Server empty until SDK
	// exposes prompts/list per connection.
	for _, sn := range names {
		_ = sn // will populate PromptInfo.Server when SDK supports it
	}
	prompts := m.ListPrompts(ctx)
	for _, p := range prompts {
		infos = append(infos, PromptInfo{
			Name:        p.Name,
			Description: p.Description,
			ArgCount:    len(p.Arguments),
		})
	}
	return infos
}
