package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
)

// mockTransport 自定义 HTTP 传输，用于拦截请求
type mockTransport struct {
	handler func(req *http.Request) (*http.Response, error)
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.handler(req)
}

// newTestAdapter 创建测试用 Telegram 适配器
func newTestAdapter() *TelegramAdapter {
	return New(config.TelegramConfig{
		Enabled: true,
		Token:   "test-token-123",
	})
}

// TestNew 测试创建适配器
func TestNew(t *testing.T) {
	cfg := config.TelegramConfig{
		Enabled: true,
		Token:   "my-bot-token",
	}
	a := New(cfg)

	if a == nil {
		t.Fatal("New() 返回 nil")
	}
	if a.cfg.Token != "my-bot-token" {
		t.Errorf("Token = %q, 期望 %q", a.cfg.Token, "my-bot-token")
	}
	if a.client == nil {
		t.Error("client 不应为 nil")
	}
	if a.stopped.Load() {
		t.Error("stopped 初始值应为 false")
	}
}

// TestName 测试 Name() 返回值
func TestName(t *testing.T) {
	a := newTestAdapter()
	if got := a.Name(); got != "telegram" {
		t.Errorf("Name() = %q, 期望 %q", got, "telegram")
	}
}

// TestPlatform 测试 Platform() 返回值
func TestPlatform(t *testing.T) {
	a := newTestAdapter()
	if got := a.Platform(); got != adapter.PlatformTelegram {
		t.Errorf("Platform() = %q, 期望 %q", got, adapter.PlatformTelegram)
	}
}

// TestSendMessage 测试 sendMessage 发送正确的 JSON 请求
func TestSendMessage(t *testing.T) {
	var capturedReq struct {
		ChatID    string `json:"chat_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}
	var capturedURL string
	var capturedContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.Path
		capturedContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedReq)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	a := newTestAdapter()
	// 使用自定义传输将请求重定向到测试服务器
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				// 将请求重定向到测试服务器
				req.URL.Scheme = "http"
				req.URL.Host = strings.TrimPrefix(server.URL, "http://")
				return http.DefaultTransport.RoundTrip(req)
			},
		},
	}

	ctx := context.Background()
	err := a.sendMessage(ctx, "12345", "hello world")
	if err != nil {
		t.Fatalf("sendMessage 失败: %v", err)
	}

	if capturedReq.ChatID != "12345" {
		t.Errorf("chat_id = %q, 期望 %q", capturedReq.ChatID, "12345")
	}
	if capturedReq.Text != "hello world" {
		t.Errorf("text = %q, 期望 %q", capturedReq.Text, "hello world")
	}
	if capturedReq.ParseMode != "Markdown" {
		t.Errorf("parse_mode = %q, 期望 %q", capturedReq.ParseMode, "Markdown")
	}
	if capturedContentType != "application/json" {
		t.Errorf("Content-Type = %q, 期望 %q", capturedContentType, "application/json")
	}
	if !strings.Contains(capturedURL, "/sendMessage") {
		t.Errorf("URL 路径 = %q, 期望包含 /sendMessage", capturedURL)
	}
}

// TestSendMessageError 测试 sendMessage 在 API 返回非 200 时报错
func TestSendMessageError(t *testing.T) {
	a := newTestAdapter()
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Body:       io.NopCloser(strings.NewReader(`{"ok":false,"description":"Forbidden"}`)),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	err := a.sendMessage(context.Background(), "123", "test")
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误信息应包含状态码 403，实际: %v", err)
	}
}

// TestSend 测试 Send 方法调用 sendMessage
func TestSend(t *testing.T) {
	var capturedBody map[string]any

	a := newTestAdapter()
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				json.Unmarshal(body, &capturedBody)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("")),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	err := a.Send(context.Background(), "chat-1", &adapter.Reply{Content: "hello"})
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}

	if capturedBody["text"] != "hello" {
		t.Errorf("text = %v, 期望 %q", capturedBody["text"], "hello")
	}
	if capturedBody["chat_id"] != "chat-1" {
		t.Errorf("chat_id = %v, 期望 %q", capturedBody["chat_id"], "chat-1")
	}
}

// TestGetUpdates 测试解析 Telegram API 更新响应
func TestGetUpdates(t *testing.T) {
	response := `{
		"ok": true,
		"result": [
			{
				"update_id": 100,
				"message": {
					"message_id": 1,
					"from": {"id": 42, "username": "testuser", "first_name": "Test"},
					"chat": {"id": 99, "type": "private"},
					"text": "hello"
				}
			},
			{
				"update_id": 101,
				"message": {
					"message_id": 2,
					"from": {"id": 43, "username": "user2", "first_name": "User"},
					"chat": {"id": 100, "type": "group"},
					"text": "world"
				}
			}
		]
	}`

	a := newTestAdapter()
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(response)),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	updates, err := a.getUpdates()
	if err != nil {
		t.Fatalf("getUpdates 失败: %v", err)
	}

	if len(updates) != 2 {
		t.Fatalf("期望 2 条更新，实际 %d 条", len(updates))
	}

	if updates[0].UpdateID != 100 {
		t.Errorf("UpdateID = %d, 期望 100", updates[0].UpdateID)
	}
	if updates[0].Message.Text != "hello" {
		t.Errorf("Text = %q, 期望 %q", updates[0].Message.Text, "hello")
	}
	if updates[0].Message.From.Username != "testuser" {
		t.Errorf("Username = %q, 期望 %q", updates[0].Message.From.Username, "testuser")
	}
	if updates[0].Message.Chat.ID != 99 {
		t.Errorf("Chat.ID = %d, 期望 99", updates[0].Message.Chat.ID)
	}
	if updates[1].Message.Chat.Type != "group" {
		t.Errorf("Chat.Type = %q, 期望 %q", updates[1].Message.Chat.Type, "group")
	}
}

// TestGetUpdatesNotOK 测试 Telegram API 返回 ok=false 时的错误处理
func TestGetUpdatesNotOK(t *testing.T) {
	a := newTestAdapter()
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"ok":false}`)),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	_, err := a.getUpdates()
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if !strings.Contains(err.Error(), "返回失败") {
		t.Errorf("错误信息应包含'返回失败'，实际: %v", err)
	}
}

