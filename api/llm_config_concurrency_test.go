package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

const llmConfigConcurrencyProviderID = "pvd_v1_00112233445566778899aabbccddeeff"

func llmConfigConcurrencyRequest(
	t *testing.T,
	model string,
	revision *uint64,
	digest *string,
) string {
	t.Helper()
	payload := map[string]any{
		"default": "custom",
		"providers": map[string]any{
			"custom": map[string]any{
				"provider_instance_id": llmConfigConcurrencyProviderID,
				"api_key":              "****-config-concurrency-secret",
				"base_url":             "https://provider.example.test/v1",
				"model":                model,
				"models":               []string{model},
				"model_specs": []map[string]any{{
					"id":           model,
					"capabilities": []string{config.LLMModelCapabilityText},
				}},
				"compatible": "openai",
				"locality":   config.ProviderLocalityCloud,
				"enabled":    true,
			},
		},
	}
	if revision != nil {
		payload["expected_config_revision"] = *revision
	}
	if digest != nil {
		payload["expected_config_digest"] = *digest
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestHandleUpdateLLMConfig_RejectsStaleConditionalWrite 锁定 GET→PUT 的乐观并发边界。
func TestHandleUpdateLLMConfig_RejectsStaleConditionalWrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	enabled := true
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "custom"
	cfg.LLM.ConfigRevision = 7
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			ProviderInstanceID: llmConfigConcurrencyProviderID,
			APIKey:             "sk-config-concurrency-secret",
			BaseURL:            "https://provider.example.test/v1",
			Model:              "chat-a",
			Models:             []string{"chat-a"},
			ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID:           "chat-a",
				Capabilities: []string{config.LLMModelCapabilityText},
			}},
			Compatible: "openai",
			Locality:   config.ProviderLocalityCloud,
			Enabled:    &enabled,
		},
	}
	engine := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, engine, nil, nil)

	get := httptest.NewRecorder()
	srv.handleGetLLMConfig(get, httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("initial GET status=%d body=%s", get.Code, get.Body.String())
	}
	var snapshot LLMConfigResponse
	if err := json.Unmarshal(get.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode initial GET: %v", err)
	}
	if snapshot.ConfigRevision != 7 || snapshot.ConfigDigest == "" {
		t.Fatalf("GET config snapshot=%+v, want revision and digest", snapshot)
	}
	if strings.Contains(get.Body.String(), "sk-config-concurrency-secret") {
		t.Fatalf("GET leaked API key: %s", get.Body.String())
	}

	// 写入方 B 使用刚读取的条件快照成功提交。
	writeB := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(writeB, httptest.NewRequest(
		http.MethodPut,
		"/api/v1/config/llm",
		strings.NewReader(llmConfigConcurrencyRequest(t, "chat-b", &snapshot.ConfigRevision, &snapshot.ConfigDigest)),
	))
	if writeB.Code != http.StatusOK {
		t.Fatalf("fresh conditional PUT status=%d body=%s", writeB.Code, writeB.Body.String())
	}
	if srv.cfg.LLM.ConfigRevision != 8 || srv.cfg.LLM.Providers["custom"].Model != "chat-b" || engine.reloadCalls != 1 {
		t.Fatalf("fresh conditional PUT did not apply exactly once: revision=%d model=%q reloads=%d", srv.cfg.LLM.ConfigRevision, srv.cfg.LLM.Providers["custom"].Model, engine.reloadCalls)
	}

	// 写入方 A 继续使用旧快照，必须在保存和热加载前被拒绝。
	stale := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(stale, httptest.NewRequest(
		http.MethodPut,
		"/api/v1/config/llm",
		strings.NewReader(llmConfigConcurrencyRequest(t, "chat-c", &snapshot.ConfigRevision, &snapshot.ConfigDigest)),
	))
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale conditional PUT status=%d body=%s", stale.Code, stale.Body.String())
	}
	var staleResponse struct {
		Code           string `json:"code"`
		ConfigRevision uint64 `json:"config_revision"`
		ConfigDigest   string `json:"config_digest"`
	}
	if err := json.Unmarshal(stale.Body.Bytes(), &staleResponse); err != nil {
		t.Fatalf("decode stale response: %v", err)
	}
	if staleResponse.Code != CodeLLMConfigStale || staleResponse.ConfigRevision != 8 || staleResponse.ConfigDigest == snapshot.ConfigDigest {
		t.Fatalf("stale response=%+v, want current non-secret revision/digest", staleResponse)
	}
	if srv.cfg.LLM.Providers["custom"].Model != "chat-b" || engine.reloadCalls != 1 {
		t.Fatalf("stale PUT mutated config/runtime: model=%q reloads=%d", srv.cfg.LLM.Providers["custom"].Model, engine.reloadCalls)
	}
}

func TestHandleUpdateLLMConfig_RequiresCompleteConditionalSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "custom"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			ProviderInstanceID: llmConfigConcurrencyProviderID,
			APIKey:             "sk-config-concurrency-secret",
			BaseURL:            "https://provider.example.test/v1",
			Model:              "chat-a",
			Models:             []string{"chat-a"},
		},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	revision := cfg.LLM.ConfigRevision
	w := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(w, httptest.NewRequest(
		http.MethodPut,
		"/api/v1/config/llm",
		strings.NewReader(llmConfigConcurrencyRequest(t, "chat-b", &revision, nil)),
	))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "expected_config_revision") {
		t.Fatalf("partial conditional snapshot status=%d body=%s", w.Code, w.Body.String())
	}
}
