package dingtalk

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// mockTransport 自定义 HTTP 传输，用于拦截请求
type mockTransport struct {
	handler func(req *http.Request) (*http.Response, error)
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.handler(req)
}

// newTestAdapter 创建测试用钉钉适配器
func newTestAdapter() *DingtalkAdapter {
	return New(config.DingtalkConfig{
		Enabled:   true,
		AppKey:    "test-app-key",
		AppSecret: "test-app-secret",
		RobotCode: "test-robot-code",
	})
}

// TestNew 测试创建适配器
func TestNew(t *testing.T) {
	cfg := config.DingtalkConfig{
		Enabled:   true,
		AppKey:    "key",
		AppSecret: "secret",
		RobotCode: "robot",
	}
	a := New(cfg)

	if a == nil {
		t.Fatal("New() 返回 nil")
	}
	if a.cfg.AppKey != "key" {
		t.Errorf("AppKey = %q, 期望 %q", a.cfg.AppKey, "key")
	}
	if a.cfg.AppSecret != "secret" {
		t.Errorf("AppSecret = %q, 期望 %q", a.cfg.AppSecret, "secret")
	}
	if a.client == nil {
		t.Error("client 不应为 nil")
	}
}

// TestName 测试 Name() 返回值
func TestName(t *testing.T) {
	a := newTestAdapter()
	if got := a.Name(); got != "dingtalk" {
		t.Errorf("Name() = %q, 期望 %q", got, "dingtalk")
	}
}

// TestPlatform 测试 Platform() 返回值
func TestPlatform(t *testing.T) {
	a := newTestAdapter()
	if got := a.Platform(); got != adapter.PlatformDingtalk {
		t.Errorf("Platform() = %q, 期望 %q", got, adapter.PlatformDingtalk)
	}
}

// TestVerifySign 测试签名验证
func TestVerifySign(t *testing.T) {
	a := newTestAdapter()
	secret := a.cfg.AppSecret

	// 生成正确签名
	timestamp := "1234567890"
	stringToSign := timestamp + "\n" + secret
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(stringToSign))
	validSign := base64.StdEncoding.EncodeToString(h.Sum(nil))

	tests := []struct {
		name      string
		timestamp string
		sign      string
		want      bool
	}{
		{
			name:      "有效签名",
			timestamp: timestamp,
			sign:      validSign,
			want:      true,
		},
		{
			name:      "无效签名",
			timestamp: timestamp,
			sign:      "invalid-sign",
			want:      false,
		},
		{
			name:      "空 timestamp",
			timestamp: "",
			sign:      validSign,
			want:      false,
		},
		{
			name:      "空 sign",
			timestamp: timestamp,
			sign:      "",
			want:      false,
		},
		{
			name:      "空 timestamp 和 sign",
			timestamp: "",
			sign:      "",
			want:      false,
		},
		{
			name:      "不同 timestamp 的签名",
			timestamp: "9999999999",
			sign:      validSign,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.verifySign(tt.timestamp, tt.sign)
			if got != tt.want {
				t.Errorf("verifySign(%q, %q) = %v, 期望 %v", tt.timestamp, tt.sign, got, tt.want)
			}
		})
	}
}

// TestHandleWebhookInvalidSignature 测试签名验证失败时返回 401
func TestHandleWebhookInvalidSignature(t *testing.T) {
	a := newTestAdapter()
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "ok"}, nil
	}

	body := `{"text":{"content":"hello"},"senderStaffId":"user1","senderNick":"Test"}`
	req := httptest.NewRequest("POST", "/dingtalk/webhook", strings.NewReader(body))
	req.Header.Set("timestamp", "1234567890")
	req.Header.Set("sign", "invalid-signature")

	w := httptest.NewRecorder()
	a.handleWebhook(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("状态码 = %d, 期望 %d", w.Code, http.StatusUnauthorized)
	}
}

