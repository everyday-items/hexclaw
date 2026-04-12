package api

import (
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
	if !strings.Contains(w.Body.String(), "配置保存失败") {
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

func TestHandleUpdateFullConfig_SandboxUpdateFailureRollsBackPersistedConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
	}
	s.cfg.Security.Auth.Enabled = true
	s.cfg.Skill.Builtin.CodeExecPolicy.Network = boolPtr(true)
	s.SetSandboxCallbacks(func(bool) error {
		return os.ErrPermission
	}, func() bool {
		return true
	})

	req := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(`{"security":{"gateway_enabled":false},"sandbox":{"network_enabled":false}}`))
	w := httptest.NewRecorder()

	s.handleUpdateFullConfig(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body=%s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "配置已回滚") {
		t.Fatalf("body = %s, want rollback message", w.Body.String())
	}
	if s.cfg.Security.Auth.Enabled != true {
		t.Fatalf("auth enabled = %v, want true", s.cfg.Security.Auth.Enabled)
	}
	if s.cfg.Skill.Builtin.CodeExecPolicy.CodeExecNetworkAllowed() != true {
		t.Fatalf("network = %v, want true", s.cfg.Skill.Builtin.CodeExecPolicy.CodeExecNetworkAllowed())
	}
}

func TestHandleUpdateFullConfig_SuccessAppliesSecurityFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	runtimeNetworkEnabled := true
	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
	}
	s.cfg.Security.Auth.Enabled = true
	s.cfg.Security.ContentFilter.Enabled = true
	s.cfg.Security.Cost.BudgetPerUser = 10
	s.cfg.Skill.Builtin.CodeExecPolicy.Network = boolPtr(true)
	s.SetSandboxCallbacks(func(enabled bool) error {
		runtimeNetworkEnabled = enabled
		return nil
	}, func() bool {
		return runtimeNetworkEnabled
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
