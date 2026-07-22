package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

const reasoningProviderID = "pvd_v1_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestLLMConfigContract_PreservesReasoningSelection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "cloud-old"
	cfg.LLM.ReasoningProvider = "cloud-old"
	cfg.LLM.ReasoningModel = "gpt-5.6-sol"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud-old": {
			ProviderInstanceID: reasoningProviderID,
			APIKey:             "sk-real", BaseURL: "https://api.example.test/v1", Model: "gpt-5.6-sol",
			Locality: config.ProviderLocalityCloud,
		},
	}
	eng := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, eng, nil, nil)

	get := httptest.NewRecorder()
	srv.handleGetLLMConfig(get, httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", get.Code, get.Body.String())
	}
	var visible LLMConfigResponse
	if err := json.Unmarshal(get.Body.Bytes(), &visible); err != nil {
		t.Fatal(err)
	}
	if visible.ReasoningProvider != "cloud-old" || visible.ReasoningModel != "gpt-5.6-sol" {
		t.Fatalf("GET lost reasoning selection: %+v", visible)
	}

	put := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(put, httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"default":"cloud-old",
		"reasoning_provider":"cloud-old",
		"reasoning_model":"gpt-5.6-sol",
		"providers":{"cloud-old":{"provider_instance_id":"`+reasoningProviderID+`","api_key":"********real","base_url":"https://api.example.test/v1","model":"gpt-5.6-sol","locality":"cloud"}}
	}`)))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", put.Code, put.Body.String())
	}
	if eng.activeLLM.ReasoningProvider != "cloud-old" || eng.activeLLM.ReasoningModel != "gpt-5.6-sol" {
		t.Fatalf("PUT/runtime lost reasoning selection: %+v", eng.activeLLM)
	}
}

func TestLLMConfigUpdate_RenamesReasoningProviderByStableIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "cloud-old"
	cfg.LLM.ReasoningProvider = "cloud-old"
	cfg.LLM.ReasoningModel = "gpt-5.6-sol"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud-old": {
			ProviderInstanceID: reasoningProviderID,
			APIKey:             "sk-old", BaseURL: "https://old.example.test/v1", Model: "gpt-5.6-sol",
			Locality: config.ProviderLocalityCloud,
		},
	}
	eng := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, eng, nil, nil)

	w := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(w, httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"default":"cloud-renamed",
		"providers":{"cloud-renamed":{"provider_instance_id":"`+reasoningProviderID+`","api_key":"sk-new","base_url":"https://new.example.test/v1","model":"gpt-5.6-sol","locality":"cloud"}}
	}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := eng.activeLLM.ReasoningProvider; got != "cloud-renamed" {
		t.Fatalf("reasoning provider was not migrated by stable identity: %q", got)
	}
	if got := eng.activeLLM.ReasoningModel; got != "gpt-5.6-sol" {
		t.Fatalf("reasoning model changed during identity-preserving rename: %q", got)
	}
}

func TestLLMConfigUpdate_DanglingReasoningCannotFallBackToLocalDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "cloud-old"
	cfg.LLM.ReasoningProvider = "cloud-old"
	cfg.LLM.ReasoningModel = "gpt-5.6-sol"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud-old": {
			ProviderInstanceID: reasoningProviderID,
			APIKey:             "sk-old", BaseURL: "https://old.example.test/v1", Model: "gpt-5.6-sol",
			Locality: config.ProviderLocalityCloud,
		},
	}
	eng := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, eng, nil, nil)

	w := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(w, httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"default":"ollama",
		"providers":{"ollama":{"api_key":"","base_url":"http://127.0.0.1:11434/v1","model":"qwen3.5:9b","locality":"local"}}
	}`)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "reasoning_provider") {
		t.Fatalf("dangling reasoning selection status=%d body=%s, want explicit 400", w.Code, w.Body.String())
	}
	if eng.reloadCalls != 0 || eng.activeLLM.ReasoningProvider != "cloud-old" {
		t.Fatalf("invalid transition reached runtime: reloads=%d active=%+v", eng.reloadCalls, eng.activeLLM)
	}
}
