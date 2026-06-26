package session

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/storage"
)

// BUG-20260626：会话中点击编辑已发送消息再发送 → 报 "删除消息失败"。
// 根因：SaveUserMessage 用 "msg-"+ShortID 作存储主键，前端却以 request_id 作消息句柄、
// 编辑/重试时调 DELETE /api/v1/messages/{request_id} → 后端按 id 查不到 "msg-xxx" → 404 → 토스트。
// 不变量：带 request_id 的用户消息，其**存储 ID 必须等于 request_id**（前端唯一持有的句柄）。
func TestSaveUserMessage_IDEqualsRequestID_BUG20260626(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()

	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{Platform: adapter.PlatformWeb, UserID: "u1", Content: "seed"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	const reqID = "req-frontend-abc123"
	if err := mgr.SaveUserMessage(ctx, sess.ID, &adapter.Message{
		Content:  "带图片的消息",
		Metadata: map[string]string{"request_id": reqID},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	msgs, err := store.ListMessages(ctx, sess.ID, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var userMsg *storage.MessageRecord
	for _, m := range msgs {
		if m.Role == "user" && m.Content == "带图片的消息" {
			userMsg = m
		}
	}
	if userMsg == nil {
		t.Fatal("未找到用户消息")
	}
	if userMsg.ID != reqID {
		t.Fatalf("BUG-20260626: 用户消息存储 ID=%q，应等于 request_id=%q（前端按 request_id 删除，不等则 DELETE 404→\"删除消息失败\"）", userMsg.ID, reqID)
	}
	// 反例：无 request_id 时仍须落库成功并有稳定生成 ID（回退路径不回归）
	if err := mgr.SaveUserMessage(ctx, sess.ID, &adapter.Message{Content: "无reqid"}); err != nil {
		t.Fatalf("save no-reqid: %v", err)
	}
	msgs2, _ := store.ListMessages(ctx, sess.ID, 50, 0)
	for _, m := range msgs2 {
		if m.Content == "无reqid" && m.ID == "" {
			t.Fatal("无 request_id 的消息仍须有非空生成 ID")
		}
	}
}
