package cron

// Coverage completion for agent_mode.go: outcome parsing, ingest-intent, the
// self-heal / alert bridges, heal-quota bookkeeping, delivery routing, and the
// small helpers — driven by direct calls against an in-memory scheduler.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func seedFailures(t *testing.T, s *Scheduler, jobID string, n int) {
	t.Helper()
	ctx := context.Background()
	for range n {
		if err := s.persistHistory(ctx, jobID, "error", "", "boom", 1, time.Now(), "", "", 1, nil); err != nil {
			t.Fatalf("seed failure: %v", err)
		}
	}
}

func TestAgentHelpers_Small(t *testing.T) {
	if scheduleMinInterval("@every not-a-duration") != 0 {
		t.Error("unparseable schedule must yield 0 interval")
	}
	if jobRequiresIngest(nil) {
		t.Error("nil job requires no ingest")
	}
	if clipForHeal("short", 100) != "short" {
		t.Error("string under cap must pass through unchanged")
	}
	if got := clipForHeal("0123456789", 4); got != "0123…" {
		t.Errorf("clip = %q, want 0123…", got)
	}
}

func TestParseAgentOutcome_Variants(t *testing.T) {
	cases := []struct {
		name           string
		content        string
		wantFailed     bool
		reasonContains string
	}{
		{"english failed", "did stuff\nTASK_STATUS: failed - upstream 500", true, "upstream 500"},
		{"chinese failed", "做了些事\n任务状态：失败 网络不通", true, "网络不通"},
		{"failed no reason", "x\nTASK_STATUS: failed", true, "not accomplished"},
		{"success done", "all good\nTASK_STATUS: done", false, ""},
		{"blank line among last lines", "first\n\nsecond\nTASK_STATUS: done", false, ""},
		{"no marker", "just a reply with no marker", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			failed, reason, _ := parseAgentOutcome(c.content)
			if failed != c.wantFailed {
				t.Fatalf("failed = %v, want %v (reason=%q)", failed, c.wantFailed, reason)
			}
			if c.reasonContains != "" && !strings.Contains(reason, c.reasonContains) {
				t.Errorf("reason %q must contain %q", reason, c.reasonContains)
			}
		})
	}
}

func TestConsecutiveFailures_Window(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	ctx := context.Background()
	seedFailures(t, s, "j", selfHealThreshold)
	if !s.consecutiveFailures(ctx, "j", selfHealThreshold) {
		t.Error("N consecutive failures must satisfy the window")
	}
	// A success resets the streak.
	_ = s.persistHistory(ctx, "j", "success", "", "", 1, time.Now(), "", "", 0, nil)
	if s.consecutiveFailures(ctx, "j", selfHealThreshold) {
		t.Error("a recent success must break the failure streak")
	}
}

func TestHealQuota_AndNotice(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	for range selfHealMaxPerWindow {
		if !s.healQuotaOK("j") {
			t.Fatal("quota must allow attempts up to the cap")
		}
		s.recordHealAttempt("j")
	}
	if s.healQuotaOK("j") {
		t.Error("quota must be exhausted after the cap")
	}
	// failure-notice throttle: first allowed, second within window suppressed.
	if !s.shouldNotifyHealFailure("j") {
		t.Error("first failure notice must be allowed")
	}
	if s.shouldNotifyHealFailure("j") {
		t.Error("second failure notice within window must be suppressed")
	}
	s.pruneAgentState("j")
	if !s.healQuotaOK("j") {
		t.Error("pruned job must start with a fresh quota")
	}
}

func TestDeliverResult_Routing(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	// empty/contract-only content → early return (no panic).
	s.deliverResult(&Job{Name: "n"}, &RunResult{Stdout: `{"status":"success"}`})
	// chat default target → desktop notify path.
	s.deliverResult(&Job{Name: "n"}, &RunResult{Data: "hello"})
	// IM target, no deliverer wired → warn-and-skip branch.
	s.deliverResult(&Job{Name: "n", Deliver: []string{"feishu"}}, &RunResult{Data: "hi"})
	// IM target with a deliverer wired → deliverer invoked.
	delivered := false
	s.agent.mu.Lock()
	s.agent.deliverer = func(*Job, string, string) error { delivered = true; return nil }
	s.agent.mu.Unlock()
	s.deliverResult(&Job{Name: "n", Deliver: []string{"feishu"}}, &RunResult{Data: "hi"})
	if !delivered {
		t.Error("wired deliverer must be invoked for an IM target")
	}
	// deliverer returning an error → logged, not fatal.
	s.agent.mu.Lock()
	s.agent.deliverer = func(*Job, string, string) error { return errors.New("im down") }
	s.agent.mu.Unlock()
	s.deliverResult(&Job{Name: "n", Deliver: []string{"discord"}}, &RunResult{Data: "hi"})
}

func TestMaybeAlertAgentFailure(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	ctx := context.Background()
	// non-agent job → early return.
	s.maybeAlertAgentFailure(ctx, &Job{ID: "x", Spec: &JobSpec{Runtime: RuntimeStarlark}}, &RunResult{})
	// agent job past the failure threshold → alert path.
	seedFailures(t, s, "ag", selfHealThreshold)
	s.maybeAlertAgentFailure(ctx, &Job{ID: "ag", Name: "agent job", Spec: &JobSpec{Runtime: RuntimeAgent}}, &RunResult{Error: "boom"})
}

func TestMaybeSelfHeal_RecompilesAndQuota(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	healed := &JobSpec{Runtime: RuntimeStarlark, Script: `emit({"status":"success"})`, TimeoutSec: 10}
	s := NewScheduler(db, &stubCompiler{ret: healed}, NewScriptExecutor().WithWorkdir(t.TempDir()))
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// agent job → early return (no script to recompile).
	s.maybeSelfHeal(ctx, &Job{ID: "a", Spec: &JobSpec{Runtime: RuntimeAgent}}, &RunResult{})

	// script job past the threshold → recompile + persist a pending candidate.
	job := &Job{
		ID: "h", Name: "heal job", Type: JobTypeCron, Schedule: "@daily", UserID: "u1",
		Status: StatusActive, SourcePrompt: "fetch x",
		Spec: &JobSpec{Runtime: RuntimeStarlark, Script: "old"},
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	seedFailures(t, s, "h", selfHealThreshold)
	s.maybeSelfHeal(ctx, job, &RunResult{Error: "boom", Stderr: "trace"})
	if handled := s.resolveSelfHealVerification(ctx, job, &RunResult{Status: "success"}); !handled {
		t.Fatal("self-heal candidate verification should be handled")
	}
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_job_runs WHERE job_id='h' AND status='healed'`).Scan(&n)
	if n == 0 {
		t.Error("a successful self-heal must persist a 'healed' history row")
	}
}