// TestGetUpdatesInvalidJSON 测试无效 JSON 响应
func TestGetUpdatesInvalidJSON(t *testing.T) {
	a := newTestAdapter()
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`not json`)),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	_, err := a.getUpdates()
	if err == nil {
		t.Fatal("期望返回 JSON 解析错误")
	}
}

// TestHandleMessage 测试消息处理（调用 handler 并发送回复）
func TestHandleMessage(t *testing.T) {
	var sentTexts []string

	a := newTestAdapter()
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				var m map[string]any
				json.Unmarshal(body, &m)
				if text, ok := m["text"].(string); ok {
					sentTexts = append(sentTexts, text)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("")),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "回复: " + msg.Content}, nil
	}

	tgMsg := &tgMessage{
		MessageID: 1,
		From:      tgUser{ID: 42, Username: "testuser"},
		Chat:      tgChat{ID: 99, Type: "private"},
		Text:      "你好",
	}

	// handleMessage 是同步调用（在测试中直接调用而非 goroutine）
	a.handleMessage(tgMsg)

	if len(sentTexts) != 1 {
		t.Fatalf("期望发送 1 条消息，实际 %d 条", len(sentTexts))
	}
	if sentTexts[0] != "回复: 你好" {
		t.Errorf("回复内容 = %q, 期望 %q", sentTexts[0], "回复: 你好")
	}
}

// TestHandleMessageNilHandler 测试 handler 为 nil 时不 panic
func TestHandleMessageNilHandler(t *testing.T) {
	a := newTestAdapter()
	a.handler = nil

	tgMsg := &tgMessage{
		MessageID: 1,
		From:      tgUser{ID: 42, Username: "test"},
		Chat:      tgChat{ID: 99, Type: "private"},
		Text:      "hello",
	}

	// 不应 panic
	a.handleMessage(tgMsg)
}

