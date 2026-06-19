package builtin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFileEditSkill_Meta(t *testing.T) {
	s := NewFileEditSkill(t.TempDir())
	if s.Name() != "file_edit" {
		t.Errorf("Name() = %q, want %q", s.Name(), "file_edit")
	}
	if s.Description() == "" {
		t.Error("Description() should not be empty")
	}
	td := s.ToolDefinition()
	if td.Function.Name != "file_edit" {
		t.Errorf("ToolDefinition().Function.Name = %q, want %q", td.Function.Name, "file_edit")
	}
	if s.Match("anything") {
		t.Error("Match() should always return false")
	}
}

func TestFileEditSkill_UniqueReplacement(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, "config.yaml")
	os.WriteFile(path, []byte("port: 8080\nhost: localhost\ntimeout: 30\n"), 0644)

	s := NewFileEditSkill(ws)
	result, err := s.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "port: 8080",
		"new_string": "port: 9090",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "Replaced 1 occurrence") {
		t.Errorf("got %q", result.Content)
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "port: 9090") {
		t.Errorf("file should contain new value, got: %s", data)
	}
	if strings.Contains(string(data), "port: 8080") {
		t.Error("file should not contain old value")
	}
}

func TestFileEditSkill_NotFound(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, "test.txt")
	os.WriteFile(path, []byte("line one\nline two\n"), 0644)

	s := NewFileEditSkill(ws)
	_, err := s.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "nonexistent text",
		"new_string": "replacement",
	})
	if err == nil {
		t.Fatal("expected error for missing old_string")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}

func TestFileEditSkill_MultipleMatches(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, "test.txt")
	os.WriteFile(path, []byte("foo bar foo baz foo\n"), 0644)

	s := NewFileEditSkill(ws)
	_, err := s.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "foo",
		"new_string": "qux",
	})
	if err == nil {
		t.Fatal("expected error for multiple matches")
	}
	if !strings.Contains(err.Error(), "3 times") {
		t.Errorf("error should mention match count, got: %v", err)
	}
}

func TestFileEditSkill_IdenticalStrings(t *testing.T) {
	ws := t.TempDir()
	s := NewFileEditSkill(ws)
	_, err := s.Execute(context.Background(), map[string]any{
		"file_path":  filepath.Join(ws, "test.txt"),
		"old_string": "same",
		"new_string": "same",
	})
	if err == nil {
		t.Fatal("expected error for identical strings")
	}
	if !strings.Contains(err.Error(), "identical") {
		t.Errorf("error should mention 'identical', got: %v", err)
	}
}

func TestFileEditSkill_MissingFilePath(t *testing.T) {
	s := NewFileEditSkill(t.TempDir())
	_, err := s.Execute(context.Background(), map[string]any{
		"old_string": "old",
		"new_string": "new",
	})
	if err == nil {
		t.Fatal("expected error for missing file_path")
	}
	if !strings.Contains(err.Error(), "file_path") {
		t.Errorf("error should mention 'file_path', got: %v", err)
	}
}

func TestFileEditSkill_NonexistentFile(t *testing.T) {
	ws := t.TempDir()
	s := NewFileEditSkill(ws)
	_, err := s.Execute(context.Background(), map[string]any{
		"file_path":  filepath.Join(ws, "definitely-not-exists.txt"),
		"old_string": "old",
		"new_string": "new",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !strings.Contains(err.Error(), "cannot stat") {
		t.Errorf("error should mention stat failure, got: %v", err)
	}
}

func TestFileEditSkill_EmptyOldString(t *testing.T) {
	ws := t.TempDir()
	s := NewFileEditSkill(ws)
	_, err := s.Execute(context.Background(), map[string]any{
		"file_path":  filepath.Join(ws, "test.txt"),
		"old_string": "",
		"new_string": "new",
	})
	if err == nil {
		t.Fatal("expected error for empty old_string")
	}
	if !strings.Contains(err.Error(), "old_string") {
		t.Errorf("error should mention 'old_string', got: %v", err)
	}
}

// F.2 验证: 编辑后保留原文件权限
func TestFileEditSkill_PreservesPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix 文件权限位 (0755) 在 Windows 上无对应语义, 本断言仅适用于类 Unix 系统")
	}
	ws := t.TempDir()
	path := filepath.Join(ws, "script.sh")
	os.WriteFile(path, []byte("#!/bin/bash\necho hello\n"), 0755)

	s := NewFileEditSkill(ws)
	_, err := s.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "echo hello",
		"new_string": "echo world",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("cannot stat: %v", err)
	}
	if fi.Mode().Perm() != 0755 {
		t.Errorf("permissions should be 0755, got %o", fi.Mode().Perm())
	}
}

// F.3 验证: 超大文件拒绝编辑
func TestFileEditSkill_LargeFileRejected(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, "huge.txt")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("cannot create: %v", err)
	}
	f.Truncate(11 * 1024 * 1024) // 11MB
	f.Close()

	s := NewFileEditSkill(ws)
	_, err = s.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "old",
		"new_string": "new",
	})
	if err == nil {
		t.Fatal("expected error for large file")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error should mention 'too large', got: %v", err)
	}
}

// 验证: 二进制文件行为
func TestFileEditSkill_BinaryFile(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, "data.bin")
	content := []byte("header\x00\x00\x00marker\x00\x00tail")
	os.WriteFile(path, content, 0644)

	s := NewFileEditSkill(ws)
	_, err := s.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "marker",
		"new_string": "REPLACED",
	})
	if err != nil {
		t.Fatalf("unexpected error on binary file: %v", err)
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "REPLACED") {
		t.Error("replacement should work even in binary-like file")
	}
}

// 验证: workspace 外的路径被拒绝
func TestFileEditSkill_OutsideWorkspaceRejected(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(outside, "secret.txt")
	os.WriteFile(path, []byte("secret data\n"), 0644)

	s := NewFileEditSkill(ws)
	_, err := s.Execute(context.Background(), map[string]any{
		"file_path":  path,
		"old_string": "secret",
		"new_string": "public",
	})
	if err == nil {
		t.Fatal("expected error for path outside workspace")
	}
	if !strings.Contains(err.Error(), "outside workspace") {
		t.Errorf("error should mention 'outside workspace', got: %v", err)
	}
}

// 验证: 路径穿越被拒绝
func TestFileEditSkill_TraversalRejected(t *testing.T) {
	ws := t.TempDir()
	s := NewFileEditSkill(ws)
	_, err := s.Execute(context.Background(), map[string]any{
		"file_path":  "../../../etc/passwd",
		"old_string": "root",
		"new_string": "hacked",
	})
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
}
