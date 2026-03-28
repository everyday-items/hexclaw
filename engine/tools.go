package engine

import (
	"sort"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/skill"
)

// ToolCollector gathers tool definitions from multiple sources.
//
// Sources (priority order):
//  1. Builtin + marketplace Skills (skill.DefaultRegistry)
//  2. MCP server tools (mcp.Manager)
//
// Deduplication: first source wins (Skill overrides MCP with same name).
// Cap: maxTools limits total tools sent to LLM (default 40).
type ToolCollector struct {
	skills   *skill.DefaultRegistry
	mcpMgr   *mcp.Manager // may be nil
	maxTools int
}

// NewToolCollector creates a new collector.
func NewToolCollector(skills *skill.DefaultRegistry, mcpMgr *mcp.Manager, maxTools int) *ToolCollector {
	if maxTools <= 0 {
		maxTools = 40
	}
	return &ToolCollector{skills: skills, mcpMgr: mcpMgr, maxTools: maxTools}
}

// Collect returns deduplicated, sorted, and capped tool definitions.
func (tc *ToolCollector) Collect() []llm.ToolDefinition {
	seen := make(map[string]bool)
	var tools []llm.ToolDefinition

	// 1. Skills (highest priority)
	if tc.skills != nil {
		for _, s := range tc.skills.All() {
			def := s.ToolDefinition()
			if def.Function.Name == "" {
				continue
			}
			if seen[def.Function.Name] {
				continue
			}
			seen[def.Function.Name] = true
			if def.Type == "" {
				def.Type = "function"
			}
			tools = append(tools, def)
		}
	}

	// 2. MCP tools
	if tc.mcpMgr != nil {
		for _, def := range tc.mcpMgr.ListToolDefinitions() {
			if def.Function.Name == "" {
				continue
			}
			if seen[def.Function.Name] {
				continue
			}
			seen[def.Function.Name] = true
			tools = append(tools, def)
		}
	}

	// Sort for deterministic ordering
	sort.Slice(tools, func(i, j int) bool {
		return tools[i].Function.Name < tools[j].Function.Name
	})

	// Cap
	if len(tools) > tc.maxTools {
		tools = tools[:tc.maxTools]
	}

	return tools
}

// Refresh re-collects tools. Call after MCP/Skill changes.
func (tc *ToolCollector) Refresh() []llm.ToolDefinition {
	return tc.Collect()
}
