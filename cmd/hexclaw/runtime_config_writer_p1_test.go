package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeConfigPathHonorsExplicitServeConfig(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "custom.yaml")
	got, err := runtimeConfigPath(explicit)
	if err != nil {
		t.Fatalf("resolve explicit config path: %v", err)
	}
	if got != explicit {
		t.Fatalf("runtime config path = %q, want explicit %q", got, explicit)
	}
}

func TestRuntimeConfigPathUsesDefaultOnlyWhenServeConfigIsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := runtimeConfigPath("")
	if err != nil {
		t.Fatalf("resolve default config path: %v", err)
	}
	want := filepath.Join(home, ".hexclaw", "hexclaw.yaml")
	if got != want {
		t.Fatalf("runtime config path = %q, want %q", got, want)
	}
}

func TestRuntimeConfigWriterPersistsOnlyToExplicitServeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	explicit := filepath.Join(t.TempDir(), "profiles", "parent-tutor.yaml")

	writer, err := newRuntimeConfigWriter(explicit, nil)
	if err != nil {
		t.Fatalf("create runtime config writer: %v", err)
	}
	if err := writer.AppendMCPServer("writer-path-proof", "stdio", "proof", []string{"--stdio"}, nil, ""); err != nil {
		t.Fatalf("persist MCP server through runtime writer: %v", err)
	}
	data, err := os.ReadFile(explicit)
	if err != nil {
		t.Fatalf("read explicit config: %v", err)
	}
	if !strings.Contains(string(data), "writer-path-proof") {
		t.Fatalf("explicit config is missing persisted server: %s", data)
	}
	defaultPath := filepath.Join(home, ".hexclaw", "hexclaw.yaml")
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Fatalf("default config was unexpectedly mutated: %v", err)
	}
}
