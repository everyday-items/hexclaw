package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
