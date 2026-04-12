package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobSkill_Meta(t *testing.T) {
	tmp := t.TempDir()
	s := NewGlobSkill(tmp)
	if s.Name() != "glob" {
		t.Errorf("Name() = %q, want %q", s.Name(), "glob")
	}
	if s.Match("anything") {
		t.Error("Match() should always return false")
	}
}

func TestGlobSkill_SimplePattern(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "main.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(tmp, "util.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(tmp, "readme.md"), []byte("# Hello"), 0644)

	s := NewGlobSkill(tmp)
	result, err := s.Execute(context.Background(), map[string]any{
		"pattern": "*.go",
		"path":    tmp,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "Found 2 files") {
		t.Errorf("should find 2 .go files, got: %s", result.Content)
	}
	if strings.Contains(result.Content, "readme.md") {
		t.Error("should not include .md files")
	}
}

func TestGlobSkill_DoublestarPattern(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src", "pkg")
	os.MkdirAll(srcDir, 0755)
	os.WriteFile(filepath.Join(tmp, "main.go"), []byte("top"), 0644)
	os.WriteFile(filepath.Join(tmp, "src", "app.go"), []byte("src"), 0644)
	os.WriteFile(filepath.Join(srcDir, "util.go"), []byte("deep"), 0644)
	os.WriteFile(filepath.Join(srcDir, "data.json"), []byte("{}"), 0644)

	s := NewGlobSkill(tmp)
	result, err := s.Execute(context.Background(), map[string]any{
		"pattern": "**/*.go",
		"path":    tmp,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "Found 3 files") {
		t.Errorf("should find 3 .go files across dirs, got: %s", result.Content)
	}
	if strings.Contains(result.Content, "data.json") {
		t.Error("should not include .json files")
	}
}

func TestGlobSkill_NoMatches(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "test.txt"), []byte("data"), 0644)

	s := NewGlobSkill(tmp)
	result, err := s.Execute(context.Background(), map[string]any{
		"pattern": "*.xyz",
		"path":    tmp,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "No files found") {
		t.Errorf("should report no files, got: %s", result.Content)
	}
}

func TestGlobSkill_SkipsDotGit(t *testing.T) {
	tmp := t.TempDir()
	gitDir := filepath.Join(tmp, ".git", "objects")
	os.MkdirAll(gitDir, 0755)
	os.WriteFile(filepath.Join(gitDir, "pack.go"), []byte("hidden"), 0644)
	os.WriteFile(filepath.Join(tmp, "main.go"), []byte("visible"), 0644)

	s := NewGlobSkill(tmp)
	result, err := s.Execute(context.Background(), map[string]any{
		"pattern": "**/*.go",
		"path":    tmp,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "Found 1 files") {
		t.Errorf("should find only 1 file (not .git), got: %s", result.Content)
	}
}

func TestGlobSkill_EmptyPattern(t *testing.T) {
	tmp := t.TempDir()
	s := NewGlobSkill(tmp)
	_, err := s.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for empty pattern")
	}
}

func TestGlobSkill_NotADirectory(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "file.txt")
	os.WriteFile(path, []byte("data"), 0644)

	s := NewGlobSkill(tmp)
	_, err := s.Execute(context.Background(), map[string]any{
		"pattern": "*.txt",
		"path":    path,
	})
	if err == nil {
		t.Fatal("expected error for non-directory path")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error should mention directory, got: %v", err)
	}
}

func TestDoublestarMatch(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "src/main.go", true},
		{"**/*.go", "src/pkg/main.go", true},
		{"**/*.go", "main.txt", false},
		{"src/**/*.go", "src/main.go", true},
		{"src/**/*.go", "src/a/b/c.go", true},
		{"src/**/*.go", "other/main.go", false},
		{"*.go", "main.go", true},
		{"*.go", "src/main.go", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.path, func(t *testing.T) {
			got := doublestarMatch(tt.pattern, tt.path)
			if got != tt.want {
				t.Errorf("doublestarMatch(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

// Bug1 回归: workspace 外路径被拒绝
func TestGlobSkill_OutsideWorkspaceRejected(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.go"), []byte("package secret"), 0644)

	s := NewGlobSkill(ws)
	_, err := s.Execute(context.Background(), map[string]any{
		"pattern": "*.go",
		"path":    outside,
	})
	if err == nil {
		t.Fatal("[BUG1] glob should reject path outside workspace")
	}
	if !strings.Contains(err.Error(), "outside workspace") {
		t.Errorf("error should mention workspace, got: %v", err)
	}
}
