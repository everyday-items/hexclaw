package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
	"github.com/everyday-items/hexclaw/gateway"
)

// mockEngine 测试用引擎
type mockEngine struct {
	reply *adapter.Reply
	err   error
}

func (e *mockEngine) Start(_ context.Context) error { return nil }
func (e *mockEngine) Stop(_ context.Context) error  { return nil }
func (e *mockEngine) Health(_ context.Context) error { return nil }
func (e *mockEngine) Process(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
	return e.reply, e.err
}
func (e *mockEngine) ProcessStream(_ context.Context, _ *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
	return nil, nil
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
	srv := NewServer(cfg, eng, nil)

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
	srv := NewServer(cfg, eng, nil)

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
}

// TestServer_ChatEmptyMessage 测试空消息
func TestServer_ChatEmptyMessage(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{}
	srv := NewServer(cfg, eng, nil)

	body := `{"message": ""}`
	req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handleChat(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", w.Code)
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
	srv := NewServer(cfg, eng, gw)

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
	srv := NewServer(cfg, eng, nil)

	body := `{invalid json}`
	req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(body))
	w := httptest.NewRecorder()

	srv.handleChat(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", w.Code)
	}
}
