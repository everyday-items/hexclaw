package cron

import (
	"context"
	"math/rand"
	"testing"
	"time"
)

// ─── Method 1: invariants / property fuzz ─────────────────────────

// TestInvariant_ClassifyDeterministicNoPanic: the classifier must not panic
// and must be idempotent on random input.
func TestInvariant_ClassifyDeterministicNoPanic(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	corpus := []rune("每天总结分析采集抓取热搜任务提醒 abcXYZ123!@#，。\n\t🦀ئۇيغۇر")
	for i := 0; i < 10000; i++ {
		n := r.Intn(64)
		b := make([]rune, n)
		for j := range b {
			b[j] = corpus[r.Intn(len(corpus))]
		}
		in := string(b)
		first := ClassifyTaskMode(in)
		if second := ClassifyTaskMode(in); second != first {
			t.Fatalf("分类不幂等: %q → %q / %q", in, first, second)
		}
		if first != "" && first != RuntimeAgent {
			t.Fatalf("非法分类值: %q", first)
		}
	}
}

// TestInvariant_ScheduleIntervalNonNegative: interval estimation must not
// panic and must be non-negative for arbitrary schedule input.
func TestInvariant_ScheduleIntervalNonNegative(t *testing.T) {
	inputs := []string{
		"@daily", "@hourly", "@weekly", "@every 5m", "@every 0s", "@every -1h",
		"*/5 * * * *", "0 9 * * *", "", "garbage", "@every", "60 25 * * *",
	}
	for _, in := range inputs {
		if iv := scheduleMinInterval(in); iv < 0 {
			t.Errorf("间隔为负: %q → %v", in, iv)
		}
	}
}

// TestInvariant_HealQuotaNeverExceeded: under any heal sequence the number of
// grants inside the 24h window never exceeds the cap.
func TestInvariant_HealQuotaNeverExceeded(t *testing.T) {
	s := &Scheduler{}
	granted := 0
	for i := 0; i < 100; i++ {
		if s.healQuotaOK("job-x") {
			s.recordHealAttempt("job-x")
			granted++
		}
	}
	if granted > selfHealMaxPerWindow {
		t.Fatalf("配额不变量被破坏: 放行 %d 次 > 上限 %d", granted, selfHealMaxPerWindow)
	}
}

// ─── Method 2: fault injection (no panic, graceful degradation) ───────

// TestChaos_SelfHealSurvivesClosedDB: the self-heal path must not panic after
// the DB is closed.
func TestChaos_SelfHealSurvivesClosedDB(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	s := NewScheduler(db, &stubCompiler{ret: &JobSpec{Runtime: "python3", Script: "x"}},
		NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)
	job := &Job{Name: "j", Type: JobTypeCron, Schedule: "@daily", UserID: "u",
		Status: StatusActive, SourcePrompt: "采集", Spec: &JobSpec{Runtime: "python3", Script: "b", TimeoutSec: 60}}
	_ = s.AddJob(ctx, job)
	db.Close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("DB 关闭后 panic: %v", r)
		}
	}()
	s.maybeSelfHeal(ctx, job, &RunResult{Status: "error", Error: "boom"})
}

// TestChaos_AgentRunnerSlowAndError: a runner that blocks past its budget or
// returns an error must degrade into a failure history row.
func TestChaos_AgentRunnerSlowAndError(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	s := NewScheduler(db, &stubCompiler{},
		NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)

	s.SetAgentRunner(func(runCtx context.Context, _ *Job) (AgentResult, error) {
		select {
		case <-runCtx.Done():
			return AgentResult{}, runCtx.Err()
		case <-time.After(50 * time.Millisecond):
			return AgentResult{}, context.DeadlineExceeded
		}
	})
	job, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name: "晨报", Schedule: "@daily", Prompt: "每天总结要点", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt: %v", err)
	}
	s.executeJob(job)
	history, _ := s.GetJobHistory(ctx, job.ID)
	if len(history) == 0 || history[0].Status == "success" {
		t.Fatalf("runner 错误应记录为失败: %+v", history)
	}
}

// ─── Method 10: performance baseline ─────────────────────────────────

func BenchmarkClassifyTaskMode(b *testing.B) {
	prompt := "每天早上 9 点采集百度热搜 TOP10 整理为 markdown 并总结要点加入知识库"
	for i := 0; i < b.N; i++ {
		ClassifyTaskMode(prompt)
	}
}

// TestExecuteJob_LongRunPersistsHistory: a run longer than 10 seconds must
// still write its history row. Guard: a dbCtx created BEFORE the run (10s
// timeout) is already expired afterwards, silently dropping the row.
func TestExecuteJob_LongRunPersistsHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("long test")
	}
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	s := NewScheduler(db, &stubCompiler{},
		NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)

	s.SetAgentRunner(func(context.Context, *Job) (AgentResult, error) {
		time.Sleep(11 * time.Second)
		return AgentResult{Content: "慢任务完成"}, nil
	})
	job, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name: "慢晨报", Schedule: "@daily", Prompt: "每天总结要点", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt: %v", err)
	}
	s.executeJob(job)

	history, _ := s.GetJobHistory(ctx, job.ID)
	if len(history) == 0 {
		t.Fatal("执行 >10s 后历史丢失（dbCtx 过期）")
	}
	if history[0].Status != "success" {
		t.Fatalf("历史状态错误: %+v", history[0])
	}
}
