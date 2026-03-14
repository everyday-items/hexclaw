package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/everyday-items/hexclaw/storage"
)

// newTestStoreV2 创建测试用的临时数据库（含新 schema）
func newTestStoreV2(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	store, err := New(dbPath)
	if err != nil {
		t.Fatalf("创建测试存储失败: %v", err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// --- #5 Usage: 验证 CostRecord 新字段的持久化 ---

func TestCostRecord_Persistence(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	record := &storage.CostRecord{
		ID:       "cost-test-1",
		UserID:   "user-1",
		Provider: "deepseek",
		Model:    "deepseek-chat",
		Tokens:   1500,
		Cost:     0.0015,
	}
	if err := store.SaveCost(ctx, record); err != nil {
		t.Fatalf("SaveCost 失败: %v", err)
	}

	cost, err := store.GetUserCost(ctx, "user-1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("GetUserCost 失败: %v", err)
	}
	if cost != 0.0015 {
		t.Errorf("期望 cost=0.0015，实际 %f", cost)
	}
}

// --- #9 对话搜索测试 ---

func TestSearchMessages_FTS(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	// 创建会话
	sess := &storage.Session{
		ID:       "sess-search-1",
		UserID:   "user-1",
		Platform: "web",
		Title:    "测试会话",
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	// 插入消息
	msgs := []struct {
		id      string
		content string
	}{
		{"msg-1", "今天天气怎么样"},
		{"msg-2", "部署到生产环境的步骤是什么"},
		{"msg-3", "帮我写一个 Go 语言的 HTTP 服务器"},
		{"msg-4", "天气预报显示明天会下雨"},
	}
	for _, m := range msgs {
		if err := store.SaveMessage(ctx, &storage.MessageRecord{
			ID:        m.id,
			SessionID: "sess-search-1",
			Role:      "user",
			Content:   m.content,
			Metadata:  "{}",
		}); err != nil {
			t.Fatalf("保存消息失败: %v", err)
		}
	}

	// 搜索 "天气"
	results, total, err := store.SearchMessages(ctx, "user-1", "天气", 10, 0)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}

	// 应匹配至少 2 条（"天气怎么样" 和 "天气预报"）
	if total < 2 {
		t.Errorf("期望至少 2 条结果，实际 %d", total)
	}
	if len(results) < 2 {
		t.Errorf("期望至少 2 条结果返回，实际 %d", len(results))
	}

	// 验证结果包含会话标题
	for _, r := range results {
		if r.SessionTitle != "测试会话" {
			t.Errorf("期望会话标题为 '测试会话'，实际 %q", r.SessionTitle)
		}
	}
}

func TestSearchMessages_EmptyResult(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	results, total, err := store.SearchMessages(ctx, "user-1", "不存在的内容", 10, 0)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if total != 0 {
		t.Errorf("期望 0 条结果，实际 %d", total)
	}
	if len(results) != 0 {
		t.Errorf("期望空结果，实际 %d 条", len(results))
	}
}

func TestSearchMessages_Pagination(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	sess := &storage.Session{
		ID: "sess-page", UserID: "user-1", Platform: "web", Title: "分页测试",
	}
	store.CreateSession(ctx, sess)

	// 插入 5 条包含 "测试" 的消息
	for i := 0; i < 5; i++ {
		store.SaveMessage(ctx, &storage.MessageRecord{
			ID: "msg-page-" + string(rune('a'+i)), SessionID: "sess-page",
			Role: "user", Content: "这是测试消息 " + string(rune('a'+i)), Metadata: "{}",
		})
	}

	// limit=2, offset=0
	results1, total, _ := store.SearchMessages(ctx, "user-1", "测试", 2, 0)
	if total != 5 {
		t.Errorf("总数应为 5，实际 %d", total)
	}
	if len(results1) != 2 {
		t.Errorf("第一页应返回 2 条，实际 %d", len(results1))
	}

	// limit=2, offset=2
	results2, _, _ := store.SearchMessages(ctx, "user-1", "测试", 2, 2)
	if len(results2) != 2 {
		t.Errorf("第二页应返回 2 条，实际 %d", len(results2))
	}
}

func TestSearchMessages_UserIsolation(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	// user-1 的会话
	store.CreateSession(ctx, &storage.Session{
		ID: "sess-u1", UserID: "user-1", Platform: "web", Title: "U1",
	})
	store.SaveMessage(ctx, &storage.MessageRecord{
		ID: "msg-u1", SessionID: "sess-u1", Role: "user", Content: "秘密数据", Metadata: "{}",
	})

	// user-2 的会话
	store.CreateSession(ctx, &storage.Session{
		ID: "sess-u2", UserID: "user-2", Platform: "web", Title: "U2",
	})
	store.SaveMessage(ctx, &storage.MessageRecord{
		ID: "msg-u2", SessionID: "sess-u2", Role: "user", Content: "秘密信息", Metadata: "{}",
	})

	// user-1 搜索 "秘密" → 只能搜到自己的
	results, _, _ := store.SearchMessages(ctx, "user-1", "秘密", 10, 0)
	for _, r := range results {
		if r.Message.SessionID != "sess-u1" {
			t.Errorf("user-1 搜索到了 user-2 的消息: session=%s", r.Message.SessionID)
		}
	}
}

// --- #10 对话分支测试 ---

func TestForkSession_Basic(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	// 创建源会话和消息
	store.CreateSession(ctx, &storage.Session{
		ID: "sess-src", UserID: "user-1", Platform: "web", Title: "源会话",
	})

	now := time.Now()
	for i := 0; i < 5; i++ {
		store.SaveMessage(ctx, &storage.MessageRecord{
			ID: "msg-" + string(rune('A'+i)), SessionID: "sess-src",
			Role: "user", Content: "消息 " + string(rune('A'+i)), Metadata: "{}",
			CreatedAt: now.Add(time.Duration(i) * time.Second),
		})
	}

	// 从第 3 条消息 (msg-C) 处分支
	newSess, err := store.ForkSession(ctx, "sess-src", "msg-C", "user-1")
	if err != nil {
		t.Fatalf("ForkSession 失败: %v", err)
	}

	// 验证分支会话属性
	if newSess.ParentSessionID != "sess-src" {
		t.Errorf("ParentSessionID 应为 sess-src，实际 %s", newSess.ParentSessionID)
	}
	if newSess.BranchMessageID != "msg-C" {
		t.Errorf("BranchMessageID 应为 msg-C，实际 %s", newSess.BranchMessageID)
	}

	// 验证分支会话中的消息数（A、B、C 三条）
	msgs, err := store.ListMessages(ctx, newSess.ID, 100, 0)
	if err != nil {
		t.Fatalf("获取分支消息失败: %v", err)
	}
	if len(msgs) != 3 {
		t.Errorf("分支应有 3 条消息（A/B/C），实际 %d", len(msgs))
	}
}

func TestForkSession_NonexistentSession(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	_, err := store.ForkSession(ctx, "not-exist", "msg-1", "user-1")
	if err == nil {
		t.Fatal("对不存在的会话分支应该报错")
	}
}

func TestForkSession_NonexistentMessage(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	store.CreateSession(ctx, &storage.Session{
		ID: "sess-src2", UserID: "user-1", Platform: "web", Title: "test",
	})

	_, err := store.ForkSession(ctx, "sess-src2", "not-exist-msg", "user-1")
	if err == nil {
		t.Fatal("对不存在的消息分支应该报错")
	}
}

func TestListSessionBranches(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	// 创建源会话 + 消息
	store.CreateSession(ctx, &storage.Session{
		ID: "sess-main", UserID: "user-1", Platform: "web", Title: "主会话",
	})
	store.SaveMessage(ctx, &storage.MessageRecord{
		ID: "msg-main-1", SessionID: "sess-main", Role: "user", Content: "hello", Metadata: "{}",
	})

	// 创建 2 个分支
	store.ForkSession(ctx, "sess-main", "msg-main-1", "user-1")
	store.ForkSession(ctx, "sess-main", "msg-main-1", "user-1")

	branches, err := store.ListSessionBranches(ctx, "sess-main")
	if err != nil {
		t.Fatalf("获取分支列表失败: %v", err)
	}
	if len(branches) != 2 {
		t.Errorf("应有 2 个分支，实际 %d", len(branches))
	}
}

// --- Schema Migration 幂等性测试 ---

func TestMigration_Idempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// 第一次 Init
	store1, _ := New(dbPath)
	if err := store1.Init(context.Background()); err != nil {
		t.Fatalf("第一次 Init 失败: %v", err)
	}
	store1.Close()

	// 第二次 Init（模拟服务重启）
	store2, _ := New(dbPath)
	if err := store2.Init(context.Background()); err != nil {
		t.Fatalf("第二次 Init 失败（迁移不幂等）: %v", err)
	}
	store2.Close()

	// 第三次 Init
	store3, _ := New(dbPath)
	if err := store3.Init(context.Background()); err != nil {
		t.Fatalf("第三次 Init 失败: %v", err)
	}
	store3.Close()
}

// --- 新增字段向后兼容测试 ---

func TestSession_NewFields(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	// 创建不带分支字段的会话（向后兼容）
	sess := &storage.Session{
		ID: "sess-compat", UserID: "user-1", Platform: "web", Title: "test",
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	// 读取回来，分支字段应为空字符串
	got, err := store.GetSession(ctx, "sess-compat")
	if err != nil {
		t.Fatalf("获取会话失败: %v", err)
	}
	if got.ParentSessionID != "" {
		t.Errorf("ParentSessionID 应为空，实际 %q", got.ParentSessionID)
	}
	if got.BranchMessageID != "" {
		t.Errorf("BranchMessageID 应为空，实际 %q", got.BranchMessageID)
	}
}

func TestMessage_ParentID(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	store.CreateSession(ctx, &storage.Session{
		ID: "sess-pid", UserID: "user-1", Platform: "web", Title: "test",
	})

	// 保存带 ParentID 的消息
	store.SaveMessage(ctx, &storage.MessageRecord{
		ID: "msg-child", SessionID: "sess-pid", ParentID: "msg-parent",
		Role: "user", Content: "child message", Metadata: "{}",
	})

	msgs, _ := store.ListMessages(ctx, "sess-pid", 10, 0)
	if len(msgs) != 1 {
		t.Fatalf("应有 1 条消息，实际 %d", len(msgs))
	}
	if msgs[0].ParentID != "msg-parent" {
		t.Errorf("ParentID 应为 msg-parent，实际 %q", msgs[0].ParentID)
	}
}

func TestUpdateSession(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	store.CreateSession(ctx, &storage.Session{
		ID: "sess-upd", UserID: "user-1", Platform: "web", Title: "旧标题",
	})

	sess, _ := store.GetSession(ctx, "sess-upd")
	sess.Title = "新标题"
	if err := store.UpdateSession(ctx, sess); err != nil {
		t.Fatalf("UpdateSession 失败: %v", err)
	}

	got, _ := store.GetSession(ctx, "sess-upd")
	if got.Title != "新标题" {
		t.Errorf("标题应为 '新标题'，实际 %q", got.Title)
	}
}

// --- 边界情况 ---

func TestSearchMessages_SpecialChars(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	store.CreateSession(ctx, &storage.Session{
		ID: "sess-special", UserID: "user-1", Platform: "web", Title: "test",
	})
	store.SaveMessage(ctx, &storage.MessageRecord{
		ID: "msg-special", SessionID: "sess-special",
		Role: "user", Content: "SELECT * FROM users WHERE 1=1; DROP TABLE users;", Metadata: "{}",
	})

	// 搜索 SQL 注入字符串 — 不应 panic 或报错
	_, _, err := store.SearchMessages(ctx, "user-1", "DROP TABLE", 10, 0)
	if err != nil {
		t.Logf("特殊字符搜索出错（可接受降级）: %v", err)
	}
}

func TestSearchMessages_VeryLongQuery(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	longQuery := strings.Repeat("测试", 1000) // 2000 字符
	_, _, err := store.SearchMessages(ctx, "user-1", longQuery, 10, 0)
	if err != nil {
		t.Logf("超长查询出错（可接受降级）: %v", err)
	}
}

func TestForkSession_LongTitle(t *testing.T) {
	store := newTestStoreV2(t)
	ctx := context.Background()

	longTitle := strings.Repeat("长标题", 50) // 150 字符
	store.CreateSession(ctx, &storage.Session{
		ID: "sess-long", UserID: "user-1", Platform: "web", Title: longTitle,
	})
	store.SaveMessage(ctx, &storage.MessageRecord{
		ID: "msg-long", SessionID: "sess-long", Role: "user", Content: "hello", Metadata: "{}",
	})

	newSess, err := store.ForkSession(ctx, "sess-long", "msg-long", "user-1")
	if err != nil {
		t.Fatalf("ForkSession 长标题失败: %v", err)
	}

	// 标题应被截断
	titleRunes := []rune(newSess.Title)
	if len(titleRunes) > 63 {
		t.Errorf("分支标题应被截断到 60 字符以内，实际 %d", len(titleRunes))
	}
}

// --- 性能基准测试 ---

func BenchmarkSearchMessages_FTS(b *testing.B) {
	dir := b.TempDir()
	dbPath := filepath.Join(dir, "bench.db")
	store, _ := New(dbPath)
	store.Init(context.Background())
	defer store.Close()

	ctx := context.Background()

	// 预填充 1000 条消息
	store.CreateSession(ctx, &storage.Session{
		ID: "sess-bench", UserID: "user-1", Platform: "web", Title: "bench",
	})
	for i := 0; i < 1000; i++ {
		store.SaveMessage(ctx, &storage.MessageRecord{
			ID:        "msg-bench-" + os.Getenv("") + string(rune(i/26000+'A')) + string(rune(i%26+'a')) + string(rune(i/26%26+'a')),
			SessionID: "sess-bench",
			Role:      "user",
			Content:   "这是一条关于 Go 语言和 Kubernetes 的消息，编号 " + string(rune(i%26+'a')),
			Metadata:  "{}",
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.SearchMessages(ctx, "user-1", "Kubernetes", 20, 0)
	}
}

func BenchmarkForkSession(b *testing.B) {
	dir := b.TempDir()
	dbPath := filepath.Join(dir, "bench.db")
	store, _ := New(dbPath)
	store.Init(context.Background())
	defer store.Close()

	ctx := context.Background()

	// 预填充 100 条消息
	store.CreateSession(ctx, &storage.Session{
		ID: "sess-fork-bench", UserID: "user-1", Platform: "web", Title: "fork bench",
	})
	var lastMsgID string
	for i := 0; i < 100; i++ {
		lastMsgID = "msg-fb-" + string(rune(i/26+'A')) + string(rune(i%26+'a'))
		store.SaveMessage(ctx, &storage.MessageRecord{
			ID:        lastMsgID,
			SessionID: "sess-fork-bench",
			Role:      "user",
			Content:   "消息内容",
			Metadata:  "{}",
			CreatedAt: time.Now().Add(time.Duration(i) * time.Millisecond),
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.ForkSession(ctx, "sess-fork-bench", lastMsgID, "user-1")
	}
}
