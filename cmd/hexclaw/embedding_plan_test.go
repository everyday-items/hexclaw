package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260717_EmbeddingAutoConfigNeverGuessesCloudChatCapability(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = ""
	cfg.Knowledge.Embedding.Model = ""
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai-compatible-chat-only": {
			APIKey:  "sk-test",
			BaseURL: "https://chat.example.test/v1",
			Model:   "gpt-compatible",
		},
	}

	plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
	if plan.Provider != "" || plan.Model != "" {
		t.Fatalf("auto config guessed unsupported cloud embedding: %#v", plan)
	}
}

func TestBug20260717_EmbeddingAutoConfigKeepsUnavailableOllamaAsStandby(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = ""
	cfg.Knowledge.Embedding.Model = ""
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"Ollama (本地)": {
			BaseURL: "http://127.0.0.1:1/v1",
			Model:   "qwen3:8b",
		},
	}

	plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
	if plan.Provider != "Ollama (本地)" || plan.Model != "qwen3-embedding:8b" {
		t.Fatalf("standby plan = %#v", plan)
	}
	if plan.Ready || plan.ServiceAvailable {
		t.Fatalf("unreachable Ollama must not be reported ready: %#v", plan)
	}
}

func TestBug20260717_EmbeddingAutoConfigPrefersInstalledModelDeterministically(t *testing.T) {
	installed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"bge-m3:latest"}]}`))
	}))
	defer installed.Close()

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer empty.Close()

	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = ""
	cfg.Knowledge.Embedding.Model = ""
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"a-ollama-empty":     {BaseURL: empty.URL + "/v1"},
		"z-ollama-installed": {BaseURL: installed.URL + "/v1"},
	}

	plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
	if plan.Provider != "z-ollama-installed" || plan.Model != "bge-m3:latest" || !plan.Ready {
		t.Fatalf("installed model plan = %#v", plan)
	}
}

func TestBug20260717_ExplicitCloudEmbeddingRequiresAnExplicitModel(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "cloud"
	cfg.Knowledge.Embedding.Model = ""
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud": {APIKey: "sk-test", BaseURL: "https://embedding.example.test/v1"},
	}

	plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
	if plan.Configured {
		t.Fatalf("model-less cloud embedding must remain disabled: %#v", plan)
	}
}

func TestKnowledgeEmbeddingPlanRequiresExplicitEndpointForCustomOrLocalCompatibleProvider(t *testing.T) {
	for _, tt := range []struct {
		name     string
		provider string
		config   config.LLMProviderConfig
	}{
		{
			name:     "custom compatible",
			provider: "my-compatible-gateway",
			config:   config.LLMProviderConfig{APIKey: "sk-test", Compatible: "openai"},
		},
		{
			name:     "declared local official name",
			provider: "openai",
			config:   config.LLMProviderConfig{APIKey: "sk-test", Locality: config.ProviderLocalityLocal},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Knowledge.Embedding.Provider = tt.provider
			cfg.Knowledge.Embedding.Model = "text-embedding-3-small"
			cfg.LLM.Providers = map[string]config.LLMProviderConfig{tt.provider: tt.config}
			plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
			if plan.Configured || plan.Ready || plan.ServiceAvailable {
				t.Fatalf("endpoint-less provider plan = %#v, want fail-closed", plan)
			}
		})
	}

	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "openai"
	cfg.Knowledge.Embedding.Model = "text-embedding-3-small"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai": {APIKey: "sk-test", Locality: config.ProviderLocalityCloud},
	}
	plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
	if !plan.Configured || !plan.Ready {
		t.Fatalf("official OpenAI default endpoint plan = %#v", plan)
	}
}

func TestKnowledgeEmbeddingPlanNeverTreatsOllamaCloudAsLocal(t *testing.T) {
	provider := config.LLMProviderConfig{
		APIKey:   "sk-test",
		BaseURL:  "https://ollama.example.test/v1",
		Locality: config.ProviderLocalityCloud,
	}
	if isOllamaEmbeddingCandidate("Ollama Cloud", provider) {
		t.Fatal("an explicitly cloud Ollama-compatible provider must not use native local Ollama management")
	}

	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "Ollama Cloud"
	cfg.Knowledge.Embedding.Model = "text-embedding-3-small"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"Ollama Cloud": provider}
	plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
	if !plan.Configured || !plan.Ready || !plan.ServiceAvailable || plan.Ollama {
		t.Fatalf("cloud Ollama-compatible embedding plan = %#v, want explicit cloud capability", plan)
	}
}

func TestKnowledgeEmbeddingPlanDoesNotProbeCloudLoopbackProxyAsNativeOllama(t *testing.T) {
	var nativeProbeCalls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nativeProbeCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer proxy.Close()

	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "Ollama Cloud Proxy"
	cfg.Knowledge.Embedding.Model = "text-embedding-3-small"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"Ollama Cloud Proxy": {
			APIKey:   "sk-test",
			BaseURL:  proxy.URL + "/v1",
			Locality: config.ProviderLocalityCloud,
		},
	}

	plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
	if plan.Ollama || !plan.Ready {
		t.Fatalf("explicit cloud reverse proxy plan = %#v", plan)
	}
	if got := nativeProbeCalls.Load(); got != 0 {
		t.Fatalf("native Ollama probe calls = %d, want zero for explicit cloud provider", got)
	}
}

func TestKnowledgeEmbeddingPlanRejectsNonLoopbackNativeOllamaEndpoints(t *testing.T) {
	for _, baseURL := range []string{
		"https://ollama.example.test/v1",
		"http://192.168.10.20:11434/v1",
		"http://ollama.local:11434/v1",
		"http://host.docker.internal:11434/v1",
	} {
		t.Run(baseURL, func(t *testing.T) {
			provider := config.LLMProviderConfig{
				APIKey:   "explicit-compatible-api-key",
				BaseURL:  baseURL,
				Locality: config.ProviderLocalityLocal,
			}
			if isOllamaEmbeddingCandidate("Ollama explicit local", provider) {
				t.Fatalf("non-loopback endpoint %q must not enable native Ollama probe/pull", baseURL)
			}

			cfg := config.DefaultConfig()
			cfg.Knowledge.Embedding.Provider = "Ollama explicit local"
			cfg.Knowledge.Embedding.Model = "text-embedding-3-small"
			cfg.LLM.Providers = map[string]config.LLMProviderConfig{
				"Ollama explicit local": provider,
			}
			plan := resolveKnowledgeEmbeddingPlan(context.Background(), cfg)
			if plan.Ollama || !plan.Configured || !plan.Ready {
				t.Fatalf("non-loopback explicit embedding plan = %#v, want compatible API without native management", plan)
			}
		})
	}
}

func TestKnowledgeEmbeddingDimensionUsesTheSelectedVectorSpace(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{model: "qwen3-embedding:8b", want: 4096},
		{model: "qwen3-embedding:latest", want: 4096},
		{model: "nomic-embed-text", want: 768},
		{model: "nomic-embed-text:latest", want: 768},
		{model: "mxbai-embed-large", want: 1024},
		{model: "mxbai-embed-large:latest", want: 1024},
		{model: "BAAI/bge-m3", want: 1024},
		{model: "BAAI/bge-m3:latest", want: 1024},
		{model: "all-minilm:latest", want: 384},
		{model: "text-embedding-3-small", want: 1536},
		{model: "text-embedding-3-large", want: 3072},
		{model: "nvidia/nemotron-3-embed-1b:free", want: 2048},
		{model: "nvidia/llama-nemotron-embed-vl-1b-v2:free", want: 2048},
		{model: "vendor/unknown-embedding-model", want: 0},
		{model: "prefix-nomic-embed-text", want: 0},
		{model: "nomic-embed-text-custom", want: 0},
		{model: "mxbai-embed-large-v2", want: 0},
		{model: "BAAI/bge-m3-custom", want: 0},
		{model: "all-minilm-extra", want: 0},
		{model: "nvidia/nemotron-3-embed-1b:paid", want: 0},
		{model: "text-embedding-3-small-v2", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := knowledgeEmbeddingDimension(tt.model); got != tt.want {
				t.Fatalf("knowledgeEmbeddingDimension(%q)=%d, want %d", tt.model, got, tt.want)
			}
		})
	}
}

func TestKnowledgeEmbeddingDimensionForProviderUsesOnlyExactDeclaredContract(t *testing.T) {
	provider := config.LLMProviderConfig{
		Models:         []string{"vendor/custom-embed", "vendor/chat"},
		ModelSpecsMode: config.LLMModelSpecsModeExplicit,
		ModelSpecs: []config.LLMProviderModelSpec{
			{
				ID: "vendor/custom-embed", Capabilities: []string{config.LLMModelCapabilityEmbedding},
				Embedding: &config.LLMEmbeddingModelSpec{
					Protocol: config.LLMEmbeddingProtocolOpenAI, Dimension: 4096,
				},
			},
			{ID: "vendor/chat", Capabilities: []string{config.LLMModelCapabilityText}},
		},
	}
	if got := knowledgeEmbeddingDimensionForProvider(provider, "vendor/custom-embed"); got != 4096 {
		t.Fatalf("declared custom dimension=%d, want 4096", got)
	}
	if got := knowledgeEmbeddingDimensionForProvider(provider, "vendor/chat"); got != 0 {
		t.Fatalf("chat-only custom dimension=%d, want fail-closed 0", got)
	}
	if got := knowledgeEmbeddingDimensionForProvider(provider, "vendor/custom-embed-near-match"); got != 0 {
		t.Fatalf("near-match custom dimension=%d, want fail-closed 0", got)
	}
}

func TestKnowledgeEmbeddingEffectiveBaseURLDefaultsNativeOllamaOnly(t *testing.T) {
	if got := knowledgeEmbeddingEffectiveBaseURL(
		knowledgeEmbeddingPlan{Ollama: true}, config.LLMProviderConfig{},
	); got != "http://localhost:11434/v1" {
		t.Fatalf("endpoint-less native Ollama effective URL = %q", got)
	}
	if got := knowledgeEmbeddingEffectiveBaseURL(
		knowledgeEmbeddingPlan{}, config.LLMProviderConfig{},
	); got != "" {
		t.Fatalf("endpoint-less compatible cloud effective URL = %q, want SDK default", got)
	}
	for _, tt := range []struct {
		name   string
		plan   knowledgeEmbeddingPlan
		input  string
		output string
	}{
		{
			name: "native root gains v1", plan: knowledgeEmbeddingPlan{Ollama: true},
			input: " http://127.0.0.1:11434/ ", output: "http://127.0.0.1:11434/v1",
		},
		{
			name: "native v1 loses trailing slash", plan: knowledgeEmbeddingPlan{Ollama: true},
			input: " http://127.0.0.1:11434/v1/ ", output: "http://127.0.0.1:11434/v1",
		},
		{
			name: "cloud custom path is preserved", plan: knowledgeEmbeddingPlan{},
			input: " https://embedding.example.test/custom/v2/ ", output: "https://embedding.example.test/custom/v2/",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := knowledgeEmbeddingEffectiveBaseURL(
				tt.plan, config.LLMProviderConfig{BaseURL: tt.input},
			); got != tt.output {
				t.Fatalf("explicit embedding effective URL = %q, want %q", got, tt.output)
			}
		})
	}
}

func TestKnowledgeEmbeddingProviderAPIKeyNeverUsesConfiguredSecretForNativeOllama(t *testing.T) {
	for _, tt := range []struct {
		name     string
		plan     knowledgeEmbeddingPlan
		apiKey   string
		expected string
	}{
		{
			name: "native configured secret", plan: knowledgeEmbeddingPlan{Ollama: true},
			apiKey: "real-provider-secret", expected: "ollama",
		},
		{
			name: "native empty secret", plan: knowledgeEmbeddingPlan{Ollama: true},
			expected: "ollama",
		},
		{
			name: "cloud keeps configured secret", plan: knowledgeEmbeddingPlan{},
			apiKey: "cloud-provider-secret", expected: "cloud-provider-secret",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := knowledgeEmbeddingProviderAPIKey(
				tt.plan, config.LLMProviderConfig{APIKey: tt.apiKey},
			)
			if got != tt.expected {
				t.Fatalf("embedding provider API key = %q, want %q", got, tt.expected)
			}
		})
	}
}
