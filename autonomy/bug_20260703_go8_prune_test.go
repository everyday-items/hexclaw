package autonomy

// GO-8（BUG-20260703）：决策审计淘汰的两处收口。
//
//  1. prune 子查询 `ORDER BY at DESC LIMIT n` 缺 `, id DESC` tie-break——同秒批量
//     写入（并发工具调用正是这种形态）时保留集不确定，同一批记录哪 5000 条活下来
//     取决于扫描顺序；List 已用 `at DESC, id DESC`，淘汰序必须与查询序一致。
//  2. writes 是进程内计数、每 200 次才触发 prune，重启即归零——每次重启写不满
//     200 条的使用形态下表可跨重启无界膨胀。契约：Init 时执行一次 prune 兜底。

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newPruneTestStore(t *testing.T) (*DecisionStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s := NewDecisionStore(db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s, db
}

// bulkInsertSameInstant 批量插入 n 条完全同时刻的记录，id 单调递增（ad-000000…）。
func bulkInsertSameInstant(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	at := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO autonomy_decisions (id, at, source, task_ref, tool, capability, profile, decision, via, reason)
			 VALUES (?, ?, 'cron', 'cron:j1', 'fs_write', '', 'balanced', 'allow', 'matrix', '')`,
			fmt.Sprintf("ad-%06d", i), at); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func countDecisions(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM autonomy_decisions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// UT-PRUNE-001: 同时刻记录淘汰必须确定性保留 id 最大的 retain 条（与 List 排序一致）。
func TestBug20260703_PruneTieBreakDeterministic(t *testing.T) {
	s, db := newPruneTestStore(t)
	total := decisionRetainRows + 100
	bulkInsertSameInstant(t, db, total)

	s.prune(context.Background())

	if n := countDecisions(t, db); n != decisionRetainRows {
		t.Fatalf("prune 后应保留 %d 条，实际 %d", decisionRetainRows, n)
	}
	var minID string
	if err := db.QueryRow(`SELECT MIN(id) FROM autonomy_decisions`).Scan(&minID); err != nil {
		t.Fatalf("min id: %v", err)
	}
	wantMin := fmt.Sprintf("ad-%06d", total-decisionRetainRows)
	if minID != wantMin {
		t.Errorf("[GO-8] 同秒 tie-break 不确定：保留集最小 id=%s，期望 %s（id DESC 前 %d 条）",
			minID, wantMin, decisionRetainRows)
	}
}

// UT-PRUNE-002: 重启后（writes 计数归零）Init 必须兜底修剪超限历史。
func TestBug20260703_InitPrunesBacklogAfterRestart(t *testing.T) {
	_, db := newPruneTestStore(t)
	total := decisionRetainRows + 150
	bulkInsertSameInstant(t, db, total)

	// 模拟重启：同一 db 上新建 store（writes 归零），Init 后不应再超限。
	restarted := NewDecisionStore(db)
	if err := restarted.Init(context.Background()); err != nil {
		t.Fatalf("restart Init: %v", err)
	}

	if n := countDecisions(t, db); n > decisionRetainRows {
		t.Errorf("[GO-8] 重启后 Init 未兜底修剪：残留 %d 条（上限 %d），writes 归零后可跨重启无界膨胀",
			n, decisionRetainRows)
	}
}
