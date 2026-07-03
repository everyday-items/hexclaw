package dingtalk

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// newTestAdapter 创建测试用钉钉适配器
func newTestAdapter() *DingtalkAdapter {
	return New(config.DingtalkConfig{
		Enabled:   true,
		AppKey:    "test-app-key",
		AppSecret: "test-app-secret",
		RobotCode: "test-robot-code",
	})
}

type fakeDingtalkOpenAPI struct {
	mu         sync.Mutex
	token      string
	ttl        time.Duration
	tokenErr   error
	sendErr    error
	sendErrs   []error
	tokenCalls int
	sendCalls  []fakeDingtalkSendCall
}

type fakeDingtalkSendCall struct {
	AccessToken string
	RobotCode   string
	UserID      string
	// Text 是从 MsgParam 载荷解出的正文（sampleText 的 content / sampleMarkdown 的 text），
	// 便于既有断言与消息类型无关地校验内容。
	Text     string
	MsgKey   string
	MsgParam string
}

func newFakeDingtalkOpenAPI(token string) *fakeDingtalkOpenAPI {
	return &fakeDingtalkOpenAPI{token: token, ttl: 2 * time.Hour}
}

func (f *fakeDingtalkOpenAPI) GetAccessToken(_ context.Context, _, _ string) (string, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenCalls++
	if f.tokenErr != nil {
		return "", 0, f.tokenErr
	}
	return f.token, f.ttl, nil
}

func (f *fakeDingtalkOpenAPI) SendOTO(_ context.Context, accessToken, robotCode, userID string, msg dingtalkOutboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var param struct {
		Content string `json:"content"`
		Text    string `json:"text"`
	}
	_ = json.Unmarshal([]byte(msg.MsgParam), &param)
	body := param.Content
	if body == "" {
		body = param.Text
	}
	f.sendCalls = append(f.sendCalls, fakeDingtalkSendCall{
		AccessToken: accessToken,
		RobotCode:   robotCode,
		UserID:      userID,
		Text:        body,
		MsgKey:      msg.MsgKey,
		MsgParam:    msg.MsgParam,
	})
	if len(f.sendErrs) > 0 {
		err := f.sendErrs[0]
		f.sendErrs = f.sendErrs[1:]
		return err
	}
	return f.sendErr
}

func (f *fakeDingtalkOpenAPI) TokenCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

func (f *fakeDingtalkOpenAPI) SendCalls() []fakeDingtalkSendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeDingtalkSendCall, len(f.sendCalls))
	copy(out, f.sendCalls)
	return out
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
	if a.openAPI != nil {
		t.Error("official SDK client 应惰性初始化")
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
		return nil, nil
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
		return nil, nil
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
	fakeAPI := newFakeDingtalkOpenAPI("test-token")
	a.openAPI = fakeAPI

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
	calls := fakeAPI.SendCalls()
	if len(calls) != 2 {
		t.Fatalf("应通过官方 SDK 先发送思考反馈再发送回复，实际 %d 次", len(calls))
	}
	if calls[0].UserID != "user123" || calls[0].Text != dingtalkThinkingFeedback {
		t.Errorf("思考反馈 send call = %+v, 期望 user123/%q", calls[0], dingtalkThinkingFeedback)
	}
	if calls[1].Text != "回复: 你好世界" {
		t.Errorf("回复发送内容 = %q, 期望 %q", calls[1].Text, "回复: 你好世界")
	}
}

func TestHandleMessageThinkingFeedbackFailureDoesNotBlockReply(t *testing.T) {
	a := newTestAdapter()
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "最终回复"}, nil
	}
	fakeAPI := newFakeDingtalkOpenAPI("test-token")
	fakeAPI.sendErrs = []error{errors.New("feedback failed"), nil}
	a.openAPI = fakeAPI

	event := dtEvent{SenderStaffId: "user123", SenderNick: "TestUser"}
	event.Text.Content = "hello"

	a.handleMessage(event)

	calls := fakeAPI.SendCalls()
	if len(calls) != 2 {
		t.Fatalf("反馈失败也应继续发送最终回复，实际发送 %d 次", len(calls))
	}
	if calls[0].Text != dingtalkThinkingFeedback {
		t.Errorf("第一次应发送思考反馈，实际 %q", calls[0].Text)
	}
	if calls[1].Text != "最终回复" {
		t.Errorf("最终回复 = %q, 期望 %q", calls[1].Text, "最终回复")
	}
}

func TestHandleMessageErrorSendsThinkingFeedbackThenErrorReply(t *testing.T) {
	a := newTestAdapter()
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		return nil, errors.New("llm failed")
	}
	fakeAPI := newFakeDingtalkOpenAPI("test-token")
	a.openAPI = fakeAPI

	event := dtEvent{SenderStaffId: "user123", SenderNick: "TestUser"}
	event.Text.Content = "hello"

	a.handleMessage(event)

	calls := fakeAPI.SendCalls()
	if len(calls) != 2 {
		t.Fatalf("handler 失败时应发送思考反馈和错误回复，实际发送 %d 次", len(calls))
	}
	if calls[0].Text != dingtalkThinkingFeedback {
		t.Errorf("第一次应发送思考反馈，实际 %q", calls[0].Text)
	}
	if calls[1].Text != "处理消息时出现错误，请稍后重试。" {
		t.Errorf("错误回复 = %q", calls[1].Text)
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
	fakeAPI := newFakeDingtalkOpenAPI("test-token")
	a.openAPI = fakeAPI
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
	if calls := fakeAPI.SendCalls(); len(calls) != 0 {
		t.Fatalf("空内容不应发送思考反馈，实际发送 %d 次", len(calls))
	}
}

