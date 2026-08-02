package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAtomicWriteFileCreatesPrivateMissingParentDirectory(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "new-profile", "nested", "hexclaw.yaml")
	w := NewWriter(configPath)
	if err := w.AppendMCPServer("example", "stdio", "example", []string{"serve"}, nil, ""); err != nil {
		t.Fatalf("first config persistence must create its private parent: %v", err)
	}
	configInfo, err := os.Stat(configPath)
	if err != nil || !configInfo.Mode().IsRegular() {
		t.Fatalf("persisted config is not a regular file: info=%v err=%v", configInfo, err)
	}
	if runtime.GOOS != "windows" {
		if got := configInfo.Mode().Perm(); got != 0o600 {
			t.Fatalf("persisted config permissions=%#o want 0600", got)
		}
		info, err := os.Stat(filepath.Dir(configPath))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("new config parent permissions=%#o want 0700", got)
		}
	}
}
