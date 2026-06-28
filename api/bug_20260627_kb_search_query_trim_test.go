package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"

	_ "modernc.org/sqlite"
)

// Bug 20260627: /knowledge/search 只校验精确空串（req.Query == ""），纯空白查询
// "   " 绕过校验进入检索层——与入库侧 (handleAddDocument 的 TrimSpace 校验) 不一致。
// 空白查询不应被当成有效检索：必须 400（与 created_after 非法日期同等的输入校验），
// 而不是静默走一趟必然落空的检索。
func TestHandleSearchKnowledge_WhitespaceQueryRejected(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := knowledge.NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	mgr := knowledge.NewManager(store, store, nil)
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetKnowledgeBase(mgr)

	for _, body := range []string{
		`{"query":"   "}`,        // 纯空格
		`{"query":"\t\n"}`,       // 制表/换行
		`{"query":"","top_k":3}`, // 空串（既有契约）
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/search", strings.NewReader(body))
		w := httptest.NewRecorder()
		srv.handleSearchKnowledge(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("空白查询 %q 应返回 400，得 %d body=%s", body, w.Code, w.Body.String())
		}
	}

	// 正常查询仍 200（不误伤）。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/search", strings.NewReader(`{"query":"widget"}`))
	w := httptest.NewRecorder()
	srv.handleSearchKnowledge(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("正常查询应 200，得 %d body=%s", w.Code, w.Body.String())
	}
}
