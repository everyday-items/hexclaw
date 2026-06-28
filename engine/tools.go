package engine

import (
	"crypto/sha1"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/skill"
)

// llmToolNamePattern 上游 OpenAI / Anthropic 通用的 tool name 校验正则。
//
// BUG-20260523 教训：marketplace 用户 skill 名字含中文（"前女友" / "前leader"），
// 注入到 Claude tools[].function.name 立即被 400 拒收：
//
//	String should match pattern '^[a-zA-Z0-9_-]{1,128}$'
//
// 任何 source（builtin / marketplace / chain / MCP）的 tool name 必须过这道关。
// 不合规的 skip + warn 日志（不影响 trigger 词召唤路径——中文 skill 仍可被
// trigger 词激活，只是不出现在 LLM tools 列表中）。
var llmToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// isValidLLMToolName 检查 name 是否符合上游 LLM 标识符规范。
func isValidLLMToolName(name string) bool {
	return llmToolNamePattern.MatchString(name)
}

// llmToolNameSlug 把任意技能名映射成合规的 LLM tool name。
//
// 合规名原样返回；否则保留 ASCII 词字符、丢弃其余，并附确定性 hash 后缀，
// 使纯中文名（如"前女友"）也能得到稳定可路由的标识符。它是纯函数——
// ToolCollector 用它暴露、ToolExecutor 用它反查，无需共享状态。
func llmToolNameSlug(name string) string {
	if isValidLLMToolName(name) {
		return name
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	hash := fmt.Sprintf("%x", sha1.Sum([]byte(name)))[:8]
	if ascii := strings.Trim(b.String(), "-_"); ascii != "" {
		return ascii + "-" + hash
	}
	return "skill-" + hash
}

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
			// persona/prompt 类技能（skill.ContentLoader，无副作用、Execute 只回吐 prompt 正文）不是
			// 可调用工具：其激活方式是把正文注入 system prompt（挂载 → buildMountedSkillsPrompt），而非
			// tool-call。暴露成工具只会让模型「调用」它拿回人设原文当工具结果 → 泄漏（BUG-20260627 #1：
			// 「这次只加载了角色设定，没有生成最终回答」）。仅副作用型技能（spawn/web_search/…）进工具集。
			if _, isContent := s.(skill.ContentLoader); isContent {
				continue
			}
			def := s.ToolDefinition()
			if def.Function.Name == "" {
				continue
			}
			// 非法 name（含中文 / 空格 / 点号等）上游会 400。派生合规 slug 暴露，
			// 让中文名技能仍可被 LLM 调用；ToolExecutor 用同一 slug 函数反查路由。
			def.Function.Name = llmToolNameSlug(def.Function.Name)
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
			if !isValidLLMToolName(def.Function.Name) {
				// BUG-20260523: 同上 —— MCP server 也可能暴露非法 name 工具。
				slog.Warn("[ToolCollector] skip MCP tool — name 不符合 LLM 标识符正则",
					"name", def.Function.Name,
					"source", "mcp",
					"pattern", `^[a-zA-Z0-9_-]{1,128}$`,
					"hint", "联系 MCP server 维护者修正 tool name 命名规范")
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
