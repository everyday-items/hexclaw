package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/security"
)

// helper：创建临时 skillDir
func setupSkillDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir
}

// helper：在 skillDir/<name>/ 下放一个真 SKILL.md
func writeLiveSkill(t *testing.T, skillDir, name, content string) {
	t.Helper()
	dir := filepath.Join(skillDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// helper：放一个 pending 文件（模拟 LLM 写过）
func writePendingSkill(t *testing.T, skillDir, name, content string) {
	t.Helper()
	dir := filepath.Join(skillDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"+PendingSuffix), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestSkillWriter_WritesToPendingNotLive(t *testing.T) {
	skillDir := setupSkillDir(t)
	scanner := security.NewSkillScanner()
	w := NewSkillWriterSkill(skillDir, scanner)

	res, err := w.Execute(context.Background(), map[string]any{
		"name":        "math-tutor",
		"description": "math tutor",
		"content":     "---\nname: math-tutor\n---\nbody",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	pending := filepath.Join(skillDir, "math-tutor", "SKILL.md"+PendingSuffix)
	if _, err := os.Stat(pending); err != nil {
		t.Errorf("expected .pending at %s, got err %v", pending, err)
	}
	live := filepath.Join(skillDir, "math-tutor", "SKILL.md")
	if _, err := os.Stat(live); err == nil {
		t.Errorf("live SKILL.md should NOT be created by create_skill; found at %s", live)
	}
	if !strings.Contains(res.Content, "NOT YET ACTIVE") {
		t.Errorf("result must warn user about pending state; got %q", res.Content)
	}
}

func TestSkillPatcher_ExactMatch(t *testing.T) {
	skillDir := setupSkillDir(t)
	scanner := security.NewSkillScanner()
	writeLiveSkill(t, skillDir, "tutor", "---\nname: tutor\n---\nstep 1: think\nstep 2: answer\n")

	p := NewSkillPatcherSkill(skillDir, scanner)
	res, err := p.Execute(context.Background(), map[string]any{
		"name":     "tutor",
		"old_text": "step 1: think",
		"new_text": "step 1: think carefully",
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if res.Metadata["match_mode"] != "exact" {
		t.Errorf("expected exact match, got %q", res.Metadata["match_mode"])
	}
	pending, err := os.ReadFile(filepath.Join(skillDir, "tutor", "SKILL.md"+PendingSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pending), "step 1: think carefully") {
		t.Errorf("pending file should contain replacement; got %s", pending)
	}
	// production SKILL.md 必须保持原状
	live, _ := os.ReadFile(filepath.Join(skillDir, "tutor", "SKILL.md"))
	if !strings.Contains(string(live), "step 1: think\n") {
		t.Errorf("production SKILL.md must NOT be modified; got %s", live)
	}
}

func TestSkillPatcher_FuzzyWhitespace(t *testing.T) {
	skillDir := setupSkillDir(t)
	scanner := security.NewSkillScanner()
	writeLiveSkill(t, skillDir, "tutor", "---\nname: tutor\n---\nstep 1: think\nstep 2: answer\n")

	p := NewSkillPatcherSkill(skillDir, scanner)
	// LLM 提供的 old_text 多了空白（行首空格 + 行末空格）
	res, err := p.Execute(context.Background(), map[string]any{
		"name":     "tutor",
		"old_text": "  step 1: think   \n   step 2: answer  ",
		"new_text": "step 1: think hard\nstep 2: respond",
	})
	if err != nil {
		t.Fatalf("fuzzy patch: %v", err)
	}
	if res.Metadata["match_mode"] != "fuzzy" {
		t.Errorf("expected fuzzy match, got %q", res.Metadata["match_mode"])
	}
}

func TestSkillPatcher_NoMatchErrors(t *testing.T) {
	skillDir := setupSkillDir(t)
	scanner := security.NewSkillScanner()
	writeLiveSkill(t, skillDir, "tutor", "hello world")

	p := NewSkillPatcherSkill(skillDir, scanner)
	_, err := p.Execute(context.Background(), map[string]any{
		"name":     "tutor",
		"old_text": "nonexistent snippet",
		"new_text": "x",
	})
	if err == nil {
		t.Fatal("expected error when old_text not found")
	}
	// Pending 不应该被创建
	if _, err := os.Stat(filepath.Join(skillDir, "tutor", "SKILL.md"+PendingSuffix)); err == nil {
		t.Error("pending file must not be created on no-match")
	}
}

func TestSkillPatcher_AmbiguousMatchErrors(t *testing.T) {
	skillDir := setupSkillDir(t)
	scanner := security.NewSkillScanner()
	writeLiveSkill(t, skillDir, "tutor", "step\nstep\nstep\n")

	p := NewSkillPatcherSkill(skillDir, scanner)
	_, err := p.Execute(context.Background(), map[string]any{
		"name":     "tutor",
		"old_text": "step",
		"new_text": "STEP",
	})
	if err == nil {
		t.Fatal("expected error on multiple matches")
	}
	if !strings.Contains(err.Error(), "matches") {
		t.Errorf("error should mention multiple matches; got %v", err)
	}
}

func TestSkillPending_ListPendingDrafts(t *testing.T) {
	skillDir := setupSkillDir(t)
	writePendingSkill(t, skillDir, "alpha", "draft alpha")
	writePendingSkill(t, skillDir, "beta", "draft beta")
	writeLiveSkill(t, skillDir, "beta", "live beta") // beta 同时存在 live + pending

	mp := NewSkillPendingSkill(skillDir)
	res, err := mp.Execute(context.Background(), map[string]any{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metadata["count"] != "2" {
		t.Errorf("expected count=2, got %q", res.Metadata["count"])
	}
	if !strings.Contains(res.Content, "alpha") || !strings.Contains(res.Content, "beta") {
		t.Errorf("listing should include both drafts; got %q", res.Content)
	}
	if !strings.Contains(res.Content, "modify") {
		t.Errorf("expected 'modify' label for beta (existing live); got %q", res.Content)
	}
	if !strings.Contains(res.Content, "new") {
		t.Errorf("expected 'new' label for alpha; got %q", res.Content)
	}
}

func TestSkillPending_ApproveAtomicallyPromotes(t *testing.T) {
	skillDir := setupSkillDir(t)
	writeLiveSkill(t, skillDir, "tutor", "old content")
	writePendingSkill(t, skillDir, "tutor", "new content")

	mp := NewSkillPendingSkill(skillDir)
	_, err := mp.Execute(context.Background(), map[string]any{"action": "approve", "name": "tutor"})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	live, _ := os.ReadFile(filepath.Join(skillDir, "tutor", "SKILL.md"))
	if string(live) != "new content" {
		t.Errorf("live should be promoted content, got %q", live)
	}
	// pending 应该被消费掉
	if _, err := os.Stat(filepath.Join(skillDir, "tutor", "SKILL.md"+PendingSuffix)); err == nil {
		t.Error("pending file should be removed after approve")
	}
}

func TestSkillPending_RejectDeletesPending(t *testing.T) {
	skillDir := setupSkillDir(t)
	writeLiveSkill(t, skillDir, "tutor", "old")
	writePendingSkill(t, skillDir, "tutor", "new")

	mp := NewSkillPendingSkill(skillDir)
	_, err := mp.Execute(context.Background(), map[string]any{"action": "reject", "name": "tutor"})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}

	live, _ := os.ReadFile(filepath.Join(skillDir, "tutor", "SKILL.md"))
	if string(live) != "old" {
		t.Errorf("production must remain unchanged after reject; got %q", live)
	}
	if _, err := os.Stat(filepath.Join(skillDir, "tutor", "SKILL.md"+PendingSuffix)); err == nil {
		t.Error("pending file should be deleted on reject")
	}
}

func TestSkillPending_RejectsPathTraversal(t *testing.T) {
	skillDir := setupSkillDir(t)
	mp := NewSkillPendingSkill(skillDir)
	_, err := mp.Execute(context.Background(), map[string]any{"action": "approve", "name": "../etc/passwd"})
	if err == nil {
		t.Fatal("expected path traversal rejection")
	}
}

func TestSkillPending_UnknownAction(t *testing.T) {
	skillDir := setupSkillDir(t)
	mp := NewSkillPendingSkill(skillDir)
	_, err := mp.Execute(context.Background(), map[string]any{"action": "delete"})
	if err == nil {
		t.Fatal("expected error on unknown action")
	}
}
