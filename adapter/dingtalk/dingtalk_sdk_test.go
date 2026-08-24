package dingtalk

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	dtchatbot "github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	dtclient "github.com/open-dingtalk/dingtalk-stream-sdk-go/client"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// TestOnChatBotMessage_MapsToAdapterMessage 差分测试（方法5）：官方 SDK 的机器人回调
// BotCallbackDataModel 经 onChatBotMessage 映射出的 adapter.Message，字段须与历史手搓 Stream
// 路径（dtEvent → handleMessage）逐项一致——确保换连接层不改变上层消息语义。
func TestOnChatBotMessage_MapsToAdapterMessage(t *testing.T) {
	captured := make(chan *adapter.Message, 1)
	a := newTestAdapter()
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		captured <- msg
		return &adapter.Reply{Content: "ok"}, nil
	}
	a.openAPI = newFakeDingtalkOpenAPI("tok")

	data := &dtchatbot.BotCallbackDataModel{
		MsgId:            "provider-msg-map-1",
		ConversationId:   "conv-1",
		ConversationType: "1",
		SenderStaffId:    "staff-42",
		SenderNick:       "张三",
	}
	data.Text.Content = "  你好世界  "

	ack, err := a.onChatBotMessage(context.Background(), data)
	if err != nil {
		t.Fatalf("onChatBotMessage 不应返回错误（应立即成功 ack，异步处理）: %v", err)
	}
	if ack == nil {
		t.Fatal("onChatBotMessage 应返回非 nil ack 数据（哪怕空串）")
	}

	select {
	case msg := <-captured:
		if msg.Content != "你好世界" {
			t.Errorf("Content = %q, 期望 %q（应 TrimSpace）", msg.Content, "你好世界")
		}
		if msg.Platform != adapter.PlatformDingtalk {
			t.Errorf("Platform = %q, 期望 %q", msg.Platform, adapter.PlatformDingtalk)
		}
		if msg.UserID != "staff-42" {
			t.Errorf("UserID = %q, 期望 %q", msg.UserID, "staff-42")
		}
		if msg.UserName != "张三" {
			t.Errorf("UserName = %q, 期望 %q", msg.UserName, "张三")
		}
		if msg.Metadata["conversation_id"] != "conv-1" {
			t.Errorf("conversation_id = %q, 期望 %q", msg.Metadata["conversation_id"], "conv-1")
		}
		if msg.Metadata["conversation_type"] != "1" {
			t.Errorf("conversation_type = %q, 期望 %q", msg.Metadata["conversation_type"], "1")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler 未被调用（onChatBotMessage 应异步派发到 handleMessage）")
	}
}

// TestOnChatBotMessage_EmptyContent_Ignored 空内容（仅空白）不应触发 handler，但仍须成功 ack，
// 避免钉钉因未收到 ack 而重投。
func TestOnChatBotMessage_EmptyContent_Ignored(t *testing.T) {
	called := make(chan struct{}, 1)
	a := newTestAdapter()
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		called <- struct{}{}
		return nil, nil
	}

	data := &dtchatbot.BotCallbackDataModel{MsgId: "provider-msg-empty-1", SenderStaffId: "u1"}
	data.Text.Content = "   "

	ack, err := a.onChatBotMessage(context.Background(), data)
	if err != nil {
		t.Fatalf("空内容也应成功 ack: %v", err)
	}
	if ack == nil {
		t.Fatal("应返回非 nil ack")
	}

	select {
	case <-called:
		t.Error("空内容不应调用 handler")
	case <-time.After(200 * time.Millisecond):
		// 正常：未调用
	}
}

// TestOnChatBotMessage_NilData nil 回调数据不应 panic，且成功 ack。
func TestOnChatBotMessage_NilData(t *testing.T) {
	a := newTestAdapter()
	a.handler = func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) { return nil, nil }
	ack, err := a.onChatBotMessage(context.Background(), nil)
	if err != nil {
		t.Fatalf("nil data 不应返回错误: %v", err)
	}
	if ack == nil {
		t.Fatal("应返回非 nil ack")
	}
}

func TestDingtalkStreamSDKUsesHexagonFork(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	src, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("读取 go.mod 失败: %v", err)
	}
	text := string(src)
	const forkReplace = "replace github.com/open-dingtalk/dingtalk-stream-sdk-go => github.com/hexagon-codes/dingtalk-stream-sdk-go"
	if !strings.Contains(text, forkReplace) {
		t.Fatalf("钉钉 Stream SDK 必须指向 Hexagon fork，go.mod 缺少 %q", forkReplace)
	}
	if strings.Contains(text, "=> ./third_party/dingtalk-stream-sdk-go") {
		t.Fatal("钉钉 Stream SDK 不应再指向本地 third_party 副本")
	}
}

// TestStop_DisablesSDKAutoReconnect 关停无泄漏契约（方法3 - goroutine 泄漏）：官方 SDK 默认
// AutoReconnect=true，其 processLoop 在连接断开（含 Close 触发的读失败）后会 `go reconnect()`
// 无限重连。若 Stop 直接 Close 而不先关掉 AutoReconnect，关停后 SDK 仍会疯狂重连 → goroutine
// 泄漏。本测试钉死：Stop 必须先把 AutoReconnect 置 false 再 Close。
func TestStop_DisablesSDKAutoReconnect(t *testing.T) {
	a := newTestAdapter()
	// 直接构造 SDK client（不发起真实网络连接），模拟 Start 之后的状态。
	a.streamClient = dtclient.NewStreamClient(
		dtclient.WithAppCredential(dtclient.NewAppCredentialConfig("k", "s")),
	)
	a.streamClient.RegisterChatBotCallbackRouter(a.onChatBotMessage)
	if !a.streamClient.AutoReconnect {
		t.Fatal("前置条件：SDK 默认 AutoReconnect 应为 true")
	}

	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}

	if a.streamClient.AutoReconnect {
		t.Error("方法3-泄漏: Stop 未关闭 SDK AutoReconnect → Close 后 SDK 会无限重连，goroutine 泄漏")
	}
	if a.connected.Load() {
		t.Error("Stop 后 connected 应为 false")
	}
	if !a.stopped.Load() {
		t.Error("Stop 后 stopped 应为 true")
	}
}

// TestStart_RequiresCredentials 缺凭据时 Start 立即返回错误，不启动连接 goroutine。
func TestStart_RequiresCredentials(t *testing.T) {
	a := New(config.DingtalkConfig{Enabled: true, AppKey: "", AppSecret: "", RobotCode: "r"})
	err := a.Start(context.Background(), func(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		return nil, nil
	})
	if err == nil {
		t.Fatal("AppKey/AppSecret 为空时 Start 应返回错误")
	}
	if a.streamClient != nil {
		t.Error("缺凭据时不应创建 SDK client")
	}
}
