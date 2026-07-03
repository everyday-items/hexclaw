package autonomy

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

// ── hex-test 审计取证 2026-07-02 ──

// HX-2（RED 取证）：权限闸在并发工具调用上同步触发 Record，
// DecisionStore 内部写计数必须并发安全——engine 的 -race 门只测过
// captureRecorder 桩，真实 store 的并发路径此前未挂 -race。
func TestDecisionStoreConcurrentRecordRaceFree(t *testing.T) {
	db := setupTestDB(t)
	s := NewDecisionStore(db)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				_ = s.Record(ctx, Decision{
					Source: "webhook", TaskRef: fmt.Sprintf("webhook:w-%d", g),
					Tool: "code_exec", Capability: "exec_sandboxed",
					Profile: "function_first", Decision: "allow", Via: "matrix",
				})
			}
		}(g)
	}
	wg.Wait()

	all, err := s.List(ctx, DecisionFilter{Limit: 500})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("并发写入后应有记录")
	}
}

// AP-005（状态累积·真文件 DB）：决策日志是无人值守长期累积面，
// 淘汰上限必须真的生效且跨重启仍有界——:memory: 测不出 373MB 类事故。
func TestDecisionStorePruneBoundedAcrossRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("状态累积测试跳过 -short")
	}
	dbPath := filepath.Join(t.TempDir(), "autonomy.db")
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		db.SetMaxOpenConns(1)
		return db
	}

	ctx := context.Background()
	db := open()
	s := NewDecisionStore(db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// 超过保留上限，触发多轮淘汰
	for i := 0; i < decisionRetainRows+400; i++ {
		if err := s.Record(ctx, Decision{
			Source: "cron", TaskRef: "cron:c-1", Tool: "search",
			Capability: "read", Profile: "function_first", Decision: "allow", Via: "matrix",
		}); err != nil {
			t.Fatalf("Record #%d: %v", i, err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM autonomy_decisions`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count > decisionRetainRows {
		t.Fatalf("淘汰未生效：%d 条 > 上限 %d", count, decisionRetainRows)
	}
	_ = db.Close()

	// 重启（新实例）继续写：仍有界（计数器不跨进程，淘汰不能依赖单次进程内状态）
	db2 := open()
	defer db2.Close()
	s2 := NewDecisionStore(db2)
	if err := s2.Init(ctx); err != nil {
		t.Fatalf("Init(重启): %v", err)
	}
	for i := 0; i < 600; i++ {
		if err := s2.Record(ctx, Decision{
			Source: "cron", TaskRef: "cron:c-1", Tool: "search",
			Capability: "read", Profile: "function_first", Decision: "allow", Via: "matrix",
		}); err != nil {
			t.Fatalf("Record(重启) #%d: %v", i, err)
		}
	}
	if err := db2.QueryRow(`SELECT COUNT(*) FROM autonomy_decisions`).Scan(&count); err != nil {
		t.Fatalf("count(重启): %v", err)
	}
	if count > decisionRetainRows {
		t.Fatalf("重启后淘汰失效：%d 条 > 上限 %d", count, decisionRetainRows)
	}
}

// FS-2（RED 取证）：含大写字母的连接器工具名，授权后运行时必须命中——
// 授权条目小写归一 vs 工具名大小写敏感匹配曾致「授权后仍被拦、徽章永不清」。
func TestGrantAllowsCaseSensitiveConnectorTool(t *testing.T) {
	db := setupTestDB(t)
	s := NewGrantStore(db)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := s.Create(ctx, Grant{
		TaskRef: "workflow:wf-1", Source: "workflow",
		Entries: []string{"createIssue", "GitLab.MR.Create"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 原始大小写工具名必须命中
	if !s.GrantAllows("workflow", "workflow:wf-1", "createIssue") {
		t.Fatal("含大写字母的连接器工具授权后应命中（曾因小写归一永假）")
	}
	if !s.GrantAllows("workflow", "workflow:wf-1", "GitLab.MR.Create") {
		t.Fatal("含大写的点分工具名授权后应命中")
	}
	// 大小写不同的工具名不应误命中（大小写敏感）
	if s.GrantAllows("workflow", "workflow:wf-1", "createissue") {
		t.Fatal("小写变体不应命中大写授权（工具名大小写敏感）")
	}
	// 类别名授权仍大小写不敏感
	if _, err := s.Create(ctx, Grant{TaskRef: "cron:c-1", Entries: []string{"Publish"}}); err != nil {
		t.Fatalf("Create category: %v", err)
	}
	if !s.GrantAllows("cron", "cron:c-1", "publish_wechat") {
		t.Fatal("类别名（大小写不敏感）授权应展开到类别内工具")
	}
}
