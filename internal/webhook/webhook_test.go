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
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	return db
}

// TestManager_RegisterAndList 测试注册和列出 Webhook
func TestManager_RegisterAndList(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	mgr := NewManager(db)
	if err := mgr.Init(ctx); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	wh := &Webhook{
		Name:   "github-deploy",
		Type:   TypeGitHub,
		Secret: "test-secret",
		Prompt: "分析这个 push 事件并汇报",
		UserID: "user-1",
	}

	if err := mgr.Register(ctx, wh); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	webhooks, err := mgr.List(ctx, "user-1")
	if err != nil {
		t.Fatalf("列出失败: %v", err)
	}
	if len(webhooks) != 1 {
		t.Fatalf("应有 1 个 webhook，实际 %d", len(webhooks))
	}
	if webhooks[0].Name != "github-deploy" {
		t.Errorf("名称不匹配: %s", webhooks[0].Name)
	}
}

// TestManager_Unregister 测试注销 Webhook
func TestManager_Unregister(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	mgr := NewManager(db)
	mgr.Init(ctx)

	mgr.Register(ctx, &Webhook{
		Name:   "test-hook",
		Prompt: "测试",
		UserID: "user-1",
	})

	if err := mgr.Unregister(ctx, "test-hook"); err != nil {
		t.Fatalf("注销失败: %v", err)
	}

	webhooks, _ := mgr.List(ctx, "user-1")
	if len(webhooks) != 0 {
		t.Fatalf("注销后应无 webhook，实际 %d", len(webhooks))
	}
}

// TestManager_HandleGenericWebhook 测试通用 Webhook 处理
func TestManager_HandleGenericWebhook(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	mgr := NewManager(db)
	mgr.Init(ctx)

	mgr.Register(ctx, &Webhook{
		Name:   "test",
		Type:   TypeGeneric,
		Prompt: "处理事件",
		UserID: "user-1",
	})

	var handled atomic.Int32
	mgr.SetHandler(func(_ context.Context, event *Event, prompt string) error {
		handled.Add(1)
		if event.WebhookName != "test" {
			t.Errorf("webhook 名称不匹配: %s", event.WebhookName)
		}
		return nil
	})

	// 发送请求
	payload, _ := json.Marshal(map[string]string{"message": "hello"})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/webhooks/{name}", mgr.Handler())

	req := httptest.NewRequest("POST", "/api/v1/webhooks/test", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d: %s", w.Code, w.Body.String())
	}

	// 等待异步处理
	time.Sleep(100 * time.Millisecond)
	if handled.Load() == 0 {
		t.Error("事件应该被处理")
	}
}

// TestManager_GitHubSignatureVerification 测试 GitHub 签名验证
func TestManager_GitHubSignatureVerification(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	mgr := NewManager(db)
	mgr.Init(ctx)

	secret := "my-github-secret"
	mgr.Register(ctx, &Webhook{
		Name:   "github",
		Type:   TypeGitHub,
		Secret: secret,
		Prompt: "分析 PR",
		UserID: "user-1",
	})
	mgr.SetHandler(func(_ context.Context, _ *Event, _ string) error { return nil })

	payload := []byte(`{"action":"opened","pull_request":{"title":"fix bug"}}`)

	// 计算正确签名
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/webhooks/{name}", mgr.Handler())

	// 正确签名
	req := httptest.NewRequest("POST", "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", sig)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("正确签名应返回 200，实际 %d", w.Code)
	}

	// 错误签名
	req = httptest.NewRequest("POST", "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-Hub-Signature-256", "sha256=invalid")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("错误签名应返回 401，实际 %d", w.Code)
	}
}

// TestManager_NotFoundWebhook 测试不存在的 Webhook
func TestManager_NotFoundWebhook(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	mgr := NewManager(db)
	mgr.Init(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/webhooks/{name}", mgr.Handler())

	req := httptest.NewRequest("POST", "/api/v1/webhooks/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("不存在的 webhook 应返回 404，实际 %d", w.Code)
	}
}

// TestParseGitHubSummary 测试 GitHub 事件摘要解析
func TestParseGitHubSummary(t *testing.T) {
	tests := []struct {
		eventType string
		payload   map[string]any
		contains  string
	}{
		{
			"push",
			map[string]any{
				"ref": "refs/heads/main",
				"repository": map[string]any{"full_name": "org/repo"},
				"commits":    []any{map[string]any{}, map[string]any{}},
			},
			"2 commit",
		},
		{
			"pull_request",
			map[string]any{
				"action":       "opened",
				"pull_request": map[string]any{"title": "Fix bug #123"},
				"repository":   map[string]any{"full_name": "org/repo"},
			},
			"Fix bug",
		},
		{
			"issues",
			map[string]any{
				"action": "opened",
				"issue":  map[string]any{"title": "New feature request"},
			},
			"New feature",
		},
	}

	for _, tt := range tests {
		t.Run(tt.eventType, func(t *testing.T) {
			summary := parseGitHubSummary(tt.eventType, tt.payload)
			if summary == "" {
				t.Error("摘要不应为空")
			}
			if !containsStr(summary, tt.contains) {
				t.Errorf("摘要 %q 应包含 %q", summary, tt.contains)
			}
		})
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsHelper(s, sub))
}

func containsHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