// TestHandleWebhookNoSignatureCheck 测试 AppSecret 为空时跳过签名验证
func TestHandleWebhookNoSignatureCheck(t *testing.T) {
	a := New(config.DingtalkConfig{
		Enabled:   true,
		AppKey:    "key",
		AppSecret: "", // 空 secret 不验证签名
		RobotCode: "robot",
	})
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "ok"}, nil
	}
	// 使用 mock transport 避免真实 HTTP 调用（handleMessage 中的 Send 会发请求）
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"accessToken":"test","expireIn":7200}`)),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	body := `{"text":{"content":"hello"},"senderStaffId":"user1","senderNick":"Test"}`
	req := httptest.NewRequest("POST", "/dingtalk/webhook", strings.NewReader(body))

	w := httptest.NewRecorder()
	a.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("状态码 = %d, 期望 %d（空 secret 应跳过签名验证）", w.Code, http.StatusOK)
	}
}

// TestHandleWebhookInvalidJSON 测试无效 JSON 请求体
func TestHandleWebhookInvalidJSON(t *testing.T) {
	a := New(config.DingtalkConfig{AppSecret: ""})
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "ok"}, nil
	}

	req := httptest.NewRequest("POST", "/dingtalk/webhook", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	a.handleWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleWebhookValidMessage 测试有效消息的处理
func TestHandleWebhookValidMessage(t *testing.T) {
	a := New(config.DingtalkConfig{AppSecret: ""})

	handlerCalled := make(chan bool, 1)
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		handlerCalled <- true
		return &adapter.Reply{Content: "ok"}, nil
	}
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"accessToken":"test","expireIn":7200}`)),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	event := dtEvent{
		ConversationId: "conv-1",
		SenderStaffId:  "user1",
		SenderNick:     "TestUser",
	}
	event.Text.Content = "hello world"

	body, _ := json.Marshal(event)
	req := httptest.NewRequest("POST", "/dingtalk/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	a.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("状态码 = %d, 期望 %d", w.Code, http.StatusOK)
	}

	// handleMessage 在 goroutine 中运行，等待它被调用
	select {
	case <-handlerCalled:
		// 正常
	case <-time.After(2 * time.Second):
		t.Error("handler 未被调用")
	}
}

// TestHandleMessage 测试消息处理逻辑
func TestHandleMessage(t *testing.T) {
	var capturedMsg *adapter.Message

	a := newTestAdapter()
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		capturedMsg = msg
		return &adapter.Reply{Content: "回复: " + msg.Content}, nil
	}
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				// getAccessToken 和 Send 都走这里
				if strings.Contains(req.URL.Path, "accessToken") {
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"accessToken":"test-token","expireIn":7200}`)),
						Header:     make(http.Header),
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	event := dtEvent{
		ConversationId:   "conv-1",
		ConversationType: "1",
		SenderStaffId:    "user123",
		SenderNick:       "TestUser",
	}
	event.Text.Content = "  你好世界  "

	a.handleMessage(event)

	if capturedMsg == nil {
		t.Fatal("handler 未被调用")
	}
	if capturedMsg.Content != "你好世界" {
		t.Errorf("Content = %q, 期望 %q（应 TrimSpace）", capturedMsg.Content, "你好世界")
	}
	if capturedMsg.Platform != adapter.PlatformDingtalk {
		t.Errorf("Platform = %q, 期望 %q", capturedMsg.Platform, adapter.PlatformDingtalk)
	}
	if capturedMsg.UserID != "user123" {
		t.Errorf("UserID = %q, 期望 %q", capturedMsg.UserID, "user123")
	}
	if capturedMsg.UserName != "TestUser" {
		t.Errorf("UserName = %q, 期望 %q", capturedMsg.UserName, "TestUser")
	}
}

// TestHandleMessageNilHandler 测试 handler 为 nil 时不 panic
func TestHandleMessageNilHandler(t *testing.T) {
	a := newTestAdapter()
	a.handler = nil

	event := dtEvent{}
	event.Text.Content = "hello"

	// 不应 panic
	a.handleMessage(event)
}

// TestHandleMessageEmptyContent 测试空内容消息被忽略
func TestHandleMessageEmptyContent(t *testing.T) {
	handlerCalled := false
	a := newTestAdapter()
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		handlerCalled = true
		return &adapter.Reply{Content: "ok"}, nil
	}

	event := dtEvent{}
	event.Text.Content = "   " // 仅空格

	a.handleMessage(event)

	if handlerCalled {
		t.Error("空内容不应调用 handler")
	}
}

// TestGetAccessToken 测试获取和缓存 Access Token
func TestGetAccessToken(t *testing.T) {
	callCount := 0

	a := newTestAdapter()
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				callCount++
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(
						fmt.Sprintf(`{"accessToken":"token-%d","expireIn":7200}`, callCount),
					)),
					Header: make(http.Header),
				}, nil
			},
		},
	}

	ctx := context.Background()

	// 第一次获取
	token1, err := a.getAccessToken(ctx)
	if err != nil {
		t.Fatalf("第一次获取 token 失败: %v", err)
	}
	if token1 != "token-1" {
		t.Errorf("token = %q, 期望 %q", token1, "token-1")
	}

	// 第二次获取应使用缓存
	token2, err := a.getAccessToken(ctx)
	if err != nil {
		t.Fatalf("第二次获取 token 失败: %v", err)
	}
	if token2 != "token-1" {
		t.Errorf("token = %q, 期望缓存值 %q", token2, "token-1")
	}
	if callCount != 1 {
		t.Errorf("API 调用次数 = %d, 期望 1（应使用缓存）", callCount)
	}
}

// TestGetAccessTokenExpired 测试过期 token 会重新获取
func TestGetAccessTokenExpired(t *testing.T) {
	callCount := 0

	a := newTestAdapter()
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				callCount++
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(strings.NewReader(
						fmt.Sprintf(`{"accessToken":"token-%d","expireIn":7200}`, callCount),
					)),
					Header: make(http.Header),
				}, nil
			},
		},
	}

	ctx := context.Background()

	// 第一次获取
	_, err := a.getAccessToken(ctx)
	if err != nil {
		t.Fatalf("获取 token 失败: %v", err)
	}

	// 手动设置过期时间为过去
	a.mu.Lock()
	a.tokenExpiry = time.Now().Add(-1 * time.Hour)
	a.mu.Unlock()

	// 应重新获取
	token2, err := a.getAccessToken(ctx)
	if err != nil {
		t.Fatalf("重新获取 token 失败: %v", err)
	}
	if token2 != "token-2" {
		t.Errorf("token = %q, 期望 %q（应重新获取）", token2, "token-2")
	}
	if callCount != 2 {
		t.Errorf("API 调用次数 = %d, 期望 2", callCount)
	}
}

// TestMarshalTextContent 测试安全 JSON 序列化
func TestMarshalTextContent(t *testing.T) {
	tests := []struct {
		input string
	}{
		{"hello"},
		{`say "hello"`},
		{"line1\nline2"},
		{`back\slash`},
		{"含\x00null字节"},
		{""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := marshalTextContent(tt.input)
			var parsed map[string]string
			if err := json.Unmarshal([]byte(result), &parsed); err != nil {
				t.Errorf("marshalTextContent(%q) 产生非法 JSON: %s", tt.input, result)
			}
			if parsed["content"] != tt.input {
				t.Errorf("marshalTextContent(%q) 反序列化后不匹配: %q", tt.input, parsed["content"])
			}
		})
	}
}

// TestSend 测试发送消息
func TestSend(t *testing.T) {
	var capturedBody map[string]any
	var capturedToken string

	a := newTestAdapter()
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "accessToken") {
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"accessToken":"my-token","expireIn":7200}`)),
						Header:     make(http.Header),
					}, nil
				}
				capturedToken = req.Header.Get("x-acs-dingtalk-access-token")
				body, _ := io.ReadAll(req.Body)
				_ = json.Unmarshal(body, &capturedBody)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	err := a.Send(context.Background(), "user1", &adapter.Reply{Content: "hello"})
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}

	if capturedToken != "my-token" {
		t.Errorf("token = %q, 期望 %q", capturedToken, "my-token")
	}
	if capturedBody["robotCode"] != "test-robot-code" {
		t.Errorf("robotCode = %v, 期望 %q", capturedBody["robotCode"], "test-robot-code")
	}
}

