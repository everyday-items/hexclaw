package feishu

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/everyday-items/hexclaw/internal/config"
)

// TestFeishuAdapter_Challenge 测试 URL 验证回调
func TestFeishuAdapter_Challenge(t *testing.T) {
	a := New(config.FeishuConfig{
		AppID:     "test-app-id",
		AppSecret: "test-secret",
	})

	body := `{"challenge":"test-challenge-token","token":"verify","type":"url_verification"}`
	req := httptest.NewRequest("POST", "/feishu/webhook", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	a.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp["challenge"] != "test-challenge-token" {
		t.Fatalf("期望 challenge=test-challenge-token，实际 %s", resp["challenge"])
	}
}

// TestFeishuAdapter_InvalidToken 测试验证 Token 不匹配
func TestFeishuAdapter_InvalidToken(t *testing.T) {
	a := New(config.FeishuConfig{
		VerificationToken: "correct-token",
	})

	body := `{"header":{"event_type":"im.message.receive_v1","token":"wrong-token"},"event":{}}`
	req := httptest.NewRequest("POST", "/feishu/webhook", strings.NewReader(body))
	w := httptest.NewRecorder()

	a.handleWebhook(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401，实际 %d", w.Code)
	}
}

// TestEscapeJSON 测试 JSON 转义
func TestEscapeJSON(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`hello`, `hello`},
		{`he"llo`, `he\"llo`},
		{"line1\nline2", `line1\nline2`},
	}

	for _, tt := range tests {
		result := escapeJSON(tt.input)
		if result != tt.expected {
			t.Errorf("escapeJSON(%q) = %q, 期望 %q", tt.input, result, tt.expected)
		}
	}
}
