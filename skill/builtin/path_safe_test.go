package builtin

import (
	"os"
	"path/filepath"
	"testing"
)

// 同前缀逃逸复现: workspace="/tmp/ws" 时 "/tmp/ws-evil/file" 不应通过
func TestResolveSafePath_PrefixEscape(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	evil := filepath.Join(base, "ws-evil")
	os.MkdirAll(ws, 0755)
	os.MkdirAll(evil, 0755)
	os.WriteFile(filepath.Join(evil, "secret.txt"), []byte("leaked"), 0644)

	// 绝对路径: /base/ws-evil/secret.txt 不在 /base/ws 内
	_, err := resolveSafePath(ws, filepath.Join(evil, "secret.txt"))
	if err == nil {
		t.Fatal("[BUG] prefix escape: ws-evil path accepted as inside ws")
	}
	if _, err := resolveSafePath(ws, evil); err == nil {
		t.Fatal("[BUG] prefix escape: ws-evil directory accepted as inside ws")
	}
}

func TestResolveSafePath_ValidPathsAccepted(t *testing.T) {
	ws := t.TempDir()
	// macOS: /var → /private/var symlink，需要解析后比较
	wsReal, _ := filepath.EvalSymlinks(ws)
	sub := filepath.Join(ws, "sub")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "file.txt"), []byte("ok"), 0644)

	// 绝对路径在 workspace 内
	p, err := resolveSafePath(ws, filepath.Join(sub, "file.txt"))
	if err != nil {
		t.Fatalf("should accept path inside workspace: %v", err)
	}
	wantAbs := filepath.Join(wsReal, "sub", "file.txt")
	if p != wantAbs {
		t.Errorf("resolved path = %q, want %q", p, wantAbs)
	}

	// 相对路径
	p2, err := resolveSafePath(ws, "sub/file.txt")
	if err != nil {
		t.Fatalf("should accept relative path: %v", err)
	}
	if p2 != wantAbs {
		t.Errorf("resolved = %q, want %q", p2, wantAbs)
	}

	// 空路径 → workspace 原始值（不解析 symlink）
	p3, err := resolveSafePath(ws, "")
	if err != nil {
		t.Fatalf("empty path should return workspace: %v", err)
	}
	if p3 != ws {
		t.Errorf("empty path = %q, want %q", p3, ws)
	}
}

func TestResolveSafePath_TraversalBlocked(t *testing.T) {
	ws := t.TempDir()

	tests := []string{
		"../../../etc/passwd",
		"sub/../../etc/hosts",
		"..secret",
	}
	for _, input := range tests {
		_, err := resolveSafePath(ws, input)
		if err == nil {
			t.Errorf("should reject traversal %q", input)
		}
	}
}

