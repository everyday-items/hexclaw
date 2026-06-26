package session

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/storage"
)

// BUG-20260626：发送图片后重启应用/重载会话，图片不显示了，只剩文字。
// 根因：SaveUserMessage 把图片 base64 编进 metadata，存储层 truncateLarge(metadata, 64KB)
// 静默截断 → 重载时 JSON 损坏/data 残缺 → 前端 metadata.attachments[].data 无法渲染。
// 不变量：图片附件 base64 经"落库→重载"必须完整还原（不受 64KB metadata 上限截断）。
func TestSaveUserMessage_LargeImageSurvivesReload_BUG20260626(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()

	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{Platform: adapter.PlatformWeb, UserID: "u1", Content: "seed"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// 100KB base64 图片，超过 64KB metadata 截断阈值
	bigData := strings.Repeat("A", 100*1024)
	if err := mgr.SaveUserMessage(ctx, sess.ID, &adapter.Message{
		Content:     "看这张图",
		Attachments: []adapter.Attachment{{Type: "image", Name: "p.png", Mime: "image/png", Data: bigData}},
		Metadata:    map[string]string{"request_id": "req-img-1"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	msgs, err := store.ListMessages(ctx, sess.ID, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var um *storage.MessageRecord
	for _, m := range msgs {
		if m.Content == "看这张图" {
			um = m
		}
	}
	if um == nil {
		t.Fatal("未找到用户消息")
	}
	// 重载后前端从 metadata.attachments[].data 渲染图片，data 必须完整（不被截断）
	if !strings.Contains(um.Metadata, bigData) {
		t.Fatalf("BUG-20260626: 图片 base64 重载后丢失/截断 —— metadata 长度=%d，不含完整图片(%d 字节)", len(um.Metadata), len(bigData))
	}
}
