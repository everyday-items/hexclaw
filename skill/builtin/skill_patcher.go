// skill_patcher.go 实现 v0.4.0 F2 patch 工具：让 LLM 通过"老文本→新文本"
// 片段编辑既有 Skill，结果统一落到 SKILL.md.pending，等用户审批后才生效。
//
// Fuzzy 匹配策略（轻量级，不依赖第三方 diff 库）：
//  1. 严格 substring 匹配：原始 old_text 在 SKILL.md 中是否原样存在
//  2. 退而求其次 — 行级宽松匹配：把 old_text/SKILL.md 拆成行，按"trimmed equality"
//     连续匹配；用于容忍尾随空白、缩进微差等 LLM 输出常见漂移
//  3. 任一匹配即把对应区段替换为 new_text；都失败则报错
//
// 不引入完整 unified diff / 三向合并：F2 的目标是"安全底线"，越简单越好审计；
// 复杂多 hunk 编辑请走 create_skill 重写整个 SKILL.md.pending 路径。
package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/security"
	"github.com/hexagon-codes/hexclaw/skill"
)

// SkillPatcherSkill 让 LLM 修改既有 Skill 的小片段，结果落到 SKILL.md.pending。
type SkillPatcherSkill struct {
	skillDir string
	scanner  *security.SkillScanner
}

// NewSkillPatcherSkill 构造 patch_skill 工具。
func NewSkillPatcherSkill(skillDir string, scanner *security.SkillScanner) *SkillPatcherSkill {
	return &SkillPatcherSkill{skillDir: skillDir, scanner: scanner}
}

func (s *SkillPatcherSkill) Name() string { return "patch_skill" }
func (s *SkillPatcherSkill) Description() string {
	return "Patch an existing skill by replacing a snippet (writes to SKILL.md.pending; user must approve)"
}
func (s *SkillPatcherSkill) Match(_ string) bool { return false }

func (s *SkillPatcherSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("patch_skill",
		"Patch an existing skill by replacing one snippet of its SKILL.md content. "+
			"Saves the result to SKILL.md.pending — the user must call manage_skill_pending to approve. "+
			"Fuzzy whitespace tolerance is applied so minor indentation drift in old_text is forgiven; "+
			"if the snippet still cannot be located the call returns an error and nothing is written.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"name":     {Type: "string", Description: "Skill name (matches the directory under the skill base)"},
				"old_text": {Type: "string", Description: "Existing snippet to replace (must appear once, fuzzy whitespace allowed)"},
				"new_text": {Type: "string", Description: "Replacement snippet"},
			},
			Required: []string{"name", "old_text", "new_text"},
		})
}

func (s *SkillPatcherSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	name, _ := args["name"].(string)
	oldText, _ := args["old_text"].(string)
	newText, _ := args["new_text"].(string)

	if name == "" || oldText == "" {
		return nil, fmt.Errorf("name and old_text are required")
	}
	if !validSkillName.MatchString(name) {
		return nil, fmt.Errorf("invalid skill name %q", name)
	}
	if strings.Contains(name, "..") {
		return nil, fmt.Errorf("invalid skill name: path traversal detected")
	}

	dir := filepath.Join(s.skillDir, name)
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("skill %q not found: %w", name, err)
	}
	resolvedBase, _ := filepath.EvalSymlinks(s.skillDir)
	if !strings.HasPrefix(resolvedDir, resolvedBase) {
		return nil, fmt.Errorf("skill directory escapes base path (symlink attack?)")
	}

	livePath := filepath.Join(resolvedDir, "SKILL.md")
	original, err := os.ReadFile(livePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read existing SKILL.md: %w", err)
	}

	patched, mode, err := fuzzyReplaceOnce(string(original), oldText, newText)
	if err != nil {
		return nil, err
	}

	if s.scanner != nil {
		if err := s.scanner.Scan(patched); err != nil {
			return nil, fmt.Errorf("security scan failed: %w", err)
		}
	}

	pendingPath := filepath.Join(resolvedDir, "SKILL.md"+PendingSuffix)
	if err := os.WriteFile(pendingPath, []byte(patched), 0644); err != nil {
		return nil, fmt.Errorf("failed to write pending file: %w", err)
	}

	return &skill.Result{
		Content: fmt.Sprintf(
			"Patched '%s' (%s match). Saved to %s. NOT YET ACTIVE — run manage_skill_pending(action='approve', name='%s') after user review.",
			name, mode, pendingPath, name,
		),
		Metadata: map[string]string{
			"skill_name":   name,
			"pending_path": pendingPath,
			"match_mode":   mode,
		},
	}, nil
}

