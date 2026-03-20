package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/instances"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

func newTestInstanceManager(t *testing.T) (*instances.Manager, func()) {
	t.Helper()

	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "api-instances.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 存储失败: %v", err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化 SQLite 存储失败: %v", err)
	}

	mgr := instances.NewManager(store.DB())
	if err := mgr.Init(context.Background()); err != nil {
		t.Fatalf("初始化实例管理器失败: %v", err)
	}
	return mgr, func() { store.Close() }
}

func TestHandleListInstanceHealth(t *testing.T) {
	mgr, cleanup := newTestInstanceManager(t)
	defer cleanup()

	if err := mgr.Upsert(context.Background(), &instances.Instance{
		Provider: "slack",
		Name:     "slack-main",
		Enabled:  false,
		Config:   []byte(`{"token":"x"}`),
	}); err != nil {
		t.Fatalf("保存实例失败: %v", err)
	}

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetInstanceManager(mgr)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/platforms/instances/health", nil)
	w := httptest.NewRecorder()

	srv.handleListInstanceHealth(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Instances []instances.HealthReport `json:"instances"`
		Total     int                      `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Total != 1 || len(resp.Instances) != 1 {
		t.Fatalf("期望 1 个实例，实际 total=%d len=%d", resp.Total, len(resp.Instances))
	}
	if resp.Instances[0].Healthy {
		t.Fatalf("停止实例不应标记为 healthy: %+v", resp.Instances[0])
	}
	if resp.Instances[0].Status != instances.StatusStopped {
		t.Fatalf("期望 status=stopped，实际 %s", resp.Instances[0].Status)
	}
}

func TestHandleGetInstanceHealth(t *testing.T) {
	mgr, cleanup := newTestInstanceManager(t)
	defer cleanup()

	if err := mgr.Upsert(context.Background(), &instances.Instance{
		Provider: "telegram",
		Name:     "telegram-main",
		Enabled:  false,
		Config:   []byte(`{"token":"x"}`),
	}); err != nil {
		t.Fatalf("保存实例失败: %v", err)
	}

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetInstanceManager(mgr)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/platforms/instances/telegram-main/health", nil)
	req.SetPathValue("name", "telegram-main")
	w := httptest.NewRecorder()

	srv.handleGetInstanceHealth(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	var resp instances.HealthReport
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Name != "telegram-main" {
		t.Fatalf("期望 name=telegram-main，实际 %q", resp.Name)
	}
	if resp.Status != instances.StatusStopped {
		t.Fatalf("期望 status=stopped，实际 %s", resp.Status)
	}
}
