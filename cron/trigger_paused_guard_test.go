package cron

import (
	"context"
	"strings"
	"testing"
)

// GO-6（RED 取证）：暂停态任务不接受手动 run / webhook TriggerJob——
// 尊重「审批未决先冻结、暂停期间根本不 dispatch」的设计。
func TestTriggerJobRejectsPaused(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	job, err := s.AddJobFromScript(ctx, AddJobRequest{
		Name: "待授权任务", Schedule: "@daily", UserID: "u", Paused: true,
	}, RuntimeStarlark, `emit({"status": "success"})`)
	if err != nil {
		t.Fatalf("AddJobFromScript: %v", err)
	}
	if job.Status != StatusPaused {
		t.Fatalf("应以暂停态创建，得到 %q", job.Status)
	}
	// 手动 run / webhook 都走 TriggerJob，暂停态必须被拦
	if err := s.TriggerJob(ctx, job.ID); err == nil {
		t.Fatal("暂停态任务不应被 TriggerJob 触发（绕过了冻结设计）")
	} else if !strings.Contains(err.Error(), "暂停") {
		t.Fatalf("拦截错误应说明已暂停，得到: %v", err)
	}
	// resume 后可正常触发
	if err := s.ResumeJob(ctx, job.ID); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	if err := s.TriggerJob(ctx, job.ID); err != nil {
		t.Fatalf("resume 后应可触发: %v", err)
	}
}
