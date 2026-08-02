//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteFileReplacesExistingTargetOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("atomicWriteFile must replace an existing Windows target without deleting it first: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("target content = %q, want new", got)
	}
}
