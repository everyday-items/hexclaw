package feishu

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/everyday-items/hexclaw/config"
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

// TestMarshalTextContent 测试安全 JSON 序列化
func TestMarshalTextContent(t *testing.T) {
	tests := []struct {
		input string
	}{
		{`hello`},
		{`he"llo`},
		{"line1\nline2"},
		{"含\x00null字节"},
		{"含\b退格符"},
	}

	for _, tt := range tests {
		result := marshalTextContent(tt.input)
		// 结果必须是合法 JSON
		var parsed map[string]string
		if err := json.Unmarshal([]byte(result), &parsed); err != nil {
			t.Errorf("marshalTextContent(%q) 产生非法 JSON: %s, err: %v", tt.input, result, err)
		}
		if parsed["text"] != tt.input {
			t.Errorf("marshalTextContent(%q) 反序列化后不匹配: %q", tt.input, parsed["text"])
		}
	}
}
