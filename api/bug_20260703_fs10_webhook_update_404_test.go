package api

// FS-10（BUG-20260703）：PATCH /webhooks/{name} 对不存在的 name 返 500（把
// 「资源不存在」误报成服务端故障）。契约：不存在 → 404。

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	"github.com/hexagon-codes/hexclaw/webhook"
)

func newWebhookTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatalf("init numbered webhook migrations: %v", err)
	}
	mgr := webhook.NewManager(db)
	if err := mgr.Init(context.Background()); err != nil {
		t.Fatalf("webhook Init: %v", err)
	}
	srv := NewServer(config.DefaultConfig(), &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)
	srv.SetWebhookManager(mgr)
	return srv
}

func TestBug20260703_UpdateWebhookUnknownNameReturns404(t *testing.T) {
	srv := newWebhookTestServer(t)

	req := httptest.NewRequest("PATCH", "/api/v1/webhooks/ghost?user_id=u", strings.NewReader(`{"enabled":true}`))
	req.SetPathValue("name", "ghost")
	w := httptest.NewRecorder()
	srv.handleUpdateWebhook(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("[FS-10] 不存在的 webhook 应 404，实际 %d: %s", w.Code, w.Body.String())
	}
}

func TestBug20260703_UpdateWebhookExistingNameReturns200(t *testing.T) {
	srv := newWebhookTestServer(t)
	if err := srv.webhookMgr.Register(context.Background(),
		&webhook.Webhook{Name: "real", Type: webhook.TypeGeneric, Prompt: "p", UserID: "u"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	req := httptest.NewRequest("PATCH", "/api/v1/webhooks/real?user_id=u", strings.NewReader(`{"enabled":true}`))
	req.SetPathValue("name", "real")
	w := httptest.NewRecorder()
	srv.handleUpdateWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("存在的 webhook 更新应 200，实际 %d: %s", w.Code, w.Body.String())
	}
}
