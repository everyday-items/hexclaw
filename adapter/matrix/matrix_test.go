package matrix

import (
	"testing"
)

func TestMatrixAdapter_NameAndPlatform(t *testing.T) {
	a := New(Config{
		HomeserverURL: "https://matrix.example.com",
		AccessToken:   "test-token",
		UserID:        "@bot:example.com",
	})
	if a.Name() != "matrix" {
		t.Errorf("期望 matrix, 得到 %s", a.Name())
	}
	if a.Platform() != PlatformMatrix {
		t.Errorf("期望 matrix, 得到 %s", a.Platform())
	}
}

func TestMatrixAdapter_DefaultConfig(t *testing.T) {
	a := New(Config{})
	if a.config.SyncTimeout != 30 {
		t.Errorf("期望默认同步超时 30, 得到 %d", a.config.SyncTimeout)
	}
}

func TestMatrixAdapter_HandleEvent_SkipSelf(t *testing.T) {
	a := New(Config{UserID: "@bot:example.com"})

	// 不应处理自己发的消息
	event := matrixEvent{
		Type:    "m.room.message",
		Sender:  "@bot:example.com",
		Content: map[string]any{"msgtype": "m.text", "body": "hello"},
	}

	// handler 未设置，handleEvent 应跳过自己的消息不 panic
	a.handleEvent("!room:example.com", event)
}

func TestMatrixAdapter_HandleEvent_SkipNonText(t *testing.T) {
	a := New(Config{UserID: "@bot:example.com"})

	// 非文本消息应跳过
	event := matrixEvent{
		Type:    "m.room.message",
		Sender:  "@user:example.com",
		Content: map[string]any{"msgtype": "m.image", "url": "mxc://..."},
	}

	a.handleEvent("!room:example.com", event)
}
