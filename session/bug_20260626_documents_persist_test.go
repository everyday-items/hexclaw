package session

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/storage"
)

// BUG-20260626：发送文档（PDF 等）后切到别的会话再回来，文档卡片没了、只剩纯文本。
// 根因：文档卡片 ref（name/mime/size）只存前端本地 metadata.documents、从不发后端持久化
// （旧设计"文档卡片仅展示、不发后端"）→ 重载从后端取消息时无 documents → 退化成纯文本。
// 不变量：随请求 metadata["documents"] 上送的文档卡片，须落库并在重载后还原到 metadata.documents。
func TestSaveUserMessage_DocumentsSurviveReload_BUG20260626(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()

	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{Platform: adapter.PlatformWeb, UserID: "u1", Content: "seed"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := mgr.SaveUserMessage(ctx, sess.ID, &adapter.Message{
		Content: "看这个文档",
		Metadata: map[string]string{
			"request_id": "req-doc-1",
			"documents":  `[{"name":"年报.pdf","mime":"application/pdf","size":12345}]`,
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	msgs, err := store.ListMessages(ctx, sess.ID, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var um *storage.MessageRecord
	for _, m := range msgs {
		if m.Content == "看这个文档" {
			um = m
		}
	}
	if um == nil {
		t.Fatal("未找到用户消息")
	}
	if !strings.Contains(um.Metadata, "年报.pdf") || !strings.Contains(um.Metadata, "application/pdf") {
		t.Fatalf("BUG-20260626: 文档卡片重载后丢失 → 退化为纯文本。reloaded metadata=%q", um.Metadata)
	}
}
