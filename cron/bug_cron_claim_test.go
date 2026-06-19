package cron

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// TestBugCronClaim_OnlyOneReplicaClaims 回归锁定 [BUG-CRON-DOUBLERUN]：
// 多副本部署时，同一到期刻度的 job 只能被一个副本领取执行——此前仅有进程内
// sync.Map 去重、无 DB 级原子领取，多 Pod 会双跑。
//
// 用两个 *sql.DB（两个"副本"）连同一 SQLite 文件库，并发领取同一 job 同一刻度，
// 断言恰好一个 claimJob 返回 true（RowsAffected==1 语义，与 MySQL 一致）。
func TestBugCronClaim_OnlyOneReplicaClaims(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "cron.db") + "?cache=shared"
	dbA, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer dbA.Close()
	dbB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer dbB.Close()

	exec := NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir())
	sA := NewScheduler(dbA, nil, exec)
	sB := NewScheduler(dbB, nil, exec)

	ctx := context.Background()
	if err := sA.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	job := &Job{Name: "claim-job", Schedule: "@hourly", SourcePrompt: "x", UserID: "u1", Spec: minimalSpec()}
	if err := sA.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// 两副本并发领取同一 job 的同一到期刻度
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, s := range []*Scheduler{sA, sB} {
		wg.Add(1)
		go func(sch *Scheduler) {
			defer wg.Done()
			if sch.claimJob(job) {
				wins.Add(1)
			}
		}(s)
	}
	wg.Wait()

	if wins.Load() != 1 {
		t.Errorf("同一到期刻度跨副本应恰好 1 个领取成功, got %d", wins.Load())
	}
}

// TestCronClaim_NoDBRowFailOpen 验证 DB 中无此行时 fail-open 放行（保持纯内存/既有行为不破）。
func TestCronClaim_NoDBRowFailOpen(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 一个未经 AddJob 持久化的 job（DB 无行）
	job := &Job{ID: "ghost", Name: "ghost", Spec: minimalSpec()}
	if !s.claimJob(job) {
		t.Error("DB 无此行时应 fail-open 放行，避免破坏纯内存调度/测试")
	}
}

// TestCronClaim_NilDBFailOpen 验证无 DB（纯内存调度）时放行。
func TestCronClaim_NilDBFailOpen(t *testing.T) {
	s := &Scheduler{}
	if !s.claimJob(&Job{ID: "x"}) {
		t.Error("无 DB 时应 fail-open 放行")
	}
}