// fuzzyReplaceOnce 在 src 中找到 oldText 的唯一位置并替换为 newText。
//
// 返回值：
//   - 第一个返回是替换后的文本
//   - 第二个返回是匹配模式（"exact" / "fuzzy"），便于审计
//   - 第三个返回是错误（找不到 / 找到多处都视作失败 —— 让 LLM 提供更精准的 old_text）
func fuzzyReplaceOnce(src, oldText, newText string) (string, string, error) {
	if oldText == "" {
		return "", "", fmt.Errorf("old_text cannot be empty")
	}

	// 1) 严格 substring：必须唯一出现
	count := strings.Count(src, oldText)
	if count == 1 {
		return strings.Replace(src, oldText, newText, 1), "exact", nil
	}
	if count > 1 {
		return "", "", fmt.Errorf("old_text matches %d times; provide more context to disambiguate", count)
	}

	// 2) 行级 fuzzy：trim 后比对，匹配整段连续行
	srcLines := strings.Split(src, "\n")
	oldLines := splitFuzzyLines(oldText)
	if len(oldLines) == 0 {
		return "", "", fmt.Errorf("old_text not found")
	}

	matchStart, matchEnd, ok := fuzzyMatchLines(srcLines, oldLines)
	if !ok {
		return "", "", fmt.Errorf("old_text not found (fuzzy whitespace match also failed)")
	}

	// 重构：matchStart..matchEnd 区间替换为 newText
	var sb strings.Builder
	for i := 0; i < matchStart; i++ {
		sb.WriteString(srcLines[i])
		if i < len(srcLines)-1 {
			sb.WriteString("\n")
		}
	}
	if matchStart > 0 && !strings.HasSuffix(sb.String(), "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString(newText)
	if !strings.HasSuffix(newText, "\n") && matchEnd < len(srcLines) {
		sb.WriteString("\n")
	}
	for i := matchEnd; i < len(srcLines); i++ {
		sb.WriteString(srcLines[i])
		if i < len(srcLines)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String(), "fuzzy", nil
}

// splitFuzzyLines 按 \n 拆并丢弃首尾纯空行（LLM 经常在 old_text 边界塞空行）。
func splitFuzzyLines(s string) []string {
	lines := strings.Split(s, "\n")
	// trim 首尾纯空行
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// fuzzyMatchLines 在 srcLines 里寻找 oldLines 连续匹配（仅按 TrimSpace 后相等比较）。
// 找到唯一匹配返回 [start, end) 区间；零或多次匹配返回 ok=false。
func fuzzyMatchLines(srcLines, oldLines []string) (int, int, bool) {
	if len(oldLines) == 0 || len(oldLines) > len(srcLines) {
		return 0, 0, false
	}

	matchStart := -1
	matches := 0

	for i := 0; i+len(oldLines) <= len(srcLines); i++ {
		ok := true
		for j := 0; j < len(oldLines); j++ {
			if strings.TrimSpace(srcLines[i+j]) != strings.TrimSpace(oldLines[j]) {
				ok = false
				break
			}
		}
		if ok {
			matches++
			matchStart = i
			if matches > 1 {
				return 0, 0, false
			}
		}
	}

	if matches != 1 {
		return 0, 0, false
	}
	return matchStart, matchStart + len(oldLines), true
}
