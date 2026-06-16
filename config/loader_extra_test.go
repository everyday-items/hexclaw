package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaskAPIKey verifies API key masking across length boundaries.
// Keys longer than 8 chars expose only the last 4; shorter (and empty)
// keys are fully masked; the empty string stays empty.
func TestMaskAPIKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"empty_stays_empty", "", ""},
		{"single_char_fully_masked", "a", "****"},
		{"len_8_boundary_fully_masked", "12345678", "****"},
		{"len_9_exposes_last_4", "123456789", "****6789"},
		{"long_key_exposes_last_4", "sk-proj-abcdefghijkl", "****ijkl"},
		{"exactly_at_boundary_plus_one", "sk-abcde", "****"}, // len 8
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MaskAPIKey(tt.key); got != tt.want {
				t.Errorf("MaskAPIKey(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

// TestIsMaskedKey verifies detection of already-masked keys by the **** prefix.
func TestIsMaskedKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"empty_not_masked", "", false},
		{"plain_key_not_masked", "sk-real-key", false},
		{"masked_prefix_only", "****", true},
		{"masked_with_tail", "****6789", true},
		{"three_stars_not_masked", "***6789", false},
		{"prefix_in_middle_not_masked", "sk-****", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsMaskedKey(tt.key); got != tt.want {
				t.Errorf("IsMaskedKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

// TestLoad_DefaultsWhenConfigFileMissing verifies that an empty configFile path
// falls back to ~/.hexclaw/hexclaw.yaml; when that file is absent the loader must
// return safe defaults plus env-derived providers without error.
func TestLoad_DefaultsWhenConfigFileMissing(t *testing.T) {
	// Redirect HOME so configDir() resolves to an empty temp directory (no yaml file).
	t.Setenv("HOME", t.TempDir())
	// Ensure no provider env keys leak in from the host environment.
	for _, k := range []string{"DEEPSEEK_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "QWEN_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(k, "")
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") with no config file should not error: %v", err)
	}
	if cfg.Server.Port != 16060 {
		t.Errorf("expected default port 16060, got %d", cfg.Server.Port)
	}
	if cfg.LLM.Default != "deepseek" {
		t.Errorf("expected default provider deepseek, got %q", cfg.LLM.Default)
	}
}

// TestLoad_EnvProviderAutoDetectedNoFile verifies that with no config file an env
// provider key is auto-detected and registered, exercising the missing-file branch
// of Load that calls applyEnvProviders.
func TestLoad_EnvProviderAutoDetectedNoFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"DEEPSEEK_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "QWEN_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("OPENAI_API_KEY", "sk-openai-from-env")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load should not error: %v", err)
	}
	p, ok := cfg.LLM.Providers["openai"]
	if !ok {
		t.Fatal("expected openai provider auto-added from env")
	}
	if p.APIKey != "sk-openai-from-env" {
		t.Errorf("expected env API key, got %q", p.APIKey)
	}
}

// TestLoad_ExpandsEnvVarsInFile verifies ${VAR} substitution happens before YAML parse.
func TestLoad_ExpandsEnvVarsInFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TEST_HEX_PROVIDER_KEY", "sk-expanded-value")

	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "hexclaw.yaml")
	content := `
server:
  host: "127.0.0.1"
  port: 16060
llm:
  default: "openai"
  providers:
    openai:
      api_key: "${TEST_HEX_PROVIDER_KEY}"
      model: "gpt-4o"
`
	if err := os.WriteFile(cfgFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.LLM.Providers["openai"].APIKey != "sk-expanded-value" {
		t.Errorf("expected ${VAR} expanded, got %q", cfg.LLM.Providers["openai"].APIKey)
	}
}

// TestLoad_InvalidYAMLReturnsError verifies malformed YAML surfaces a parse error.
func TestLoad_InvalidYAMLReturnsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "hexclaw.yaml")
	// Tab indentation + broken structure is invalid YAML.
	if err := os.WriteFile(cfgFile, []byte("server:\n\tport: : : ]["), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(cfgFile); err == nil {
		t.Fatal("expected parse error on malformed YAML")
	}
}

// TestLoad_ValidationFailureSurfacesFieldAndPath verifies that a config which
// parses but violates Validate() returns an error mentioning both the offending
// field and the config file path (fail-fast on startup).
func TestLoad_ValidationFailureSurfacesFieldAndPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "hexclaw.yaml")
	content := `
server:
  host: "127.0.0.1"
  port: 999999
  mcp_port: 16070
`
	if err := os.WriteFile(cfgFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfgFile)
	if err == nil {
		t.Fatal("expected validation error for out-of-range port")
	}
	if !strings.Contains(err.Error(), "server.port") {
		t.Errorf("error should mention server.port: %v", err)
	}
	if !strings.Contains(err.Error(), cfgFile) {
		t.Errorf("error should mention config file path %q: %v", cfgFile, err)
	}
}

