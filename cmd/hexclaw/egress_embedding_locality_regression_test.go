package main

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestEmbeddingEgressLocalityRequiresDeclaredLocality(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		baseURL  string
		locality string
		want     bool
	}{
		{name: "public hostname containing localhost", provider: "remote", baseURL: "https://localhost.evil.example/v1", want: false},
		{name: "public hostname containing loopback text", provider: "remote", baseURL: "https://127.0.0.1.evil.example/v1", want: false},
		{name: "public path containing localhost", provider: "remote", baseURL: "https://api.example/v1/localhost", want: false},
		{name: "public query containing loopback", provider: "remote", baseURL: "https://api.example/v1?upstream=http://127.0.0.1:11434", want: false},
		{name: "public hosted provider named ollama", provider: "ollama", baseURL: "https://ollama.example/v1", want: false},
		{name: "undeclared localhost gateway", provider: "remote", baseURL: "http://localhost:11434/v1", want: false},
		{name: "declared local ipv4 gateway", provider: "remote", baseURL: "http://127.42.0.9:11434/v1", locality: config.ProviderLocalityLocal, want: true},
		{name: "declared local ipv6 gateway", provider: "remote", baseURL: "http://[::1]:11434/v1", locality: config.ProviderLocalityLocal, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isLocalEmbeddingProvider(tt.provider, config.LLMProviderConfig{BaseURL: tt.baseURL, Locality: tt.locality})
			if got != tt.want {
				t.Fatalf("isLocalEmbeddingProvider(%q, %q)=%v, want %v", tt.provider, tt.baseURL, got, tt.want)
			}
		})
	}
}
