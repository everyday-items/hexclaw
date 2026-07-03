package api

// BUG-20260703 P2b：handleUpdateSession 标题无任何校验原样入库（边界空洞）。
//
// 症状：`sess.Title = req.Title` 直写——空串/纯空白把标题清成不可辨识；超长标题
// （粘贴整段文章）无上限入库，列表渲染与 FTS 索引都被拖累。
// 修法：trim 后空 → 400；rune 计数超上限（200）→ 400；合法标题去首尾空白后入库。

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

func newTitleValidationServer(t *testing.T) *Server {
	t.Helper()
	store := newTestStoreForAPI(t)
	srv := NewServer(config.DefaultConfig(), &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, store)
	if err := store.CreateSession(context.Background(), &storage.Session{
		ID: "sess-title", UserID: "test", Platform: "web", Title: "旧标题",
	}); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	return srv
}

func patchTitle(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/sessions/sess-title?user_id=test", strings.NewReader(body))
	req.SetPathValue("id", "sess-title")
	w := httptest.NewRecorder()
	srv.handleUpdateSession(w, req)
	return w
}

func TestBug20260703P2b_EmptyTitleRejected(t *testing.T) {
	srv := newTitleValidationServer(t)
	if w := patchTitle(t, srv, `{"title":""}`); w.Code != http.StatusBadRequest {
		t.Fatalf("空标题应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if w := patchTitle(t, srv, `{"title":"   \t  "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("纯空白标题应 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestBug20260703P2b_OverlongTitleRejected(t *testing.T) {
	srv := newTitleValidationServer(t)
	long := strings.Repeat("题", 201) // 201 个多字节 rune：上限必须按 rune 数不按字节数
	body, _ := json.Marshal(map[string]string{"title": long})
	if w := patchTitle(t, srv, string(body)); w.Code != http.StatusBadRequest {
		t.Fatalf("超长标题(201 rune)应 400，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestBug20260703P2b_ValidTitleTrimmedAndSaved(t *testing.T) {
	srv := newTitleValidationServer(t)
	// 200 rune 恰好在上限内；首尾空白应被去除后入库。
	edge := strings.Repeat("边", 200)
	body, _ := json.Marshal(map[string]string{"title": "  " + edge + "  "})
	w := patchTitle(t, srv, string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("200 rune 合法标题应保存成功，实际 %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp["title"] != edge {
		t.Fatalf("标题应去首尾空白后入库，实际 %q", resp["title"])
	}
}
