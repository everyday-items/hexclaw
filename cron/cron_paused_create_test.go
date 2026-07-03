package cron

import (
	"context"
	"testing"
)

// 初始暂停态创建：审批未决时任务意图先冻结（入库但不入调度），授权后 resume。
func TestAddJobFromPromptPausedInitialStatus(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	exec := NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir())
	s := NewScheduler(db, &stubCompiler{ret: minimalSpec()}, exec)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// 脚本编译路径
	job, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name:     "需要审批的任务",
		Schedule: "@daily",
		Prompt:   "抓取页面并写入外部系统",
		UserID:   "u",
		Paused:   true,
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt: %v", err)
	}
	if job.Status != StatusPaused {
		t.Fatalf("Paused=true 应以暂停态创建，得到 %q", job.Status)
	}

	// agent 模式路径（Continuous 强制 agent 分支）
	agentJob, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name:       "持续任务待授权",
		Schedule:   "0 9 * * *",
		Prompt:     "持续推进长目标",
		UserID:     "u",
		Continuous: true,
		Paused:     true,
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt(agent): %v", err)
	}
	if agentJob.Status != StatusPaused {
		t.Fatalf("agent 分支 Paused=true 应暂停态，得到 %q", agentJob.Status)
	}

	// 默认不受影响：不带 Paused 仍为 active
	activeJob, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name:     "常规任务",
		Schedule: "@daily",
		Prompt:   "汇总日报",
		UserID:   "u",
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt(默认): %v", err)
	}
	if activeJob.Status != StatusActive {
		t.Fatalf("默认应 active，得到 %q", activeJob.Status)
	}

	// 暂停任务授权后 resume → active
	if err := s.ResumeJob(ctx, job.ID); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	got, ok := s.GetJob(ctx, job.ID)
	if !ok || got.Status != StatusActive {
		t.Fatalf("resume 后应 active：%+v", got)
	}
}

// 脚本直采路径同样尊重 Paused。
func TestAddJobFromScriptPausedInitialStatus(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	job, err := s.AddJobFromScript(ctx, AddJobRequest{
		Name:     "脚本任务待授权",
		Schedule: "@daily",
		UserID:   "u",
		Paused:   true,
	}, RuntimeStarlark, `emit({"status": "success"})`)
	if err != nil {
		t.Fatalf("AddJobFromScript: %v", err)
	}
	if job.Status != StatusPaused {
		t.Fatalf("脚本路径 Paused=true 应暂停态，得到 %q", job.Status)
	}
}
