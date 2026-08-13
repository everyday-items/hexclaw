package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProviderCredentialRefP0_YAMLPersistsKeyAlongsideStableRef(t *testing.T) {
	provider := LLMProviderConfig{
		ProviderInstanceID: "pvd_v1_00112233445566778899aabbccddeeff",
		CredentialRef:      "llm_provider/pvd_v1_00112233445566778899aabbccddeeff/api_key",
		APIKey:             "sk-runtime-secret-must-not-persist",
		Model:              "chat",
	}
	raw, err := marshalConfigForPersistence(&Config{LLM: LLMConfig{
		Providers: map[string]LLMProviderConfig{"provider": provider},
	}})
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(raw)
	if !strings.Contains(encoded, provider.APIKey) {
		t.Fatal("provider API key was removed from YAML persistence")
	}
	if !strings.Contains(encoded, provider.CredentialRef) {
		t.Fatal("credential ref missing from YAML persistence")
	}
}

func TestProviderCredentialRefP0_SaveOwnerOnlyDefaultConfigPersistsKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDirectory := filepath.Join(home, ".hexclaw")
	if err := os.MkdirAll(configDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configDirectory, 0o755); err != nil {
		t.Fatal(err)
	}

	provider := LLMProviderConfig{
		ProviderInstanceID: "pvd_v1_00112233445566778899aabbccddeeff",
		CredentialRef:      "llm_provider/pvd_v1_00112233445566778899aabbccddeeff/api_key",
		APIKey:             "sk-yaml-persisted-owner-only",
		Model:              "chat",
	}
	cfg := &Config{LLM: LLMConfig{Providers: map[string]LLMProviderConfig{"provider": provider}}}
	if err := Save(cfg, ""); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(configDirectory, "hexclaw.yaml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), provider.APIKey) || !strings.Contains(string(raw), provider.CredentialRef) {
		t.Fatal("default YAML save did not retain the provider key and stable reference")
	}
	fileInfo, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("config file permission=%#o, want 0600", fileInfo.Mode().Perm())
	}
	directoryInfo, err := os.Stat(configDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("config directory permission=%#o, want 0700", directoryInfo.Mode().Perm())
	}
}

func TestProviderCredentialRefP0_WriterRetainsKeyAndRepairsDefaultDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDirectory := filepath.Join(home, ".hexclaw")
	if err := os.MkdirAll(configDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configDirectory, 0o755); err != nil {
		t.Fatal(err)
	}

	provider := LLMProviderConfig{
		ProviderInstanceID: "pvd_v1_00112233445566778899aabbccddeeff",
		CredentialRef:      "llm_provider/pvd_v1_00112233445566778899aabbccddeeff/api_key",
		APIKey:             "sk-writer-yaml-persisted",
		Model:              "chat",
	}
	configPath := filepath.Join(configDirectory, "hexclaw.yaml")
	writer := NewWriter(configPath)
	if err := writer.writeConfig(&Config{LLM: LLMConfig{Providers: map[string]LLMProviderConfig{"provider": provider}}}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), provider.APIKey) || !strings.Contains(string(raw), provider.CredentialRef) {
		t.Fatal("writer removed the provider key or stable reference from YAML")
	}
	fileInfo, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("writer config file permission=%#o, want 0600", fileInfo.Mode().Perm())
	}
	directoryInfo, err := os.Stat(configDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("writer config directory permission=%#o, want 0700", directoryInfo.Mode().Perm())
	}
}

func TestProviderCredentialRefP0_LegacyInlineKeyStillRoundTrips(t *testing.T) {
	provider := LLMProviderConfig{APIKey: "legacy-key", Model: "chat"}
	raw, err := yaml.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "legacy-key") {
		t.Fatalf("legacy provider key unexpectedly removed: %s", raw)
	}
}
