package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/storage"
)

// 辅助：创建 server + 预置会话，返回 srv + sessionID。
func newAppendTestServer(t *testing.T, ownerUserID string) (*Server, string) {
	t.Helper()
	store := newTestStoreForAPI(t)
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, store)
	sid := "sess-append"
	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: sid, UserID: ownerUserID, Platform: "web", Title: "gen",
	}); err != nil {
		t.Fatal(err)
	}
	return srv, sid
}

func TestAppendMessage_Success(t *testing.T) {
	srv, sid := newAppendTestServer(t, "test")
	body := `{"id":"msg-1","role":"user","content":"hello","metadata":{"mode":"image_gen","model":"cogview-4"}}`
	req := httptest.NewRequest("POST", "/api/v1/sessions/"+sid+"/messages?user_id=test", strings.NewReader(body))
	req.SetPathValue("id", sid)
	w := httptest.NewRecorder()
	srv.handleAppendMessage(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["id"] != "msg-1" {
		t.Errorf("id=%q want msg-1", resp["id"])
	}
	// 验证 DB 真的写入了
	msg, err := srv.store.GetMessage(context.Background(), "msg-1")
	if err != nil {
		t.Fatalf("message not in DB: %v", err)
	}
	if msg.Content != "hello" {
		t.Errorf("content=%q want hello", msg.Content)
	}
	if !strings.Contains(msg.Metadata, "image_gen") {
		t.Errorf("metadata should contain image_gen, got %q", msg.Metadata)
	}
}

func TestBUG20260726001_AppendMessagePreservesLargeAttachmentMetadata(t *testing.T) {
	srv, sid := newAppendTestServer(t, "test")
	imageData := "data:image/png;base64," + strings.Repeat("A", 70*1024)
	body, err := json.Marshal(map[string]any{
		"id":      "msg-large-attachment",
		"role":    "user",
		"content": "",
		"metadata": map[string]any{
			"attachments": []map[string]string{{
				"type": "image",
				"name": "k12-test.png",
				"mime": "image/png",
				"data": imageData,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v1/sessions/"+sid+"/messages?user_id=test", strings.NewReader(string(body)))
	req.SetPathValue("id", sid)
	w := httptest.NewRecorder()
	srv.handleAppendMessage(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	msg, err := srv.store.GetMessage(context.Background(), "msg-large-attachment")
	if err != nil {
		t.Fatalf("message not in DB: %v", err)
	}
	var metadata struct {
		Attachments []struct {
			Data string `json:"data"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal([]byte(msg.Metadata), &metadata); err != nil {
		t.Fatalf("persisted attachment metadata must remain valid JSON: %v", err)
	}
	if len(metadata.Attachments) != 1 {
		t.Fatalf("attachments=%d want 1", len(metadata.Attachments))
	}
	if metadata.Attachments[0].Data != imageData {
		t.Fatalf("attachment data length=%d want %d", len(metadata.Attachments[0].Data), len(imageData))
	}
}

func TestAppendMessage_GeneratesIDIfMissing(t *testing.T) {
	srv, sid := newAppendTestServer(t, "test")
	body := `{"role":"user","content":"auto-id"}`
	req := httptest.NewRequest("POST", "/api/v1/sessions/"+sid+"/messages?user_id=test", strings.NewReader(body))
	req.SetPathValue("id", sid)
	w := httptest.NewRecorder()
	srv.handleAppendMessage(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d", w.Code)
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if !strings.HasPrefix(resp["id"], "msg-") {
		t.Errorf("generated id should start with msg-, got %q", resp["id"])
	}
}

func TestAppendMessage_InvalidRole(t *testing.T) {
	srv, sid := newAppendTestServer(t, "test")
	body := `{"role":"god","content":"x"}`
	req := httptest.NewRequest("POST", "/api/v1/sessions/"+sid+"/messages?user_id=test", strings.NewReader(body))
	req.SetPathValue("id", sid)
	w := httptest.NewRecorder()
	srv.handleAppendMessage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid role should 400, got %d", w.Code)
	}
}

func TestAppendMessage_RejectsCrossUser(t *testing.T) {
	srv, sid := newAppendTestServer(t, "alice")
	body := `{"role":"user","content":"intrude"}`
	// bob 尝试向 alice 的会话写消息
	req := httptest.NewRequest("POST", "/api/v1/sessions/"+sid+"/messages?user_id=bob", strings.NewReader(body))
	req.SetPathValue("id", sid)
	w := httptest.NewRecorder()
	srv.handleAppendMessage(w, req)

	if w.Code == http.StatusCreated {
		t.Errorf("cross-user append should be rejected, got 201")
	}
}

func TestAppendMessage_InvalidJSON(t *testing.T) {
	srv, sid := newAppendTestServer(t, "test")
	req := httptest.NewRequest("POST", "/api/v1/sessions/"+sid+"/messages?user_id=test",
		strings.NewReader("{not json"))
	req.SetPathValue("id", sid)
	w := httptest.NewRecorder()
	srv.handleAppendMessage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON should 400, got %d", w.Code)
	}
}

// --- batch append ---

func TestBatchAppendMessages_Success(t *testing.T) {
	srv, sid := newAppendTestServer(t, "test")
	body := `{"messages":[
	  {"id":"u1","role":"user","content":"prompt","metadata":{"mode":"image_gen"}},
	  {"id":"a1","role":"assistant","content":"ok","parent_id":"u1"}
	]}`
	req := httptest.NewRequest("POST", "/api/v1/sessions/"+sid+"/messages/batch?user_id=test", strings.NewReader(body))
	req.SetPathValue("id", sid)
	w := httptest.NewRecorder()
	srv.handleBatchAppendMessages(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		IDs       []string `json:"ids"`
		SessionID string   `json:"session_id"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.IDs) != 2 || resp.IDs[0] != "u1" || resp.IDs[1] != "a1" {
		t.Errorf("ids mismatch: %v", resp.IDs)
	}
	// 验证 DB
	for _, id := range []string{"u1", "a1"} {
		if _, err := srv.store.GetMessage(context.Background(), id); err != nil {
			t.Errorf("message %s not persisted: %v", id, err)
		}
	}
}

// 事务原子性：第二条校验失败（无效 role）应回滚第一条
func TestBatchAppendMessages_AtomicRollback(t *testing.T) {
	srv, sid := newAppendTestServer(t, "test")
	// 构造：第一条合法 + 第二条非法 role，期望整批拒绝
	body := `{"messages":[
	  {"id":"batch-ok","role":"user","content":"ok"},
	  {"id":"batch-bad","role":"bogus","content":"x"}
	]}`
	req := httptest.NewRequest("POST", "/api/v1/sessions/"+sid+"/messages/batch?user_id=test", strings.NewReader(body))
	req.SetPathValue("id", sid)
	w := httptest.NewRecorder()
	srv.handleBatchAppendMessages(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid role should 400, got %d", w.Code)
	}
	// 校验失败在事务前发生 — 第一条不应该被写入
	if _, err := srv.store.GetMessage(context.Background(), "batch-ok"); err == nil {
		t.Error("batch-ok should NOT be persisted (whole batch rejected pre-tx)")
	}
}

func TestBatchAppendMessages_EmptyList(t *testing.T) {
	srv, sid := newAppendTestServer(t, "test")
	body := `{"messages":[]}`
	req := httptest.NewRequest("POST", "/api/v1/sessions/"+sid+"/messages/batch?user_id=test", strings.NewReader(body))
	req.SetPathValue("id", sid)
	w := httptest.NewRecorder()
	srv.handleBatchAppendMessages(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("empty list should 400, got %d", w.Code)
	}
}

func TestBatchAppendMessages_TooMany(t *testing.T) {
	srv, sid := newAppendTestServer(t, "test")
	var sb strings.Builder
	sb.WriteString(`{"messages":[`)
	for i := 0; i < 51; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"role":"user","content":"x"}`)
	}
	sb.WriteString(`]}`)
	req := httptest.NewRequest("POST", "/api/v1/sessions/"+sid+"/messages/batch?user_id=test", strings.NewReader(sb.String()))
	req.SetPathValue("id", sid)
	w := httptest.NewRecorder()
	srv.handleBatchAppendMessages(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("51 messages should 400, got %d", w.Code)
	}
}

func TestBatchAppendMessages_RejectsCrossUser(t *testing.T) {
	srv, sid := newAppendTestServer(t, "alice")
	body := `{"messages":[{"role":"user","content":"x"}]}`
	req := httptest.NewRequest("POST", "/api/v1/sessions/"+sid+"/messages/batch?user_id=bob", strings.NewReader(body))
	req.SetPathValue("id", sid)
	w := httptest.NewRecorder()
	srv.handleBatchAppendMessages(w, req)

	if w.Code == http.StatusCreated {
		t.Errorf("cross-user batch should be rejected, got 201")
	}
}
