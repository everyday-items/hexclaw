package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/webhook"

	_ "modernc.org/sqlite"
)

// AP-031 — webhook 绑定 cron job (§13.3) 无写入路径：RegisterWebhookRequest 不收 job_id，
// 即便后端 webhook.Webhook.JobID + DB 列 + 接收路由(webhook.go:270→main.go:845) 全在。
// RED（未修）：POST 携 job_id，但创建后存储的 webhook.JobID 恒为空。
func TestAP031_RegisterWebhook_PersistsJobID(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	mgr := webhook.NewManager(db)
	if err := mgr.Init(ctx); err != nil {
		t.Fatalf("mgr init: %v", err)
	}

	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)
	srv.webhookMgr = mgr

	body := `{"name":"cronhook","type":"generic","prompt":"p","user_id":"u1","job_id":"job-42"}`
	req := httptest.NewRequest("POST", "/api/v1/webhooks", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleRegisterWebhook(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("register failed: %d %s", w.Code, w.Body.String())
	}

	list, err := mgr.List(ctx, "u1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *webhook.Webhook
	for _, wh := range list {
		if wh.Name == "cronhook" {
			found = wh
		}
	}
	if found == nil {
		t.Fatal("webhook 未创建")
	}
	if found.JobID != "job-42" {
		t.Fatalf("AP-031: webhook 应绑定 job_id=job-42，实际=%q（创建端点丢弃了 job_id，能力 UI/HTTP 不可达）", found.JobID)
	}
}

// AP-032 — saveWorkflow 契约错位：前端声明 Promise<Workflow>，后端只回 {id,message}。
// RED（未修）：响应缺 name/nodes 等 Workflow 字段。
func TestAP032_SaveWorkflow_ReturnsFullWorkflow(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"id":"wf-1","name":"flow","nodes":[{"id":"n1"}],"edges":[]}`
	req := httptest.NewRequest("POST", "/api/v1/canvas/workflows", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleSaveWorkflow(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("save failed: %d %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp["name"] != "flow" {
		t.Fatalf("AP-032: 响应应回完整 Workflow（含 name=flow），实际 keys=%v（只回 {id,message}，前端 Promise<Workflow> 类型撒谎）", keysOf(resp))
	}
	if _, ok := resp["nodes"]; !ok {
		t.Fatalf("AP-032: 响应缺 nodes 字段，调用方拿不到完整资源")
	}
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
