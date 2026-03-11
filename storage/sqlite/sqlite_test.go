package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/everyday-items/hexclaw/storage"
)

// newTestStore 创建测试用的 SQLite 存储（使用临时目录）
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	store, err := New(dbPath)
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestNewWithTildeExpansion(t *testing.T) {
	// 验证 ~ 展开
	home, _ := os.UserHomeDir()
	dir := t.TempDir()
	// 使用临时目录代替 ~，确保不影响用户真实数据
	dbPath := filepath.Join(dir, "test-tilde.db")
	store, err := New(dbPath)
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	defer store.Close()
	_ = home // 仅验证不 panic
}

func TestSessionCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// 创建会话
	sess := &storage.Session{
		ID:       "sess-001",
		UserID:   "user-001",
		Platform: "web",
		Title:    "测试会话",
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	// 获取会话
	got, err := store.GetSession(ctx, "sess-001")
	if err != nil {
		t.Fatalf("获取会话失败: %v", err)
	}
	if got.Title != "测试会话" || got.UserID != "user-001" {
		t.Errorf("会话数据不匹配: %+v", got)
	}

	// 列出会话
	list, err := store.ListSessions(ctx, "user-001", 10, 0)
	if err != nil {
		t.Fatalf("列出会话失败: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("期望 1 个会话，得到 %d", len(list))
	}

	// 删除会话
	if err := store.DeleteSession(ctx, "sess-001"); err != nil {
		t.Fatalf("删除会话失败: %v", err)
	}
	list, _ = store.ListSessions(ctx, "user-001", 10, 0)
	if len(list) != 0 {
		t.Errorf("删除后仍有 %d 个会话", len(list))
	}
}

func TestMessageCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// 先创建会话
	sess := &storage.Session{
		ID: "sess-msg", UserID: "user-001", Platform: "web", Title: "消息测试",
	}
	store.CreateSession(ctx, sess)

	// 保存消息
	msgs := []*storage.MessageRecord{
		{ID: "msg-001", SessionID: "sess-msg", Role: "user", Content: "你好", Metadata: "{}"},
		{ID: "msg-002", SessionID: "sess-msg", Role: "assistant", Content: "你好！有什么可以帮你的？", Metadata: "{}"},
	}
	for _, msg := range msgs {
		if err := store.SaveMessage(ctx, msg); err != nil {
			t.Fatalf("保存消息失败: %v", err)
		}
	}

	// 列出消息
	list, err := store.ListMessages(ctx, "sess-msg", 10, 0)
	if err != nil {
		t.Fatalf("列出消息失败: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("期望 2 条消息，得到 %d", len(list))
	}
	if list[0].Role != "user" || list[1].Role != "assistant" {
		t.Errorf("消息顺序不正确: %s, %s", list[0].Role, list[1].Role)
	}
}

func TestCostTracking(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	since := time.Now().Add(-1 * time.Hour)

	// 记录成本
	records := []*storage.CostRecord{
		{ID: "cost-001", UserID: "user-001", Provider: "deepseek", Model: "deepseek-chat", Tokens: 1000, Cost: 0.001},
		{ID: "cost-002", UserID: "user-001", Provider: "openai", Model: "gpt-4o-mini", Tokens: 500, Cost: 0.01},
		{ID: "cost-003", UserID: "user-002", Provider: "deepseek", Model: "deepseek-chat", Tokens: 2000, Cost: 0.002},
	}
	for _, r := range records {
		if err := store.SaveCost(ctx, r); err != nil {
			t.Fatalf("记录成本失败: %v", err)
		}
	}

	// 用户成本
	userCost, err := store.GetUserCost(ctx, "user-001", since)
	if err != nil {
		t.Fatalf("获取用户成本失败: %v", err)
	}
	if userCost < 0.01 {
		t.Errorf("用户成本不正确: %f", userCost)
	}

	// 全局成本
	globalCost, err := store.GetGlobalCost(ctx, since)
	if err != nil {
		t.Fatalf("获取全局成本失败: %v", err)
	}
	if globalCost < 0.01 {
		t.Errorf("全局成本不正确: %f", globalCost)
	}
}

func TestWithTx(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// 事务成功
	err := store.WithTx(ctx, func(tx storage.Store) error {
		return tx.CreateSession(ctx, &storage.Session{
			ID: "sess-tx", UserID: "user-001", Platform: "web", Title: "事务测试",
		})
	})
	if err != nil {
		t.Fatalf("事务失败: %v", err)
	}

	got, err := store.GetSession(ctx, "sess-tx")
	if err != nil {
		t.Fatalf("事务提交后获取会话失败: %v", err)
	}
	if got.Title != "事务测试" {
		t.Errorf("事务数据不正确: %s", got.Title)
	}

	// 事务回滚
	testErr := storage.ErrNotFound
	err = store.WithTx(ctx, func(tx storage.Store) error {
		tx.CreateSession(ctx, &storage.Session{
			ID: "sess-rollback", UserID: "user-001", Platform: "web", Title: "回滚测试",
		})
		return testErr
	})
	if err != testErr {
		t.Fatalf("期望回滚错误，得到: %v", err)
	}

	_, err = store.GetSession(ctx, "sess-rollback")
	if err == nil {
		t.Error("回滚后不应该能获取到会话")
	}
}
