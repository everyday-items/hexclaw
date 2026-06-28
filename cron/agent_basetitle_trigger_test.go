package cron

// Two gaps closed:
//   1. The cron snapshot base title (job.Name) is now stamped by runAgentJob —
//      the contract that used to live in an untestable cmd/hexclaw closure is
//      now package-testable, and covers every path into runAgentJob (timer,
//      webhook TriggerJob, escalation).
//   2. TriggerJob no longer requires a script executor for AGENT-mode jobs
//      (they run via the injected runner, not the sandbox), so a webhook can
//      trigger an agent job in an agent-only deployment.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/skill"
)

// runAgentJob must stamp the job name as the snapshot base title on the ctx the
// runner receives, so any knowledge_ingest in the round keys one coherent
// series. Empty job name → no stamp (falls back to model/derived title).
func TestRunAgentJob_StampsSnapshotBaseTitle(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}

	var gotBase string
	s.SetAgentRunner(func(ctx context.Context, _ *Job) (AgentResult, error) {
		gotBase = skill.SnapshotBaseTitle(ctx)
		return AgentResult{Content: "ok"}, nil
	})

	s.runAgentJob(context.Background(),
		&Job{Name: "每日科技简报", SourcePrompt: "总结今天的科技要闻", Spec: &JobSpec{Runtime: RuntimeAgent}})
	if gotBase != "每日科技简报" {
		t.Errorf("runAgentJob must stamp job.Name as the snapshot base title, got %q", gotBase)
	}

	gotBase = "sentinel"
	s.runAgentJob(context.Background(),
		&Job{Name: "   ", SourcePrompt: "x", Spec: &JobSpec{Runtime: RuntimeAgent}})
	if gotBase != "" {
		t.Errorf("blank job name must stamp no base title (let the skill fall back), got %q", gotBase)
	}
}

// TriggerJob on an AGENT-mode job must run even when no script executor is wired
// (agent jobs don't use the sandbox). Before the fix this returned
// "脚本执行器未就绪"; the webhook→agent-job chain would have been dead in an
// agent-only deployment.
func TestTriggerJob_AgentJobRunsWithoutScriptExec(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s := NewScheduler(db, nil, nil) // scriptExec == nil on purpose
	if err := s.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	var ran int32
	s.SetAgentRunner(func(context.Context, *Job) (AgentResult, error) {
		atomic.AddInt32(&ran, 1)
		return AgentResult{Content: "ok"}, nil
	})

	job := &Job{Name: "agent-job", Schedule: "@hourly", UserID: "u1",
		SourcePrompt: "做点事", Spec: &JobSpec{Runtime: RuntimeAgent, TimeoutSec: 60}}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	if err := s.TriggerJob(ctx, job.ID); err != nil {
		t.Fatalf("TriggerJob on an agent job must not require a script executor, got: %v", err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&ran) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&ran) != 1 {
		t.Errorf("triggered agent job must actually run its runner, ran=%d", ran)
	}
}

// A SCRIPT-mode job still requires the executor (regression: the fix must be
// scoped to agent jobs, not a blanket removal of the guard).
func TestTriggerJob_ScriptJobStillNeedsExec(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s := NewScheduler(db, nil, nil) // no script executor
	if err := s.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	job := &Job{Name: "script-job", Schedule: "@hourly", UserID: "u1",
		SourcePrompt: "x", Spec: &JobSpec{Runtime: "starlark", Script: "emit({})"}}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := s.TriggerJob(ctx, job.ID); err == nil {
		t.Error("a script-mode job with no executor must still fail TriggerJob")
	}
}