// TestGetAccessToken 测试获取和缓存 Access Token
func TestGetAccessToken(t *testing.T) {
	a := newTestAdapter()
	fakeAPI := newFakeDingtalkOpenAPI("token-1")
	a.openAPI = fakeAPI

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
	if fakeAPI.TokenCalls() != 1 {
		t.Errorf("官方 SDK token 调用次数 = %d, 期望 1（应使用缓存）", fakeAPI.TokenCalls())
	}
}

// TestGetAccessTokenExpired 测试过期 token 会重新获取
func TestGetAccessTokenExpired(t *testing.T) {
	a := newTestAdapter()
	fakeAPI := newFakeDingtalkOpenAPI("token-1")
	a.openAPI = fakeAPI

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
	fakeAPI.mu.Lock()
	fakeAPI.token = "token-2"
	fakeAPI.mu.Unlock()

	// 应重新获取
	token2, err := a.getAccessToken(ctx)
	if err != nil {
		t.Fatalf("重新获取 token 失败: %v", err)
	}
	if token2 != "token-2" {
		t.Errorf("token = %q, 期望 %q（应重新获取）", token2, "token-2")
	}
	if fakeAPI.TokenCalls() != 2 {
		t.Errorf("官方 SDK token 调用次数 = %d, 期望 2", fakeAPI.TokenCalls())
	}
}

// TestMarshalMarkdownContent 测试出站 markdown 载荷的安全 JSON 序列化
// （BUG-20260703 B7 后出站统一 sampleMarkdown，{"title","text"} 载荷）。
func TestMarshalMarkdownContent(t *testing.T) {
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
			result := marshalMarkdownContent("标题", tt.input)
			var parsed map[string]string
			if err := json.Unmarshal([]byte(result), &parsed); err != nil {
				t.Errorf("marshalMarkdownContent(%q) 产生非法 JSON: %s", tt.input, result)
			}
			if parsed["text"] != tt.input {
				t.Errorf("marshalMarkdownContent(%q) 反序列化后不匹配: %q", tt.input, parsed["text"])
			}
			if parsed["title"] != "标题" {
				t.Errorf("title 不匹配: %q", parsed["title"])
			}
		})
	}
}

// TestSend 测试发送消息
func TestSend(t *testing.T) {
	a := newTestAdapter()
	fakeAPI := newFakeDingtalkOpenAPI("my-token")
	a.openAPI = fakeAPI

	err := a.Send(context.Background(), "user1", &adapter.Reply{Content: "hello"})
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}

	calls := fakeAPI.SendCalls()
	if len(calls) != 1 {
		t.Fatalf("官方 SDK send 调用次数 = %d, 期望 1", len(calls))
	}
	if calls[0].AccessToken != "my-token" {
		t.Errorf("token = %q, 期望 %q", calls[0].AccessToken, "my-token")
	}
	if calls[0].RobotCode != "test-robot-code" {
		t.Errorf("robotCode = %v, 期望 %q", calls[0].RobotCode, "test-robot-code")
	}
	if calls[0].UserID != "user1" || calls[0].Text != "hello" {
		t.Errorf("send call = %+v, 期望 user1/hello", calls[0])
	}
}

// TestSendStream 测试流式发送（拼接后发送）
func TestSendStream(t *testing.T) {
	a := newTestAdapter()
	fakeAPI := newFakeDingtalkOpenAPI("tok")
	a.openAPI = fakeAPI

	chunks := make(chan *adapter.ReplyChunk, 3)
	chunks <- &adapter.ReplyChunk{Content: "Hello "}
	chunks <- &adapter.ReplyChunk{Content: "World"}
	chunks <- &adapter.ReplyChunk{Content: "!", Done: true}
	close(chunks)

	err := a.SendStream(context.Background(), "user1", chunks)
	if err != nil {
		t.Fatalf("SendStream 失败: %v", err)
	}

	calls := fakeAPI.SendCalls()
	if len(calls) != 1 {
		t.Fatalf("官方 SDK send 调用次数 = %d, 期望 1", len(calls))
	}
	if calls[0].Text != "Hello World!" {
		t.Errorf("发送内容 = %q, 期望 %q", calls[0].Text, "Hello World!")
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

// TestStopNoConnection 测试无连接时 Stop 不报错
func TestStopNoConnection(t *testing.T) {
	a := newTestAdapter()

	err := a.Stop(context.Background())
	if err != nil {
		t.Errorf("Stop 不应报错: %v", err)
	}
}
