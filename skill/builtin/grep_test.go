package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepSkill_Meta(t *testing.T) {
	tmp := t.TempDir()
	s := NewGrepSkill(tmp)
	if s.Name() != "grep" {
		t.Errorf("Name() = %q, want %q", s.Name(), "grep")
	}
	if s.Match("anything") {
		t.Error("Match() should always return false")
	}
}

func TestGrepSkill_BasicSearch(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "main.go"), []byte("package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"), 0644)
	os.WriteFile(filepath.Join(tmp, "util.go"), []byte("package main\n\nfunc helper() string {\n\treturn \"world\"\n}\n"), 0644)

	s := NewGrepSkill(tmp)
	result, err := s.Execute(context.Background(), map[string]any{
		"pattern": "func",
		"path":    tmp,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "func main()") {
		t.Errorf("should find func main(), got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "func helper()") {
		t.Errorf("should find func helper(), got: %s", result.Content)
	}
}

func TestGrepSkill_RegexPattern(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "data.txt"), []byte("error: bad input\nwarn: slow query\nerror: timeout\ninfo: ok\n"), 0644)

	s := NewGrepSkill(tmp)
	result, err := s.Execute(context.Background(), map[string]any{
		"pattern": "^error:",
		"path":    tmp,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "Found 2 matches") {
		t.Errorf("should find 2 matches, got: %s", result.Content)
	}
}

func TestGrepSkill_GlobFilter(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "main.go"), []byte("func main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmp, "main.py"), []byte("def main():\n    pass\n"), 0644)

	s := NewGrepSkill(tmp)
	result, err := s.Execute(context.Background(), map[string]any{
		"pattern": "main",
		"path":    tmp,
		"glob":    "*.go",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "main.go") {
		t.Errorf("should match .go file, got: %s", result.Content)
	}
	if strings.Contains(result.Content, "main.py") {
		t.Errorf("should not match .py file with *.go glob")
	}
}

func TestGrepSkill_NoMatches(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "test.txt"), []byte("hello world\n"), 0644)

	s := NewGrepSkill(tmp)
	result, err := s.Execute(context.Background(), map[string]any{
		"pattern": "nonexistent_string_xyz",
		"path":    tmp,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "No matches") {
		t.Errorf("should report no matches, got: %s", result.Content)
	}
}

func TestGrepSkill_InvalidRegex(t *testing.T) {
	tmp := t.TempDir()
	s := NewGrepSkill(tmp)
	_, err := s.Execute(context.Background(), map[string]any{
		"pattern": "[invalid",
	})
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
	if !strings.Contains(err.Error(), "invalid regex") {
		t.Errorf("error should mention regex, got: %v", err)
	}
}

func TestGrepSkill_SkipsDotGit(t *testing.T) {
	tmp := t.TempDir()
	gitDir := filepath.Join(tmp, ".git")
	os.MkdirAll(gitDir, 0755)
	os.WriteFile(filepath.Join(gitDir, "config"), []byte("secret_token=abc123\n"), 0644)
	os.WriteFile(filepath.Join(tmp, "main.go"), []byte("package main\n"), 0644)

	s := NewGrepSkill(tmp)
	result, err := s.Execute(context.Background(), map[string]any{
		"pattern": "secret_token",
		"path":    tmp,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "No matches") {
		t.Errorf("should not search .git directory, got: %s", result.Content)
	}
}

func TestGrepSkill_MaxResults(t *testing.T) {
	tmp := t.TempDir()
	var lines strings.Builder
	for i := 0; i < 50; i++ {
		lines.WriteString("match line\n")
	}
	os.WriteFile(filepath.Join(tmp, "big.txt"), []byte(lines.String()), 0644)

	s := NewGrepSkill(tmp)
	result, err := s.Execute(context.Background(), map[string]any{
		"pattern":     "match",
		"path":        tmp,
		"max_results": float64(5),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "truncated") {
		t.Errorf("should indicate truncation, got: %s", result.Content)
	}
}

func TestGrepSkill_EmptyPattern(t *testing.T) {
	tmp := t.TempDir()
	s := NewGrepSkill(tmp)
	_, err := s.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for empty pattern")
	}
}

func TestGrepSkill_SingleFile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.txt")
	os.WriteFile(path, []byte("alpha\nbeta\ngamma\nalpha again\n"), 0644)

	s := NewGrepSkill(tmp)
	result, err := s.Execute(context.Background(), map[string]any{
		"pattern": "alpha",
		"path":    path,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "Found 2 matches") {
		t.Errorf("should find 2 matches in single file, got: %s", result.Content)
	}
}

// 未验证项: symlink 循环不会导致无限递归
func TestGrepSkill_SymlinkLoop(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "real.txt"), []byte("findme\n"), 0644)

	// 创建 symlink 循环: tmp/loop -> tmp
	loopPath := filepath.Join(tmp, "loop")
	err := os.Symlink(tmp, loopPath)
	if err != nil {
		t.Skip("cannot create symlink on this platform")
	}

	s := NewGrepSkill(tmp)
	// filepath.WalkDir 默认不跟 symlink，所以不会无限递归
	result, err := s.Execute(context.Background(), map[string]any{
		"pattern": "findme",
		"path":    tmp,
	})
	if err != nil {
		t.Fatalf("should not error on symlink loop: %v", err)
	}
	// 应该找到 real.txt 中的 match，但不会递归进 loop/
	if !strings.Contains(result.Content, "findme") {
		t.Errorf("should find match in real.txt, got: %s", result.Content)
	}
}

// Bug1 回归: workspace 外路径被拒绝
func TestGrepSkill_OutsideWorkspaceRejected(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("password=hunter2\n"), 0644)

	s := NewGrepSkill(ws)
	_, err := s.Execute(context.Background(), map[string]any{
		"pattern": "password",
		"path":    outside,
	})
	if err == nil {
		t.Fatal("[BUG1] grep should reject path outside workspace")
	}
	if !strings.Contains(err.Error(), "outside workspace") {
		t.Errorf("error should mention workspace, got: %v", err)
	}
}
