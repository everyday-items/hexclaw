package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
)

type mockCompletionProvider struct {
	err error
}

func (m *mockCompletionProvider) Complete(_ context.Context, _ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &hexagon.CompletionResponse{Content: "OK"}, nil
}

func TestHandleTestLLMConfig_Success(t *testing.T) {
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(cfg llmConnectionTestProvider) completionProvider {
		if cfg.Type != "openai" || cfg.Model != "gpt-4o-mini" || cfg.APIKey != "sk-test" {
			t.Fatalf("工厂收到错误参数: %+v", cfg)
		}
		return &mockCompletionProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/test", strings.NewReader(`{"provider":{"type":"openai","base_url":"https://example.com","api_key":"sk-test","model":"gpt-4o-mini"}}`))
	w := httptest.NewRecorder()

	srv.handleTestLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp LLMConnectionTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !resp.OK {
		t.Fatalf("期望 ok=true，实际 %+v", resp)
	}
	if resp.Provider != "openai" || resp.Model != "gpt-4o-mini" {
		t.Fatalf("provider/model 不正确: %+v", resp)
	}
}

func TestHandleTestLLMConfig_ReturnsFailurePayload(t *testing.T) {
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider {
		return &mockCompletionProvider{err: errors.New("unauthorized")}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/test", strings.NewReader(`{"provider":{"type":"openai","api_key":"sk-test","model":"gpt-4o-mini"}}`))
	w := httptest.NewRecorder()

	srv.handleTestLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp LLMConnectionTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.OK {
		t.Fatalf("期望 ok=false，实际 %+v", resp)
	}
	if !strings.Contains(resp.Message, "unauthorized") {
		t.Fatalf("失败信息不正确: %+v", resp)
	}
}

func TestHandleTestLLMConfig_OllamaAllowsEmptyAPIKey(t *testing.T) {
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(cfg llmConnectionTestProvider) completionProvider {
		if cfg.Type != "ollama" || cfg.Model != "llama3.1" || cfg.APIKey != "" {
			t.Fatalf("工厂收到错误参数: %+v", cfg)
		}
		return &mockCompletionProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/test", strings.NewReader(`{"provider":{"type":"ollama","base_url":"http://localhost:11434/v1","api_key":"","model":"llama3.1"}}`))
	w := httptest.NewRecorder()

	srv.handleTestLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var resp LLMConnectionTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !resp.OK {
		t.Fatalf("期望 ok=true，实际 %+v", resp)
	}
}

func TestHandleTestLLMConfig_RejectsEmptyAPIKeyForOpenAI(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/test", strings.NewReader(`{"provider":{"type":"openai","api_key":"","model":"gpt-4o-mini"}}`))
	w := httptest.NewRecorder()

	srv.handleTestLLMConfig(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleGetLLMConfig_UsesRuntimeActiveConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "openai"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai": {APIKey: "sk-openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"},
	}

	runtimeCfg := config.LLMConfig{
		Default: "智谱",
		Providers: map[string]config.LLMProviderConfig{
			"智谱": {APIKey: "sk-zhipu", BaseURL: "https://open.bigmodel.cn/api/paas/v4", Model: "glm-5"},
		},
	}

	srv := NewServer(cfg, &mockEngine{activeLLM: runtimeCfg}, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil)
	w := httptest.NewRecorder()

	srv.handleGetLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp LLMConfigResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if resp.Default != "智谱" {
		t.Fatalf("期望默认 provider 为运行时配置，实际 %q", resp.Default)
	}
	if _, ok := resp.Providers["智谱"]; !ok {
		t.Fatalf("期望返回运行时 provider，实际 %+v", resp.Providers)
	}
	if _, ok := resp.Providers["openai"]; ok {
		t.Fatalf("不应回退到磁盘配置，实际 %+v", resp.Providers)
	}
}

func TestHandleUpdateLLMConfig_HotReloadsAndPersists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.DefaultConfig()
	cfg.LLM.Default = "openai"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai": {APIKey: "sk-openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"},
	}

	eng := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, eng, nil, nil)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"default":"智谱",
		"providers":{
			"智谱":{"api_key":"sk-zhipu","base_url":"https://open.bigmodel.cn/api/paas/v4","model":"glm-5","compatible":"openai"}
		}
	}`))
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if eng.reloadCalls != 1 {
		t.Fatalf("期望热更新 1 次，实际 %d", eng.reloadCalls)
	}
	if eng.activeLLM.Default != "智谱" {
		t.Fatalf("引擎未热更新到新默认 provider，实际 %q", eng.activeLLM.Default)
	}
	if srv.cfg.LLM.Default != "智谱" {
		t.Fatalf("服务端内存配置未更新，实际 %q", srv.cfg.LLM.Default)
	}

	configFile := filepath.Join(home, ".hexclaw", "hexclaw.yaml")
	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("读取持久化配置失败: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "glm-5") || !strings.Contains(content, "智谱") {
		t.Fatalf("配置文件未写入新 provider: %s", content)
	}
}

func TestHandleUpdateLLMConfig_RollsBackPersistedFileOnReloadFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.DefaultConfig()
	cfg.LLM.Default = "openai"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai": {APIKey: "sk-openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"},
	}
	if err := config.Save(cfg, ""); err != nil {
		t.Fatalf("写入初始配置失败: %v", err)
	}

	eng := &mockEngine{
		activeLLM: cfg.LLM,
		reloadErr: errors.New("reload failed"),
	}
	srv := NewServer(cfg, eng, nil, nil)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"default":"智谱",
		"providers":{
			"智谱":{"api_key":"sk-zhipu","base_url":"https://open.bigmodel.cn/api/paas/v4","model":"glm-5","compatible":"openai"}
		}
	}`))
	w := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d: %s", w.Code, w.Body.String())
	}
	if eng.reloadCalls != 1 {
		t.Fatalf("期望热更新 1 次，实际 %d", eng.reloadCalls)
	}
	if srv.cfg.LLM.Default != "openai" {
		t.Fatalf("热更新失败后不应污染内存配置，实际 %q", srv.cfg.LLM.Default)
	}
	if eng.activeLLM.Default != "openai" {
		t.Fatalf("引擎活跃配置不应变化，实际 %q", eng.activeLLM.Default)
	}

	configFile := filepath.Join(home, ".hexclaw", "hexclaw.yaml")
	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("读取回滚后的配置失败: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "openai") || strings.Contains(content, "glm-5") {
		t.Fatalf("配置文件未回滚到旧配置: %s", content)
	}
}
