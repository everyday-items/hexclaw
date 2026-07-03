package autonomy

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	// :memory: 每连接独立内存库；限单连接保证建表与查询命中同一库。
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// --- DecisionStore ---

func TestDecisionStoreRecordAndList(t *testing.T) {
	db := setupTestDB(t)
	s := NewDecisionStore(db)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	base := time.Now().Add(-time.Minute)
	records := []Decision{
		{Source: "webhook", TaskRef: "webhook:wh-1", Tool: "github.issues.write_label", Decision: "pending", Via: "matrix", Profile: "function_first", At: base},
		{Source: "webhook", TaskRef: "webhook:wh-1", Tool: "search", Capability: "read", Decision: "allow", Via: "matrix", Profile: "function_first", At: base.Add(time.Second)},
		{Source: "cron", TaskRef: "cron:c-1", Tool: "code_exec", Capability: "exec_sandboxed", Decision: "allow", Via: "matrix", Profile: "function_first", At: base.Add(2 * time.Second)},
	}
	for _, d := range records {
		if err := s.Record(ctx, d); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	all, err := s.List(ctx, DecisionFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("期望 3 条，得到 %d", len(all))
	}
	if all[0].Tool != "code_exec" {
		t.Fatalf("应按时间倒序，首条 tool=%q", all[0].Tool)
	}

	pendings, err := s.List(ctx, DecisionFilter{Decision: "pending"})
	if err != nil || len(pendings) != 1 || pendings[0].Tool != "github.issues.write_label" {
		t.Fatalf("按 decision 过滤失败: %v %+v", err, pendings)
	}
	bySource, err := s.List(ctx, DecisionFilter{Source: "cron"})
	if err != nil || len(bySource) != 1 {
		t.Fatalf("按 source 过滤失败: %v %+v", err, bySource)
	}
	byTask, err := s.List(ctx, DecisionFilter{TaskRef: "webhook:wh-1"})
	if err != nil || len(byTask) != 2 {
		t.Fatalf("按 task_ref 过滤失败: %v %+v", err, byTask)
	}
}

func TestDecisionStorePendingTaskRefs(t *testing.T) {
	db := setupTestDB(t)
	s := NewDecisionStore(db)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	base := time.Now().Add(-time.Minute)
	// 任务 A：write_label 先 pending，随后同工具 allow（已解决，不应再报 pending）。
	mustRecord(t, s, Decision{Source: "webhook", TaskRef: "webhook:a", Tool: "github.issues.write_label", Decision: "pending", At: base})
	mustRecord(t, s, Decision{Source: "webhook", TaskRef: "webhook:a", Tool: "github.issues.write_label", Decision: "allow", Via: "task_grant", At: base.Add(time.Second)})
	// 任务 B：write_label pending 后，另一个工具 send_message allow —— 不应掩盖 write_label 的阻断。
	mustRecord(t, s, Decision{Source: "webhook", TaskRef: "webhook:b", Tool: "github.issues.write_label", Decision: "pending", At: base.Add(2 * time.Second)})
	mustRecord(t, s, Decision{Source: "webhook", TaskRef: "webhook:b", Tool: "send_message", Decision: "allow", At: base.Add(3 * time.Second)})

	pending, err := s.PendingTaskRefs(ctx)
	if err != nil {
		t.Fatalf("PendingTaskRefs: %v", err)
	}
	if _, ok := pending["webhook:a"]; ok {
		t.Fatalf("任务 A 已被 grant 解决，不应仍在 pending：%+v", pending)
	}
	got, ok := pending["webhook:b"]
	if !ok {
		t.Fatalf("任务 B 的工具级阻断被同任务其他工具 allow 掩盖了：%+v", pending)
	}
	if got.Tool != "github.issues.write_label" {
		t.Fatalf("任务 B 代表记录应为被拦工具，得到 %q", got.Tool)
	}
}

func mustRecord(t *testing.T, s *DecisionStore, d Decision) {
	t.Helper()
	if err := s.Record(context.Background(), d); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

// --- GrantStore ---

func TestGrantStoreCreateAllowsRevoke(t *testing.T) {
	db := setupTestDB(t)
	s := NewGrantStore(db)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	g, err := s.Create(ctx, Grant{
		TaskRef: "webhook:wh-1",
		Source:  "webhook",
		Entries: []string{"github.issues.write_label", "publish"},
		Note:    "仅本任务写标签",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 精确工具名命中
	if !s.GrantAllows("webhook", "webhook:wh-1", "github.issues.write_label") {
		t.Fatal("精确工具名授权未命中")
	}
	// 类别名 entry 覆盖类别内 glob 工具（publish -> publish_*）
	if !s.GrantAllows("webhook", "webhook:wh-1", "publish_wechat") {
		t.Fatal("类别名授权未展开到类别内工具")
	}
	// 其他任务不受影响
	if s.GrantAllows("webhook", "webhook:wh-2", "github.issues.write_label") {
		t.Fatal("授权泄漏到其他任务")
	}
	// source 限定：cron 来源不应命中 webhook 授权
	if s.GrantAllows("cron", "webhook:wh-1", "github.issues.write_label") {
		t.Fatal("source 限定失效")
	}
	// 未授权工具不放行
	if s.GrantAllows("webhook", "webhook:wh-1", "shell") {
		t.Fatal("未授权工具被放行")
	}

	// Revoke 后立刻失效
	if err := s.Revoke(ctx, g.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if s.GrantAllows("webhook", "webhook:wh-1", "github.issues.write_label") {
		t.Fatal("撤销后仍放行")
	}
	if err := s.Revoke(ctx, g.ID); err == nil {
		t.Fatal("重复撤销应报错")
	}
}

func TestGrantStoreRevokeByTaskAndReload(t *testing.T) {
	db := setupTestDB(t)
	s := NewGrantStore(db)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := s.Create(ctx, Grant{TaskRef: "cron:c-1", Entries: []string{"publish"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(ctx, Grant{TaskRef: "cron:c-1", Entries: []string{"exec_host"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// source 为空 = 不限来源
	if !s.GrantAllows("cron", "cron:c-1", "shell") || !s.GrantAllows("webhook", "cron:c-1", "shell") {
		t.Fatal("空 source 授权应对任意来源生效")
	}

	if err := s.RevokeByTask(ctx, "cron:c-1"); err != nil {
		t.Fatalf("RevokeByTask: %v", err)
	}
	if len(s.ListActive("cron:c-1")) != 0 {
		t.Fatal("任务删除回收后仍有活跃授权")
	}

	// 持久化验证：新实例 Init 重载后不应恢复已撤销授权
	s2 := NewGrantStore(db)
	if err := s2.Init(ctx); err != nil {
		t.Fatalf("Init(重载): %v", err)
	}
	if s2.GrantAllows("cron", "cron:c-1", "publish_wechat") {
		t.Fatal("重载后出现已撤销授权")
	}
}

func TestGrantStoreRejectsWildcardAndEmpty(t *testing.T) {
	db := setupTestDB(t)
	s := NewGrantStore(db)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := s.Create(ctx, Grant{TaskRef: "cron:c-1", Entries: []string{"*"}}); err == nil {
		t.Fatal("全放行 * 不应被任务级授权接受（那是全功能 Profile 的职责）")
	}
	if _, err := s.Create(ctx, Grant{TaskRef: "", Entries: []string{"read"}}); err == nil {
		t.Fatal("空 task_ref 应被拒绝")
	}
}

// --- Preflight ---

func functionFirstPolicy() *engine.SystemDispatchPolicy {
	return engine.NewSystemDispatchPolicyFromConfig(config.AutonomyConfig{Profile: "function_first"})
}

func TestPreflightAllClearForRoutineCron(t *testing.T) {
	res := Preflight(functionFirstPolicy(), nil, PreflightRequest{
		Source:  "cron",
		Prompt:  "汇总昨日产品指标，生成日报并发送到飞书群",
		Deliver: true,
	})
	if !res.AllClear {
		t.Fatalf("常规 cron 任务应全绿，needs_decision=%v", res.NeedsDecision)
	}
	if res.Profile != "function_first" {
		t.Fatalf("profile 应为 function_first，得到 %q", res.Profile)
	}
	// 预估应含 read/exec_sandboxed/delivery
	want := map[string]bool{"read": true, "exec_sandboxed": true, "delivery": true}
	got := map[string]bool{}
	for _, c := range res.Estimated {
		got[c] = true
	}
	for c := range want {
		if !got[c] {
			t.Fatalf("预估缺少 %s：%v", c, res.Estimated)
		}
	}
}

func TestPreflightPublishNeedsDecision(t *testing.T) {
	res := Preflight(functionFirstPolicy(), nil, PreflightRequest{
		Source: "workflow",
		Prompt: "每周五生成摘要并发布到公众号草稿箱",
		Tools:  []string{"code_exec", "publish_wechat", "send_message"},
	})
	if res.AllClear {
		t.Fatal("命中发布内容不应全绿")
	}
	found := false
	for _, c := range res.NeedsDecision {
		if c == "publish" {
			found = true
		}
	}
	if !found {
		t.Fatalf("needs_decision 应含 publish：%v", res.NeedsDecision)
	}
}

func TestPreflightGrantElevatesCategory(t *testing.T) {
	db := setupTestDB(t)
	grants := NewGrantStore(db)
	ctx := context.Background()
	if err := grants.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := grants.Create(ctx, Grant{TaskRef: "workflow:wf-1", Source: "workflow", Entries: []string{"publish"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	res := Preflight(functionFirstPolicy(), grants, PreflightRequest{
		Source:  "workflow",
		TaskRef: "workflow:wf-1",
		Tools:   []string{"publish_wechat"},
	})
	if !res.AllClear {
		t.Fatalf("已有任务级授权应全绿：needs_decision=%v", res.NeedsDecision)
	}
	for _, c := range res.Capabilities {
		if c.Category == "publish" && c.State != "granted" {
			t.Fatalf("publish 类别应为 granted，得到 %q", c.State)
		}
	}
}

func TestPreflightUncategorizedConnectorTool(t *testing.T) {
	// MCP/连接器工具不属于内置类别：function_first 下应转审批；
	// 有任务级 grant 后为 granted。
	res := Preflight(functionFirstPolicy(), nil, PreflightRequest{
		Source: "webhook",
		Tools:  []string{"github.issues.write_label"},
	})
	if res.AllClear {
		t.Fatal("未归类连接器工具不应全绿")
	}
	if len(res.ExtraTools) != 1 || res.ExtraTools[0].State != "approval" {
		t.Fatalf("连接器工具应出现在 extra_tools 且转审批：%+v", res.ExtraTools)
	}

	db := setupTestDB(t)
	grants := NewGrantStore(db)
	if err := grants.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := grants.Create(context.Background(), Grant{TaskRef: "webhook:wh-1", Entries: []string{"github.issues.write_label"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	res2 := Preflight(functionFirstPolicy(), grants, PreflightRequest{
		Source:  "webhook",
		TaskRef: "webhook:wh-1",
		Tools:   []string{"github.issues.write_label"},
	})
	if !res2.AllClear || res2.ExtraTools[0].State != "granted" {
		t.Fatalf("grant 后连接器工具应 granted 且全绿：%+v", res2)
	}
}

func TestPreflightHostExecApprovalForExternalSources(t *testing.T) {
	// exec 拆两级信任：外部源沙箱执行 auto、宿主执行 approval；
	// 内部编排源（workflow）两者都 auto。
	external := Preflight(functionFirstPolicy(), nil, PreflightRequest{Source: "webhook"})
	states := map[string]string{}
	for _, c := range external.Capabilities {
		states[c.Category] = c.State
	}
	if states["exec_sandboxed"] != "auto" || states["exec_host"] != "approval" {
		t.Fatalf("webhook 源 exec 拆分判定错：sandboxed=%s host=%s", states["exec_sandboxed"], states["exec_host"])
	}

	internal := Preflight(functionFirstPolicy(), nil, PreflightRequest{Source: "workflow"})
	states = map[string]string{}
	for _, c := range internal.Capabilities {
		states[c.Category] = c.State
	}
	if states["exec_host"] != "auto" {
		t.Fatalf("workflow 源宿主执行应 auto，得到 %s", states["exec_host"])
	}
}

func TestMatrixSnapshotStableShape(t *testing.T) {
	view := MatrixSnapshot(functionFirstPolicy())
	if view.Profile != "function_first" {
		t.Fatalf("profile=%q", view.Profile)
	}
	if len(view.Categories) != 11 {
		t.Fatalf("类别数应为 11，得到 %d", len(view.Categories))
	}
	if len(view.Rows) != 6 {
		t.Fatalf("来源行数应为 6，得到 %d", len(view.Rows))
	}
	for _, row := range view.Rows {
		if len(row.Cells) != len(view.Categories) {
			t.Fatalf("来源 %s 单元数 %d != 类别数 %d", row.Source, len(row.Cells), len(view.Categories))
		}
	}
	// solve 行全部 approval（solve 默认空矩阵，凭 grant 放行）
	for _, row := range view.Rows {
		if row.Source != "solve" {
			continue
		}
		for _, cell := range row.Cells {
			if cell.State != "approval" {
				t.Fatalf("solve 行应全 approval：%s=%s", cell.Category, cell.State)
			}
		}
	}
}
