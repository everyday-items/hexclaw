package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func boolPtr(b bool) *bool { return &b }

func TestHandleGetFullConfig_ProviderSwitchableStatus(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openrouter": {
			APIKey:  "sk-openrouter",
			BaseURL: "https://openrouter.ai/api/v1",
			Model:   "deepseek/deepseek-chat-v3-0324:free",
		},
		"ollama": {
			BaseURL: "http://127.0.0.1:11434/v1",
			Model:   "qwen3:0.6b",
		},
		"disabled": {
			APIKey:  "sk-disabled",
			BaseURL: "https://api.example.com/v1",
			Model:   "x",
			Enabled: boolPtr(false),
		},
	}
	s := &Server{cfg: cfg, logCollector: NewLogCollector(10)}

	w := httptest.NewRecorder()
	s.handleGetFullConfig(w, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	llmSection := body["llm"].(map[string]any)
	providers := llmSection["providers"].(map[string]any)
	openrouter := providers["openrouter"].(map[string]any)
	if openrouter["switchable"] != false {
		t.Fatalf("OpenRouter free model should not be switchable: %+v", openrouter)
	}
	if openrouter["switch_disabled_reason"] != "openrouter_free_model_rate_limited" {
		t.Fatalf("unexpected openrouter reason: %+v", openrouter)
	}
	if strings.Contains(w.Body.String(), "sk-openrouter") {
		t.Fatalf("full config leaked API key: %s", w.Body.String())
	}

	ollama := providers["ollama"].(map[string]any)
	if ollama["switchable"] != true || ollama["local"] != true {
		t.Fatalf("Ollama local provider should be switchable without API key: %+v", ollama)
	}

	disabled := providers["disabled"].(map[string]any)
	if disabled["switchable"] != false || disabled["enabled"] != false {
		t.Fatalf("disabled provider should not be switchable: %+v", disabled)
	}
}

func TestHandleUpdateFullConfig_SaveFailureReturnsError(t *testing.T) {
	homeFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(homeFile, []byte("x"), 0600); err != nil {
		t.Fatalf("写入 HOME 测试文件失败: %v", err)
	}
	t.Setenv("HOME", homeFile)

	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
	}
	s.cfg.Security.Auth.Enabled = true
	s.cfg.Security.ContentFilter.Enabled = true
	s.cfg.Security.Cost.BudgetPerUser = 10

	req := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(`{"security":{"gateway_enabled":false,"content_filter":false,"max_tokens_per_request":42},"sandbox":{"network_enabled":false}}`))
	w := httptest.NewRecorder()

	s.handleUpdateFullConfig(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body=%s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Failed to save configuration") {
		t.Fatalf("body = %s, want persistence failure message", w.Body.String())
	}
	if s.cfg.Security.Auth.Enabled != true {
		t.Fatalf("auth enabled = %v, want true", s.cfg.Security.Auth.Enabled)
	}
	if s.cfg.Security.ContentFilter.Enabled != true {
		t.Fatalf("content filter = %v, want true", s.cfg.Security.ContentFilter.Enabled)
	}
	if s.cfg.Security.Cost.BudgetPerUser != 10 {
		t.Fatalf("budget per user = %v, want 10", s.cfg.Security.Cost.BudgetPerUser)
	}
}

func TestHandleUpdateFullConfig_SandboxPrepareFailureDoesNotPersistConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
	}
	s.cfg.Security.Auth.Enabled = true
	s.cfg.Skill.Builtin.CodeExecPolicy.Network = boolPtr(false)
	s.SetSandboxPolicyRuntime(SandboxPolicyRuntime{
		Prepare: func(context.Context, SandboxPolicy) (SandboxPolicyCandidate, error) {
			return SandboxPolicyCandidate{}, os.ErrPermission
		},
		Snapshot: func() SandboxPolicy {
			return SandboxPolicy{NetworkEnabled: false}
		},
	})

	req := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(`{"security":{"gateway_enabled":false},"sandbox":{"network_enabled":false}}`))
	w := httptest.NewRecorder()

	s.handleUpdateFullConfig(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body=%s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Sandbox policy validation failed") {
		t.Fatalf("body = %s, want validation failure message", w.Body.String())
	}
	if s.cfg.Security.Auth.Enabled != true {
		t.Fatalf("auth enabled = %v, want true", s.cfg.Security.Auth.Enabled)
	}
	if s.cfg.Skill.Builtin.CodeExecPolicy.CodeExecNetworkAllowed() {
		t.Fatalf("network = %v, want false", s.cfg.Skill.Builtin.CodeExecPolicy.CodeExecNetworkAllowed())
	}
}

func TestHandleUpdateFullConfig_SuccessAppliesSecurityFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	runtimeNetworkEnabled := false
	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
	}
	s.cfg.Security.Auth.Enabled = true
	s.cfg.Security.ContentFilter.Enabled = true
	s.cfg.Security.Cost.BudgetPerUser = 10
	s.cfg.Skill.Builtin.CodeExecPolicy.Network = boolPtr(false)
	s.SetSandboxPolicyRuntime(SandboxPolicyRuntime{
		Prepare: func(_ context.Context, policy SandboxPolicy) (SandboxPolicyCandidate, error) {
			return NewSandboxPolicyCandidate(func() {
				runtimeNetworkEnabled = policy.NetworkEnabled
			}, func() {}), nil
		},
		Snapshot: func() SandboxPolicy {
			return SandboxPolicy{NetworkEnabled: runtimeNetworkEnabled}
		},
	})

	req := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(`{"security":{"gateway_enabled":false,"content_filter":false,"max_tokens_per_request":42},"sandbox":{"network_enabled":false}}`))
	w := httptest.NewRecorder()

	s.handleUpdateFullConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if s.cfg.Security.Auth.Enabled != false {
		t.Fatalf("auth enabled = %v, want false", s.cfg.Security.Auth.Enabled)
	}
	if s.cfg.Security.ContentFilter.Enabled != false {
		t.Fatalf("content filter = %v, want false", s.cfg.Security.ContentFilter.Enabled)
	}
	// max_tokens_per_request 不再映射到 BudgetPerUser（美元预算由后端独立管理）
	// 确认 BudgetPerUser 保持原值不被前端覆盖
	if s.cfg.Security.Cost.BudgetPerUser != 10 {
		t.Fatalf("budget per user = %v, want 10 (should not be overwritten by frontend)", s.cfg.Security.Cost.BudgetPerUser)
	}
	if runtimeNetworkEnabled != false {
		t.Fatalf("runtime sandbox network = %v, want false", runtimeNetworkEnabled)
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body["message"] == "" {
		t.Fatalf("response body = %s, want message", w.Body.String())
	}
}
