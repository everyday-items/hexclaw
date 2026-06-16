package builtin

// BUG-20260616: safePath used strings.HasPrefix(resolved, wsResolved) with no
// trailing separator, so a workspace "/x/ws" treated a sibling "/x/ws-evil" as
// inside the workspace. A symlink to the sibling escaped the sandbox.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBug20260616_FileOpsSymlinkSiblingEscape(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	evil := filepath.Join(base, "ws-evil")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evil, "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, filepath.Join(ws, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	s := NewFileOpsSkill(ws)
	if _, err := s.safePath("link/secret"); err == nil {
		t.Fatal("safePath allowed escape into sibling dir via symlink (missing path-separator boundary)")
	}
}
