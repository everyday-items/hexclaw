package sqlite

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hexagon-codes/hexclaw/storage"
)

// TestBug20260622_PreviewUTF8Safe 回归锁定 [BUG-PREVIEW-UTF8]：
// SaveMessage 更新会话 last_message_preview 时用 `preview[:200]` 按字节硬切，
// CJK 内容（3 字节/字）会在 200 字节处切到半个字符 → 预览出现 `�`（非法 UTF-8）。
// 期望：预览必须是合法 UTF-8（回退到 rune 边界），且 ≤200 字节。
func TestBug20260622_PreviewUTF8Safe(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	sess := &storage.Session{ID: "sess-preview", UserID: "u1", Platform: "web", Title: "预览测试"}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// 100 个「中」= 300 字节，远超 200 字节阈值；200 字节边界落在某个 3 字节字符中间。
	content := strings.Repeat("中", 100)
	if err := store.SaveMessage(ctx, &storage.MessageRecord{
		ID: "msg-preview", SessionID: "sess-preview", Role: "user", Content: content, Metadata: "{}",
	}); err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	got, err := store.GetSession(ctx, "sess-preview")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !utf8.ValidString(got.LastMessagePreview) {
		t.Errorf("last_message_preview 含非法 UTF-8（截断切碎了 CJK 字符）: %q", got.LastMessagePreview)
	}
	if len(got.LastMessagePreview) > 200 {
		t.Errorf("预览应 ≤200 字节, got %d", len(got.LastMessagePreview))
	}
}
