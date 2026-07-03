package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"
)

func setupDisabledTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newDisabledTestManager(t *testing.T) *Manager {
	t.Helper()
	mgr := NewManager(setupDisabledTestDB(t))
	if err := mgr.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return mgr
}

func serveWebhook(mgr *Manager, req *http.Request) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/webhooks/{name}", mgr.Handler())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// 创建即得端点、默认未启用：Register 不再硬置 Enabled=true。
func TestRegisterDefaultsDisabled(t *testing.T) {
	mgr := newDisabledTestManager(t)
	ctx := context.Background()
	if err := mgr.Register(ctx, &Webhook{Name: "triage", Type: TypeGeneric, Prompt: "p", UserID: "u"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	list, err := mgr.List(ctx, "u")
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %v %+v", err, list)
	}
	if list[0].Enabled {
		t.Fatal("默认应为未启用（enabled=false）")
	}
}

// 未启用端点：真实事件验签照跑、返回 423、不派发 Agent、不写统计（GO-4 收口）。
func TestDisabledEndpointRecordsWithoutDispatch(t *testing.T) {
	mgr := newDisabledTestManager(t)
	ctx := context.Background()
	secret := "s3cret"
	if err := mgr.Register(ctx, &Webhook{Name: "triage", Type: TypeGeneric, Secret: secret, Prompt: "p", UserID: "u"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var dispatched atomic.Int32
	mgr.SetHandler(func(context.Context, *Event, string) error {
		dispatched.Add(1)
		return nil
	})

	payload := []byte(`{"hello":"world"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	// 错签名仍 401（验签先于启用判断——未启用不等于不设防）
	badReq := httptest.NewRequest("POST", "/api/v1/webhooks/triage", bytes.NewReader(payload))
	badReq.Header.Set("X-Webhook-Signature", "sha256=deadbeef")
	if rec := serveWebhook(mgr, badReq); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错签名应 401，得到 %d", rec.Code)
	}

	// 正确签名 → 423 不派发
	req := httptest.NewRequest("POST", "/api/v1/webhooks/triage", bytes.NewReader(payload))
	req.Header.Set("X-Webhook-Signature", sig)
	rec := serveWebhook(mgr, req)
	if rec.Code != http.StatusLocked {
		t.Fatalf("未启用端点应 423，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if dispatched.Load() != 0 {
		t.Fatal("未启用端点不应派发 Agent")
	}
	// GO-4 收口：未启用端点统计零写入（原「记录不派发」语义作废，见 bug_20260703_go4 测试）
	list, _ := mgr.List(ctx, "u")
	if len(list) != 1 || list[0].EventCount != 0 {
		t.Fatalf("未启用端点不应记录事件计数：%+v", list)
	}
}

// 测试事件（?test=1）：验签 + 回显解析结果，不派发、不计数——对启用与未启用端点都成立。
func TestTestEventEchoesWithoutDispatch(t *testing.T) {
	mgr := newDisabledTestManager(t)
	ctx := context.Background()
	secret := "s3cret"
	if err := mgr.Register(ctx, &Webhook{Name: "triage", Type: TypeGeneric, Secret: secret, Prompt: "p", UserID: "u", Enabled: true}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var dispatched atomic.Int32
	mgr.SetHandler(func(context.Context, *Event, string) error {
		dispatched.Add(1)
		return nil
	})

	payload := []byte(`{"hello":"world"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest("POST", "/api/v1/webhooks/triage?test=1", bytes.NewReader(payload))
	req.Header.Set("X-Webhook-Signature", sig)
	req.Header.Set("X-Event-Type", "demo")
	rec := serveWebhook(mgr, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("测试事件应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp["status"] != "test" || resp["signature"] != "ok" || resp["event_type"] != "demo" {
		t.Fatalf("测试事件回显不符: %v", resp)
	}
	if dispatched.Load() != 0 {
		t.Fatal("测试事件不应派发")
	}
	list, _ := mgr.List(ctx, "u")
	if list[0].EventCount != 0 {
		t.Fatal("测试事件不应计入事件统计")
	}
}

// GitHub ping（对端创建 webhook 时自动发送）按测试事件处理。
func TestGitHubPingTreatedAsTest(t *testing.T) {
	mgr := newDisabledTestManager(t)
	ctx := context.Background()
	if err := mgr.Register(ctx, &Webhook{Name: "gh", Type: TypeGitHub, Prompt: "p", UserID: "u"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/v1/webhooks/gh", bytes.NewReader([]byte(`{"zen":"keep it simple"}`)))
	req.Header.Set("X-GitHub-Event", "ping")
	rec := serveWebhook(mgr, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GitHub ping 应 200 回显，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

// SetEnabled 启停 + 重载（loadWebhooks 现在必须包含未启用端点）。
func TestSetEnabledAndReloadKeepsDisabledEndpoints(t *testing.T) {
	db := setupDisabledTestDB(t)
	ctx := context.Background()
	mgr := NewManager(db)
	if err := mgr.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := mgr.Register(ctx, &Webhook{Name: "triage", Type: TypeGeneric, Prompt: "p", UserID: "u"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var dispatched atomic.Int32
	mgr.SetHandler(func(context.Context, *Event, string) error {
		dispatched.Add(1)
		return nil
	})

	// 启用后真实事件应派发
	if err := mgr.SetEnabled(ctx, "triage", true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/v1/webhooks/triage", bytes.NewReader([]byte(`{"a":1}`)))
	if rec := serveWebhook(mgr, req); rec.Code != http.StatusOK {
		t.Fatalf("启用后应 200，得到 %d", rec.Code)
	}

	// 停用回到 423
	if err := mgr.SetEnabled(ctx, "triage", false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	req = httptest.NewRequest("POST", "/api/v1/webhooks/triage", bytes.NewReader([]byte(`{"a":2}`)))
	if rec := serveWebhook(mgr, req); rec.Code != http.StatusLocked {
		t.Fatalf("停用后应 423，得到 %d", rec.Code)
	}

	// 不存在的 webhook 报错
	if err := mgr.SetEnabled(ctx, "nope", true); err == nil {
		t.Fatal("不存在的 webhook 应报错")
	}

	// 重启重载：未启用端点必须仍可寻址（此前 loadWebhooks 只装启用的）
	mgr2 := NewManager(db)
	if err := mgr2.Init(ctx); err != nil {
		t.Fatalf("Init(重载): %v", err)
	}
	if _, ok := mgr2.Get("triage"); !ok {
		t.Fatal("重载后未启用端点丢失")
	}
	req = httptest.NewRequest("POST", "/api/v1/webhooks/triage", bytes.NewReader([]byte(`{"a":3}`)))
	if rec := serveWebhook(mgr2, req); rec.Code != http.StatusLocked {
		t.Fatalf("重载后的未启用端点应 423，得到 %d", rec.Code)
	}
}
