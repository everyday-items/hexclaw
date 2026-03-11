package cron

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	return db
}

// TestScheduler_AddAndListJobs 测试添加和列出任务
func TestScheduler_AddAndListJobs(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	s := NewScheduler(db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	// 添加任务
	job := &Job{
		Name:     "每日摘要",
		Type:     JobTypeCron,
		Schedule: "@daily",
		Prompt:   "总结今天的待办事项",
		UserID:   "user-1",
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("添加任务失败: %v", err)
	}

	if job.ID == "" {
		t.Fatal("任务 ID 不应为空")
	}
	if job.NextRunAt.IsZero() {
		t.Fatal("下次执行时间不应为空")
	}

	// 列出任务
	jobs, err := s.ListJobs(ctx, "user-1")
	if err != nil {
		t.Fatalf("列出任务失败: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("应有 1 个任务，实际 %d 个", len(jobs))
	}
	if jobs[0].Name != "每日摘要" {
		t.Errorf("任务名不匹配: %s", jobs[0].Name)
	}
}

// TestScheduler_RemoveJob 测试删除任务
func TestScheduler_RemoveJob(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	s := NewScheduler(db)
	s.Init(ctx)

	job := &Job{
		Name:     "测试任务",
		Schedule: "@hourly",
		Prompt:   "测试",
		UserID:   "user-1",
	}
	s.AddJob(ctx, job)

	if err := s.RemoveJob(ctx, job.ID); err != nil {
		t.Fatalf("删除任务失败: %v", err)
	}

	jobs, _ := s.ListJobs(ctx, "user-1")
	if len(jobs) != 0 {
		t.Fatalf("删除后应无任务，实际 %d 个", len(jobs))
	}
}

// TestScheduler_PauseResumeJob 测试暂停/恢复任务
func TestScheduler_PauseResumeJob(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	s := NewScheduler(db)
	s.Init(ctx)

	job := &Job{
		Name:     "测试任务",
		Schedule: "@hourly",
		Prompt:   "测试",
		UserID:   "user-1",
	}
	s.AddJob(ctx, job)

	// 暂停
	if err := s.PauseJob(ctx, job.ID); err != nil {
		t.Fatalf("暂停任务失败: %v", err)
	}

	s.mu.RLock()
	if s.jobs[job.ID].Status != StatusPaused {
		t.Error("任务应为暂停状态")
	}
	s.mu.RUnlock()

	// 恢复
	if err := s.ResumeJob(ctx, job.ID); err != nil {
		t.Fatalf("恢复任务失败: %v", err)
	}

	s.mu.RLock()
	if s.jobs[job.ID].Status != StatusActive {
		t.Error("任务应为活跃状态")
	}
	s.mu.RUnlock()
}

// TestScheduler_Execute 测试任务执行
func TestScheduler_Execute(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	s := NewScheduler(db)
	s.Init(ctx)

	// 添加一个立即执行的任务（过去时间）
	job := &Job{
		Name:     "立即执行",
		Type:     JobTypeCron,
		Schedule: "@every 1s",
		Prompt:   "测试执行",
		UserID:   "user-1",
		Status:   StatusActive,
	}
	s.AddJob(ctx, job)

	// 手动设置 NextRunAt 为过去
	s.mu.Lock()
	s.jobs[job.ID].NextRunAt = time.Now().Add(-time.Second)
	s.mu.Unlock()

	var executed atomic.Int32
	s.Start(ctx, func(_ context.Context, j *Job) error {
		executed.Add(1)
		return nil
	})
	defer s.Stop()

	// 等待执行
	time.Sleep(2 * time.Second)

	if executed.Load() == 0 {
		t.Error("任务应该被执行至少一次")
	}
}

// TestNextRunTime 测试下次执行时间计算
func TestNextRunTime(t *testing.T) {
	now := time.Date(2026, 3, 10, 14, 30, 0, 0, time.UTC)

	tests := []struct {
		name     string
		schedule string
		jobType  JobType
		wantErr  bool
	}{
		{"@daily", "@daily", JobTypeCron, false},
		{"@hourly", "@hourly", JobTypeCron, false},
		{"@weekly", "@weekly", JobTypeCron, false},
		{"@every 5m", "@every 5m", JobTypeCron, false},
		{"@every 1h", "@every 1h", JobTypeCron, false},
		{"cron 每天9点", "0 9 * * *", JobTypeCron, false},
		{"cron 每小时", "30 * * * *", JobTypeCron, false},
		{"一次性", "2026-03-15T10:00:00", JobTypeOnce, false},
		{"一次性短格式", "2026-03-15 10:00", JobTypeOnce, false},
		{"无效", "invalid", JobTypeCron, true},
		{"无效间隔", "@every invalid", JobTypeCron, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next, err := nextRunTime(tt.schedule, tt.jobType, now)
			if tt.wantErr {
				if err == nil {
					t.Error("应该返回错误")
				}
				return
			}
			if err != nil {
				t.Fatalf("不应返回错误: %v", err)
			}
			if next.Before(now) && tt.jobType != JobTypeOnce {
				t.Errorf("下次执行时间 %s 不应早于当前时间 %s", next, now)
			}
			t.Logf("schedule=%s next=%s", tt.schedule, next.Format(time.RFC3339))
		})
	}
}
