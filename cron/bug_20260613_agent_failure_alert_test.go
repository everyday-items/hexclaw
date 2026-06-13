package cron

// BUG-20260613 (audit H1): an agent-mode job that fails forever was silent —
// maybeSelfHeal returns early for agent jobs (there is no script to recompile),
// and nothing else alerted the user. A persistently failing agent job now
// raises a failure-class notification once it crosses selfHealThreshold,
// throttled by the same 24h anti-bombing budget the heal path uses.

import (
	"context"
	"strings"
	"testing"
)

type alertCapture struct {
	level, title, body string
}

func newAgentAlertScheduler(t *testing.T) (*Scheduler, *[]alertCapture) {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	s := NewScheduler(db, &stubCompiler{}, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	s.SetAgentRunner(func(context.Context, *Job) (AgentResult, error) {
		return AgentResult{Content: "无法完成采集。\nTASK_STATUS: failed - upstream unreachable"}, nil
	})
	var alerts []alertCapture
	s.SetNotifier(func(_ *Job, level, title, body string) {
		alerts = append(alerts, alertCapture{level, title, body})
	})
	return s, &alerts
}

func TestBug20260613_AgentPersistentFailureAlertsOnce(t *testing.T) {
	s, alerts := newAgentAlertScheduler(t)
	ctx := context.Background()

	job, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name: "晨报", Schedule: "@daily", Prompt: "每天总结要点", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt: %v", err)
	}

	// Below threshold: no alert yet.
	s.executeJob(job)
	s.executeJob(job)
	if len(*alerts) != 0 {
		t.Fatalf("must not alert before %d consecutive failures, got %v", selfHealThreshold, *alerts)
	}

	// Crossing the threshold fires exactly one warning alert.
	s.executeJob(job)
	if len(*alerts) != 1 {
		t.Fatalf("crossing the failure threshold must alert exactly once, got %d: %v", len(*alerts), *alerts)
	}
	a := (*alerts)[0]
	if a.level != NotifyLevelWarning {
		t.Errorf("alert must be warning-level, got %q", a.level)
	}
	if !strings.Contains(a.title, "持续失败") {
		t.Errorf("alert title must signal persistent failure, got %q", a.title)
	}
	if !strings.Contains(a.body, "upstream unreachable") {
		t.Errorf("alert body must carry the failure reason, got %q", a.body)
	}

	// Anti-bombing: further failures inside the window do NOT re-alert.
	s.executeJob(job)
	s.executeJob(job)
	if len(*alerts) != 1 {
		t.Errorf("repeated failures within the 24h window must not re-alert, got %d", len(*alerts))
	}
}
