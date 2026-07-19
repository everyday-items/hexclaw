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
		{name: "loopback", baseURL: "http://localhost:18080/v1"},
		{name: "metadata ip", baseURL: "http://169.254.169.254/latest", wantErr: true},
		{name: "metadata host", baseURL: "http://metadata.google.internal/", wantErr: true},
		{name: "unspecified", baseURL: "http://0.0.0.0:8000/v1", wantErr: true},
		{name: "private missing authorization", baseURL: "http://10.0.0.8:8080/v1", wantErr: true},
		{
			name:    "private exact authorization",
			baseURL: "http://10.0.0.8:8080/v1",
			access:  ProviderPrivateNetworkAccess{Host: "10.0.0.8", Allowed: true},
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
