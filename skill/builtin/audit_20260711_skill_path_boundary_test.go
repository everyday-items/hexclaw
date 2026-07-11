package builtin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/hexagon-codes/hexclaw/security"
)

func auditSiblingPrefixSymlink(t *testing.T) (base, sibling string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably available on Windows test hosts")
	}
	root := t.TempDir()
	base = filepath.Join(root, "skills")
	sibling = filepath.Join(root, "skills-escape")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sibling, filepath.Join(base, "safe-name")); err != nil {
		t.Fatal(err)
	}
	return base, sibling
}

func TestAudit20260711SkillPendingRejectsSiblingPrefixSymlink(t *testing.T) {
	base, _ := auditSiblingPrefixSymlink(t)
	pending := NewSkillPendingSkill(base)

	if resolved, err := pending.resolveDir("safe-name"); err == nil {
		t.Fatalf("sibling-prefix symlink escaped skill root: resolved=%q", resolved)
	}
}

func TestAudit20260711SkillWriterDoesNotWriteThroughSiblingPrefixSymlink(t *testing.T) {
	base, sibling := auditSiblingPrefixSymlink(t)
	writer := NewSkillWriterSkill(base, security.NewSkillScanner())

	_, err := writer.Execute(context.Background(), map[string]any{
		"name":    "safe-name",
		"content": "---\nname: safe-name\n---\nbody",
	})
	if err == nil {
		t.Fatal("writer accepted a skill directory symlinked outside its root")
	}
	if _, statErr := os.Stat(filepath.Join(sibling, "SKILL.md"+PendingSuffix)); !os.IsNotExist(statErr) {
		t.Fatalf("writer created an out-of-root pending file: stat err=%v", statErr)
	}
}

func TestAudit20260711SkillPatcherDoesNotWriteThroughSiblingPrefixSymlink(t *testing.T) {
	base, sibling := auditSiblingPrefixSymlink(t)
	if err := os.WriteFile(filepath.Join(sibling, "SKILL.md"), []byte("old text"), 0o644); err != nil {
		t.Fatal(err)
	}
	patcher := NewSkillPatcherSkill(base, security.NewSkillScanner())

	_, err := patcher.Execute(context.Background(), map[string]any{
		"name":     "safe-name",
		"old_text": "old text",
		"new_text": "new text",
	})
	if err == nil {
		t.Fatal("patcher accepted a skill directory symlinked outside its root")
	}
	if _, statErr := os.Stat(filepath.Join(sibling, "SKILL.md"+PendingSuffix)); !os.IsNotExist(statErr) {
		t.Fatalf("patcher created an out-of-root pending file: stat err=%v", statErr)
	}
}

func TestAudit20260711SkillWriterRejectsPendingFileSymlink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "safe-name")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("KEEP"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "SKILL.md"+PendingSuffix)); err != nil {
		t.Fatal(err)
	}

	writer := NewSkillWriterSkill(base, security.NewSkillScanner())
	if _, err := writer.Execute(context.Background(), map[string]any{
		"name":    "safe-name",
		"content": "---\nname: safe-name\n---\nbody",
	}); err == nil {
		t.Fatal("writer followed an existing pending-file symlink")
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "KEEP" {
		t.Fatalf("writer modified symlink target: got=%q err=%v", got, err)
	}
}

func TestAudit20260711SkillPatcherRejectsLiveFileSymlink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "safe-name")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(victim, []byte("OLD SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	patcher := NewSkillPatcherSkill(base, security.NewSkillScanner())
	if _, err := patcher.Execute(context.Background(), map[string]any{
		"name": "safe-name", "old_text": "OLD", "new_text": "NEW",
	}); err == nil {
		t.Fatal("patcher read a live-file symlink outside the skill directory")
	}
	if _, err := os.Lstat(filepath.Join(dir, "SKILL.md"+PendingSuffix)); !os.IsNotExist(err) {
		t.Fatalf("patcher created pending output after symlinked live input: %v", err)
	}
}

func TestAudit20260711SkillPatcherRejectsPendingFileSymlink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "safe-name")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("KEEP"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "SKILL.md"+PendingSuffix)); err != nil {
		t.Fatal(err)
	}

	patcher := NewSkillPatcherSkill(base, security.NewSkillScanner())
	if _, err := patcher.Execute(context.Background(), map[string]any{
		"name": "safe-name", "old_text": "OLD", "new_text": "NEW",
	}); err == nil {
		t.Fatal("patcher followed an existing pending-file symlink")
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "KEEP" {
		t.Fatalf("patcher modified symlink target: got=%q err=%v", got, err)
	}
}

func TestAudit20260711SkillPendingRejectsApproveSymlink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "safe-name")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "outside-skill")
	if err := os.WriteFile(victim, []byte("EXTERNAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "SKILL.md"+PendingSuffix)); err != nil {
		t.Fatal(err)
	}

	pending := NewSkillPendingSkill(base)
	if _, err := pending.Execute(context.Background(), map[string]any{
		"action": "approve", "name": "safe-name",
	}); err == nil {
		t.Fatal("approve promoted a pending-file symlink into the live registry")
	}
	if _, err := os.Lstat(filepath.Join(dir, "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("approve created a live skill from symlink input: %v", err)
	}
}
