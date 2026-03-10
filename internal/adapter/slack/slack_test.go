package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"

	"github.com/everyday-items/hexclaw/internal/adapter"
	"github.com/everyday-items/hexclaw/internal/config"
)

// TestNew 测试创建适配器
func TestNew(t *testing.T) {
	a := New(config.SlackConfig{Token: "xoxb-test"})
	if a == nil {
		t.Fatal("适配器不应为 nil")
	}
	if a.Name() != "slack" {
		t.Errorf("名称不匹配: %q", a.Name())
	}
	if a.Platform() != adapter.PlatformSlack {
		t.Errorf("平台不匹配: %q", a.Platform())
	}
}

// TestStart_EmptyToken 测试空 Token
func TestStart_EmptyToken(t *testing.T) {
	a := New(config.SlackConfig{})
	err := a.Start(nil, nil)
	if err == nil {
		t.Error("空 Token 应返回错误")
	}
}

// TestStop_NotStarted 测试未启动时停止
func TestStop_NotStarted(t *testing.T) {
	a := New(config.SlackConfig{Token: "test"})
	if err := a.Stop(nil); err != nil {
		t.Errorf("未启动时停止应无错: %v", err)
	}
}

// TestVerifySignature 测试签名验证
func TestVerifySignature(t *testing.T) {
	secret := "test-signing-secret-12345"
	a := New(config.SlackConfig{SigningSecret: secret})

	timestamp := "1234567890"
	body := []byte(`{"type":"event_callback"}`)

	// 计算正确的签名
	baseString := fmt.Sprintf("v0:%s:%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(baseString))
	validSig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	// 正确签名应通过
	r := &http.Request{Header: http.Header{}}
	r.Header.Set("X-Slack-Request-Timestamp", timestamp)
	r.Header.Set("X-Slack-Signature", validSig)

	if !a.verifySignature(r, body) {
		t.Error("正确签名应通过验证")
	}

	// 错误签名应失败
	r.Header.Set("X-Slack-Signature", "v0=invalid")
	if a.verifySignature(r, body) {
		t.Error("错误签名应失败")
	}
}

// TestVerifySignature_MissingHeaders 测试缺少头部
func TestVerifySignature_MissingHeaders(t *testing.T) {
	a := New(config.SlackConfig{SigningSecret: "secret"})
	r := &http.Request{Header: http.Header{}}

	if a.verifySignature(r, []byte("body")) {
		t.Error("缺少头部时应返回 false")
	}
}

// TestProcessEvent_IgnoreBotMessage 测试忽略 Bot 消息
func TestProcessEvent_IgnoreBotMessage(t *testing.T) {
	a := New(config.SlackConfig{Token: "test"})

	// bot_id 非空的消息应被忽略（不 panic）
	event := []byte(`{"type":"message","channel":"C123","user":"U456","text":"hello","bot_id":"B789"}`)
	a.processEvent(event)
}

// TestProcessEvent_IgnoreSubtype 测试忽略子类型消息
func TestProcessEvent_IgnoreSubtype(t *testing.T) {
	a := New(config.SlackConfig{Token: "test"})

	// 有 subtype 的消息应被忽略
	event := []byte(`{"type":"message","subtype":"message_changed","channel":"C123","user":"U456"}`)
	a.processEvent(event)
}

// TestProcessEvent_IgnoreNonMessage 测试忽略非消息事件
func TestProcessEvent_IgnoreNonMessage(t *testing.T) {
	a := New(config.SlackConfig{Token: "test"})

	// 非 message 类型应被忽略
	event := []byte(`{"type":"reaction_added","channel":"C123"}`)
	a.processEvent(event)
}
