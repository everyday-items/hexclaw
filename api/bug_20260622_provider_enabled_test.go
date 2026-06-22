package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// TestBug20260622_DisableProviderPreservesKey 回归锁定 [BUG-PROVIDER-DISABLE]：
// 禁用 provider（enabled:false）时，前端回传脱敏 Key（****xxxx）→ 后端必须保留原 Key、
// 持久化 enabled=false（而非整表替换把 Key 从磁盘删除）。
func TestBug20260622_DisableProviderPreservesKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.DefaultConfig()
	cfg.LLM.Default = "openai"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"openai": {APIKey: "sk-openai-secret-1234", BaseURL: "https://api.openai.com/v1", Model: "gpt-4o"},
	}
	masked := config.MaskAPIKey("sk-openai-secret-1234") // "****1234"

	eng := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, eng, nil, nil)

	// 前端禁用 openai：回传脱敏 Key + enabled:false
	body := `{"default":"openai","providers":{"openai":{"api_key":"` + masked +
		`","base_url":"https://api.openai.com/v1","model":"gpt-4o","enabled":false}}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200, 实际 %d: %s", w.Code, w.Body.String())
	}

	// 1) 内存配置：Key 保留（未被脱敏值覆盖）
	got := srv.cfg.LLM.Providers["openai"]
	if got.APIKey != "sk-openai-secret-1234" {
		t.Errorf("禁用后 Key 丢失/被脱敏值覆盖, got %q", got.APIKey)
	}
	// 2) enabled=false 持久化
	if got.Enabled == nil || *got.Enabled != false {
		t.Errorf("enabled 未持久化为 false, got %v", got.Enabled)
	}

	// 3) 磁盘配置仍含真实 Key（禁用 ≠ 删除）
	data, err := os.ReadFile(filepath.Join(home, ".hexclaw", "hexclaw.yaml"))
	if err != nil {
		t.Fatalf("读配置文件: %v", err)
	}
	if !strings.Contains(string(data), "sk-openai-secret-1234") {
		t.Errorf("磁盘配置丢失真实 Key（禁用误删）:\n%s", string(data))
	}

	// 4) GET 回显 enabled=false（前端据此还原禁用态）
	greq := httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil)
	gw := httptest.NewRecorder()
	srv.handleGetLLMConfig(gw, greq)
	if !strings.Contains(gw.Body.String(), `"enabled":false`) {
		t.Errorf("GET 未回显 enabled:false: %s", gw.Body.String())
	}
}
