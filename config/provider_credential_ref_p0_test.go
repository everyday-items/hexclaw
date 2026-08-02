package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProviderCredentialRefP0_YAMLNeverPersistsHydratedSecret(t *testing.T) {
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
	if strings.Contains(encoded, provider.APIKey) {
		t.Fatalf("hydrated secret persisted in YAML: %s", encoded)
	}
	if !strings.Contains(encoded, provider.CredentialRef) {
		t.Fatalf("credential ref missing from YAML: %s", encoded)
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