// TestSendStream 测试流式发送（拼接后发送）
func TestSendStream(t *testing.T) {
	var capturedBody map[string]any

	a := newTestAdapter()
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "accessToken") {
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"accessToken":"tok","expireIn":7200}`)),
						Header:     make(http.Header),
					}, nil
				}
				body, _ := io.ReadAll(req.Body)
				_ = json.Unmarshal(body, &capturedBody)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	chunks := make(chan *adapter.ReplyChunk, 3)
	chunks <- &adapter.ReplyChunk{Content: "Hello "}
	chunks <- &adapter.ReplyChunk{Content: "World"}
	chunks <- &adapter.ReplyChunk{Content: "!", Done: true}
	close(chunks)

	err := a.SendStream(context.Background(), "user1", chunks)
	if err != nil {
		t.Fatalf("SendStream 失败: %v", err)
	}

	// 验证发送的 msgParam 包含拼接后的内容
	if msgParam, ok := capturedBody["msgParam"].(string); ok {
		if !strings.Contains(msgParam, "Hello World!") {
			t.Errorf("msgParam = %q, 期望包含 %q", msgParam, "Hello World!")
		}
	}
}

// TestSendStreamError 测试流式发送中遇到错误
func TestSendStreamError(t *testing.T) {
	a := newTestAdapter()

	chunks := make(chan *adapter.ReplyChunk, 3)
	chunks <- &adapter.ReplyChunk{Content: "Hello "}
	chunks <- &adapter.ReplyChunk{Error: context.Canceled}
	close(chunks)

	err := a.SendStream(context.Background(), "user1", chunks)
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if err != context.Canceled {
		t.Errorf("错误 = %v, 期望 context.Canceled", err)
	}
}

// TestStopNilServer 测试 server 为 nil 时 Stop 不报错
func TestStopNilServer(t *testing.T) {
	a := newTestAdapter()
	a.server = nil

	err := a.Stop(context.Background())
	if err != nil {
		t.Errorf("Stop 不应报错: %v", err)
	}
}
