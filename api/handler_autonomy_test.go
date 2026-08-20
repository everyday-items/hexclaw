package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/autonomy"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/skill"
)

func newAutonomyTestServer(t *testing.T) (*Server, *autonomy.GrantStore, *autonomy.DecisionStore, string) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	decisions := autonomy.NewDecisionStore(db)
	if err := decisions.Init(ctx); err != nil {
		t.Fatalf("decisions Init: %v", err)
	}
	grants := autonomy.NewGrantStore(db)
	if err := grants.Init(ctx); err != nil {
		t.Fatalf("grants Init: %v", err)
	}

	cfg := config.DefaultConfig()
	hook := engine.NewPermissionHook(engine.NewPermissionHub(0),
		engine.WithSystemDispatchPolicy(engine.NewSystemDispatchPolicyFromConfig(cfg.Security.Autonomy)),
		engine.WithTaskGrants(grants),
	)

	// Profile 持久化落到临时文件，不碰真实 ~/.hexclaw/hexclaw.yaml。
	cfgPath := filepath.Join(t.TempDir(), "hexclaw.yaml")

	srv := NewServer(cfg, &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)
	srv.SetAutonomy(hook, decisions, grants, cfgPath)
	return srv, grants, decisions, cfgPath
}

func doAutonomyJSON(t *testing.T, srv *Server, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	// 桌面 sidecar 场景：本机回环放行写操作（apiAuthMiddleware）。
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

func TestAutonomyProfileGetAndUpdatePersistsAndHotSwaps(t *testing.T) {
	srv, _, _, cfgPath := newAutonomyTestServer(t)

	rec, resp := doAutonomyJSON(t, srv, "GET", "/api/v1/autonomy/profile", nil)
	if rec.Code != http.StatusOK || resp["profile"] != "function_first" {
		t.Fatalf("GET profile: %d %v", rec.Code, resp)
	}
	matrix, ok := resp["matrix"].(map[string]any)
	if !ok || matrix["profile"] != "function_first" {
		t.Fatalf("profile 响应应含矩阵快照: %v", resp)
	}

	// 非法 profile 400
	rec, _ = doAutonomyJSON(t, srv, "PUT", "/api/v1/autonomy/profile", map[string]string{"profile": "yolo"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 profile 应 400，得到 %d", rec.Code)
	}

	// 合法切换：持久化 + 热更新
	rec, resp = doAutonomyJSON(t, srv, "PUT", "/api/v1/autonomy/profile", map[string]string{"profile": "strict"})
	if rec.Code != http.StatusOK || resp["profile"] != "strict" {
		t.Fatalf("PUT profile: %d %v", rec.Code, resp)
	}
	// 热更新已生效：strict 下 webhook 源沙箱执行也转审批
	if srv.autonomyPolicy().Allows("webhook", "code_exec") {
		t.Fatal("strict 热更后 code_exec 不应自动放行")
	}
	// 配置已落盘
	raw, err := os.ReadFile(cfgPath)
	if err != nil || !bytes.Contains(raw, []byte("strict")) {
		t.Fatalf("profile 未持久化到配置文件: %v", err)
	}
}

// full_access 允许运行时经弹窗确认热切（产品决策 2026-07-03：单用户桌面去掉
// 手动改配置+重启的摩擦；不再返回 403）。安全权衡：loopback 端点无鉴权，运行时
// 放行 full_access 意味着放弃 GO-1 的自提权防护——后续以 token 门控（方案 B）加固。
func TestAutonomyProfileFullAccessHotSwapsAtRuntime(t *testing.T) {
	srv, _, _, cfgPath := newAutonomyTestServer(t)

	rec, resp := doAutonomyJSON(t, srv, "PUT", "/api/v1/autonomy/profile", map[string]string{"profile": "full_access"})
	if rec.Code != http.StatusOK || resp["profile"] != "full_access" {
		t.Fatalf("full_access 应运行时可切并返回 200，得到 %d %v", rec.Code, resp)
	}
	// 热更新已生效：full_access 下 webhook 源宿主直执行也自动放行
	if !srv.autonomyPolicy().Allows("webhook", "shell") {
		t.Fatal("full_access 热更后 webhook shell 应自动放行")
	}
	// 配置已落盘
	raw, err := os.ReadFile(cfgPath)
	if err != nil || !bytes.Contains(raw, []byte("full_access")) {
		t.Fatalf("full_access 未持久化到配置文件: %v", err)
	}
}

func TestAutonomyProfileFullAccessHotSwapSuppressesInteractiveApproval(t *testing.T) {
	srv, grants, decisions, cfgPath := newAutonomyTestServer(t)
	hook := engine.NewPermissionHook(engine.NewPermissionHub(0),
		engine.WithPolicy(engine.DefaultBaselinePolicy()),
		engine.WithSystemDispatchPolicy(engine.DefaultSystemDispatchPolicy()),
		engine.WithTaskGrants(grants),
	)
	srv.SetAutonomy(hook, decisions, grants, cfgPath)

	rec, resp := doAutonomyJSON(t, srv, "PUT", "/api/v1/autonomy/profile", map[string]string{"profile": "full_access"})
	if rec.Code != http.StatusOK || resp["profile"] != "full_access" {
		t.Fatalf("full_access 应运行时可切并返回 200，得到 %d %v", rec.Code, resp)
	}

	ctx := skill.WithAuthenticatedUser(context.Background(), "interactive-owner")
	if err := srv.autonomyHook.BeforeToolCall(ctx, &engine.ToolCallInfo{Name: "browser", Source: "skill"}); err != nil {
		t.Fatalf("设置 API 热更新后交互 browser 不应再要求审批: %v", err)
	}
}

func TestAutonomyPreflightEndpoint(t *testing.T) {
	srv, grants, _, _ := newAutonomyTestServer(t)

	// source 必填
	rec, _ := doAutonomyJSON(t, srv, "POST", "/api/v1/autonomy/preflight", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 source 应 400，得到 %d", rec.Code)
	}

	// 常规 cron 全绿
	rec, resp := doAutonomyJSON(t, srv, "POST", "/api/v1/autonomy/preflight", map[string]any{
		"source": "cron", "prompt": "汇总昨日指标发送到飞书", "deliver": true,
	})
	if rec.Code != http.StatusOK || resp["all_clear"] != true {
		t.Fatalf("常规 cron 应全绿: %d %v", rec.Code, resp)
	}

	// 连接器工具转审批；授权后 granted
	rec, resp = doAutonomyJSON(t, srv, "POST", "/api/v1/autonomy/preflight", map[string]any{
		"source": "webhook", "task_ref": "webhook:wh-1", "tools": []string{"github.issues.write_label"},
	})
	if rec.Code != http.StatusOK || resp["all_clear"] != false {
		t.Fatalf("连接器工具应转审批: %d %v", rec.Code, resp)
	}
	if _, err := grants.Create(context.Background(), autonomy.Grant{
		TaskRef: "webhook:wh-1", Entries: []string{"github.issues.write_label"},
	}); err != nil {
		t.Fatalf("Create grant: %v", err)
	}
	rec, resp = doAutonomyJSON(t, srv, "POST", "/api/v1/autonomy/preflight", map[string]any{
		"source": "webhook", "task_ref": "webhook:wh-1", "tools": []string{"github.issues.write_label"},
	})
	if rec.Code != http.StatusOK || resp["all_clear"] != true {
		t.Fatalf("授权后应全绿: %d %v", rec.Code, resp)
	}
}

func TestAutonomyPreflightWorkflowStaticAnalysis(t *testing.T) {
	srv, _, _, _ := newAutonomyTestServer(t)

	// 直接塞一个带 publish 节点的工作流进内存 store
	srv.workflowStore.mu.Lock()
	srv.workflowStore.workflows["wf-1"] = &WorkflowData{
		ID:   "wf-1",
		Name: "每周发布摘要",
		Nodes: []any{
			map[string]any{"type": "input"},
			map[string]any{"type": "agent", "data": map[string]any{"prompt": "生成摘要"}},
			map[string]any{"type": "tool", "data": map[string]any{"tool": "publish_wechat"}},
			map[string]any{"type": "tool", "data": map[string]any{"name": "send_message"}},
		},
	}
	srv.workflowStore.mu.Unlock()

	rec, resp := doAutonomyJSON(t, srv, "POST", "/api/v1/autonomy/preflight", map[string]any{
		"source": "workflow", "workflow_id": "wf-1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("preflight: %d %v", rec.Code, resp)
	}
	if resp["all_clear"] != false {
		t.Fatalf("publish 节点应触发需审批: %v", resp)
	}
	needs, _ := resp["needs_decision"].([]any)
	found := false
	for _, n := range needs {
		if n == "publish" {
			found = true
		}
	}
	if !found {
		t.Fatalf("needs_decision 应含 publish: %v", needs)
	}
}

func TestAutonomyGrantsCRUDAndSummary(t *testing.T) {
	srv, _, decisions, _ := newAutonomyTestServer(t)

	// 创建授权
	rec, resp := doAutonomyJSON(t, srv, "POST", "/api/v1/autonomy/grants", map[string]any{
		"task_ref": "webhook:wh-1", "source": "webhook",
		"entries": []string{"github.issues.write_label"}, "note": "仅本任务写标签",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("创建授权: %d %v", rec.Code, resp)
	}
	grant, _ := resp["grant"].(map[string]any)
	grantID, _ := grant["id"].(string)
	if grantID == "" {
		t.Fatalf("授权应返回 id: %v", resp)
	}

	// 列表可见
	rec, resp = doAutonomyJSON(t, srv, "GET", "/api/v1/autonomy/grants?task_ref=webhook:wh-1", nil)
	if rec.Code != http.StatusOK || resp["total"] != float64(1) {
		t.Fatalf("授权列表: %d %v", rec.Code, resp)
	}

	// 决策日志端点
	mustRecordDecision(t, decisions, autonomy.Decision{
		Source: "webhook", TaskRef: "webhook:wh-9", Tool: "github.issues.write_label",
		Decision: "pending", Via: "matrix", Profile: "function_first",
	})
	rec, resp = doAutonomyJSON(t, srv, "GET", "/api/v1/autonomy/decisions?decision=pending", nil)
	if rec.Code != http.StatusOK || resp["total"] != float64(1) {
		t.Fatalf("决策日志查询: %d %v", rec.Code, resp)
	}

	// 总览：无 scheduler/webhookMgr 也应工作（空任务 + grants 计数）
	rec, resp = doAutonomyJSON(t, srv, "GET", "/api/v1/autonomy/summary", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("summary: %d %v", rec.Code, resp)
	}
	counts, _ := resp["counts"].(map[string]any)
	if counts["grants"] != float64(1) {
		t.Fatalf("summary grants 计数不符: %v", counts)
	}

	// 撤销
	rec, _ = doAutonomyJSON(t, srv, "DELETE", "/api/v1/autonomy/grants/"+grantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("撤销授权: %d", rec.Code)
	}
	rec, resp = doAutonomyJSON(t, srv, "GET", "/api/v1/autonomy/grants", nil)
	if resp["total"] != float64(0) {
		t.Fatalf("撤销后列表应空: %v", resp)
	}
	// 再撤销 404
	rec, _ = doAutonomyJSON(t, srv, "DELETE", "/api/v1/autonomy/grants/"+grantID, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复撤销应 404，得到 %d", rec.Code)
	}
}

func mustRecordDecision(t *testing.T, s *autonomy.DecisionStore, d autonomy.Decision) {
	t.Helper()
	if err := s.Record(context.Background(), d); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// BUG-20260801-003：创建授权时 owner 由服务端从可信上下文冻结（客户端不
// 可伪造），可选的 security_scope_digest 透传并持久化；同一授权在重启
// （重建 store 重新 reload）后仍能以精确证据授权链命中。
func TestAutonomyGrantFreezesTrustedOwnerAndPersistsScopeDigest(t *testing.T) {
	srv, grants, _, _ := newAutonomyTestServer(t)

	rec, resp := doAutonomyJSON(t, srv, "POST", "/api/v1/autonomy/grants", map[string]any{
		"task_ref":              "cron:job-1",
		"source":                "cron",
		"entries":               []string{"shell"},
		"security_scope_digest": "scope-digest-abc",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("创建授权: %d %v", rec.Code, resp)
	}
	grant, _ := resp["grant"].(map[string]any)
	if grant["owner_id"] != defaultDesktopUserID {
		t.Fatalf("grant owner = %v, want frozen %q（客户端不可伪造）", grant["owner_id"], defaultDesktopUserID)
	}
	if grant["security_scope_digest"] != "scope-digest-abc" {
		t.Fatalf("security_scope_digest 未透传: %v", grant["security_scope_digest"])
	}

	// 重启恢复：重新 Init 即从同一库 reload（等价进程重启后重建 store）。
	if err := grants.Init(context.Background()); err != nil {
		t.Fatalf("reload Init: %v", err)
	}
	if !grants.GrantAllowsUntrustedEvidence(defaultDesktopUserID, "cron", "cron:job-1", "shell", "scope-digest-abc") {
		t.Fatal("重启后精确证据授权链必须仍可命中")
	}
}

// summary 应把决策日志里的未解决阻断叠加到任务状态上（事实口径优先于预估）。
func TestAutonomySummaryOverlaysPendingBlocks(t *testing.T) {
	srv, _, decisions, _ := newAutonomyTestServer(t)

	// 内存 workflow store 放一个全绿工作流
	srv.workflowStore.mu.Lock()
	srv.workflowStore.workflows["wf-2"] = &WorkflowData{
		ID: "wf-2", Name: "只读工作流",
		Nodes: []any{map[string]any{"type": "tool", "data": map[string]any{"tool": "search"}}},
	}
	srv.workflowStore.mu.Unlock()

	// 决策日志说它运行时被拦（比如模型临时调了未授权工具）
	mustRecordDecision(t, decisions, autonomy.Decision{
		Source: "workflow", TaskRef: "workflow:wf-2", Tool: "notion.pages.write",
		Decision: "pending", Via: "matrix", Profile: "function_first",
	})

	_, resp := doAutonomyJSON(t, srv, "GET", "/api/v1/autonomy/summary", nil)
	pending, _ := resp["pending"].([]any)
	if len(pending) != 1 {
		t.Fatalf("运行时阻断应把任务列入待处理: %v", resp)
	}
	item, _ := pending[0].(map[string]any)
	if item["task_ref"] != "workflow:wf-2" || item["all_clear"] != false {
		t.Fatalf("待处理任务不符: %v", item)
	}
	if item["last_block"] == nil {
		t.Fatal("待处理任务应带最近阻断记录")
	}
}
