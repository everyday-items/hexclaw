// skill_view.go 提供 v0.4.0 F1 Skill 渐进披露的 Tier 3 入口工具。
//
// 渐进披露 3 个层次：
//   - Tier 1: ToolCollector.CollectFiltered() 在每次 LLM 调用时按 query+activation
//     召回 Top-K Skill，仅暴露 ToolDefinition（name + 1 行 description + 参数 schema）。
//   - Tier 2: SkillMeta 元信息（triggers/tags/when/preferred_mode/tools/requires），
//     在 skill_view 输出里展开。
//   - Tier 3: ContentLoader.LoadContent 提供的完整 Prompt / Markdown 正文，
//     仅在 LLM 主动调用 skill_view 时加载，避免无差别地把所有 Skill 完整内容塞进 context。
package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// SkillViewSkill 是一个内置工具，让 LLM 按 name 加载 Skill 完整说明。
//
// 与普通 Skill 的不同：
//   - Match() 永远 false（不走快速路径，只能由 LLM 通过 Tool Use 调用）
//   - Execute() 不触发被查 Skill 的副作用——通过 skill.ContentLoader 接口
//     提取无副作用的 prompt 正文；不实现该接口的 Skill 退化为元信息 + Description。
type SkillViewSkill struct {
	registry *skill.DefaultRegistry
}

// NewSkillViewSkill 构造 skill_view 工具。registry 不能为 nil。
func NewSkillViewSkill(reg *skill.DefaultRegistry) *SkillViewSkill {
	return &SkillViewSkill{registry: reg}
}

// Name 返回工具名 "skill_view"。
func (s *SkillViewSkill) Name() string { return "skill_view" }

// Description 返回供 LLM 理解的简要说明。
func (s *SkillViewSkill) Description() string {
	return "View the full prompt / instructions of a registered skill by name (Tier 3 progressive disclosure)."
}

// Match 永远 false：只能由 LLM Tool Use 调用，禁止快速路径。
func (s *SkillViewSkill) Match(_ string) bool { return false }

// ToolDefinition 暴露给 LLM 的 function schema。
func (s *SkillViewSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(
		"skill_view",
		"Load the full description and prompt of a registered skill by its name. "+
			"Use this when the brief description shown in the tool list is insufficient and "+
			"you need detailed instructions before deciding how to apply the skill.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"skill_name": {
					Type:        "string",
					Description: "Exact name of the skill to view (matches Skill.Name()).",
				},
			},
			Required: []string{"skill_name"},
		},
	)
}

// Execute 渲染 Tier 2/3 视图。registry 为 nil 时返回错误；name 不存在时返回提示
// 字符串（不抛错），让 LLM 自行重试或换工具。
func (s *SkillViewSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	if s.registry == nil {
		return nil, fmt.Errorf("skill_view: registry not configured")
	}

	name, _ := args["skill_name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("skill_view: skill_name is required")
	}

	target, ok := s.registry.Get(name)
	if !ok {
		return &skill.Result{
			Content: fmt.Sprintf("skill %q not found in registry. Use the tool list to see available skills.", name),
			Metadata: map[string]string{
				"viewed_skill": name,
				"found":        "false",
			},
		}, nil
	}

	var sb strings.Builder
	sb.WriteString("# Skill: ")
	sb.WriteString(target.Name())
	sb.WriteString("\n\n")
	if desc := strings.TrimSpace(target.Description()); desc != "" {
		sb.WriteString(desc)
		sb.WriteString("\n\n")
	}

	// Tier 2: 元信息（triggers/tags/when/not_when/tools/requires/preferred_mode）
	if mp, ok := target.(skill.MetaProvider); ok {
		meta := mp.SkillMeta()
		writeMetaList(&sb, "Triggers", meta.Triggers)
		writeMetaList(&sb, "Tags", meta.Tags)
		writeMetaList(&sb, "When", meta.When)
		writeMetaList(&sb, "Not When", meta.NotWhen)
		writeMetaList(&sb, "Required Tools", meta.Tools)
		writeMetaList(&sb, "Required Capabilities", meta.Requires)
		if meta.PreferredMode != "" {
			sb.WriteString("Preferred Mode: ")
			sb.WriteString(meta.PreferredMode)
			sb.WriteString("\n")
		}
		// 元信息和正文之间留一行空白，便于 LLM 阅读
		if hasMetaContent(meta) {
			sb.WriteString("\n")
		}
	}

	// Tier 3: ContentLoader 提供的无副作用完整内容
	tier3Loaded := false
	if cl, ok := target.(skill.ContentLoader); ok {
		content, err := cl.LoadContent()
		if err == nil && strings.TrimSpace(content) != "" {
			sb.WriteString("## Full Content\n\n")
			sb.WriteString(content)
			sb.WriteString("\n")
			tier3Loaded = true
		}
	}

	meta := map[string]string{
		"viewed_skill": name,
		"found":        "true",
	}
	if tier3Loaded {
		meta["tier"] = "3"
	} else {
		meta["tier"] = "2"
	}

	return &skill.Result{
		Content:  sb.String(),
		Metadata: meta,
	}, nil
}

func writeMetaList(sb *strings.Builder, label string, items []string) {
	cleaned := make([]string, 0, len(items))
	for _, it := range items {
		if t := strings.TrimSpace(it); t != "" {
			cleaned = append(cleaned, t)
		}
	}
	if len(cleaned) == 0 {
		return
	}
	sb.WriteString(label)
	sb.WriteString(": ")
	sb.WriteString(strings.Join(cleaned, ", "))
	sb.WriteString("\n")
}

func hasMetaContent(meta skill.SkillMetaInfo) bool {
	return len(meta.Triggers) > 0 ||
		len(meta.Tags) > 0 ||
		len(meta.When) > 0 ||
		len(meta.NotWhen) > 0 ||
		len(meta.Tools) > 0 ||
		len(meta.Requires) > 0 ||
		meta.PreferredMode != ""
}
