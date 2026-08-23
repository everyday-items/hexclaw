package webhook

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"
)

type webhookOwnerManagerRow struct {
	ID      string
	Name    string
	OwnerID string
	Enabled bool
}

type webhookOwnerManagerSnapshot struct {
	DBRows []webhookOwnerManagerRow
	Memory []webhookOwnerManagerRow
}

func snapshotWebhookOwnerManager(t *testing.T, mgr *Manager) webhookOwnerManagerSnapshot {
	t.Helper()
	rows, err := mgr.db.QueryContext(context.Background(),
		`SELECT id, name, user_id, enabled FROM webhooks ORDER BY name`)
	if err != nil {
		t.Fatalf("query webhooks: %v", err)
	}
	var out webhookOwnerManagerSnapshot
	for rows.Next() {
		var row webhookOwnerManagerRow
		var enabled int
		if err := rows.Scan(&row.ID, &row.Name, &row.OwnerID, &enabled); err != nil {
			_ = rows.Close()
			t.Fatalf("scan webhook: %v", err)
		}
		row.Enabled = enabled == 1
		out.DBRows = append(out.DBRows, row)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close webhook rows: %v", err)
	}

	mgr.mu.RLock()
	for _, wh := range mgr.webhooks {
		out.Memory = append(out.Memory, webhookOwnerManagerRow{
			ID: wh.ID, Name: wh.Name, OwnerID: wh.UserID, Enabled: wh.Enabled,
		})
	}
	mgr.mu.RUnlock()
	sort.Slice(out.Memory, func(i, j int) bool { return out.Memory[i].Name < out.Memory[j].Name })
	return out
}

func seedWebhookOwnerManager(t *testing.T, mgr *Manager, name, owner string, enabled bool) {
	t.Helper()
	if err := mgr.Register(context.Background(), &Webhook{
		ID: "wh-" + name, Name: name, Type: TypeGeneric, Secret: "owner-secret",
		Prompt: "run", UserID: owner, Enabled: enabled,
	}); err != nil {
		t.Fatalf("register webhook %q: %v", name, err)
	}
}

