package llmrouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
)

// REG-K12-RECOGNIZING-POLICY-001 release boundary: a module-mode build must
// obtain the standard reasoning_effort wire mapping from the published
// ai-core dependency, not from the workspace's local go.work.
func TestBug20260730ProviderFactoryThinkingOffUsesReasoningEffortNoneOnWire(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode provider request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"completion-1",
			"object":"chat.completion",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.Close()

	provider := NewProviderFromConfig("hexclaw-gpt", config.LLMProviderConfig{
		APIKey:  "test-only",
		BaseURL: server.URL,
		Model:   "gpt-5.6-sol",
	})
	if _, err := provider.Complete(context.Background(), hexagon.CompletionRequest{
		Messages:             []hexagon.Message{{Role: hexagon.RoleUser, Content: "recognize"}},
		Metadata:             map[string]any{"thinking": "off"},
		ReasoningPolicyScope: llm.ReasoningPolicyScopeStructuredVisionRecognition,
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if payload["model"] != "gpt-5.6-sol" {
		t.Fatalf("wire model=%v, want gpt-5.6-sol", payload["model"])
	}
	if payload["reasoning_effort"] != "none" {
		t.Fatalf("wire reasoning_effort=%v, want none; payload=%v", payload["reasoning_effort"], payload)
	}
	if _, exists := payload["enable_thinking"]; exists {
		t.Fatalf("OpenAI reasoning route must not emit vendor enable_thinking: %v", payload)
	}
}

func TestBug20260730ProviderFactoryUnscopedThinkingOffKeepsWireUnchanged(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode provider request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"completion-unscoped",
			"object":"chat.completion",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.Close()

	provider := NewProviderFromConfig("hexclaw-gpt", config.LLMProviderConfig{
		APIKey:  "test-only",
		BaseURL: server.URL,
		Model:   "gpt-5.6-sol",
	})
	if _, err := provider.Complete(context.Background(), hexagon.CompletionRequest{
		Messages: []hexagon.Message{{Role: hexagon.RoleUser, Content: "ordinary-title"}},
		Metadata: map[string]any{"thinking": "off"},
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, exists := payload["reasoning_effort"]; exists {
		t.Fatalf("unscoped thinking=off changed non-recognizing wire: %v", payload)
	}
}
