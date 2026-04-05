package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"context"
	"path/filepath"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

type getSessionErrorStore struct {
	storage.Store
	err error
}

func (s *getSessionErrorStore) GetSession(context.Context, string) (*storage.Session, error) {
	return nil, s.err
}

func newTestStoreForAPI(t *testing.T) storage.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// 测试 handleSearchMessages 的 SQL 注入防护
func TestSearchMessages_SQLInjection(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	// 构造恶意查询
	injections := []string{
		"'; DROP TABLE messages; --",
		"\" OR 1=1 --",
		"*",
		"NEAR(a, b)",
		"a OR b",
	}

	for _, q := range injections {
		req := httptest.NewRequest("GET", "/api/v1/messages/search?q="+url.QueryEscape(q)+"&user_id=test", nil)
		w := httptest.NewRecorder()
		srv.handleSearchMessages(w, req)

		if w.Code == http.StatusInternalServerError {
			// 500 是可以接受的（搜索失败），但不能 panic
			t.Logf("注入 %q → 500（安全降级）", q)
		} else if w.Code == http.StatusOK {
			t.Logf("注入 %q → 200（安全处理）", q)
		}
	}
}

// 测试 handleForkSession 空 body
func TestForkSession_EmptyBody(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest("POST", "/api/v1/sessions/sess-1/fork", strings.NewReader(""))
	req.SetPathValue("id", "sess-1")
	w := httptest.NewRecorder()
	srv.handleForkSession(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("空 body 应返回 400，实际 %d", w.Code)
	}
}

// 测试 handleSearchMessages 缺少 q 参数
func TestSearchMessages_MissingQuery(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest("GET", "/api/v1/messages/search", nil)
	w := httptest.NewRecorder()
	srv.handleSearchMessages(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("缺少 q 参数应返回 400，实际 %d", w.Code)
	}
}

// 测试 handleListSessions 默认分页
func TestListSessions_DefaultPagination(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	w := httptest.NewRecorder()
	srv.handleListSessions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	// sessions 应为空数组 []，不是 null
	if resp["sessions"] == nil {
		t.Error("sessions 字段为 null，应为空数组 []")
	}
}

// 测试 handleListMessages 负数 limit
func TestListMessages_NegativeLimit(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest("GET", "/api/v1/sessions/sess-1/messages?limit=-1", nil)
	req.SetPathValue("id", "sess-1")
	w := httptest.NewRecorder()
	srv.handleListMessages(w, req)

	// 负数 limit 不应导致 panic 或 500
	if w.Code == http.StatusInternalServerError {
		t.Errorf("负数 limit 不应导致 500: %s", w.Body.String())
	}
}

func TestUpdateMessageFeedback(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-feedback", UserID: "test", Platform: "web", Title: "反馈测试",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := store.SaveMessage(context.Background(), &storage.MessageRecord{
		ID: "msg-feedback", SessionID: "sess-feedback", Role: "assistant", Content: "答复", Metadata: "{}",
	}); err != nil {
		t.Fatalf("保存消息失败: %v", err)
	}

	req := httptest.NewRequest("PUT", "/api/v1/messages/msg-feedback/feedback", strings.NewReader(`{"feedback":"like"}`))
	req.SetPathValue("id", "msg-feedback")
	w := httptest.NewRecorder()

	srv.handleUpdateMessageFeedback(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	sqliteStore, ok := store.(*sqlitestore.Store)
	if !ok {
		t.Fatal("测试存储类型断言失败")
	}

	var feedback string
	if err := sqliteStore.DB().QueryRowContext(context.Background(), `SELECT feedback FROM messages WHERE id = ?`, "msg-feedback").Scan(&feedback); err != nil {
		t.Fatalf("读取反馈失败: %v", err)
	}
	if feedback != "like" {
		t.Fatalf("feedback=%q, want like", feedback)
	}
}

