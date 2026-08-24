package dingtalk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestDingTalkAllOutboundPathsPreferMarkdown(t *testing.T) {
	t.Parallel()

	a := newTestAdapter()
	fake := newFakeConversationOpenAPI()
	a.openAPI = fake
	// 关闭队列，使每条断言精确对应一次同步发送。
	a.queue = nil

	ctx := context.Background()
	if err := a.Send(ctx, "parent", &adapter.Reply{Content: "普通回复\n\n- 第一步\n- 第二步"}); err != nil {
		t.Fatalf("发送普通单聊回复: %v", err)
	}
	if _, err := a.SendWithReceipt(ctx, "parent", &adapter.Reply{Content: "**带回执的回复**"}); err != nil {
		t.Fatalf("发送带回执单聊回复: %v", err)
	}
	_ = a.sendReplyToEvent(ctx, dtEvent{
		ConversationType: "2",
		ConversationId:   "family-group",
	}, &adapter.Reply{Content: "> v0.5 不发送群聊回复"})
	if key := a.sendThinkingFeedback(ctx, "parent"); key == "" {
		t.Fatal("发送单聊处理中占位未返回消息标识")
	}
	if key := a.sendThinkingFeedbackForEvent(ctx, dtEvent{
		ConversationType: "2",
		ConversationId:   "family-group",
	}); key != "" {
		t.Fatalf("v0.5 群聊处理中占位不应返回消息标识: %q", key)
	}
	if err := a.Send(ctx, "parent", &adapter.Reply{
		Content: "## 作品点评",
		Attachments: []adapter.Attachment{{
			Type: "image",
			Name: "creative-work.png",
			Mime: "image/png",
			Data: base64.StdEncoding.EncodeToString([]byte("test-image-bytes")),
		}},
	}); err != nil {
		t.Fatalf("发送带图 Markdown 回复: %v", err)
	}

	messages := make([]dingtalkOutboundMessage, 0, 4)
	for _, call := range fake.SendCalls() {
		messages = append(messages, dingtalkOutboundMessage{MsgKey: call.MsgKey, MsgParam: call.MsgParam})
	}
	fake.mu.Lock()
	if len(fake.groupSends) != 0 {
		t.Errorf("v0.5 群聊触发 SendGroup: %#v", fake.groupSends)
	}
	uploadCount := len(fake.uploads)
	fake.mu.Unlock()

	if len(messages) != 4 {
		t.Fatalf("OTO 出站消息数 = %d，期望 4", len(messages))
	}
	if uploadCount != 1 {
		t.Fatalf("真实媒体上传次数 = %d，期望 1", uploadCount)
	}
	for index, message := range messages {
		assertDingTalkSampleMarkdown(t, index, message)
	}
}

func assertDingTalkSampleMarkdown(t *testing.T, index int, message dingtalkOutboundMessage) {
	t.Helper()
	if message.MsgKey != "sampleMarkdown" {
		t.Fatalf("消息 %d MsgKey = %q，期望 sampleMarkdown", index, message.MsgKey)
	}
	var payload struct {
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal([]byte(message.MsgParam), &payload); err != nil {
		t.Fatalf("消息 %d 解析 Markdown payload: %v", index, err)
	}
	if strings.TrimSpace(payload.Title) == "" || strings.TrimSpace(payload.Text) == "" {
		t.Fatalf("消息 %d 的 title/text 不得为空", index)
	}
	if strings.Contains(payload.Text, "asset://") || strings.Contains(payload.Text, "test-image-bytes") {
		t.Fatalf("消息 %d 暴露内部资产身份或图片字节", index)
	}
}
