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

// Collect 向后兼容入口：等价于 CollectFiltered("", zero) —— 不做查询召回也不过滤激活。
// 仅用于不知道当前 query/agent_mode 的兜底场景（如 system prompt 工具列表）。
func (tc *ToolCollector) Collect() []llm.ToolDefinition {
	return tc.CollectFiltered("", skill.Activation{})
}

// CollectFiltered 按 query 召回 + 按 activation 过滤后返回工具定义。
//
// v0.4.x C1+C2 真闭环路径：
//   - query 非空时，skill 包按 trigger/tag/name/desc 打分 Top-K（K=maxTools）
//   - act.Mode/Subject/Grade/Custom 用于条件激活（when/not_when 过滤）
//   - 内置 Skill（不实现 MetaProvider）默认通过条件检查，全部出现在召回里
//   - MCP 工具不参与 activation 过滤（MCP 不在 Skills 闭环范围）
//
// 与 Collect() 一样保持去重 / 排序 / Cap 一致。
func (tc *ToolCollector) CollectFiltered(query string, act skill.Activation) []llm.ToolDefinition {
	seen := make(map[string]bool)
	var tools []llm.ToolDefinition

	// 1. Skills（按 query+activation 过滤召回；query="" 时 SelectByContext 内部 score=1 兜底全返回）
	if tc.skills != nil {
		var skills []skill.Skill
		if query != "" || hasActivation(act) {
			skills = tc.skills.SelectByContext(query, act, tc.maxTools)
		} else {
			skills = tc.skills.All()
		}
		for _, s := range skills {
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

	// 2. MCP tools（不参与激活过滤）
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

// hasActivation 判断 Activation 是否非零（任一字段非空即视为有上下文，触发条件激活路径）。
func hasActivation(act skill.Activation) bool {
	return act.Mode != "" || act.Subject != "" || act.Grade != "" || len(act.Custom) > 0
}

// Refresh re-collects tools. Call after MCP/Skill changes.
func (tc *ToolCollector) Refresh() []llm.ToolDefinition {
	return tc.Collect()
}
