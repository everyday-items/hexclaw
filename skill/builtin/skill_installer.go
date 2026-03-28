package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/hub"
)

// SkillInstallerSkill allows the LLM to search, install, remove, and list Skills from Hub.
type SkillInstallerSkill struct {
	hub *hub.Hub
}

// NewSkillInstallerSkill creates a new SkillInstallerSkill.
func NewSkillInstallerSkill(h *hub.Hub) *SkillInstallerSkill {
	return &SkillInstallerSkill{hub: h}
}

func (s *SkillInstallerSkill) Name() string        { return "manage_skill" }
func (s *SkillInstallerSkill) Description() string  { return "Search, install, or remove skills from HexClaw Hub" }
func (s *SkillInstallerSkill) Match(_ string) bool  { return false }

func (s *SkillInstallerSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("manage_skill",
		"Search, install, or remove skills from HexClaw Hub. Use when the user asks to add/remove a skill or capability.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"action":  {Type: "string", Description: "Action to perform: search, install, remove, list"},
				"keyword": {Type: "string", Description: "Search keyword or skill name (required for search/install/remove)"},
			},
			Required: []string{"action"},
		})
}

func (s *SkillInstallerSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	action, _ := args["action"].(string)
	keyword, _ := args["keyword"].(string)

	switch action {
	case "search":
		if keyword == "" {
			return nil, fmt.Errorf("keyword is required for search")
		}
		results := s.hub.Search(keyword)
		if len(results) == 0 {
			return &skill.Result{Content: fmt.Sprintf("No skills found for '%s'", keyword)}, nil
		}
		return &skill.Result{Content: formatSkillSearchResults(results)}, nil

	case "install":
		if keyword == "" {
			return nil, fmt.Errorf("keyword (skill name) is required for install")
		}
		if err := s.hub.Install(ctx, keyword); err != nil {
			return nil, fmt.Errorf("failed to install skill '%s': %w", keyword, err)
		}
		return &skill.Result{Content: fmt.Sprintf("Skill '%s' installed. Restart to load.", keyword)}, nil

	case "remove":
		if keyword == "" {
			return nil, fmt.Errorf("keyword (skill name) is required for remove")
		}
		if err := s.hub.Uninstall(keyword); err != nil {
			return nil, fmt.Errorf("failed to remove skill '%s': %w", keyword, err)
		}
		return &skill.Result{Content: fmt.Sprintf("Skill '%s' removed.", keyword)}, nil

	case "list":
		catalog := s.hub.GetCatalog()
		if catalog == nil || len(catalog.Skills) == 0 {
			return &skill.Result{Content: "No skills in catalog. Run search to discover available skills."}, nil
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Available skills (%d):\n", len(catalog.Skills)))
		for i, sk := range catalog.Skills {
			if i >= 20 {
				sb.WriteString(fmt.Sprintf("... and %d more\n", len(catalog.Skills)-20))
				break
			}
			sb.WriteString(fmt.Sprintf("  %d. %s — %s\n", i+1, sk.Name, sk.Description))
		}
		return &skill.Result{Content: sb.String()}, nil

	default:
		return nil, fmt.Errorf("unknown action %q: use search, install, remove, or list", action)
	}
}

func formatSkillSearchResults(results []hub.SkillMeta) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d skill(s):\n", len(results)))
	for i, r := range results {
		if i >= 10 {
			sb.WriteString(fmt.Sprintf("... and %d more\n", len(results)-10))
			break
		}
		sb.WriteString(fmt.Sprintf("  %d. %s — %s", i+1, r.Name, r.Description))
		if r.Category != "" {
			sb.WriteString(fmt.Sprintf(" [%s]", r.Category))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
