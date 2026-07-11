package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/security"
)

// setupSymlinkEscape 构造一个"兄弟目录逃逸"场景：
//
//	skillDir       = <base>/skills
//	evilDir        = <base>/skills-evil      (共享前缀 "skills"，但不在 skillDir 下)
//	<skillDir>/<name> 是指向 <evilDir>/<name> 的 symlink
//
// resolvedBase = EvalSymlinks(skillDir) = <real>/skills
// resolvedDir  = EvalSymlinks(skillDir/name) = <real>/skills-evil/name
// 无分隔符的 HasPrefix(resolvedDir, resolvedBase) 会误判 "<real>/skills-evil/x"
// 以 "<real>/skills" 开头 → 放行逃逸（AP-158 / R10）。
func setupSymlinkEscape(t *testing.T, name string) (skillDir, evilTargetDir string) {
	t.Helper()
	base := t.TempDir()
	skillDir = filepath.Join(base, "skills")
	evilDir := filepath.Join(base, "skills-evil")
	evilTargetDir = filepath.Join(evilDir, name)

	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skillDir: %v", err)
	}
	if err := os.MkdirAll(evilTargetDir, 0o755); err != nil {
		t.Fatalf("mkdir evilTargetDir: %v", err)
	}
	// <skillDir>/<name> -> <evilDir>/<name>
	if err := os.Symlink(evilTargetDir, filepath.Join(skillDir, name)); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	return skillDir, evilTargetDir
}

const escapeErrSubstr = "escapes base path"

// TestR10_AP158_SkillWriter_SiblingDirEscape 验证 create_skill 的 symlink 逃逸边界。
//
// 真断言：RED 下无分隔符前缀检查放行逃逸，Execute 会把 .pending 文件写入
// <base>/skills-evil（兄弟目录，越权写盘）并返回 nil error。测试断言：
//  1. 必须返回错误（被拒），
//  2. 逃逸目标目录里绝不能出现 pending 文件（无数据落盘泄漏）。
func TestR10_AP158_SkillWriter_SiblingDirEscape(t *testing.T) {
	const name = "payload"
	skillDir, evilTargetDir := setupSymlinkEscape(t, name)

	s := NewSkillWriterSkill(skillDir, security.NewSkillScanner())
	_, err := s.Execute(context.Background(), map[string]any{
		"name":        name,
		"description": "d",
		"content":     "harmless skill body",
	})

	if err == nil {
		t.Fatalf("BUG AP-158: create_skill 放行了兄弟目录逃逸（应拒绝）")
	}
	if !strings.Contains(err.Error(), escapeErrSubstr) {
		t.Fatalf("期望逃逸拒绝错误(含 %q)，实际: %v", escapeErrSubstr, err)
	}
	// 逃逸目录不得出现任何写入
	entries, _ := os.ReadDir(evilTargetDir)
	for _, e := range entries {
		t.Fatalf("BUG AP-158: 文件泄漏进兄弟目录 %s/%s", evilTargetDir, e.Name())
	}
}

// TestR10_AP158_SkillPending_SiblingDirEscape 直接测真实的 resolveDir（同包可见）。
//
// 真断言：resolveDir 是 approve/reject 的安全闸门。RED 下它对逃逸目标返回 nil error
// （放行），GREEN 下返回 escapes base path。断言错误必须是逃逸拒绝。
func TestR10_AP158_SkillPending_SiblingDirEscape(t *testing.T) {
	const name = "payload"
	skillDir, _ := setupSymlinkEscape(t, name)

	s := NewSkillPendingSkill(skillDir)
	_, err := s.resolveDir(name)

	if err == nil {
		t.Fatalf("BUG AP-158: resolveDir 放行了兄弟目录逃逸（应拒绝）")
	}
	if !strings.Contains(err.Error(), escapeErrSubstr) {
		t.Fatalf("期望逃逸拒绝错误(含 %q)，实际: %v", escapeErrSubstr, err)
	}
}

// TestR10_AP158_SkillPatcher_SiblingDirEscape 验证 patch_skill 的 symlink 逃逸边界。
//
// 真断言：RED 下前缀检查放行逃逸后会继续 ReadFile 逃逸目录里的 SKILL.md（越权读盘）。
// 我们在逃逸目录放一个 SKILL.md，使 RED 不会因"文件不存在"提前失败——若前缀检查失效，
// patcher 会真的读到兄弟目录的文件并返回非逃逸错误。断言错误必须是逃逸拒绝。
func TestR10_AP158_SkillPatcher_SiblingDirEscape(t *testing.T) {
	const name = "payload"
	skillDir, evilTargetDir := setupSymlinkEscape(t, name)
	// 在逃逸目标放一个真实 SKILL.md，坐实"越权读盘"路径
	if err := os.WriteFile(filepath.Join(evilTargetDir, "SKILL.md"), []byte("OLD secret\n"), 0o644); err != nil {
		t.Fatalf("write evil SKILL.md: %v", err)
	}

	s := NewSkillPatcherSkill(skillDir, security.NewSkillScanner())
	_, err := s.Execute(context.Background(), map[string]any{
		"name":     name,
		"old_text": "OLD",
		"new_text": "NEW",
	})

	if err == nil {
		t.Fatalf("BUG AP-158: patch_skill 放行了兄弟目录逃逸（应拒绝）")
	}
	if !strings.Contains(err.Error(), escapeErrSubstr) {
		t.Fatalf("期望逃逸拒绝错误(含 %q)，实际: %v", escapeErrSubstr, err)
	}
}
