package main

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestLoopbackCloudGatewayUsesRemotePolicies(t *testing.T) {
	provider := config.LLMProviderConfig{
		APIKey:   "sk-test",
		BaseURL:  "http://localhost:18080/v1",
		Model:    "gpt-5.6-sol",
		Locality: config.ProviderLocalityCloud,
	}
	router := fakeRouter{
		defName: "openai",
		configs: map[string]config.LLMProviderConfig{"openai": provider},
	}
	if !isRemoteProvider(router, "openai") {
		t.Fatal("cron compiler must treat loopback cloud gateway as remote")
	}

	cfg := config.DefaultConfig()
	cfg.LLM.Default = "openai"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"openai": provider}
	if defaultProviderIsLocal(cfg) {
		t.Fatal("orchestration concurrency must not treat cloud-backed loopback gateway as local")
	}
	if isLocalEmbeddingProvider("openai", provider) {
		t.Fatal("embedding egress must not treat cloud-backed loopback gateway as local")
	}
}
