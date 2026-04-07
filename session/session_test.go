package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
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

type getSessionErrorStore struct {
	*mockStore
	err error
}

func (s *getSessionErrorStore) GetSession(context.Context, string) (*storage.Session, error) {
	return nil, s.err
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

func TestGetOrCreate_RejectsCrossUserSessionRestore(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()

	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{
		Platform: adapter.PlatformWeb,
		UserID:   "user-a",
		Content:  "第一条消息",
	})
	if err != nil {
		t.Fatalf("创建首个会话失败: %v", err)
	}

	restored, err := mgr.GetOrCreate(ctx, &adapter.Message{
		Platform:  adapter.PlatformWeb,
		UserID:    "user-b",
		SessionID: sess.ID,
		Content:   "第二条消息",
	})
	if err == nil {
		t.Fatalf("不同用户不应恢复到同一会话: %+v", restored)
	}

	userBSessions, err := store.ListSessions(ctx, "user-b", 10, 0)
	if err != nil {
		t.Fatalf("列出 user-b 会话失败: %v", err)
	}
	if len(userBSessions) != 0 {
		t.Fatalf("越权恢复失败时不应创建 user-b 会话，实际 %d", len(userBSessions))
	}
}

func TestGetOrCreate_PropagatesGetSessionError(t *testing.T) {
	expectedErr := errors.New("storage down")
	store := &getSessionErrorStore{
		mockStore: newMockStore(),
		err:       expectedErr,
	}
	mgr := NewManager(store, config.MemoryConfig{})

	sess, err := mgr.GetOrCreate(context.Background(), &adapter.Message{
		Platform:  adapter.PlatformWeb,
		UserID:    "user-001",
		SessionID: "sess-existing",
		Content:   "你好",
	})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("GetSession 非 not found 错误应上抛, session=%+v err=%v", sess, err)
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

	mgr.SaveUserMessage(ctx, sess.ID, &adapter.Message{Content: "你好"})
	mgr.SaveAssistantMessage(ctx, sess.ID, "你好！有什么可以帮你的？")
	mgr.SaveUserMessage(ctx, sess.ID, &adapter.Message{Content: "帮我翻译 hello"})

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

func TestBuildContextPreservesImageAttachments(t *testing.T) {
	mgr, _ := newTestManager(t)
	ctx := context.Background()

	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{
		Platform: adapter.PlatformWeb,
		UserID:   "user-001",
		Content:  "描述图片",
	})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	err = mgr.SaveUserMessage(ctx, sess.ID, &adapter.Message{
		Content: "描述图片",
		Attachments: []adapter.Attachment{
			{Type: "image", Mime: "image/png", Data: "abc123"},
		},
	})
	if err != nil {
		t.Fatalf("保存用户消息失败: %v", err)
	}

	messages, err := mgr.BuildContext(ctx, sess.ID)
	if err != nil {
		t.Fatalf("构建上下文失败: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("期望 1 条消息，实际为 %d", len(messages))
	}
	if !messages[0].HasMultiContent() {
		t.Fatal("历史中的图片消息应保留 MultiContent")
	}
}

func TestSaveMessagesPersistRequestID(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()

	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{
		Platform: adapter.PlatformWeb,
		UserID:   "user-001",
		Content:  "你好",
	})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	err = mgr.SaveUserMessage(ctx, sess.ID, &adapter.Message{
		Content:  "你好",
		Metadata: map[string]string{"request_id": "req-001"},
	})
	if err != nil {
		t.Fatalf("保存用户消息失败: %v", err)
	}
	_, err = mgr.SaveAssistantMessageWithMetaAndRequestID(ctx, sess.ID, "回答", "", "req-001")
	if err != nil {
		t.Fatalf("保存助手消息失败: %v", err)
	}

	records, err := store.ListMessages(ctx, sess.ID, 10, 0)
	if err != nil {
		t.Fatalf("查询消息失败: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("期望 2 条消息，实际 %d", len(records))
	}
	for _, record := range records {
		if record.RequestID != "req-001" {
			t.Fatalf("%s 消息 request_id 未持久化，实际 %q", record.Role, record.RequestID)
		}
	}
}

func TestGetOrCreate_ReusesSessionByScope(t *testing.T) {
	mgr, _ := newTestManager(t)
	ctx := context.Background()

	msg1 := &adapter.Message{
		Platform:   adapter.PlatformTelegram,
		InstanceID: "telegram-main",
		ChatID:     "chat-001",
		UserID:     "user-001",
		Content:    "第一条消息",
	}
	sess1, err := mgr.GetOrCreate(ctx, msg1)
	if err != nil {
		t.Fatalf("创建首个会话失败: %v", err)
	}

	msg2 := &adapter.Message{
		Platform:   adapter.PlatformTelegram,
		InstanceID: "telegram-main",
		ChatID:     "chat-001",
		UserID:     "user-001",
		Content:    "第二条消息",
	}
	sess2, err := mgr.GetOrCreate(ctx, msg2)
	if err != nil {
		t.Fatalf("按 scope 复用会话失败: %v", err)
	}
	if sess2.ID != sess1.ID {
		t.Fatalf("同一 scope 应复用同一会话，得到 %s 和 %s", sess1.ID, sess2.ID)
	}

	msg3 := &adapter.Message{
		Platform:   adapter.PlatformTelegram,
		InstanceID: "telegram-main",
		ChatID:     "chat-002",
		UserID:     "user-001",
		Content:    "第三条消息",
	}
	sess3, err := mgr.GetOrCreate(ctx, msg3)
	if err != nil {
		t.Fatalf("创建新 scope 会话失败: %v", err)
	}
	if sess3.ID == sess1.ID {
		t.Fatalf("不同 chat_id 不应复用同一会话: %s", sess3.ID)
	}
}

func TestGenerateTitle(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"短标题", "短标题"},
		{"这是一个非常长的消息内容需要被截断因为超过了三十个字符的长度限制", "这是一个非常长的消息内容需要被截断因为超过了三十个字符..."},
	}

	for _, tt := range tests {
		got := generateTitle(tt.input)
		if got != tt.expected {
			t.Errorf("generateTitle(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestGenerateTitleForImageOnlyMessage(t *testing.T) {
	title := generateTitleForMessage(&adapter.Message{
		Attachments: []adapter.Attachment{
			{Type: "image", Mime: "image/png", Data: "abc123"},
		},
	})
	if title != "图片消息" {
		t.Fatalf("图片消息标题不匹配: %q", title)
	}
}
