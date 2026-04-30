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
// v0.4.0 F2 K12 安全底线（强制 .pending 流）：
//   - LLM 永远不直接覆盖 production 的 SKILL.md
//   - 所有写入落到 SKILL.md.pending；marketplace loader 不会扫描 .pending
//   - 用户/家长通过 manage_skill_pending(approve|reject) 审核闭环
//
// Security:
//   - Name validation (no path traversal)
//   - Content scanned by SkillScanner before writing
//   - Written to user's skill directory only as .pending file
type SkillWriterSkill struct {
	skillDir string
	scanner  *security.SkillScanner
}

// PendingSuffix 是 LLM-written Skill 文件的待审批后缀。marketplace loader 不会
// 把这些文件加载进 registry，必须经 SkillPendingSkill.approve 改名为 SKILL.md
// 才会生效。
const PendingSuffix = ".pending"

// NewSkillWriterSkill creates a new SkillWriterSkill.
func NewSkillWriterSkill(skillDir string, scanner *security.SkillScanner) *SkillWriterSkill {
	return &SkillWriterSkill{
		skillDir: skillDir,
		scanner:  scanner,
	}
}

func (s *SkillWriterSkill) Name() string { return "create_skill" }
func (s *SkillWriterSkill) Description() string {
	return "Draft a new reusable skill (writes to SKILL.md.pending; user must approve before activation)"
}
func (s *SkillWriterSkill) Match(_ string) bool { return false }

func (s *SkillWriterSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("create_skill",
		"Draft a new reusable skill. The content is saved as SKILL.md.pending and is NOT loaded "+
			"into the live skill registry until the user runs manage_skill_pending(action='approve'). "+
			"Use when a needed capability doesn't yet exist.",
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

	// 3. Write to disk as .pending (with symlink protection)
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
	pendingPath := filepath.Join(resolvedDir, "SKILL.md"+PendingSuffix)
	if err := os.WriteFile(pendingPath, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("failed to write skill pending file: %w", err)
	}

	livePath := filepath.Join(resolvedDir, "SKILL.md")
	exists := false
	if _, err := os.Stat(livePath); err == nil {
		exists = true
	}

	verb := "drafted"
	if exists {
		verb = "drafted (overwrites existing)"
	}

	return &skill.Result{
		Content: fmt.Sprintf(
			"Skill '%s' %s at %s. NOT YET ACTIVE — call manage_skill_pending(action='approve', name='%s') after the user reviews it.",
			name, verb, pendingPath, name,
		),
		Metadata: map[string]string{
			"skill_name":   name,
			"pending_path": pendingPath,
			"overwrites":   fmt.Sprintf("%t", exists),
		},
	}, nil
}
