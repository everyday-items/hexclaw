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
	"time"

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

func TestForkSession_ExclusivePrefixRequest(t *testing.T) {
	store := newTestStoreForAPI(t)
	ctx := context.Background()
	if err := store.CreateSession(ctx, &storage.Session{
		ID: "sess-edit-source", UserID: "editor", Platform: "web", Title: "编辑源会话",
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"before", "edited", "tail"} {
		if err := store.SaveMessage(ctx, &storage.MessageRecord{
			ID: id, SessionID: "sess-edit-source", Role: "user", Content: id, Metadata: "{}",
		}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, store)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-edit-source/fork",
		strings.NewReader(`{"message_id":"edited","user_id":"editor","include_message":false}`))
	req.SetPathValue("id", "sess-edit-source")
	w := httptest.NewRecorder()
	srv.handleForkSession(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("fork status = %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Session storage.Session `json:"session"`
	}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	messages, err := store.ListMessages(ctx, response.Session.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Content != "before" {
		t.Fatalf("exclusive API fork copied %#v, want only before", messages)
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

func TestSearchMessages_Success_PaginationAndUserIsolation(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	for _, session := range []*storage.Session{
		{ID: "sess-a-1", UserID: "user-a", Platform: "web", Title: "会话 A1", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{ID: "sess-a-2", UserID: "user-a", Platform: "web", Title: "会话 A2", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{ID: "sess-b-1", UserID: "user-b", Platform: "web", Title: "会话 B1", CreatedAt: time.Now(), UpdatedAt: time.Now()},
	} {
		if err := store.CreateSession(context.Background(), session); err != nil {
			t.Fatalf("创建会话失败: %v", err)
		}
	}

	for _, msg := range []*storage.MessageRecord{
		{ID: "msg-a-1", SessionID: "sess-a-1", Role: "user", Content: "Vue 测试一", Metadata: "{}", CreatedAt: time.Now()},
		{ID: "msg-a-2", SessionID: "sess-a-2", Role: "assistant", Content: "Vue 测试二", Metadata: "{}", CreatedAt: time.Now()},
		{ID: "msg-b-1", SessionID: "sess-b-1", Role: "user", Content: "Vue 测试三", Metadata: "{}", CreatedAt: time.Now()},
	} {
		if err := store.SaveMessage(context.Background(), msg); err != nil {
			t.Fatalf("保存消息失败: %v", err)
		}
	}

	req := httptest.NewRequest("GET", "/api/v1/messages/search?q=Vue%20测试&user_id=user-a&limit=1&offset=1", nil)
	w := httptest.NewRecorder()
	srv.handleSearchMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Results []storage.SearchResult `json:"results"`
		Total   int                    `json:"total"`
		Query   string                 `json:"query"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Total != 2 {
		t.Fatalf("total=%d, want 2", resp.Total)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("len(results)=%d, want 1", len(resp.Results))
	}
	if resp.Query != "Vue 测试" {
		t.Fatalf("query=%q, want %q", resp.Query, "Vue 测试")
	}
	if resp.Results[0].Message == nil {
		t.Fatal("result message should not be nil")
	}
	if resp.Results[0].Message.SessionID == "sess-b-1" {
		t.Fatalf("cross-user result leaked into response: %+v", resp.Results[0])
	}
}

func TestSearchMessages_DefaultUserIDAndDefaultLimit(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-api-user", UserID: "api-user", Platform: "web", Title: "默认用户会话", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := store.SaveMessage(context.Background(), &storage.MessageRecord{
		ID: "msg-api-user", SessionID: "sess-api-user", Role: "user", Content: "默认用户查询命中", Metadata: "{}", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("保存消息失败: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/v1/messages/search?q=默认用户", nil)
	w := httptest.NewRecorder()
	srv.handleSearchMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Results []storage.SearchResult `json:"results"`
		Total   int                    `json:"total"`
		Query   string                 `json:"query"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Total != 1 || len(resp.Results) != 1 {
		t.Fatalf("expected one default-user result, got total=%d len=%d", resp.Total, len(resp.Results))
	}
	if resp.Results[0].Message == nil || resp.Results[0].Message.SessionID != "sess-api-user" {
		t.Fatalf("unexpected default-user result: %+v", resp.Results[0])
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

	req := httptest.NewRequest("PUT", "/api/v1/messages/msg-feedback/feedback?user_id=test", strings.NewReader(`{"feedback":"like"}`))
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

func TestSuggestSessionTitle_UpdatesWhenExpectedTitleMatches(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{
		reply: &adapter.Reply{Content: "ok"},
		title: "杭州周末露营计划",
	}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-suggest", UserID: "test", Platform: "web", Title: "帮我规划这个周末去杭州露营需要带什么",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	for _, msg := range []*storage.MessageRecord{
		{ID: "msg-1", SessionID: "sess-suggest", Role: "user", Content: "帮我规划这个周末去杭州露营需要带什么", Metadata: "{}", CreatedAt: time.Now()},
		{ID: "msg-2", SessionID: "sess-suggest", Role: "assistant", Content: "可以从帐篷、睡袋、炊具、照明和衣物开始准备", Metadata: "{}", CreatedAt: time.Now()},
	} {
		if err := store.SaveMessage(context.Background(), msg); err != nil {
			t.Fatalf("保存消息失败: %v", err)
		}
	}

	body := `{"expected_title":"帮我规划这个周末去杭州露营需要带什么"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-suggest/suggest-title?user_id=test", strings.NewReader(body))
	req.SetPathValue("id", "sess-suggest")
	w := httptest.NewRecorder()
	srv.handleSuggestSessionTitle(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if updated, _ := resp["updated"].(bool); !updated {
		t.Fatalf("expected updated=true, got %+v", resp)
	}
	if title, _ := resp["title"].(string); title != "杭州周末露营计划" {
		t.Fatalf("title=%q", title)
	}

	updatedSession, err := store.GetSession(context.Background(), "sess-suggest")
	if err != nil {
		t.Fatalf("读取会话失败: %v", err)
	}
	if updatedSession.Title != "杭州周末露营计划" {
		t.Fatalf("session title not persisted: %q", updatedSession.Title)
	}
}

func TestSuggestSessionTitle_DoesNotOverrideManualRename(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{
		reply: &adapter.Reply{Content: "ok"},
		title: "自动摘要标题",
	}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-manual", UserID: "test", Platform: "web", Title: "我手动改过的标题",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := store.SaveMessage(context.Background(), &storage.MessageRecord{
		ID: "msg-manual-1", SessionID: "sess-manual", Role: "user", Content: "帮我整理一份杭州露营清单", Metadata: "{}", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("保存消息失败: %v", err)
	}

	body := `{"expected_title":"帮我整理一份杭州露营清单"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-manual/suggest-title?user_id=test", strings.NewReader(body))
	req.SetPathValue("id", "sess-manual")
	w := httptest.NewRecorder()
	srv.handleSuggestSessionTitle(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if updated, _ := resp["updated"].(bool); updated {
		t.Fatalf("manual title should not be overridden: %+v", resp)
	}
	if title, _ := resp["title"].(string); title != "我手动改过的标题" {
		t.Fatalf("title=%q", title)
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

// --- 删除会话测试 ---

func TestDeleteSession_Success(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-del", UserID: "test", Platform: "web", Title: "待删除会话",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/sess-del?user_id=test", nil)
	req.SetPathValue("id", "sess-del")
	w := httptest.NewRecorder()
	srv.handleDeleteSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["message"] != "会话已删除" {
		t.Errorf("message=%q, want 会话已删除", resp["message"])
	}

	// 软删除后会话不可读：GetSession 返回 ErrNotFound，防止已删除会话被读取或 fork 复活。
	if _, err := store.GetSession(context.Background(), "sess-del"); err != storage.ErrNotFound {
		t.Fatalf("软删除后 GetSession 应返回 ErrNotFound，实际 err=%v", err)
	}

	// 确认不再出现在列表中（ListSessions 过滤 status >= 0）
	sessions, err := store.ListSessions(context.Background(), "test", 20, 0)
	if err != nil {
		t.Fatalf("ListSessions err=%v", err)
	}
	for _, s := range sessions {
		if s.ID == "sess-del" {
			t.Fatal("软删除的会话不应出现在 ListSessions 结果中")
		}
	}
}

func TestDeleteSession_NotFound(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/nonexistent?user_id=test", nil)
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()
	srv.handleDeleteSession(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("删除不存在的会话应返回 404，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteSession_CrossUser(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-other", UserID: "user-a", Platform: "web", Title: "别人的会话",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/sess-other?user_id=user-b", nil)
	req.SetPathValue("id", "sess-other")
	w := httptest.NewRecorder()
	srv.handleDeleteSession(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("跨用户删除应返回 404，实际 %d: %s", w.Code, w.Body.String())
	}
}

// --- 删除消息测试 ---

func TestDeleteMessage_Success(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-msg-del", UserID: "test", Platform: "web", Title: "消息测试",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := store.SaveMessage(context.Background(), &storage.MessageRecord{
		ID: "msg-del-1", SessionID: "sess-msg-del", Role: "user", Content: "待删除消息", Metadata: "{}",
	}); err != nil {
		t.Fatalf("保存消息失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/messages/msg-del-1?user_id=test", nil)
	req.SetPathValue("id", "msg-del-1")
	w := httptest.NewRecorder()
	srv.handleDeleteMessage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["message"] != "消息已删除" {
		t.Errorf("message=%q, want 消息已删除", resp["message"])
	}

	// 确认消息已被删除
	_, err := store.GetMessage(context.Background(), "msg-del-1")
	if err == nil {
		t.Fatal("消息删除后 GetMessage 应返回错误")
	}
}

func TestDeleteMessage_NotFound(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/messages/nonexistent?user_id=test", nil)
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()
	srv.handleDeleteMessage(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("删除不存在的消息应返回 404，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteMessage_EmptyID(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/messages/?user_id=test", nil)
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()
	srv.handleDeleteMessage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("空消息 ID 应返回 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteMessage_CrossUser(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-msg-cross", UserID: "user-a", Platform: "web", Title: "他人会话",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := store.SaveMessage(context.Background(), &storage.MessageRecord{
		ID: "msg-cross-1", SessionID: "sess-msg-cross", Role: "user", Content: "机密消息", Metadata: "{}",
	}); err != nil {
		t.Fatalf("保存消息失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/messages/msg-cross-1?user_id=user-b", nil)
	req.SetPathValue("id", "msg-cross-1")
	w := httptest.NewRecorder()
	srv.handleDeleteMessage(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("跨用户删除消息应返回 403，实际 %d: %s", w.Code, w.Body.String())
	}
}

// --- 分支列表测试 ---

func TestListBranches_Empty(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-no-branch", UserID: "test", Platform: "web", Title: "无分支会话",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-no-branch/branches?user_id=test", nil)
	req.SetPathValue("id", "sess-no-branch")
	w := httptest.NewRecorder()
	srv.handleListBranches(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Branches []any `json:"branches"`
		Total    int   `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Total != 0 {
		t.Errorf("total=%d, want 0", resp.Total)
	}
	if resp.Branches == nil {
		t.Error("branches 字段不应为 null")
	}
	if len(resp.Branches) != 0 {
		t.Errorf("branches 应为空数组，实际长度 %d", len(resp.Branches))
	}
}

func TestListBranches_WithBranches(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	// 创建主会话
	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-main-br", UserID: "test", Platform: "web", Title: "主会话",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	// 创建消息用于 fork
	if err := store.SaveMessage(context.Background(), &storage.MessageRecord{
		ID: "msg-br-1", SessionID: "sess-main-br", Role: "user", Content: "分支前的消息", Metadata: "{}",
	}); err != nil {
		t.Fatalf("保存消息失败: %v", err)
	}

	// 通过 ForkSession 创建分支
	_, err := store.ForkSession(context.Background(), "sess-main-br", "msg-br-1", "test")
	if err != nil {
		t.Fatalf("创建分支失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/sess-main-br/branches?user_id=test", nil)
	req.SetPathValue("id", "sess-main-br")
	w := httptest.NewRecorder()
	srv.handleListBranches(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Branches []map[string]any `json:"branches"`
		Total    int              `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("total=%d, want 1", resp.Total)
	}
	if len(resp.Branches) != 1 {
		t.Fatalf("branches 长度=%d, want 1", len(resp.Branches))
	}
}

func TestListBranches_NotFound(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/nonexistent/branches?user_id=test", nil)
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()
	srv.handleListBranches(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在的会话应返回 404，实际 %d: %s", w.Code, w.Body.String())
	}
}

// --- 消息反馈测试 (补充) ---

func TestMessageFeedback_Success_Like(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-fb", UserID: "test", Platform: "web", Title: "反馈测试",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := store.SaveMessage(context.Background(), &storage.MessageRecord{
		ID: "msg-fb-like", SessionID: "sess-fb", Role: "assistant", Content: "答复", Metadata: "{}",
	}); err != nil {
		t.Fatalf("保存消息失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/messages/msg-fb-like/feedback?user_id=test",
		strings.NewReader(`{"feedback":"like"}`))
	req.SetPathValue("id", "msg-fb-like")
	w := httptest.NewRecorder()
	srv.handleUpdateMessageFeedback(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["message"] != "反馈已更新" {
		t.Errorf("message=%q, want 反馈已更新", resp["message"])
	}
}

func TestMessageFeedback_Success_Dislike(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-fb2", UserID: "test", Platform: "web", Title: "反馈测试2",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := store.SaveMessage(context.Background(), &storage.MessageRecord{
		ID: "msg-fb-dislike", SessionID: "sess-fb2", Role: "assistant", Content: "答复", Metadata: "{}",
	}); err != nil {
		t.Fatalf("保存消息失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/messages/msg-fb-dislike/feedback?user_id=test",
		strings.NewReader(`{"feedback":"dislike"}`))
	req.SetPathValue("id", "msg-fb-dislike")
	w := httptest.NewRecorder()
	srv.handleUpdateMessageFeedback(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestMessageFeedback_Success_ClearFeedback(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-fb3", UserID: "test", Platform: "web", Title: "反馈测试3",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if err := store.SaveMessage(context.Background(), &storage.MessageRecord{
		ID: "msg-fb-clear", SessionID: "sess-fb3", Role: "assistant", Content: "答复", Metadata: "{}",
	}); err != nil {
		t.Fatalf("保存消息失败: %v", err)
	}

	// 空字符串 feedback 用于清除反馈
	req := httptest.NewRequest(http.MethodPut, "/api/v1/messages/msg-fb-clear/feedback?user_id=test",
		strings.NewReader(`{"feedback":""}`))
	req.SetPathValue("id", "msg-fb-clear")
	w := httptest.NewRecorder()
	srv.handleUpdateMessageFeedback(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("清除反馈应返回 200，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestMessageFeedback_InvalidValue_Rejected(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	invalidValues := []string{"love", "hate", "thumbsup", "1", "true"}
	for _, val := range invalidValues {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/messages/msg-any/feedback",
			strings.NewReader(`{"feedback":"`+val+`"}`))
		req.SetPathValue("id", "msg-any")
		w := httptest.NewRecorder()
		srv.handleUpdateMessageFeedback(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("feedback=%q 应返回 400，实际 %d", val, w.Code)
		}
	}
}

func TestMessageFeedback_EmptyMessageID(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/messages//feedback",
		strings.NewReader(`{"feedback":"like"}`))
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()
	srv.handleUpdateMessageFeedback(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("空消息 ID 应返回 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestMessageFeedback_MessageNotFound(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/messages/nonexistent/feedback?user_id=test",
		strings.NewReader(`{"feedback":"like"}`))
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()
	srv.handleUpdateMessageFeedback(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在的消息应返回 404，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestMessageFeedback_InvalidJSON(t *testing.T) {
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/messages/msg-any/feedback",
		strings.NewReader(`{invalid json}`))
	req.SetPathValue("id", "msg-any")
	w := httptest.NewRecorder()
	srv.handleUpdateMessageFeedback(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("无效 JSON 应返回 400，实际 %d: %s", w.Code, w.Body.String())
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
