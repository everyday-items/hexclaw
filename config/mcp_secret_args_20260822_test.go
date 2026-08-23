package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/secret"
)

func TestMCPSecretArgsPersistEncryptedAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hexclaw.yaml")
	box, err := secret.LoadBox(dir)
	if err != nil {
		t.Fatalf("LoadBox: %v", err)
	}

	w := NewWriter(path)
	w.SetSecretBox(box)
	want := MCPServerConfig{
		Name:           "postgres",
		Transport:      "stdio",
		Command:        "npx",
		Args:           []string{"-y", "server-postgres", "postgresql://user:super-secret@localhost/db"},
		ArgsSecretRefs: map[int]string{2: "sidecar-connection:v1:postgres:password"},
		Env:            map[string]string{"PUBLIC_HOST": "localhost"},
		EnvSecretRefs:  map[string]string{},
		Enabled:        true,
	}
	if err := w.UpsertMCPServer(want); err != nil {
		t.Fatalf("UpsertMCPServer: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "super-secret") {
		t.Fatalf("MCP secret arg was persisted in plaintext: %s", text)
	}
	if !strings.Contains(text, "enc:v1:") {
		t.Fatalf("MCP secret arg did not use Box encryption: %s", text)
	}
	if !strings.Contains(text, "args_secret_refs") {
		t.Fatalf("MCP secret arg metadata is missing: %s", text)
	}

	loaded := NewWriter(path)
	loaded.SetSecretBox(box)
	got, err := loaded.GetMCPServer("postgres")
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	if got == nil || got.Args[2] != want.Args[2] {
		t.Fatalf("secret arg did not round-trip: got %#v", got)
	}
	if got.ArgsSecretRefs[2] != want.ArgsSecretRefs[2] {
		t.Fatalf("secret arg ref did not round-trip: got %#v", got.ArgsSecretRefs)
	}
}

func TestMCPSecretArgsFailClosedWithoutBox(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hexclaw.yaml")
	w := NewWriter(path)
	err := w.UpsertMCPServer(MCPServerConfig{
		Name:           "postgres",
		Transport:      "stdio",
		Command:        "npx",
		Args:           []string{"-y", "server-postgres", "postgresql://user:secret@localhost/db"},
		ArgsSecretRefs: map[int]string{2: "sidecar-connection:v1:postgres:password"},
		Enabled:        true,
	})
	if err == nil || !strings.Contains(err.Error(), "secret.Box") {
		t.Fatalf("expected fail-closed Box error, got %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("failed secret write must not create config, stat=%v", statErr)
	}
}
