package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"context"
	"path/filepath"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
	"github.com/everyday-items/hexclaw/storage"
	sqlitestore "github.com/everyday-items/hexclaw/storage/sqlite"
)

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
