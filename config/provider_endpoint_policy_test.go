package config

import (
	"strings"
	"testing"
)

func TestProviderLocality_AutoLoopbackIsCloudSafeUnlessItIsBuiltInOllama(t *testing.T) {
	if IsLocalLLMProviderNamed("openai", LLMProviderConfig{
		BaseURL:  "http://localhost:18080/v1",
		Locality: ProviderLocalityAuto,
	}) {
		t.Fatal("non-Ollama loopback endpoint is ambiguous and must default to cloud-safe")
	}

	if !IsLocalLLMProviderNamed("ollama", LLMProviderConfig{
		BaseURL:  "http://localhost:11434/v1",
		Locality: ProviderLocalityAuto,
	}) {
		t.Fatal("built-in Ollama loopback endpoint must remain local")
	}
}

func TestProviderEndpointPolicy_BlockedAndPrivateHostsFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		access  ProviderPrivateNetworkAccess
		wantErr bool
	}{
		{name: "public", baseURL: "https://api.example.com/v1"},
		{name: "nested path", baseURL: "https://api.example.com/custom/openai/v1"},
		{name: "query", baseURL: "https://api.example.com/v1?tenant=private", wantErr: true},
		{name: "empty query marker", baseURL: "https://api.example.com/v1?", wantErr: true},
		{name: "fragment", baseURL: "https://api.example.com/v1#embeddings", wantErr: true},
		{name: "public plaintext", baseURL: "http://api.example.com/v1", wantErr: true},
		{name: "loopback", baseURL: "http://localhost:18080/v1"},
		{name: "userinfo", baseURL: "https://user:secret@api.example.com/v1", wantErr: true},
		{name: "metadata ip", baseURL: "http://169.254.169.254/latest", wantErr: true},
		{name: "metadata host", baseURL: "http://metadata.google.internal/", wantErr: true},
		{name: "unspecified", baseURL: "http://0.0.0.0:8000/v1", wantErr: true},
		{name: "ipv4 multicast", baseURL: "http://224.0.0.1:8000/v1", wantErr: true},
		{name: "ipv6 multicast", baseURL: "http://[ff02::1]:8000/v1", wantErr: true},
		{name: "carrier grade nat", baseURL: "http://100.100.100.200:8000/v1", wantErr: true},
		{name: "ipv4 mapped metadata", baseURL: "http://[::ffff:169.254.169.254]/latest", wantErr: true},
		{name: "benchmark", baseURL: "http://198.18.0.1:8000/v1", wantErr: true},
		{name: "documentation", baseURL: "http://203.0.113.1:8000/v1", wantErr: true},
		{name: "reserved", baseURL: "http://240.0.0.1:8000/v1", wantErr: true},
		{name: "ipv6 documentation", baseURL: "http://[2001:db8::1]:8000/v1", wantErr: true},
		{name: "ipv6 deprecated site local", baseURL: "https://[fec0::1]/v1", wantErr: true},
		{name: "private missing authorization", baseURL: "http://10.0.0.8:8080/v1", wantErr: true},
		{
			name:    "private exact authorization",
			baseURL: "http://10.0.0.8:8080/v1",
			access:  ProviderPrivateNetworkAccess{Host: "10.0.0.8", Allowed: true},
		},
		{
			name:    "private hostname plaintext authorization",
			baseURL: "http://embedding.lan.example:8080/v1",
			access:  ProviderPrivateNetworkAccess{Host: "embedding.lan.example", Allowed: true},
		},
		{
			name:    "private mismatched authorization",
			baseURL: "http://10.0.0.9:8080/v1",
			access:  ProviderPrivateNetworkAccess{Host: "10.0.0.8", Allowed: true},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateProviderEndpointAccess(tt.baseURL, tt.access)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateProviderEndpointAccess() error=%v wantErr=%v", err, tt.wantErr)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "provider endpoint") {
				t.Fatalf("error should identify provider endpoint policy, got %q", err)
			}
		})
	}
}