// TestBUG20260801003GenericWebhookCarriesPersistedOwner 钉死通用 Webhook
// 只能使用定义中持久化的可信所有者，不能接受请求侧伪造身份。
func TestBUG20260801003GenericWebhookCarriesPersistedOwner(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	writer := NewManager(db)
	if err := writer.Init(ctx); err != nil {
		t.Fatalf("init writer: %v", err)
	}
	if err := writer.Register(ctx, &Webhook{
		Name: "owner-bound", Type: TypeGeneric, Secret: "owner-secret",
		Prompt: "run", UserID: "trusted-owner", Enabled: true,
	}); err != nil {
		t.Fatalf("register webhook: %v", err)
	}

	// 用新的 Manager 从数据库恢复，避免只验证进程内对象。
	reader := NewManager(db)
	if err := reader.Init(ctx); err != nil {
		t.Fatalf("init reader: %v", err)
	}
	received := make(chan Event, 1)
	reader.SetHandler(func(_ context.Context, event *Event, _ string) error {
		received <- *event
		return nil
	})

	body := []byte(`{"user_id":"forged-payload-owner","owner_id":"forged-owner"}`)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/webhooks/{name}", reader.Handler())
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/webhooks/owner-bound?user_id=forged-query-owner", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "forged-header-owner")
	req.Header.Set("X-Webhook-Signature", genericSigHeader("owner-secret", body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	select {
	case event := <-received:
		if event.UserID != "trusted-owner" {
			t.Fatalf("event owner=%q, want persisted trusted owner", event.UserID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook dispatch")
	}
}

// TestBUG20260801003RegisterRejectsEmptyOwner 钉死所有新建通用 Webhook
// 必须携带非空可信所有者，空字符串和纯空白均不得持久化。
func TestBUG20260801003RegisterRejectsEmptyOwner(t *testing.T) {
	tests := []struct {
		name  string
		owner string
	}{
		{name: "empty", owner: ""},
		{name: "spaces", owner: "   "},
		{name: "mixed whitespace", owner: "\t\n "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupTestDB(t)
			defer db.Close()

			mgr := NewManager(db)
			if err := mgr.Init(context.Background()); err != nil {
				t.Fatalf("initialize webhook manager: %v", err)
			}
			err := mgr.Register(context.Background(), &Webhook{
				Name: "owner-required-" + tt.name, Type: TypeGeneric,
				Secret: "owner-secret", Prompt: "run", UserID: tt.owner,
			})
			if !errors.Is(err, ErrWebhookOwnerRequired) {
				t.Fatalf("register error=%v, want ErrWebhookOwnerRequired", err)
			}
		})
	}
}

// TestBUG20260801003LoadQuarantinesOwnerlessHistoryWithoutFailingStartup
// 钉死历史空 owner 记录只被隔离，不得阻断启动或进入事件派发链。
func TestBUG20260801003LoadQuarantinesOwnerlessHistoryWithoutFailingStartup(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	schema := NewManager(db)
	if err := schema.Init(ctx); err != nil {
		t.Fatalf("initialize webhook schema: %v", err)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO webhooks
			(id, name, type, secret, prompt, user_id, enabled, created_at, job_id)
		VALUES
			('wh-valid-history', 'valid-history', 'generic', 'valid-secret', 'run', 'trusted-owner', 1, CURRENT_TIMESTAMP, ''),
			('wh-ownerless-history', 'ownerless-history', 'generic', 'ownerless-secret', 'run', '', 1, CURRENT_TIMESTAMP, '')`)
	if err != nil {
		t.Fatalf("insert historical webhooks: %v", err)
	}

	dispatched := make(chan struct{}, 1)
	restored := NewManager(db)
	restored.SetHandler(func(context.Context, *Event, string) error {
		dispatched <- struct{}{}
		return nil
	})
	if err := restored.Init(ctx); err != nil {
		t.Fatalf("restore manager with ownerless history: %v", err)
	}
	if _, ok := restored.Get("valid-history"); !ok {
		t.Error("valid historical webhook was not restored")
	}
	if _, ok := restored.Get("ownerless-history"); ok {
		t.Error("ownerless historical webhook was restored into the routing map")
	}

	body := []byte(`{"event":"ownerless-history-probe"}`)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/webhooks/{name}", restored.Handler())
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/webhooks/ownerless-history", bytes.NewReader(body))
	req.Header.Set("X-Webhook-Signature", genericSigHeader("ownerless-secret", body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("ownerless historical endpoint status=%d, want 404", rec.Code)
	}
	select {
	case <-dispatched:
		t.Fatal("ownerless historical webhook reached the event handler")
	default:
	}
}

// TestBUG20260801003SetEnabledForOwnerHidesCrossOwnerAndMissing 钉死启停管理面：
// 跨 owner 与不存在统一返回 ErrWebhookNotFound，数据库和内存镜像均不得变化。
func TestBUG20260801003SetEnabledForOwnerHidesCrossOwnerAndMissing(t *testing.T) {
	for _, tt := range []struct {
		name       string
		targetName string
		owner      string
	}{
		{name: "cross owner", targetName: "owner-a-hook", owner: "owner-b"},
		{name: "missing", targetName: "missing-hook", owner: "owner-a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := setupTestDB(t)
			defer db.Close()
			mgr := NewManager(db)
			if err := mgr.Init(context.Background()); err != nil {
				t.Fatalf("initialize manager: %v", err)
			}
			seedWebhookOwnerManager(t, mgr, "owner-a-hook", "owner-a", false)
			before := snapshotWebhookOwnerManager(t, mgr)

			err := mgr.SetEnabledForOwner(context.Background(), tt.targetName, tt.owner, true)
			if !errors.Is(err, ErrWebhookNotFound) {
				t.Errorf("error=%v, want ErrWebhookNotFound", err)
			}
			after := snapshotWebhookOwnerManager(t, mgr)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected SetEnabledForOwner changed state\nbefore=%+v\nafter=%+v", before, after)
			}
		})
	}
}

// TestBUG20260801003UnregisterForOwnerHidesCrossOwnerAndMissing 钉死删除管理面：
// 跨 owner 与不存在统一返回 ErrWebhookNotFound，数据库和内存镜像均不得变化。
func TestBUG20260801003UnregisterForOwnerHidesCrossOwnerAndMissing(t *testing.T) {
	for _, tt := range []struct {
		name       string
		targetName string
		owner      string
	}{
		{name: "cross owner", targetName: "owner-a-hook", owner: "owner-b"},
		{name: "missing", targetName: "missing-hook", owner: "owner-a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := setupTestDB(t)
			defer db.Close()
			mgr := NewManager(db)
			if err := mgr.Init(context.Background()); err != nil {
				t.Fatalf("initialize manager: %v", err)
			}
			seedWebhookOwnerManager(t, mgr, "owner-a-hook", "owner-a", true)
			before := snapshotWebhookOwnerManager(t, mgr)

			_, err := mgr.UnregisterForOwner(context.Background(), tt.targetName, tt.owner)
			if !errors.Is(err, ErrWebhookNotFound) {
				t.Errorf("error=%v, want ErrWebhookNotFound", err)
			}
			after := snapshotWebhookOwnerManager(t, mgr)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected UnregisterForOwner changed state\nbefore=%+v\nafter=%+v", before, after)
			}
		})
	}
}
