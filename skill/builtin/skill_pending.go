// skill_pending.go 实现 v0.4.0 F2 "用户审批闭环" 工具：
//
//   - list:    扫描 skillDir 下所有 SKILL.md.pending，输出名字 + diff 摘要
//   - approve: 把指定 skill 的 SKILL.md.pending 原子 rename 为 SKILL.md
//   - reject:  删除指定 skill 的 SKILL.md.pending
//
// 安全约束：
//   - approve/reject 都校验 skill_name（防 path traversal）
//   - approve 用 os.Rename 原子替换，避免半写入状态
//   - 永远不写 production 路径（只 rename / unlink）
//
// 该工具同样作为 LLM tool 暴露，但预期由用户在前端确认后才让 LLM 触发；
// 真实部署可在前端把这个工具放进"需要 approval"白名单。
package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// SkillPendingSkill 暴露 manage_skill_pending 工具：list/approve/reject 三动作。
type SkillPendingSkill struct {
	skillDir string
}

// NewSkillPendingSkill 构造 manage_skill_pending 工具。
func NewSkillPendingSkill(skillDir string) *SkillPendingSkill {
	return &SkillPendingSkill{skillDir: skillDir}
}

func (s *SkillPendingSkill) Name() string { return "manage_skill_pending" }
func (s *SkillPendingSkill) Description() string {
	return "List / approve / reject pending Skill drafts (SKILL.md.pending) created by create_skill or patch_skill"
}
func (s *SkillPendingSkill) Match(_ string) bool { return false }

func (s *SkillPendingSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("manage_skill_pending",
		"Review and decide on Skill drafts. action='list' returns all pending skills with their diff summary. "+
			"action='approve' atomically promotes the named skill's SKILL.md.pending to SKILL.md. "+
			"action='reject' deletes the named skill's SKILL.md.pending. "+
			"Never bypass this flow — production SKILL.md is the only file the runtime loads.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"action": {Type: "string", Description: "One of: list, approve, reject"},
				"name":   {Type: "string", Description: "Skill name (required for approve/reject)"},
			},
			Required: []string{"action"},
		})
}

func (s *SkillPendingSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	action, _ := args["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	switch action {
	case "list":
		return s.listPending()
	case "approve":
		name, _ := args["name"].(string)
		return s.approve(name)
	case "reject":
		name, _ := args["name"].(string)
		return s.reject(name)
	default:
		return nil, fmt.Errorf("unknown action %q (expected list/approve/reject)", action)
	}
}

func (s *SkillPendingSkill) listPending() (*skill.Result, error) {
	if _, err := os.Stat(s.skillDir); os.IsNotExist(err) {
		return &skill.Result{Content: "no skill directory yet"}, nil
	}

	entries, err := os.ReadDir(s.skillDir)
	if err != nil {
		return nil, fmt.Errorf("read skill dir: %w", err)
	}

	type pendingItem struct {
		name        string
		path        string
		size        int64
		hasOriginal bool
	}
	var items []pendingItem

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(s.skillDir, e.Name())
		pending := filepath.Join(dir, "SKILL.md"+PendingSuffix)
		fi, err := os.Stat(pending)
		if err != nil {
			continue
		}
		_, origErr := os.Stat(filepath.Join(dir, "SKILL.md"))
		items = append(items, pendingItem{
			name:        e.Name(),
			path:        pending,
			size:        fi.Size(),
			hasOriginal: origErr == nil,
		})
	}

	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })

	if len(items) == 0 {
		return &skill.Result{Content: "no pending skill drafts"}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d pending skill draft(s):\n\n", len(items)))
	for _, it := range items {
		mode := "new"
		if it.hasOriginal {
			mode = "modify"
		}
		sb.WriteString(fmt.Sprintf("- %s (%s, %d bytes) → %s\n", it.name, mode, it.size, it.path))
	}
	sb.WriteString("\nUse manage_skill_pending(action='approve', name=<name>) or action='reject' to decide.")
	return &skill.Result{
		Content:  sb.String(),
		Metadata: map[string]string{"count": fmt.Sprintf("%d", len(items))},
	}, nil
}

func (s *SkillPendingSkill) approve(name string) (*skill.Result, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required for approve")
	}
	if !validSkillName.MatchString(name) {
		return nil, fmt.Errorf("invalid skill name %q", name)
	}
	dir, err := s.resolveDir(name)
	if err != nil {
		return nil, err
	}

	pending := filepath.Join(dir, "SKILL.md"+PendingSuffix)
	live := filepath.Join(dir, "SKILL.md")

	if _, err := os.Stat(pending); err != nil {
		return nil, fmt.Errorf("no pending draft for skill %q: %w", name, err)
	}

	// Atomic rename — replaces existing SKILL.md if present.
	if err := os.Rename(pending, live); err != nil {
		return nil, fmt.Errorf("approve rename failed: %w", err)
	}
	return &skill.Result{
		Content: fmt.Sprintf("Approved: %s pending draft promoted to %s. Restart or hot-reload registry to activate.", name, live),
		Metadata: map[string]string{
			"skill_name": name,
			"action":     "approved",
			"live_path":  live,
		},
	}, nil
}

func (s *SkillPendingSkill) reject(name string) (*skill.Result, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required for reject")
	}
	if !validSkillName.MatchString(name) {
		return nil, fmt.Errorf("invalid skill name %q", name)
	}
	dir, err := s.resolveDir(name)
	if err != nil {
		return nil, err
	}

	pending := filepath.Join(dir, "SKILL.md"+PendingSuffix)
	if _, err := os.Stat(pending); err != nil {
		return nil, fmt.Errorf("no pending draft for skill %q: %w", name, err)
	}
	if err := os.Remove(pending); err != nil {
		return nil, fmt.Errorf("reject failed: %w", err)
	}
	return &skill.Result{
		Content: fmt.Sprintf("Rejected: %s pending draft deleted (%s).", name, pending),
		Metadata: map[string]string{
			"skill_name": name,
			"action":     "rejected",
		},
	}, nil
}

// resolveDir 校验目录存在且不通过 symlink 逃出 skillDir。
func (s *SkillPendingSkill) resolveDir(name string) (string, error) {
	if strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("invalid skill name: path traversal detected")
	}
	dir := filepath.Join(s.skillDir, name)
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("skill %q not found: %w", name, err)
	}
	resolvedBase, _ := filepath.EvalSymlinks(s.skillDir)
	if !strings.HasPrefix(resolvedDir, resolvedBase) {
		return "", fmt.Errorf("skill directory escapes base path (symlink attack?)")
	}
	return resolvedDir, nil
}
