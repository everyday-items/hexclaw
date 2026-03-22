package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/gateway"
)

// mockEngine 测试用引擎
type mockEngine struct {
	reply       *adapter.Reply
	err         error
	lastMsg     *adapter.Message
	calls       int
	activeLLM   config.LLMConfig
	reloadErr   error
	reloadCalls int
}

func (e *mockEngine) Start(_ context.Context) error  { return nil }
func (e *mockEngine) Stop(_ context.Context) error   { return nil }
func (e *mockEngine) Health(_ context.Context) error { return nil }
func (e *mockEngine) Process(_ context.Context, msg *adapter.Message) (*adapter.Reply, error) {
	e.calls++
	e.lastMsg = msg
	return e.reply, e.err
}
func (e *mockEngine) ProcessStream(_ context.Context, _ *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
	return nil, nil
}
func (e *mockEngine) ActiveLLMConfig() config.LLMConfig { return e.activeLLM }
func (e *mockEngine) ReloadLLMConfig(_ context.Context, cfg config.LLMConfig) error {
	e.reloadCalls++
	if e.reloadErr != nil {
		return e.reloadErr
	}
	e.activeLLM = cfg
	return nil
}

// mockGateway 测试用安全网关
type mockGateway struct {
	err error
}

func (g *mockGateway) Check(_ context.Context, _ *adapter.Message) error {
	return g.err
}
func (g *mockGateway) RecordUsage(_ context.Context, _ *adapter.Message, _ *gateway.Usage) error {
	return nil
}

// TestServer_Health 测试健康检查端点
func TestServer_Health(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	srv.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "healthy" {
		t.Fatalf("期望 healthy，实际 %s", resp["status"])
	}
}

// TestServer_Chat 测试聊天端点
func TestServer_Chat(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{
		reply: &adapter.Reply{
			Content:  "你好！有什么可以帮你的？",
			Metadata: map[string]string{"provider": "test"},
		},
	}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"message": "你好", "user_id": "test-user"}`
	req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d, body: %s", w.Code, w.Body.String())
	}

	var resp ChatResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Reply != "你好！有什么可以帮你的？" {
		t.Fatalf("回复内容不匹配: %s", resp.Reply)
	}
	if eng.lastMsg == nil || eng.lastMsg.Content != "你好" {
		t.Fatalf("引擎未收到正确消息: %#v", eng.lastMsg)
	}
}

func TestServer_ChatForwardsExplicitProviderAndModel(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{
		reply: &adapter.Reply{
			Content:  "收到",
			Metadata: map[string]string{"provider": "智谱", "model": "glm-5"},
		},
	}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"message":"你好","user_id":"test-user","provider":"智谱","model":"glm-5","role":"analyst"}`
	req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d, body: %s", w.Code, w.Body.String())
	}
	if eng.lastMsg == nil {
		t.Fatal("引擎未收到消息")
	}
	if got := eng.lastMsg.Metadata["provider"]; got != "智谱" {
		t.Fatalf("provider 未透传，实际 %q", got)
	}
	if got := eng.lastMsg.Metadata["model"]; got != "glm-5" {
		t.Fatalf("model 未透传，实际 %q", got)
	}
	if got := eng.lastMsg.Metadata["role"]; got != "analyst" {
		t.Fatalf("role 未透传，实际 %q", got)
	}
}

