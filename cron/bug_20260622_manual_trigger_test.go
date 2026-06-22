package cron

import (
	"context"
	"testing"
	"time"
)

// TestBug20260622_ManualTriggerAlwaysRuns 回归守卫：手动「立即运行」(TriggerJob) 连续两次
// 都必须真正执行并各落一条历史，不被 claimJob 的「同到期刻度跨副本去重」误吞。
//
// 背景：审计曾怀疑 TriggerJob 复用 claimJob 会让第二次手动触发命中
// last_run_at >= NextRunAt 而静默 no-op。本测试在真实 DB 上证伪了该怀疑
// （executeJob 完成后 NextRunAt 已推进，第二次 claim 仍成功）——保留为防回归锁。
// 期望：两次触发 → 2 条历史。
func TestBug20260622_ManualTriggerAlwaysRuns(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := newTestScheduler(t, db)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// @hourly：NextRunAt 落在未来整点，初始 last_run_at 为 NULL。
	job := &Job{Name: "manual-job", Schedule: "@hourly", SourcePrompt: "x", UserID: "u1", Spec: minimalSpec()}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	waitHistory := func(want int) int {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		var n int
		for time.Now().Before(deadline) {
			h, err := s.GetJobHistory(ctx, job.ID)
			if err != nil {
				t.Fatalf("GetJobHistory: %v", err)
			}
			n = len(h)
			if n >= want {
				return n
			}
			time.Sleep(50 * time.Millisecond)
		}
		return n
	}

	// 第一次手动触发
	if err := s.TriggerJob(ctx, job.ID); err != nil {
		t.Fatalf("TriggerJob #1: %v", err)
	}
	if got := waitHistory(1); got != 1 {
		t.Fatalf("第一次触发后应有 1 条历史, got %d", got)
	}

	// 第二次手动触发（在下一个调度刻度之前）
	if err := s.TriggerJob(ctx, job.ID); err != nil {
		t.Fatalf("TriggerJob #2: %v", err)
	}
	got := waitHistory(2)
	if got != 2 {
		t.Errorf("第二次手动触发应再落一条历史 (共 2), got %d —— 手动触发被 claimJob 静默吞掉", got)
	}
}
