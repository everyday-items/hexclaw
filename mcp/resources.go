package mcp

import (
	"context"
	"fmt"
	"log"

	hexagon "github.com/hexagon-codes/hexagon"
)

// Resource represents an MCP Resource (data source).
//
// MCP protocol defines three primitives: Tools, Resources, Prompts.
// Resources are data sources the LLM can read (files, databases, APIs).
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// Prompt represents an MCP Prompt (parameterized template).
//
// Prompts are reusable prompt templates with parameters.
type Prompt struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Arguments   []PromptArg    `json:"arguments,omitempty"`
}

// PromptArg is a prompt argument definition.
type PromptArg struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// PromptMessage is a resolved prompt message.
type PromptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ListResources returns all resources from all connected MCP servers.
func (m *Manager) ListResources(ctx context.Context) []Resource {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var resources []Resource
	for _, server := range m.servers {
		if !server.connected {
			continue
		}
		// MCP SDK resources are accessed via the session
		// Current hexagon SDK exposes tools only; resources need session-level access
		// This is a placeholder for when the SDK adds resource support
		_ = server
	}
	return resources
}

// ReadResource reads a specific resource by URI.
func (m *Manager) ReadResource(ctx context.Context, uri string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Find which server owns this resource
	for _, server := range m.servers {
		if !server.connected {
			continue
		}
		// Placeholder: SDK integration needed
		_ = server
	}
	return "", fmt.Errorf("resource %q not found", uri)
}

// ListPrompts returns all prompts from all connected MCP servers.
func (m *Manager) ListPrompts(ctx context.Context) []Prompt {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var prompts []Prompt
	for _, server := range m.servers {
		if !server.connected {
			continue
		}
		_ = server
	}
	return prompts
}

// GetPrompt resolves a prompt with arguments and returns messages.
func (m *Manager) GetPrompt(ctx context.Context, name string, args map[string]string) ([]PromptMessage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, server := range m.servers {
		if !server.connected {
			continue
		}
		_ = server
	}
	return nil, fmt.Errorf("prompt %q not found", name)
}

// InjectResourceContext adds available resources as context to the system prompt.
// Called during buildStreamMessages to enrich the LLM's knowledge.
func (m *Manager) InjectResourceContext(ctx context.Context) string {
	resources := m.ListResources(ctx)
	if len(resources) == 0 {
		return ""
	}

	result := "\n[Available MCP Resources]\n"
	for _, r := range resources {
		result += fmt.Sprintf("- %s (%s): %s\n", r.Name, r.URI, r.Description)
	}
	return result
}

// Suppress unused import warning for hexagon
var _ = hexagon.ConnectMCPStreamable
func init() { _ = log.Println }
