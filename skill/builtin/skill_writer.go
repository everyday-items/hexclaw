package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/security"
	"github.com/hexagon-codes/hexclaw/skill"
)

var validSkillName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// SkillWriterSkill allows the LLM to create new Skills as SKILL.md files.
//
// Security:
//   - Name validation (no path traversal)
//   - Content scanned by SkillScanner before writing
//   - Written to user's skill directory only
type SkillWriterSkill struct {
	skillDir string
	scanner  *security.SkillScanner
}

// NewSkillWriterSkill creates a new SkillWriterSkill.
func NewSkillWriterSkill(skillDir string, scanner *security.SkillScanner) *SkillWriterSkill {
	return &SkillWriterSkill{
		skillDir: skillDir,
		scanner:  scanner,
	}
}

func (s *SkillWriterSkill) Name() string        { return "create_skill" }
func (s *SkillWriterSkill) Description() string  { return "Create a new reusable skill" }
func (s *SkillWriterSkill) Match(_ string) bool  { return false }

func (s *SkillWriterSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("create_skill",
		"Create a new reusable skill as a SKILL.md file. The skill will be available for future tool calls. Use when you need a capability that doesn't exist yet.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"name":        {Type: "string", Description: "Skill name (lowercase, hyphens only, e.g. 'csv-analyzer')"},
				"description": {Type: "string", Description: "One-line description of what this skill does"},
				"content":     {Type: "string", Description: "Full SKILL.md content with YAML frontmatter and instructions"},
			},
			Required: []string{"name", "description", "content"},
		})
}

func (s *SkillWriterSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	name, _ := args["name"].(string)
	content, _ := args["content"].(string)

	if name == "" || content == "" {
		return nil, fmt.Errorf("name and content are required")
	}

	// 1. Name validation (prevent path traversal)
	if !validSkillName.MatchString(name) {
		return nil, fmt.Errorf("invalid skill name %q: must be lowercase alphanumeric with hyphens, 1-64 chars", name)
	}
	if strings.Contains(name, "..") {
		return nil, fmt.Errorf("invalid skill name: path traversal detected")
	}

	// 2. Content security scan
	if err := s.scanner.Scan(content); err != nil {
		return nil, fmt.Errorf("security scan failed: %w", err)
	}

	// 3. Write to disk (with symlink protection)
	dir := filepath.Join(s.skillDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create skill directory: %w", err)
	}
	// Resolve symlinks and verify the target is still within skillDir
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve skill directory: %w", err)
	}
	resolvedBase, _ := filepath.EvalSymlinks(s.skillDir)
	if !strings.HasPrefix(resolvedDir, resolvedBase) {
		return nil, fmt.Errorf("skill directory escapes base path (symlink attack?)")
	}
	path := filepath.Join(resolvedDir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("failed to write skill file: %w", err)
	}

	return &skill.Result{
		Content: fmt.Sprintf("Skill '%s' created at %s. Restart to load, or use manage_skill to hot-reload.", name, path),
	}, nil
}
