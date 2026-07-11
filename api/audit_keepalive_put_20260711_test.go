package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// C4 keep_alive 全链零校验 —— handler 边界。
//
// PUT /api/v1/config/llm 携带非法 keep_alive 应 400 回显，不落盘、不下发到 Ollama。
// 非事务路径（flag OFF）此前完全跳过校验直接 Save，故校验必须在 handler 显式兜底。

func putLLMConfig(t *testing.T, keepAlive string) *httptest.ResponseRecorder {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.DefaultConfig()
	cfg.LLM.Default = "ollama"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"ollama": {BaseURL: "http://localhost:11434", Model: "qwen3:8b"},
	}
	eng := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"providers":{"ollama":{"base_url":"http://localhost:11434","model":"qwen3:8b","keep_alive":"` + keepAlive + `"}}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(w, req)
	return w
}

func TestHandleUpdateLLMConfig_RejectsIllegalKeepAlive(t *testing.T) {
	w := putLLMConfig(t, "banana")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 keep_alive 期望 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "keep_alive") {
		t.Fatalf("400 响应未提示 keep_alive：%s", w.Body.String())
	}
}

func TestHandleUpdateLLMConfig_AcceptsLegalKeepAlive(t *testing.T) {
	for _, ka := range []string{"30m", "-1", "0", "3600"} {
		w := putLLMConfig(t, ka)
		if w.Code != http.StatusOK {
			t.Fatalf("合法 keep_alive=%q 期望 200，实际 %d: %s", ka, w.Code, w.Body.String())
		}
	}
}
