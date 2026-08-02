package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/hub"
)

// McpInstallerSkill allows the LLM to search, install, remove, and list MCP servers from Hub.
type McpInstallerSkill struct {
	mcpHub    mcpCatalog
	mcpMgr    *mcp.Manager
	cfgWriter *config.Writer
}

// mcpCatalog keeps the installer on the narrow read boundary it consumes.
// Hub admission validates remote/cache snapshots first; the installer repeats
// pinned-artifact validation as defense in depth at the execution boundary.
type mcpCatalog interface {
	Search(string) []hub.McpServerMeta
	Get(string) (*hub.McpServerMeta, error)
}

// NewMcpInstallerSkill creates a new McpInstallerSkill.
func NewMcpInstallerSkill(mcpHub *hub.McpHub, mcpMgr *mcp.Manager, cfgWriter *config.Writer) *McpInstallerSkill {
	return newMcpInstallerSkill(mcpHub, mcpMgr, cfgWriter)
}

func newMcpInstallerSkill(mcpHub mcpCatalog, mcpMgr *mcp.Manager, cfgWriter *config.Writer) *McpInstallerSkill {
	return &McpInstallerSkill{
		mcpHub:    mcpHub,
		mcpMgr:    mcpMgr,
		cfgWriter: cfgWriter,
	}
}

func (m *McpInstallerSkill) Name() string { return "manage_mcp_server" }
func (m *McpInstallerSkill) Description() string {
	return "Search, install, or remove MCP servers from HexClaw Hub"
}
func (m *McpInstallerSkill) Match(_ string) bool { return false }

func (m *McpInstallerSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("manage_mcp_server",
		"Search, install, or remove MCP servers from HexClaw Hub. Use when the user asks to add/remove an MCP server or tool integration.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"action":  {Type: "string", Description: "Action to perform: search, install, remove, list"},
				"keyword": {Type: "string", Description: "Search keyword or MCP server name (required for search/install/remove)"},
			},
			Required: []string{"action"},
		})
}

func (m *McpInstallerSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	action, _ := args["action"].(string)
	keyword, _ := args["keyword"].(string)

	switch action {
	case "search":
		if keyword == "" {
			return nil, fmt.Errorf("keyword is required for search")
		}
		results := m.mcpHub.Search(keyword)
		if len(results) == 0 {
			return &skill.Result{Content: fmt.Sprintf("No MCP servers found for '%s'", keyword)}, nil
		}
		return &skill.Result{Content: formatMcpSearchResults(results)}, nil

	case "install":
		if keyword == "" {
			return nil, fmt.Errorf("keyword (server name) is required for install")
		}
		entry, err := m.mcpHub.Get(keyword)
		if err != nil {
			return nil, fmt.Errorf("MCP server '%s' not found in Hub: %w", keyword, err)
		}
		validated, err := hub.ValidatePinnedMCPServer(*entry)
		if err != nil {
			return nil, fmt.Errorf("MCP server '%s' failed pinned artifact validation: %w", keyword, err)
		}
		cfg := mcp.ServerConfig{
			Name:      validated.Name(),
			Transport: "stdio",
			Command:   validated.Command(),
			Args:      validated.Args(),
			Enabled:   true,
		}
		if err := m.mcpMgr.AddServer(ctx, cfg); err != nil {
			return nil, fmt.Errorf("failed to add MCP server: %w", err)
		}
		// Persist to config file so it survives restart
		if m.cfgWriter != nil {
			if err := m.cfgWriter.AppendMCPServer(validated.Name(), "stdio", validated.Command(), validated.Args(), nil, ""); err != nil {
				// Non-fatal: server is running but won't persist
				return &skill.Result{
					Content: fmt.Sprintf("MCP server '%s' installed (running), but failed to persist config: %v. Will be lost on restart.", validated.Name(), err),
				}, nil
			}
		}
		desc := validated.Description()
		if validated.ConfigHint() != "" {
			desc += fmt.Sprintf("\nNote: %s", validated.ConfigHint())
		}
		return &skill.Result{Content: fmt.Sprintf("MCP server '%s' installed and running. %s", validated.Name(), desc)}, nil

	case "remove":
		if keyword == "" {
			return nil, fmt.Errorf("keyword (server name) is required for remove")
		}
		if err := m.mcpMgr.RemoveServer(keyword); err != nil {
			return nil, fmt.Errorf("failed to remove MCP server: %w", err)
		}
		if m.cfgWriter != nil {
			_ = m.cfgWriter.RemoveMCPServer(keyword)
		}
		return &skill.Result{Content: fmt.Sprintf("MCP server '%s' removed.", keyword)}, nil

	case "list":
		infos := m.mcpMgr.ToolInfos()
		if len(infos) == 0 {
			return &skill.Result{Content: "No MCP servers currently running."}, nil
		}
		servers := make(map[string][]string)
		for _, info := range infos {
			servers[info.ServerName] = append(servers[info.ServerName], info.Name)
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Running MCP servers (%d):\n", len(servers)))
		for name, tools := range servers {
			sb.WriteString(fmt.Sprintf("  - %s (%d tools)\n", name, len(tools)))
		}
		return &skill.Result{Content: sb.String()}, nil

	default:
		return nil, fmt.Errorf("unknown action %q: use search, install, remove, or list", action)
	}
}

func formatMcpSearchResults(results []hub.McpServerMeta) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d MCP server(s):\n", len(results)))
	for i, r := range results {
		if i >= 10 {
			sb.WriteString(fmt.Sprintf("... and %d more\n", len(results)-10))
			break
		}
		sb.WriteString(fmt.Sprintf("  %d. %s — %s", i+1, r.Name, r.Description))
		if r.Category != "" {
			sb.WriteString(fmt.Sprintf(" [%s]", r.Category))
		}
		if r.ConfigHint != "" {
			sb.WriteString(fmt.Sprintf(" (requires: %s)", r.ConfigHint))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