// TestLoad_ExplicitMissingFileReturnsDefaults verifies that a non-empty but
// nonexistent path returns defaults without error (IsNotExist branch).
func TestLoad_ExplicitMissingFileReturnsDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	cfg, err := Load(missing)
	if err != nil {
		t.Fatalf("missing explicit file should not error: %v", err)
	}
	if cfg.Server.MCPPort != 16070 {
		t.Errorf("expected default MCPPort 16070, got %d", cfg.Server.MCPPort)
	}
}

// TestApplyEnvProviders_AllRecognizedKeys verifies every known env provider key is
// detected and seeded with its conventional default model.
func TestApplyEnvProviders_AllRecognizedKeys(t *testing.T) {
	for _, k := range []string{"DEEPSEEK_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "QWEN_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("DEEPSEEK_API_KEY", "k1")
	t.Setenv("OPENAI_API_KEY", "k2")
	t.Setenv("ANTHROPIC_API_KEY", "k3")
	t.Setenv("QWEN_API_KEY", "k4")
	t.Setenv("GEMINI_API_KEY", "k5")

	cfg := &Config{} // nil Providers map exercises lazy init branch
	applyEnvProviders(cfg)

	wantModels := map[string]string{
		"deepseek":  "deepseek-chat",
		"openai":    "gpt-4o-mini",
		"anthropic": "claude-sonnet-4-20250514",
		"qwen":      "qwen-plus",
		"gemini":    "gemini-2.0-flash",
	}
	for name, model := range wantModels {
		p, ok := cfg.LLM.Providers[name]
		if !ok {
			t.Errorf("provider %q should be auto-added", name)
			continue
		}
		if p.Model != model {
			t.Errorf("provider %q default model = %q, want %q", name, p.Model, model)
		}
	}
}

// TestApplyEnvProviders_DefaultFallbackDeterministic verifies that when the
// configured Default provider is absent, the first provider by sorted name is
// chosen deterministically.
func TestApplyEnvProviders_DefaultFallbackDeterministic(t *testing.T) {
	for _, k := range []string{"DEEPSEEK_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "QWEN_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("OPENAI_API_KEY", "k2")
	t.Setenv("ANTHROPIC_API_KEY", "k3")

	cfg := DefaultConfig()
	cfg.LLM.Default = "deepseek" // not provided via env
	applyEnvProviders(cfg)

	// Sorted names: anthropic < openai -> default becomes "anthropic".
	if cfg.LLM.Default != "anthropic" {
		t.Errorf("expected deterministic default 'anthropic', got %q", cfg.LLM.Default)
	}
}

// TestApplyEnvProviders_DoesNotOverrideExistingKey verifies an existing provider
// with a non-empty API key is left untouched even if the env var differs.
func TestApplyEnvProviders_DoesNotOverrideExistingKey(t *testing.T) {
	for _, k := range []string{"DEEPSEEK_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "QWEN_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(k, "")
	}
	t.Setenv("DEEPSEEK_API_KEY", "sk-env")

	cfg := DefaultConfig()
	cfg.LLM.Providers["deepseek"] = LLMProviderConfig{APIKey: "sk-explicit", Model: "deepseek-chat"}
	applyEnvProviders(cfg)

	if cfg.LLM.Providers["deepseek"].APIKey != "sk-explicit" {
		t.Errorf("explicit API key must not be overridden by env, got %q", cfg.LLM.Providers["deepseek"].APIKey)
	}
}

// TestExpandTildePaths verifies ~ prefixes in storage and file-memory paths expand
// to the user home, while non-tilde and bare-segment paths are preserved.
func TestExpandTildePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := DefaultConfig() // Storage path and FileMemory dir both start with "~/"
	expandTildePaths(cfg)

	wantDB := filepath.Join(home, ".hexclaw/data.db")
	if cfg.Storage.SQLite.Path != wantDB {
		t.Errorf("SQLite path = %q, want %q", cfg.Storage.SQLite.Path, wantDB)
	}
	wantMem := filepath.Join(home, ".hexclaw/memory")
	if cfg.FileMemory.Dir != wantMem {
		t.Errorf("FileMemory dir = %q, want %q", cfg.FileMemory.Dir, wantMem)
	}
}

// TestExpandTildePaths_NonTildePreserved verifies absolute and bare paths pass through.
func TestExpandTildePaths_NonTildePreserved(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := DefaultConfig()
	cfg.Storage.SQLite.Path = "/var/lib/hexclaw/data.db"
	cfg.FileMemory.Dir = "relative/memory"
	expandTildePaths(cfg)

	if cfg.Storage.SQLite.Path != "/var/lib/hexclaw/data.db" {
		t.Errorf("absolute path should be unchanged, got %q", cfg.Storage.SQLite.Path)
	}
	if cfg.FileMemory.Dir != "relative/memory" {
		t.Errorf("relative path should be unchanged, got %q", cfg.FileMemory.Dir)
	}
}

// TestExpandTildePaths_BareTilde verifies a lone "~" expands to home exactly.
func TestExpandTildePaths_BareTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := DefaultConfig()
	cfg.Storage.SQLite.Path = "~"
	expandTildePaths(cfg)

	if cfg.Storage.SQLite.Path != home {
		t.Errorf("bare ~ should expand to home %q, got %q", home, cfg.Storage.SQLite.Path)
	}
}
