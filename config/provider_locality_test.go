package config

import (
	"strings"
	"testing"
)

func TestIsLocalLLMProvider_ExplicitLocalityOverridesEndpointHeuristic(t *testing.T) {
	tests := []struct {
		name string
		cfg  LLMProviderConfig
		want bool
	}{
		{
			name: "auto loopback is local",
			cfg:  LLMProviderConfig{BaseURL: "http://127.0.0.1:11434/v1"},
			want: true,
		},
		{
			name: "loopback cloud gateway stays cloud",
			cfg: LLMProviderConfig{
				BaseURL:  "http://localhost:18080/v1",
				Locality: ProviderLocalityCloud,
			},
			want: false,
		},
		{
			name: "lan deployment can be declared local",
			cfg: LLMProviderConfig{
				BaseURL:  "http://192.168.1.20:8000/v1",
				Locality: ProviderLocalityLocal,
			},
			want: true,
		},
		{
			name: "public endpoint is cloud by default",
			cfg:  LLMProviderConfig{BaseURL: "https://api.openai.com/v1"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsLocalLLMProvider(tt.cfg); got != tt.want {
				t.Fatalf("IsLocalLLMProvider(%+v) = %v, want %v", tt.cfg, got, tt.want)
			}
		})
	}
}

func TestIsLocalLLMProviderNamed_LegacyOllamaFallbackOnlyAppliesWithoutEndpoint(t *testing.T) {
	if !IsLocalLLMProviderNamed("Ollama (本地)", LLMProviderConfig{}) {
		t.Fatal("endpoint-less legacy Ollama provider must remain local")
	}
	if IsLocalLLMProviderNamed("Ollama Cloud", LLMProviderConfig{BaseURL: "https://ollama.example.com/v1"}) {
		t.Fatal("a public endpoint must override the legacy name fallback")
	}
	if IsLocalLLMProviderNamed("Ollama (本地)", LLMProviderConfig{Locality: ProviderLocalityCloud}) {
		t.Fatal("explicit cloud locality must override the legacy name fallback")
	}
}

func TestValidate_RejectsUnknownProviderLocality(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LLM.Providers = map[string]LLMProviderConfig{
		"openai": {
			BaseURL:  "http://localhost:18080/v1",
			Model:    "gpt-5.6-sol",
			Locality: "somewhere",
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("unknown provider locality must be rejected")
	}
	if !strings.Contains(err.Error(), "locality") {
		t.Fatalf("validation error must identify locality: %v", err)
	}
}

func TestApplyReasoningDefault_LoopbackCloudGatewayIsEligible(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Providers = map[string]LLMProviderConfig{
		"openai": {
			APIKey:   "sk-test",
			BaseURL:  "http://localhost:18080/v1",
			Model:    "gpt-5.6-sol",
			Locality: ProviderLocalityCloud,
		},
	}
	chosen, applied := cfg.ApplyReasoningDefault()
	if !applied || chosen != "openai" {
		t.Fatalf("loopback cloud gateway must remain eligible, chosen=%q applied=%v", chosen, applied)
	}
}
