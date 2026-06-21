package cron

import (
	"context"
	"testing"
	"time"
)

// ── H1 wake_agent 条件升级门 ──────────────────────────────────

func TestStarlarkWakeAgent_SetsWakeAndContext(t *testing.T) {
	eng := NewStarlarkEngine()
	res, err := eng.Execute(context.Background(), &JobSpec{
		Runtime: RuntimeStarlark, Script: `wake_agent("found 3 new items")`, TimeoutSec: 10,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !res.Wake {
		t.Error("expected Wake=true after wake_agent()")
	}
	if res.WakeContext != "found 3 new items" {
		t.Errorf("WakeContext=%q, want %q", res.WakeContext, "found 3 new items")
	}
	// wake_agent 是门任务的输出契约：调了 wake_agent 而未 emit 不应判 error。
	if res.Status != "success" {
		t.Errorf("status=%q, want success (wake is the output contract)", res.Status)
	}
}

func TestStarlarkNoWakeNoEmit_IsError(t *testing.T) {
	eng := NewStarlarkEngine()
	res, err := eng.Execute(context.Background(), &JobSpec{
		Runtime: RuntimeStarlark, Script: `x = 1 + 1`, TimeoutSec: 10,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Wake {
		t.Error("Wake should be false when wake_agent not called")
	}
	if res.Status != "error" {
		t.Errorf("status=%q, want error (neither emit nor wake_agent)", res.Status)
	}
}

func TestValidateStarlark_WakeAgentSatisfiesContract(t *testing.T) {
	if err := validateStarlarkSource(`wake_agent("x")`); err != nil {
		t.Errorf("wake_agent-only gate should validate: %v", err)
	}
	if err := validateStarlarkSource(`x = 1 + 1`); err == nil {
		t.Error("script with neither emit nor wake_agent should fail validation")
	}
}

func TestCanEscalate_MinIntervalGuard(t *testing.T) {
	s := NewScheduler(nil, nil, nil)
	if !s.canEscalate("j1") {
		t.Error("first escalation should be allowed")
	}
	s.markEscalated("j1")
	if s.canEscalate("j1") {
		t.Error("immediate re-escalation should be blocked (broken-gate token guard)")
	}
	s.lastEscalated.Store("j1", time.Now().Add(-2*time.Hour))
	if !s.canEscalate("j1") {
		t.Error("escalation after min interval should be allowed again")
	}
}

// ── H2 misfire 宽限 + 快进 ────────────────────────────────────

func TestGraceFor_HalfPeriodClamped(t *testing.T) {
	cases := []struct {
		sched string
		want  time.Duration
	}{
		{"@hourly", 30 * time.Minute},  // 半小时
		{"@daily", 2 * time.Hour},      // 12h → clamp 2h
		{"@every 1m", 2 * time.Minute}, // 30s → floor 2m
		{"@every 1h", 30 * time.Minute},
		{"bad-schedule", 2 * time.Hour}, // 算不出 → 上限
	}
	for _, c := range cases {
		got := graceFor(c.sched, JobTypeCron, "")
		if got != c.want {
			t.Errorf("graceFor(%q) = %v, want %v", c.sched, got, c.want)
		}
	}
}

func TestFastForward_SkipsBacklogAndStaysActive(t *testing.T) {
	s := NewScheduler(nil, nil, nil) // db nil → fastForward 仅更新内存
	past := time.Now().Add(-26 * time.Hour)
	job := &Job{ID: "ff1", Schedule: "@daily", Type: JobTypeCron, Status: StatusActive, NextRunAt: past}
	s.jobs[job.ID] = job

	cp := *job
	s.fastForward(&cp, time.Now())

	got := s.jobs[job.ID]
	if !got.NextRunAt.After(time.Now()) {
		t.Errorf("fastForward should advance NextRunAt to a future point; got %v", got.NextRunAt)
	}
	if got.Status != StatusActive {
		t.Errorf("fastForward must keep job active; got %v", got.Status)
	}
}

// ── H3 [SILENT] 抑制投递 ──────────────────────────────────────

func TestDeliverResult_SilentSuppresses(t *testing.T) {
	s := NewScheduler(nil, nil, nil)
	var delivered []string
	s.SetDeliverer(func(_ *Job, target, content string) error {
		delivered = append(delivered, target+":"+content)
		return nil
	})
	job := &Job{ID: "sj1", Name: "monitor", Deliver: []string{"feishu:grp"}}
	s.jobs[job.ID] = job

	s.deliverResult(job, &RunResult{Status: "success", Stdout: "[SILENT] nothing new this tick"})
	if len(delivered) != 0 {
		t.Errorf("[SILENT] result must not be delivered; got %v", delivered)
	}

	s.deliverResult(job, &RunResult{Status: "success", Stdout: "3 new items"})
	if len(delivered) != 1 {
		t.Errorf("non-silent result must be delivered; got %v", delivered)
	}
}

// ── H4 投递失败/成功记账解耦 ──────────────────────────────────

func TestRecordDeliveryOutcome_SplitsAndClears(t *testing.T) {
	s := NewScheduler(nil, nil, nil)
	job := &Job{ID: "rd1"}
	s.jobs[job.ID] = job

	s.recordDeliveryOutcome(job.ID, []string{"feishu: timeout"})
	if s.jobs[job.ID].LastDeliveryError == "" {
		t.Error("delivery failure should set LastDeliveryError")
	}
	s.recordDeliveryOutcome(job.ID, nil)
	if s.jobs[job.ID].LastDeliveryError != "" {
		t.Error("successful delivery should clear LastDeliveryError")
	}
}