// TestHandleMessageError 测试 handler 返回错误时发送错误提示
func TestHandleMessageError(t *testing.T) {
	var sentTexts []string

	a := newTestAdapter()
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				var m map[string]any
				json.Unmarshal(body, &m)
				if text, ok := m["text"].(string); ok {
					sentTexts = append(sentTexts, text)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("")),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		return nil, context.DeadlineExceeded
	}

	tgMsg := &tgMessage{
		MessageID: 1,
		From:      tgUser{ID: 42, Username: "test"},
		Chat:      tgChat{ID: 99, Type: "private"},
		Text:      "hello",
	}

	a.handleMessage(tgMsg)

	if len(sentTexts) != 1 {
		t.Fatalf("期望发送 1 条错误提示，实际 %d 条", len(sentTexts))
	}
	if !strings.Contains(sentTexts[0], "错误") {
		t.Errorf("错误提示应包含'错误'，实际: %q", sentTexts[0])
	}
}

// TestStartAndStop 测试启动和停止
func TestStartAndStop(t *testing.T) {
	a := newTestAdapter()
	// 使用一个总是返回错误的 transport，防止真的发 HTTP 请求
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				// 返回空更新，避免 pollLoop 长时间阻塞
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":[]}`)),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	handlerCalled := false
	handler := func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		handlerCalled = true
		return &adapter.Reply{Content: "ok"}, nil
	}

	err := a.Start(context.Background(), handler)
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}

	if a.handler == nil {
		t.Error("handler 不应为 nil")
	}
	if a.stopped.Load() {
		t.Error("启动后 stopped 应为 false")
	}

	// 立即停止
	err = a.Stop(context.Background())
	if err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}

	if !a.stopped.Load() {
		t.Error("停止后 stopped 应为 true")
	}

	// handler 不应被调用（没有消息）
	_ = handlerCalled
}

// TestPollLoopStops 测试 pollLoop 在 stopped=true 时退出
func TestPollLoopStops(t *testing.T) {
	a := newTestAdapter()
	a.stopped.Store(true)

	// pollLoop 应该立即退出
	done := make(chan struct{})
	go func() {
		a.pollLoop()
		close(done)
	}()

	select {
	case <-done:
		// 正常退出
	case <-time.After(2 * time.Second):
		t.Fatal("pollLoop 未在 stopped=true 时退出")
	}
}

// TestSendStream 测试流式发送（拼接后发送）
func TestSendStream(t *testing.T) {
	var sentText string

	a := newTestAdapter()
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				var m map[string]any
				json.Unmarshal(body, &m)
				if text, ok := m["text"].(string); ok {
					sentText = text
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("")),
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

	err := a.SendStream(context.Background(), "123", chunks)
	if err != nil {
		t.Fatalf("SendStream 失败: %v", err)
	}

	if sentText != "Hello World!" {
		t.Errorf("发送内容 = %q, 期望 %q", sentText, "Hello World!")
	}
}

// TestSendStreamError 测试流式发送中遇到错误时中止
func TestSendStreamError(t *testing.T) {
	a := newTestAdapter()
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("")),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	chunks := make(chan *adapter.ReplyChunk, 3)
	chunks <- &adapter.ReplyChunk{Content: "Hello "}
	chunks <- &adapter.ReplyChunk{Error: context.Canceled}
	close(chunks)

	err := a.SendStream(context.Background(), "123", chunks)
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if err != context.Canceled {
		t.Errorf("错误 = %v, 期望 context.Canceled", err)
	}
}

// TestGetUpdatesURLFormat 测试 getUpdates 构建的 URL 格式
func TestGetUpdatesURLFormat(t *testing.T) {
	var capturedURL string

	a := newTestAdapter()
	a.offset = 42
	a.client = &http.Client{
		Transport: &mockTransport{
			handler: func(req *http.Request) (*http.Response, error) {
				capturedURL = req.URL.String()
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":[]}`)),
					Header:     make(http.Header),
				}, nil
			},
		},
	}

	a.getUpdates()

	if !strings.Contains(capturedURL, "test-token-123") {
		t.Errorf("URL 应包含 token，实际: %s", capturedURL)
	}
	if !strings.Contains(capturedURL, "offset=42") {
		t.Errorf("URL 应包含 offset=42，实际: %s", capturedURL)
	}
	if !strings.Contains(capturedURL, "getUpdates") {
		t.Errorf("URL 应包含 getUpdates，实际: %s", capturedURL)
	}
}
