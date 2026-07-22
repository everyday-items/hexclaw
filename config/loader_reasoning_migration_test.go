package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_MigratesStaleReasoningPairWithoutRewritingSource(t *testing.T) {
	t.Setenv("HEXCLAW_TEST_REASONING_KEY", "test-only-expanded-value")
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	content := `
llm:
  default: hexclaw-gpt
  reasoning_provider: openai
  reasoning_model: gpt-5.6-sol
  providers:
    hexclaw-gpt:
      api_key: "${HEXCLAW_TEST_REASONING_KEY}"
      base_url: http://127.0.0.1:18080/v1
      model: gpt-5.6-sol
      locality: cloud
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("stale optional reasoning selection must not block startup: %v", err)
	}
	if cfg.LLM.ReasoningProvider != "hexclaw-gpt" {
		t.Fatalf("reasoning provider=%q, want safely re-derived hexclaw-gpt", cfg.LLM.ReasoningProvider)
	}
	if cfg.LLM.ReasoningModel != "" {
		t.Fatalf("reasoning model=%q, want stale provider/model pair cleared before re-derivation", cfg.LLM.ReasoningModel)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != content {
		t.Fatal("Load must not rewrite the source file after environment expansion")
	}
}

func TestLoad_ClearsDisabledReasoningPairWithoutUnsafeFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	content := `
llm:
  default: ollama
  reasoning_provider: openai
  reasoning_model: gpt-5.6-sol
  providers:
    ollama:
      base_url: http://127.0.0.1:11434/v1
      model: qwen3.5:9b
      locality: local
    openai:
      api_key: test-only-disabled-value
      model: gpt-5.6-sol
      enabled: false
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("disabled stale optional reasoning selection must not block startup: %v", err)
	}
	if cfg.LLM.ReasoningProvider != "" || cfg.LLM.ReasoningModel != "" {
		t.Fatalf("reasoning pair=(%q,%q), want empty when no safe replacement exists",
			cfg.LLM.ReasoningProvider, cfg.LLM.ReasoningModel)
	}
}

func TestLoad_ReasoningCompatibilityMigrationDoesNotMaskRequiredValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	content := `
server:
  port: 0
llm:
  default: ollama
  reasoning_provider: removed-provider
  reasoning_model: removed-model
  providers:
    ollama:
      base_url: http://127.0.0.1:11434/v1
      model: qwen3.5:9b
      locality: local
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("invalid required server.port must still fail closed")
	}
	if !strings.Contains(err.Error(), "server.port") {
		t.Fatalf("error=%v, want required server.port validation", err)
	}
	if strings.Contains(err.Error(), "llm.reasoning_provider") {
		t.Fatalf("error=%v, stale optional reasoning selection was not migrated at Load boundary", err)
	}
}
