package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/everyday-items/hexclaw/internal/adapter"
	"github.com/everyday-items/hexclaw/internal/config"
	"github.com/everyday-items/hexclaw/internal/storage"
	sqlitestore "github.com/everyday-items/hexclaw/internal/storage/sqlite"
)

func newTestManager(t *testing.T) (*Manager, storage.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := config.MemoryConfig{
		Conversation: config.ConversationMemoryConfig{
			MaxTurns:     50,
			SummaryAfter: 20,
		},
	}
	return NewManager(store, cfg), store
}

func TestGetOrCreate_NewSession(t *testing.T) {
	mgr, _ := newTestManager(t)
	ctx := context.Background()

	msg := &adapter.Message{
		Platform: adapter.PlatformWeb,
		UserID:   "user-001",
		Content:  "你好",
	}

	sess, err := mgr.GetOrCreate(ctx, msg)
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	if sess.ID == "" {
		t.Error("会话 ID 不应为空")
	}
	if sess.UserID != "user-001" {
		t.Errorf("期望 UserID=user-001，得到 %s", sess.UserID)
	}
	if sess.Title != "你好" {
		t.Errorf("期望 Title=你好，得到 %s", sess.Title)
	}
}

func TestGetOrCreate_RestoreSession(t *testing.T) {
	mgr, _ := newTestManager(t)
	ctx := context.Background()

	// 创建会话
	msg := &adapter.Message{
		Platform: adapter.PlatformWeb,
		UserID:   "user-001",
		Content:  "第一条消息",
	}
	sess1, _ := mgr.GetOrCreate(ctx, msg)

	// 使用已有 SessionID 恢复
	msg2 := &adapter.Message{
		Platform:  adapter.PlatformWeb,
		UserID:    "user-001",
		SessionID: sess1.ID,
		Content:   "第二条消息",
	}
	sess2, err := mgr.GetOrCreate(ctx, msg2)
	if err != nil {
		t.Fatalf("恢复会话失败: %v", err)
	}

	if sess2.ID != sess1.ID {
		t.Errorf("期望恢复同一会话 %s，得到 %s", sess1.ID, sess2.ID)
	}
}

func TestBuildContext(t *testing.T) {
	mgr, _ := newTestManager(t)
	ctx := context.Background()

	// 创建会话并保存消息
	msg := &adapter.Message{
		Platform: adapter.PlatformWeb,
		UserID:   "user-001",
		Content:  "你好",
	}
	sess, _ := mgr.GetOrCreate(ctx, msg)

	mgr.SaveUserMessage(ctx, sess.ID, "你好")
	mgr.SaveAssistantMessage(ctx, sess.ID, "你好！有什么可以帮你的？")
	mgr.SaveUserMessage(ctx, sess.ID, "帮我翻译 hello")

	// 构建上下文
	messages, err := mgr.BuildContext(ctx, sess.ID)
	if err != nil {
		t.Fatalf("构建上下文失败: %v", err)
	}

	if len(messages) != 3 {
		t.Fatalf("期望 3 条消息，得到 %d", len(messages))
	}

	if messages[0].Content != "你好" {
		t.Errorf("第一条消息内容不匹配: %s", messages[0].Content)
	}
	if messages[2].Content != "帮我翻译 hello" {
		t.Errorf("第三条消息内容不匹配: %s", messages[2].Content)
	}
}

func TestGenerateTitle(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"短标题", "短标题"},
		{"这是一个非常长的消息内容需要被截断因为超过了三十个字符的长度限制", "这是一个非常长的消息内容需要被截断因为超过了三十个字符的长度..."},
	}

	for _, tt := range tests {
		got := generateTitle(tt.input)
		if got != tt.expected {
			t.Errorf("generateTitle(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
