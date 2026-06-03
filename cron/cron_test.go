package cron

import (
	"context"
	"database/sql"
	"errors"
	"os/exec"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var errStubCompile = errors.New("stub compile error")

// stubCompiler 实现 JobSpecCompiler，用于测试不实际调 LLM 的 Compile 路径。
type stubCompiler struct {
	ret  *JobSpec
	err  error
	last struct {
		Prompt string
		Hints  CompileHints
	}
}

func (s *stubCompiler) Compile(_ context.Context, prompt string, hints CompileHints) (*JobSpec, error) {
	s.last.Prompt = prompt
	s.last.Hints = hints
	if s.err != nil {
		return nil, s.err
	}
	return s.ret, nil
}

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	return db
}

// newTestScheduler 构造一个无 compiler / 自定义 ScriptExecutor 的 Scheduler。
// 用于测试 prebuilt-Spec 路径（不走 LLM 编译）。
func newTestScheduler(t *testing.T, db *sql.DB) *Scheduler {
	t.Helper()
	exec := NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir())
	return NewScheduler(db, nil, exec)
}

// minimalSpec 一个合规的最小 JobSpec — 仅用于测试 AddJob 等不实际执行的路径。
func minimalSpec() *JobSpec {
	return &JobSpec{
		Runtime:    "python3",
		Script:     `import json; print(json.dumps({"status":"success"}))`,
		TimeoutSec: 10,
	}
}

// TestScheduler_AddAndListJobs 测试添加和列出任务
func TestScheduler_AddAndListJobs(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	job := &Job{
		Name:         "每日摘要",
		Type:         JobTypeCron,
		Schedule:     "@daily",
		SourcePrompt: "总结今天的待办事项",
		Spec:         minimalSpec(),
		UserID:       "user-1",
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

func TestScheduler_AddJob_RejectsMissingSpec(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	s := newTestScheduler(t, db)
	_ = s.Init(ctx)

	err := s.AddJob(ctx, &Job{Name: "x", Schedule: "@hourly", SourcePrompt: "y", UserID: "u1"})
	if err == nil {
		t.Fatal("无 Spec 的 Job 应被拒收")
	}
}

func TestScheduler_RemoveJob(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	s := newTestScheduler(t, db)
	_ = s.Init(ctx)

	job := &Job{Name: "测试任务", Schedule: "@hourly", SourcePrompt: "测试", Spec: minimalSpec(), UserID: "user-1"}
	_ = s.AddJob(ctx, job)

	if err := s.RemoveJob(ctx, job.ID); err != nil {
		t.Fatalf("删除任务失败: %v", err)
	}
	jobs, _ := s.ListJobs(ctx, "user-1")
	if len(jobs) != 0 {
		t.Fatalf("删除后应无任务，实际 %d 个", len(jobs))
	}
}

func TestScheduler_PauseResumeJob(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	s := newTestScheduler(t, db)
	_ = s.Init(ctx)

	job := &Job{Name: "测试任务", Schedule: "@hourly", SourcePrompt: "测试", Spec: minimalSpec(), UserID: "user-1"}
	_ = s.AddJob(ctx, job)

	if err := s.PauseJob(ctx, job.ID); err != nil {
		t.Fatalf("暂停: %v", err)
	}
	s.mu.RLock()
	if s.jobs[job.ID].Status != StatusPaused {
		t.Error("任务应为暂停状态")
	}
	s.mu.RUnlock()

	if err := s.ResumeJob(ctx, job.ID); err != nil {
		t.Fatalf("恢复: %v", err)
	}
	s.mu.RLock()
	if s.jobs[job.ID].Status != StatusActive {
		t.Error("任务应为活跃状态")
	}
	s.mu.RUnlock()
}

// TestScheduler_ExecuteJobInvokesScriptExecutor v2：tick 触发后 ScriptExecutor.Run
// 真实跑 Python 沙箱，history 写入完整 stdout/exit_code/data。
func TestScheduler_ExecuteJobInvokesScriptExecutor(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("跳过：本机无 python3")
	}
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s := newTestScheduler(t, db)
	_ = s.Init(ctx)

	job := &Job{
		Name:     "立即执行",
		Type:     JobTypeCron,
		Schedule: "@every 1s",
		Spec: &JobSpec{
			Runtime:    "python3",
			Script:     `import json; print(json.dumps({"status":"success","data":42}))`,
			TimeoutSec: 10,
		},
		SourcePrompt: "测试",
		UserID:       "user-1",
		Status:       StatusActive,
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// 手动设置 NextRunAt 为过去
	s.mu.Lock()
	s.jobs[job.ID].NextRunAt = time.Now().Add(-time.Second)
	s.mu.Unlock()

	s.Start(ctx)
	defer s.Stop()

	// 等待 tick 触发 + 沙箱跑完
	time.Sleep(3 * time.Second)

	history, err := s.GetJobHistory(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJobHistory: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("应至少有 1 条 history")
	}
	h := history[0]
	if h.Status != "success" {
		t.Errorf("Status: %s err=%s stderr=%q", h.Status, h.Error, h.Stderr)
	}
	if h.Data != float64(42) {
		t.Errorf("Data: %v (%T)", h.Data, h.Data)
	}
}

// TestScheduler_AddJobFromPrompt_CompilesAndPersists 验证 compiler 注入路径。
func TestScheduler_AddJobFromPrompt_CompilesAndPersists(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	stubSpec := &JobSpec{Runtime: "python3", Script: `import json; print(json.dumps({"status":"success"}))`, TimeoutSec: 60}
	c := &stubCompiler{ret: stubSpec}
	s := NewScheduler(db, c,
		NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)

	job, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name: "采集", Schedule: "@daily", Prompt: "每天采微博", UserID: "u1",
		AvailableSkills: []string{"web-search"},
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt: %v", err)
	}
	if job.SourcePrompt != "每天采微博" {
		t.Errorf("SourcePrompt: %q", job.SourcePrompt)
	}
	if job.Spec == nil || job.Spec.Script == "" {
		t.Error("Spec 未写入")
	}
	if c.last.Prompt != "每天采微博" || len(c.last.Hints.AvailableSkills) != 1 {
		t.Errorf("compiler 入参未透传: %+v", c.last)
	}
}

func TestScheduler_AddJobFromPrompt_CompileErrorBlocksInsert(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	c := &stubCompiler{err: errStubCompile}
	s := NewScheduler(db, c,
		NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)

	_, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name: "x", Schedule: "@daily", Prompt: "p", UserID: "u1",
	})
	if err == nil {
		t.Fatal("compile 失败应阻断入库")
	}
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs`).Scan(&n)
	if n != 0 {
		t.Errorf("失败任务不应入库，实际 %d 行", n)
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
