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
	mcpHub    *hub.McpHub
	mcpMgr    *mcp.Manager
	cfgWriter *config.Writer
}

// NewMcpInstallerSkill creates a new McpInstallerSkill.
func NewMcpInstallerSkill(mcpHub *hub.McpHub, mcpMgr *mcp.Manager, cfgWriter *config.Writer) *McpInstallerSkill {
	return &McpInstallerSkill{
		mcpHub:    mcpHub,
		mcpMgr:    mcpMgr,
		cfgWriter: cfgWriter,
	}
}

func (m *McpInstallerSkill) Name() string        { return "manage_mcp_server" }
func (m *McpInstallerSkill) Description() string  { return "Search, install, or remove MCP servers from HexClaw Hub" }
func (m *McpInstallerSkill) Match(_ string) bool  { return false }

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
		// 安全校验: 只允许已知安全的 MCP 启动命令 + 参数验证
		if !isSafeMCPCommand(entry.Command) {
			return nil, fmt.Errorf("MCP server '%s' uses untrusted command %q (allowed: npx, uvx, node, python3, python, deno)", keyword, entry.Command)
		}
		if err := validateMCPArgs(entry.Args); err != nil {
			return nil, fmt.Errorf("MCP server '%s' has unsafe args: %w", keyword, err)
		}
		cfg := mcp.ServerConfig{
			Name:      entry.Name,
			Transport: "stdio",
			Command:   entry.Command,
			Args:      entry.Args,
			Enabled:   true,
		}
		if err := m.mcpMgr.AddServer(ctx, cfg); err != nil {
			return nil, fmt.Errorf("failed to add MCP server: %w", err)
		}
		// Persist to config file so it survives restart
		if m.cfgWriter != nil {
			if err := m.cfgWriter.AppendMCPServer(entry.Name, "stdio", entry.Command, entry.Args, ""); err != nil {
				// Non-fatal: server is running but won't persist
				return &skill.Result{
					Content: fmt.Sprintf("MCP server '%s' installed (running), but failed to persist config: %v. Will be lost on restart.", entry.Name, err),
				}, nil
			}
		}
		desc := entry.Description
		if entry.ConfigHint != "" {
			desc += fmt.Sprintf("\nNote: %s", entry.ConfigHint)
		}
		return &skill.Result{Content: fmt.Sprintf("MCP server '%s' installed and running. %s", entry.Name, desc)}, nil

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

// isSafeMCPCommand checks if the MCP server command is in the trusted whitelist.
// Only well-known package runners and interpreters are allowed.
var safeMCPCommands = map[string]bool{
	"npx": true, "uvx": true, "node": true, "deno": true,
	"python": true, "python3": true, "bun": true, "bunx": true,
}

func isSafeMCPCommand(cmd string) bool {
	// Extract base command name (handle paths like /usr/bin/npx)
	parts := strings.Split(cmd, "/")
	base := parts[len(parts)-1]
	return safeMCPCommands[base]
}

// validateMCPArgs checks for dangerous argument patterns.
func validateMCPArgs(args []string) error {
	for _, arg := range args {
		lower := strings.ToLower(arg)
		// Block eval/exec flags
		if arg == "-e" || arg == "--eval" || strings.HasPrefix(lower, "-e ") {
			return fmt.Errorf("eval flag not allowed in MCP server args")
		}
		// Block shell injection via semicolons or pipes
		if strings.ContainsAny(arg, ";|&`$") {
			return fmt.Errorf("shell metacharacters not allowed in MCP server args")
		}
	}
	return nil
}
