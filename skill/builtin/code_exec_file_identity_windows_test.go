//go:build windows

package builtin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodeExecWindowsNoFollowHandleRejectsReparsePoint(t *testing.T) {
	rootPath := t.TempDir()
	writeCodeExecTestFile(t, filepath.Join(rootPath, "target.txt"), "target")
	if err := os.Symlink("target.txt", filepath.Join(rootPath, "link.txt")); err != nil {
		t.Skipf("Windows symbolic links are unavailable: %v", err)
	}
	root, _, err := openCodeExecRootNoFollow(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, err := openCodeExecRegularFileNoFollow(root, "link.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := snapshotCodeExecOpenedFile(file); err == nil {
		t.Fatal("Windows no-follow handle accepted a reparse point")
	}
}
