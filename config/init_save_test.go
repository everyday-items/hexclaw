package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestInit_CreatesConfigInHomeDir verifies Init writes a default config template
// under ~/.hexclaw/ with restrictive permissions and a parseable body.
func TestInit_CreatesConfigInHomeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := Init()
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	want := filepath.Join(home, ".hexclaw", "hexclaw.yaml")
	if path != want {
		t.Errorf("Init path = %q, want %q", path, want)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config file not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("config file perm = %o, want 0600", perm)
	}

	// The generated template must be valid YAML and decode into a Config.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Errorf("generated template should be valid YAML: %v", err)
	}
}

// TestInit_RefusesWhenFileExists verifies Init does not clobber an existing config
// and reports the conflict.
func TestInit_RefusesWhenFileExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if _, err := Init(); err != nil {
		t.Fatalf("first Init should succeed: %v", err)
	}
	path, err := Init()
	if err == nil {
		t.Fatal("second Init should report file already exists")
	}
	// Path is still returned for the caller's reference.
	if path == "" {
		t.Error("expected existing path to be returned on conflict")
	}
}

// TestSave_WritesToExplicitPath verifies Save serializes a Config to the given file
// with 0600 permissions and round-trips back to equal field values.
func TestSave_WritesToExplicitPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "saved.yaml")

	cfg := DefaultConfig()
	cfg.Server.Port = 18080
	cfg.LLM.Default = "openai"

	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("saved file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("saved file perm = %o, want 0600", perm)
	}

	data, _ := os.ReadFile(path)
	var got Config
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("saved config should be valid YAML: %v", err)
	}
	if got.Server.Port != 18080 {
		t.Errorf("round-trip Port = %d, want 18080", got.Server.Port)
	}
	if got.LLM.Default != "openai" {
		t.Errorf("round-trip Default = %q, want openai", got.LLM.Default)
	}
}

// TestSave_EmptyPathUsesHomeDir verifies Save with an empty path creates the
// ~/.hexclaw directory and writes hexclaw.yaml there.
func TestSave_EmptyPathUsesHomeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := DefaultConfig()
	if err := Save(cfg, ""); err != nil {
		t.Fatalf("Save with empty path failed: %v", err)
	}
	want := filepath.Join(home, ".hexclaw", "hexclaw.yaml")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected config at %q: %v", want, err)
	}
}

// TestSaveLoadRoundTrip verifies a saved config reloads via Load with equal core
// fields, exercising the full Save -> file -> Load path.
func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"DEEPSEEK_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "QWEN_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "rt.yaml")

	cfg := DefaultConfig()
	cfg.Server.Port = 17777
	cfg.Server.MCPPort = 17778
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded.Server.Port != 17777 {
		t.Errorf("reloaded Port = %d, want 17777", loaded.Server.Port)
	}
	if loaded.Server.MCPPort != 17778 {
		t.Errorf("reloaded MCPPort = %d, want 17778", loaded.Server.MCPPort)
	}
}