func TestServer_ChatReturnsUnderlyingErrorMessage(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{err: context.DeadlineExceeded}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(`{"message":"你好"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleChat(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d", w.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if got := resp["error"]; got != context.DeadlineExceeded.Error() {
		t.Fatalf("错误文案未透传，实际 %q", got)
	}
}

func TestServer_ChatSanitizesProviderErrorBody(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{
		err: errors.New(`openai api error: 400 Bad Request, body: {"error":{"message":"Access denied, please make sure your account is in good standing.","type":"Arrearage","code":"Arrearage"},"id":"chatcmpl-123","request_id":"req-123"}`),
	}
	srv := NewServer(cfg, eng, nil, nil)

	req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(`{"message":"你好"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleChat(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d", w.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	want := "Access denied, please make sure your account is in good standing. (code: Arrearage)"
	if got := resp["error"]; got != want {
		t.Fatalf("错误文案未脱敏，期望 %q，实际 %q", want, got)
	}
}

// TestServer_ChatEmptyMessage 测试空消息
func TestServer_ChatEmptyMessage(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"message": ""}`
	req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handleChat(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", w.Code)
	}
}

func TestServer_ChatAllowsImageOnlyMessage(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "已收到图片"}}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"message":"","attachments":[{"type":"image","mime":"image/png","data":"abc123"}]}`
	req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d, body: %s", w.Code, w.Body.String())
	}
	if eng.lastMsg == nil || len(eng.lastMsg.Attachments) != 1 {
		t.Fatalf("引擎未收到图片附件: %#v", eng.lastMsg)
	}
}

func TestServer_ChatRejectsUnsupportedAttachment(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"message":"帮我看文件","attachments":[{"type":"file","mime":"application/pdf","url":"https://example.com/a.pdf"}]}`
	req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleChat(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", w.Code)
	}
	if eng.calls != 0 {
		t.Fatalf("不支持的附件不应进入引擎，实际调用 %d 次", eng.calls)
	}
}

func TestServer_ChatPlatformResolution(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		origin       string
		wantPlatform adapter.Platform
		wantCode     int
	}{
		{
			name:         "默认API平台",
			body:         `{"message":"你好","user_id":"api-user"}`,
			wantPlatform: adapter.PlatformAPI,
			wantCode:     http.StatusOK,
		},
		{
			name:         "显式desktop平台",
			body:         `{"message":"你好","platform":"desktop","user_id":"u1"}`,
			wantPlatform: adapter.PlatformDesktop,
			wantCode:     http.StatusOK,
		},
		{
			name:         "tauri origin自动识别desktop",
			body:         `{"message":"你好"}`,
			origin:       "tauri://localhost",
			wantPlatform: adapter.PlatformDesktop,
			wantCode:     http.StatusOK,
		},
		{
			name:         "桌面兼容用户ID自动识别desktop",
			body:         `{"message":"你好","user_id":"desktop-user"}`,
			wantPlatform: adapter.PlatformDesktop,
			wantCode:     http.StatusOK,
		},
		{
			name:     "拒绝不支持的平台",
			body:     `{"message":"你好","platform":"telegram"}`,
			wantCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			eng := &mockEngine{
				reply: &adapter.Reply{
					Content:  "你好！有什么可以帮你的？",
					Metadata: map[string]string{"provider": "test"},
				},
			}
			srv := NewServer(cfg, eng, nil, nil)

			req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			w := httptest.NewRecorder()

			srv.handleChat(w, req)

			if w.Code != tt.wantCode {
				t.Fatalf("期望 %d，实际 %d, body: %s", tt.wantCode, w.Code, w.Body.String())
			}
			if tt.wantCode != http.StatusOK {
				if eng.calls != 0 {
					t.Fatalf("失败请求不应调用引擎，实际调用 %d 次", eng.calls)
				}
				return
			}
			if eng.lastMsg == nil || eng.lastMsg.Platform != tt.wantPlatform {
				t.Fatalf("平台不匹配: %#v", eng.lastMsg)
			}
		})
	}
}

// TestServer_GatewayReject 测试安全网关拒绝
func TestServer_GatewayReject(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	gw := &mockGateway{
		err: &gateway.GatewayError{
			Layer:   "rate_limit",
			Code:    "minute_exceeded",
			Message: "请求过于频繁",
		},
	}
	srv := NewServer(cfg, eng, gw, nil)

	body := `{"message": "你好", "user_id": "test-user"}`
	req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handleChat(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("期望 403，实际 %d", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["code"] != "minute_exceeded" {
		t.Fatalf("期望 minute_exceeded，实际 %s", resp["code"])
	}
}

// TestServer_InvalidJSON 测试无效 JSON
func TestServer_InvalidJSON(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{invalid json}`
	req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handleChat(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", w.Code)
	}
}
