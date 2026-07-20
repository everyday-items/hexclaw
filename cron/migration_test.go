package cron

import (
	"context"
	"testing"
	"time"
)

// TestMigration_QuarantinesLegacyJobsAndLogsWarn 验证无损 migration：
//   - 旧库（含 prompt 列，无 spec_json）启动时被自动加上新列
//   - 无 spec_json 的旧 row 保留并暂停（不自动编译，提示用户重建）
func TestMigration_QuarantinesLegacyJobsAndLogsWarn(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// 构造 v1 schema：含 prompt 列，无 spec_json / source_prompt
	if _, err := db.ExecContext(ctx, `CREATE TABLE cron_jobs (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		type TEXT NOT NULL DEFAULT 'cron',
		schedule TEXT NOT NULL,
		prompt TEXT NOT NULL DEFAULT '',
		user_id TEXT NOT NULL,
		platform TEXT DEFAULT '',
		chat_id TEXT DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active',
		last_run_at DATETIME,
		next_run_at DATETIME NOT NULL,
		run_count INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("CREATE legacy table: %v", err)
	}

	// 插入 2 条 v1 旧任务
	for _, id := range []string{"old-1", "old-2"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO cron_jobs (id, name, type, schedule, prompt, user_id, next_run_at)
			 VALUES (?, ?, 'cron', '@daily', '老 prompt', 'u1', ?)`,
			id, "旧任务-"+id, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("INSERT %s: %v", id, err)
		}
	}

	// Scheduler.Init 应：
	//   1. ALTER TABLE 加 spec_json / source_prompt 列（旧库兼容）
	//   2. detectAndCleanupLegacyJobs 暂停 2 条旧任务且保留审计证据
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// 验证旧 row 被保留并暂停，不进入活跃内存调度表。
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs`).Scan(&count); err != nil {
		t.Fatalf("COUNT: %v", err)
	}
	if count != 2 {
		t.Errorf("Init 后应保留 2 条 v1 旧任务，实际 %d 行", count)
	}
	var paused int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs WHERE status='paused'`).Scan(&paused); err != nil {
		t.Fatal(err)
	}
	if paused != 2 {
		t.Errorf("v1 旧任务暂停数=%d，期望 2", paused)
	}
	if len(s.jobs) != 0 {
		t.Errorf("内存 jobs map 应为空，实际 %d", len(s.jobs))
	}

	// 验证新列已加（写入一条带 spec_json 的不应再被清理）
	if _, err := db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, name, type, schedule, spec_json, source_prompt, user_id, next_run_at)
		 VALUES (?, ?, 'cron', '@daily', '{"runtime":"python3","script":"print(1)"}', '示例', 'u1', ?)`,
		"new-1", "新任务", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("INSERT v2 row: %v", err)
	}

	// 再次启动 — v2 任务应被保留
	s2 := newTestScheduler(t, db)
	if err := s2.Init(ctx); err != nil {
		t.Fatalf("Init2: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs`).Scan(&count); err != nil {
		t.Fatalf("COUNT2: %v", err)
	}
	if count != 3 {
		t.Errorf("2 条隔离旧任务 + 1 条 v2 任务应全部保留，实际 %d 行", count)
	}
	if len(s2.jobs) != 1 || s2.jobs["new-1"] == nil {
		t.Errorf("仅 v2 活跃任务应加载，内存 jobs=%v", s2.jobs)
	}
}

// TestMigration_FreshDB_NoLegacyClean 新建库（无遗留）不应触发清理日志风暴。
func TestMigration_FreshDB_NoLegacyClean(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// 新库 Init 后 cron_jobs 应为空且能正常 AddJob
	job := &Job{
		Name:         "测试",
		Schedule:     "@hourly",
		SourcePrompt: "测试 source prompt",
		Spec: &JobSpec{
			Runtime: "python3",
			Script:  "print('{\"status\":\"success\"}')",
		},
		UserID: "u1",
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if job.ID == "" {
		t.Fatal("ID 未生成")
	}

	// 重启读回应有 1 条
	s2 := newTestScheduler(t, db)
	if err := s2.Init(ctx); err != nil {
		t.Fatalf("Init2: %v", err)
	}
	if len(s2.jobs) != 1 {
		t.Errorf("重启后应加载 1 条任务，实际 %d", len(s2.jobs))
	}
	loaded := s2.jobs[job.ID]
	if loaded == nil {
		t.Fatal("任务未加载")
	}
	if loaded.Spec == nil || loaded.Spec.Script != "print('{\"status\":\"success\"}')" {
		t.Errorf("Spec 反序列化失败: %+v", loaded.Spec)
	}
	if loaded.SourcePrompt != "测试 source prompt" {
		t.Errorf("SourcePrompt 丢失: %q", loaded.SourcePrompt)
	}
}