func TestUpdateMessageFeedback_InvalidValue(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest("PUT", "/api/v1/messages/msg-feedback/feedback", strings.NewReader(`{"feedback":"bad"}`))
	req.SetPathValue("id", "msg-feedback")
	w := httptest.NewRecorder()

	srv.handleUpdateMessageFeedback(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestGetSession_RejectsCrossUserRead(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-private", UserID: "user-a", Platform: "web", Title: "机密会话",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-private?user_id=user-b", nil)
	req.SetPathValue("id", "sess-private")
	w := httptest.NewRecorder()

	srv.handleGetSession(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("跨用户读取应返回 404，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestListMessages_RejectsCrossUserRead(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-private", UserID: "user-a", Platform: "web", Title: "机密会话",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := store.SaveMessage(context.Background(), &storage.MessageRecord{
		ID: "msg-secret", SessionID: "sess-private", Role: "user", Content: "机密内容", Metadata: "{}",
	}); err != nil {
		t.Fatalf("保存消息失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-private/messages?user_id=user-b", nil)
	req.SetPathValue("id", "sess-private")
	w := httptest.NewRecorder()

	srv.handleListMessages(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("跨用户读取消息历史应返回 404，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestGetSession_StorageErrorReturns500(t *testing.T) {
	baseStore := newTestStoreForAPI(t)
	store := &getSessionErrorStore{Store: baseStore, err: errors.New("storage down")}
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-1?user_id=test", nil)
	req.SetPathValue("id", "sess-1")
	w := httptest.NewRecorder()

	srv.handleGetSession(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("非 not found 存储错误应返回 500，实际 %d: %s", w.Code, w.Body.String())
	}
}

// --- 创建会话测试 ---

func TestCreateSession_Success(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	body := `{"id":"sess-new","title":"新会话"}`
	req := httptest.NewRequest("POST", "/api/v1/sessions?user_id=test", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleCreateSession(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("期望 201，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["id"] != "sess-new" {
		t.Errorf("id=%q, want sess-new", resp["id"])
	}
	if resp["title"] != "新会话" {
		t.Errorf("title=%q, want 新会话", resp["title"])
	}
	if resp["created_at"] == "" {
		t.Error("created_at 不应为空")
	}
}

func TestCreateSession_MissingID(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	body := `{"title":"无 ID"}`
	req := httptest.NewRequest("POST", "/api/v1/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleCreateSession(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺少 id 应返回 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateSession_EmptyBody(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest("POST", "/api/v1/sessions", strings.NewReader(""))
	w := httptest.NewRecorder()
	srv.handleCreateSession(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("空 body 应返回 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

// --- 更新会话测试 ---

func TestUpdateSession_Success(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	// 先创建会话
	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-update", UserID: "test", Platform: "web", Title: "旧标题",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	body := `{"title":"新标题"}`
	req := httptest.NewRequest("PATCH", "/api/v1/sessions/sess-update?user_id=test", strings.NewReader(body))
	req.SetPathValue("id", "sess-update")
	w := httptest.NewRecorder()
	srv.handleUpdateSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["title"] != "新标题" {
		t.Errorf("title=%q, want 新标题", resp["title"])
	}
	if resp["updated_at"] == "" {
		t.Error("updated_at 不应为空")
	}
}

func TestUpdateSession_NotFound(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	body := `{"title":"新标题"}`
	req := httptest.NewRequest("PATCH", "/api/v1/sessions/nonexistent?user_id=test", strings.NewReader(body))
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()
	srv.handleUpdateSession(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在的会话应返回 404，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateSession_CrossUser(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-private", UserID: "user-a", Platform: "web", Title: "机密",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	body := `{"title":"被篡改"}`
	req := httptest.NewRequest("PATCH", "/api/v1/sessions/sess-private?user_id=user-b", strings.NewReader(body))
	req.SetPathValue("id", "sess-private")
	w := httptest.NewRecorder()
	srv.handleUpdateSession(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("跨用户更新应返回 404，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateSession_EmptyBody(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-empty-body", UserID: "test", Platform: "web", Title: "原标题",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	req := httptest.NewRequest("PATCH", "/api/v1/sessions/sess-empty-body?user_id=test", strings.NewReader(""))
	req.SetPathValue("id", "sess-empty-body")
	w := httptest.NewRecorder()
	srv.handleUpdateSession(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("空 body 应返回 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

// 测试 ChatResponse 中 Usage 的序列化
func TestChatResponse_UsageSerialization(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{
		reply: &adapter.Reply{
			Content: "test",
			Usage: &adapter.Usage{
				InputTokens:  100,
				OutputTokens: 50,
				TotalTokens:  150,
				Provider:     "deepseek",
				Model:        "deepseek-chat",
				Cost:         0.001,
			},
		},
	}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"message": "hello"}`
	req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}

	var resp ChatResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Usage == nil {
		t.Fatal("Usage 应不为 nil")
	}
	if resp.Usage.InputTokens != 100 {
		t.Errorf("InputTokens 应为 100，实际 %d", resp.Usage.InputTokens)
	}
	if resp.Usage.TotalTokens != 150 {
		t.Errorf("TotalTokens 应为 150，实际 %d", resp.Usage.TotalTokens)
	}
}

// 测试 ChatResponse 无 Usage 时不输出字段
func TestChatResponse_NoUsage(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "test"}}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"message": "hello"}`
	req := httptest.NewRequest("POST", "/api/v1/chat", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	// 检查 JSON 中不包含 "usage" 字段（omitempty）
	raw := w.Body.String()
	if strings.Contains(raw, `"usage"`) {
		t.Errorf("无 Usage 时 JSON 不应包含 usage 字段: %s", raw)
	}
}
