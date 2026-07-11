package llmrouter

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
)

func newLocalitySelector(name, baseURL string, provider *egressCaptureProvider) *Selector {
	cfg := config.LLMConfig{
		Default: name,
		Providers: map[string]config.LLMProviderConfig{
			name: {BaseURL: baseURL, APIKey: "test", Model: "test"},
		},
	}
	r := NewWithProviders(cfg, map[string]hexagon.Provider{name: provider})
	r.SetEgressPolicy(&egress.Policy{})
	return r
}

func TestEgressLocality_PublicURLContainingLocalTextStillGuarded(t *testing.T) {
	tests := []string{
		"https://localhost.evil.example/v1",
		"https://127.0.0.1.evil.example/v1",
		"https://api.example/v1/localhost/chat",
		"https://api.example/v1?upstream=http://127.0.0.1:11434",
	}
	for _, baseURL := range tests {
		t.Run(baseURL, func(t *testing.T) {
			p := &egressCaptureProvider{}
			r := newLocalitySelector("remote", baseURL, p)

			_, err := r.Default().Complete(context.Background(), hexagon.CompletionRequest{})
			if err == nil || !strings.Contains(err.Error(), "egress") {
				t.Fatalf("public endpoint %q must retain cloud egress guard, got error %v", baseURL, err)
			}
			if p.completeCalls != 0 {
				t.Fatalf("public endpoint %q bypassed guard: calls=%d", baseURL, p.completeCalls)
			}
		})
	}
}

func TestEgressLocality_PublicHostedOllamaStillGuarded(t *testing.T) {
	p := &egressCaptureProvider{}
	r := newLocalitySelector("ollama", "https://ollama.example/v1", p)

	_, err := r.Default().Complete(context.Background(), hexagon.CompletionRequest{})
	if err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("public hosted provider named ollama must retain cloud egress guard, got error %v", err)
	}
	if p.completeCalls != 0 {
		t.Fatalf("public hosted ollama bypassed guard: calls=%d", p.completeCalls)
	}
}

func TestEgressLocality_ExactLoopbackEndpointsStayLocal(t *testing.T) {
	tests := []string{
		"http://localhost:11434/v1",
		"http://127.0.0.1:11434/v1",
		"http://127.42.0.9:11434/v1",
		"http://[::1]:11434/v1",
	}
	for _, baseURL := range tests {
		t.Run(baseURL, func(t *testing.T) {
			p := &egressCaptureProvider{}
			r := newLocalitySelector("local", baseURL, p)

			if _, err := r.Default().Complete(context.Background(), hexagon.CompletionRequest{}); err != nil {
				t.Fatalf("loopback endpoint %q must stay local: %v", baseURL, err)
			}
			if p.completeCalls != 1 {
				t.Fatalf("loopback endpoint %q call count=%d, want 1", baseURL, p.completeCalls)
			}
		})
	}
}
