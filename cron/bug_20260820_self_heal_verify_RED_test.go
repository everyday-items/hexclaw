package cron

import (
	"context"
	"strings"
	"testing"
	"time"
)

// RED: 编译候选写回后必须等待下一次真实执行，不得立即把 healed 当作恢复事实。
func TestBug20260820_SelfHealRequiresVerifiedExecution(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	compiler := &stubCompiler{ret: &JobSpec{
		Runtime:    RuntimeStarlark,
		Script:     "emit({\"status\": \"success\"})",
		TimeoutSec: 10,
	}}
	s := NewScheduler(db, compiler, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	job := &Job{
		Name:         "K12 周练投递",
		Type:         JobTypeCron,
		Schedule:     "@daily",
		UserID:       "u1",
		Status:       StatusActive,
		SourcePrompt: "每周汇总错题并投递到会话",
		Spec:         &JobSpec{Runtime: RuntimeStarlark, Script: "emit({\"status\": \"error\", \"error\": \"401\"})", TimeoutSec: 10},
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	for i := 0; i < selfHealThreshold; i++ {
		_ = s.persistHistory(ctx, job.ID, "error", "", "401", 10,
			time.Now().Add(-time.Duration(selfHealThreshold-i)*time.Second), "", "", 1, nil)
	}

	s.maybeSelfHeal(ctx, job, &RunResult{Status: "error", Error: "401"})

	history, err := s.GetJobHistory(ctx, job.ID)
	if err != nil || len(history) == 0 {
		t.Fatalf("self-heal history missing: err=%v history=%+v", err, history)
	}
	if history[0].Status == "healed" {
		t.Fatalf("compile success alone must not be recorded as healed: %+v", history[0])
	}
	if history[0].Status != "heal_pending" {
		t.Fatalf("expected heal_pending before a real verification run, got %+v", history[0])
	}

	s.executeJob(job)
	history, _ = s.GetJobHistory(ctx, job.ID)
	statuses := make([]string, 0, len(history))
	for _, item := range history {
		statuses = append(statuses, item.Status)
	}
	if !strings.Contains(strings.Join(statuses, ","), "healed") {
		t.Fatalf("successful verification should append healed, statuses=%v", statuses)
	}
}
