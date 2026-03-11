package discord

import (
	"testing"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
)

// TestNew 测试创建适配器
func TestNew(t *testing.T) {
	a := New(config.DiscordConfig{Token: "test-token"})
	if a == nil {
		t.Fatal("适配器不应为 nil")
	}
	if a.Name() != "discord" {
		t.Errorf("名称不匹配: %q", a.Name())
	}
	if a.Platform() != adapter.PlatformDiscord {
		t.Errorf("平台不匹配: %q", a.Platform())
	}
}

// TestStart_EmptyToken 测试空 Token 启动
func TestStart_EmptyToken(t *testing.T) {
	a := New(config.DiscordConfig{})
	err := a.Start(nil, nil)
	if err == nil {
		t.Error("空 Token 应返回错误")
	}
}

// TestStop 测试停止
func TestStop(t *testing.T) {
	a := New(config.DiscordConfig{Token: "test"})
	// 未启动时停止应不报错
	if err := a.Stop(nil); err != nil {
		t.Errorf("停止应无错: %v", err)
	}
}

// TestHandleEvent_Heartbeat 测试心跳 ACK 处理
func TestHandleEvent_Heartbeat(t *testing.T) {
	a := New(config.DiscordConfig{Token: "test"})
	// op=11 是心跳 ACK，应正常处理不 panic
	a.handleEvent([]byte(`{"op":11}`))
}

// TestHandleEvent_InvalidJSON 测试无效 JSON
func TestHandleEvent_InvalidJSON(t *testing.T) {
	a := New(config.DiscordConfig{Token: "test"})
	// 无效 JSON 应不 panic
	a.handleEvent([]byte(`{invalid}`))
}

// TestHandleEvent_MessageCreate_BotIgnored 测试忽略 Bot 消息
func TestHandleEvent_MessageCreate_BotIgnored(t *testing.T) {
	a := New(config.DiscordConfig{Token: "test"})

	// Bot 消息应被忽略（不设 handler 也不 panic）
	event := `{"op":0,"t":"MESSAGE_CREATE","d":{"id":"1","channel_id":"ch1","author":{"id":"bot1","username":"bot","bot":true},"content":"hello"}}`
	a.handleEvent([]byte(event))
}

// TestHandleEvent_Ready 测试 READY 事件
func TestHandleEvent_Ready(t *testing.T) {
	a := New(config.DiscordConfig{Token: "test"})
	event := `{"op":0,"t":"READY","d":{"session_id":"sess-123"}}`
	a.handleEvent([]byte(event))

	if a.sessionID != "sess-123" {
		t.Errorf("sessionID 不匹配: %q", a.sessionID)
	}
}

// TestSequenceTracking 测试序列号追踪
func TestSequenceTracking(t *testing.T) {
	a := New(config.DiscordConfig{Token: "test"})
	a.handleEvent([]byte(`{"op":0,"s":42,"t":"READY","d":{}}`))

	if a.seq.Load() != 42 {
		t.Errorf("序列号不匹配: %d", a.seq.Load())
	}
}
