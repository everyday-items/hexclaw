package llmrouter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
)

func TestProviderFactoryBlocks307And308CrossOriginBodyReplay(t *testing.T) {
	const secretPrompt = "PRIVATE-RAG-DOCUMENT-DO-NOT-LEAK"
	tests := []struct {
		name     string
		provider string
		basePath string
		apiKey   string
	}{
		{name: "openai compatible", provider: "openai", basePath: "/v1", apiKey: "openai-secret"},
		{name: "anthropic", provider: "anthropic", basePath: "/v1", apiKey: "anthropic-secret"},
		{name: "ollama", provider: "ollama", apiKey: "ollama-secret"},
	}

	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, tt := range tests {
			t.Run(tt.name+"/"+http.StatusText(status), func(t *testing.T) {
				var targetCalls atomic.Int64
				var leakedBody atomic.Value
				var leakedAuthorization atomic.Value
				var leakedAPIKey atomic.Value
				leakedBody.Store("")
				leakedAuthorization.Store("")
				leakedAPIKey.Store("")

				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					targetCalls.Add(1)
					body, _ := io.ReadAll(req.Body)
					leakedBody.Store(string(body))
					leakedAuthorization.Store(req.Header.Get("Authorization"))
					leakedAPIKey.Store(req.Header.Get("x-api-key"))
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{}`))
				}))
				defer target.Close()

				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					http.Redirect(w, req, target.URL+req.URL.Path, status)
				}))
				defer origin.Close()

				provider := NewProviderFromConfig(tt.provider, config.LLMProviderConfig{
					BaseURL: origin.URL + tt.basePath,
					APIKey:  tt.apiKey,
					Model:   "test-model",
				})
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_, err := provider.Complete(ctx, hexagon.CompletionRequest{
					Messages: []hexagon.Message{{Role: hexagon.RoleUser, Content: secretPrompt}},
				})
				if calls := targetCalls.Load(); calls != 0 {
					t.Fatalf("cross-origin redirect target calls = %d, want 0", calls)
				}
				if tt.provider == "ollama" {
					// ai-core deliberately returns 307/308 as a terminal status for
					// /api/chat, so the configured CheckRedirect hook is not reached.
					// The security invariant is still zero replay to the target.
					if err == nil {
						t.Fatal("ollama redirect unexpectedly succeeded")
					}
				} else if !errors.Is(err, egress.ErrProviderEndpointPolicy) {
					t.Fatalf("redirect error = %v, want ErrProviderEndpointPolicy", err)
				}
				if body := leakedBody.Load().(string); strings.Contains(body, secretPrompt) {
					t.Fatalf("private prompt reached redirect target: %q", body)
				}
				if auth := leakedAuthorization.Load().(string); auth != "" {
					t.Fatalf("authorization reached redirect target: %q", auth)
				}
				if apiKey := leakedAPIKey.Load().(string); apiKey != "" {
					t.Fatalf("x-api-key reached redirect target: %q", apiKey)
				}
			})
		}
	}
}

func TestProviderFactoryDoesNotHonorEnvironmentProxy(t *testing.T) {
	for _, providerName := range []string{"openai", "anthropic", "ollama"} {
		t.Run(providerName, func(t *testing.T) {
			var proxyCalls atomic.Int64
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				proxyCalls.Add(1)
				http.Error(w, "proxy must not receive provider traffic", http.StatusBadGateway)
			}))
			defer proxy.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProviderFactoryProxyHelper$")
			cmd.Env = append(os.Environ(),
				"HEXCLAW_LLMROUTER_PROXY_HELPER="+providerName,
				"HEXCLAW_LLMROUTER_PROXY_BASE_URL=https://provider.invalid/v1",
				"HTTP_PROXY="+proxy.URL,
				"HTTPS_PROXY="+proxy.URL,
				"ALL_PROXY="+proxy.URL,
				"NO_PROXY=",
				"no_proxy=",
			)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("proxy helper failed: %v\n%s", err, output)
			}
			if calls := proxyCalls.Load(); calls != 0 {
				t.Fatalf("environment proxy calls = %d, want 0", calls)
			}
		})
	}
}

func TestProviderEndpointBaseURLUsesProtocolDefaults(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		baseURL  string
		want     string
	}{
		{name: "openai", provider: "openai", want: "https://api.openai.com/v1"},
		{name: "openai compatible", provider: "deepseek", want: "https://api.openai.com/v1"},
		{name: "anthropic", provider: "anthropic", want: "https://api.anthropic.com/v1"},
		{name: "ollama", provider: "ollama", want: "http://localhost:11434"},
		{
			name: "ollama explicit v1 suffix", provider: "ollama",
			baseURL: " http://127.0.0.1:11434/v1/ ", want: "http://127.0.0.1:11434",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := providerEndpointBaseURL(tt.provider, config.LLMProviderConfig{BaseURL: tt.baseURL})
			if got != tt.want {
				t.Fatalf("providerEndpointBaseURL(%q, %q) = %q, want %q", tt.provider, tt.baseURL, got, tt.want)
			}
		})
	}
}

func TestProviderFactoryKeepsEndpointPolicyErrorForRejectedConfiguration(t *testing.T) {
	for _, providerName := range []string{"openai", "anthropic", "ollama"} {
		t.Run(providerName, func(t *testing.T) {
			provider := NewProviderFromConfig(providerName, config.LLMProviderConfig{
				BaseURL: "http://api.example.com/v1",
				APIKey:  "must-not-send",
				Model:   "test-model",
			})
			_, err := provider.Complete(context.Background(), hexagon.CompletionRequest{
				Messages: []hexagon.Message{{Role: hexagon.RoleUser, Content: "private document"}},
			})
			if !errors.Is(err, egress.ErrProviderEndpointPolicy) {
				t.Fatalf("rejected endpoint error = %v, want ErrProviderEndpointPolicy", err)
			}
		})
	}
}

func TestOllamaProviderHTTPClientKeepsLongResponseHeaderBudget(t *testing.T) {
	client := providerHTTPClient("ollama", config.LLMProviderConfig{
		BaseURL: "http://localhost:11434",
	})
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("ollama transport = %T, want *http.Transport", client.Transport)
	}
	if got := transport.ResponseHeaderTimeout; got != 10*time.Minute {
		t.Fatalf("ollama response header timeout = %s, want 10m", got)
	}
}

func TestOllamaProviderFactoryAllowsConfiguredLoopbackComplete(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.URL.Path != "/api/chat" {
			t.Fatalf("path = %q, want /api/chat", req.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model":"test-model",
			"done":true,
			"message":{"role":"assistant","content":"ok"}
		}`))
	}))
	defer server.Close()

	provider := NewProviderFromConfig("ollama", config.LLMProviderConfig{
		BaseURL: server.URL + "/v1",
		Model:   "test-model",
	})
	response, err := provider.Complete(context.Background(), hexagon.CompletionRequest{
		Messages: []hexagon.Message{{Role: hexagon.RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.Content != "ok" {
		t.Fatalf("response = %#v, want content ok", response)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("loopback calls = %d, want 1", got)
	}
}

func TestProviderFactoryProxyHelper(t *testing.T) {
	providerName := os.Getenv("HEXCLAW_LLMROUTER_PROXY_HELPER")
	if providerName == "" {
		return
	}
	provider := NewProviderFromConfig(providerName, config.LLMProviderConfig{
		BaseURL: os.Getenv("HEXCLAW_LLMROUTER_PROXY_BASE_URL"),
		APIKey:  "proxy-secret",
		Model:   "test-model",
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = provider.Complete(ctx, hexagon.CompletionRequest{
		Messages: []hexagon.Message{{Role: hexagon.RoleUser, Content: "PRIVATE-PROXY-PROMPT"}},
	})
}
