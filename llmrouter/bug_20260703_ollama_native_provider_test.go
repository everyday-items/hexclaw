package llmrouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260703_OllamaConfigUsesNativeProvider(t *testing.T) {
	cfg := config.LLMConfig{
		Default: "Ollama (本地)",
		Providers: map[string]config.LLMProviderConfig{
			"Ollama (本地)": {
				BaseURL: "http://127.0.0.1:11434/v1",
				Model:   "qwen3.5:9b",
			},
		},
	}

	selector, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	provider, ok := selector.Get("Ollama (本地)")
	if !ok {
		t.Fatal("ollama provider should be loaded")
	}
	if got := provider.Name(); got != "ollama" {
		t.Fatalf("provider.Name() = %q, want native ollama provider", got)
	}
	if got := selector.ProviderModel("Ollama (本地)"); got != "qwen3.5:9b" {
		t.Fatalf("ProviderModel() = %q, want qwen3.5:9b", got)
	}

	routed, model, err := selector.RouteModel(context.Background())
	if err != nil {
		t.Fatalf("RouteModel() error = %v", err)
	}
	if routed.Name() != "ollama" || model != "qwen3.5:9b" {
		t.Fatalf("RouteModel() = (%s, %q), want (ollama, qwen3.5:9b)", routed.Name(), model)
	}
}

func TestBug20260703_NormalizeOllamaBaseURL(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:11434/v1":  "http://127.0.0.1:11434",
		"http://localhost:11434/v1/": "http://localhost:11434",
		"http://localhost:11434":     "http://localhost:11434",
		"":                           "",
	}
	for in, want := range cases {
		if got := normalizeOllamaBaseURL(in); got != want {
			t.Fatalf("normalizeOllamaBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBug20260703_OllamaNativeProviderSendsImagesToAPIChat(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if gotPath != "/api/chat" {
			t.Fatalf("unexpected path %s", gotPath)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		_, _ = w.Write([]byte(`{"model":"qwen3.5:9b","created_at":"t1","done":true,"message":{"role":"assistant","content":"ok"}}`))
	}))
	defer srv.Close()

	cfg := config.LLMConfig{
		Default: "Ollama (本地)",
		Providers: map[string]config.LLMProviderConfig{
			"Ollama (本地)": {
				BaseURL:        srv.URL + "/v1",
				Model:          "qwen3.5:9b",
				Models:         []string{"qwen3.5:9b"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{{
					ID: "qwen3.5:9b",
					Capabilities: []string{
						config.LLMModelCapabilityText,
						config.LLMModelCapabilityVision,
					},
				}},
			},
		},
	}

	selector, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	provider, ok := selector.Get("Ollama (本地)")
	if !ok {
		t.Fatal("ollama provider should be loaded")
	}
	resp, err := provider.Complete(context.Background(), llm.CompletionRequest{
		Model: "qwen3.5:9b",
		Messages: []llm.Message{
			{
				Role: llm.RoleUser,
				MultiContent: []llm.ContentPart{
					{Type: "text", Text: "识别图片文字"},
					{Type: "image_url", ImageURL: &llm.ImageURL{URL: "data:image/png;base64,QUJDRA=="}},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("response content = %q, want ok", resp.Content)
	}

	messages := gotBody["messages"].([]any)
	msg := messages[0].(map[string]any)
	if msg["content"] != "识别图片文字" {
		t.Fatalf("content = %#v, want text content", msg["content"])
	}
	images := msg["images"].([]any)
	if len(images) != 1 || images[0] != "QUJDRA==" {
		t.Fatalf("images = %#v, want Ollama base64 images field", msg["images"])
	}
}
