package llmrouter

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestProviderLocality_LoopbackCloudGatewayIsNotLocal(t *testing.T) {
	cfg := config.LLMProviderConfig{
		APIKey:   "sk-test",
		BaseURL:  "http://localhost:18080/v1",
		Model:    "gpt-5.6-sol",
		Locality: config.ProviderLocalityCloud,
	}
	if isLocalProvider(cfg) {
		t.Fatal("explicit cloud gateway on loopback must not bypass cloud routing/egress policy")
	}

	selector, err := New(config.LLMConfig{
		Default: "openai",
		Providers: map[string]config.LLMProviderConfig{
			"openai": cfg,
		},
	})
	if err != nil {
		t.Fatalf("New selector: %v", err)
	}
	if selector.IsLocalProviderName("openai") {
		t.Fatal("selector must preserve explicit cloud locality")
	}
}

func TestProviderLocality_ExplicitLANLocalIsLocal(t *testing.T) {
	cfg := config.LLMProviderConfig{
		BaseURL:  "http://192.168.1.20:8000/v1",
		Model:    "local-model",
		Locality: config.ProviderLocalityLocal,
	}
	if !isLocalProvider(cfg) {
		t.Fatal("explicit LAN-local deployment must receive local resource policy")
	}
}
