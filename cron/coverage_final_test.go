package cron

// Final coverage pass: the remaining reachable branches across the scheduler,
// executor, and agent helpers — legacy cleanup, error injection via a closed DB
// or unmarshalable data, runtime edge cases, and the cron-field validators.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// nilResultEngine returns (nil, err) from Execute to drive executeJob's
// "executor returned nil" recovery branch.
type nilResultEngine struct{}

func (nilResultEngine) Name() string          { return "nilres" }
func (nilResultEngine) Available() bool       { return true }
func (nilResultEngine) Validate(string) error { return nil }
func (nilResultEngine) Execute(context.Context, *JobSpec) (*RunResult, error) {
	return nil, errors.New("venv prep failed")
}

func TestDetectAndCleanupLegacyJobs_Quarantines(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	ctx := context.Background()
	// v2 schema (no prompt column → rebuild won't fire); insert spec-less rows.
	for _, id := range []string{"leg1", "leg2"} {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO cron_jobs (id,name,type,schedule,spec_json,user_id,status,next_run_at) VALUES (?,?,?,?,?,?,?,?)`,
			id, "old-"+id, "cron", "@daily", "", "u", "active", time.Now())
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := s.detectAndCleanupLegacyJobs(ctx); err != nil {
		t.Fatalf("detect: %v", err)
	}
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs WHERE spec_json='' AND status='paused'`).Scan(&n)
	if n != 2 {
		t.Errorf("spec-less legacy jobs must be retained and paused, got %d", n)
	}
}

func TestInit_OnClosedDB(t *testing.T) {
	db := setupTestDB(t)
	_ = db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err == nil {
		t.Error("Init on a closed DB must error")
	}
}

func TestLoadJobs_ScanError(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id,name,type,schedule,spec_json,user_id,status,next_run_at) VALUES (?,?,?,?,?,?,?,?)`,
		"corrupt", "c", "cron", "@daily", "{bad json", "u", "active", time.Now())
	if err := s.loadJobs(ctx); err == nil {
		t.Error("loadJobs must surface a scan error on corrupt spec_json")
	}
}

func TestScanJobRow_WithLastRun(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	ctx := context.Background()
	job := &Job{Name: "x", Schedule: "@every 1h", UserID: "u", Status: StatusActive, Spec: starlarkSuccessSpec()}
	_ = s.AddJob(ctx, job)
	_, _ = s.db.ExecContext(ctx, `UPDATE cron_jobs SET last_run_at=?, source_prompt='p' WHERE id=?`, time.Now(), job.ID)
	jobs, err := s.ListJobs(ctx, "u")
	if err != nil || len(jobs) != 1 || jobs[0].LastRunAt.IsZero() {
		t.Errorf("last_run_at must scan into the job: err=%v jobs=%d", err, len(jobs))
	}
}

func TestAddJobFromScript_UnavailableEngine(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	s.engines["fake-unavail"] = unavailEngine{}
	_, err := s.AddJobFromScript(context.Background(),
		AddJobRequest{Name: "x", Schedule: "@every 1h"}, "fake-unavail", `emit({"status":"success"})`)
	if err == nil {
		t.Error("an unavailable engine must reject AddJobFromScript")
	}
}

func TestAddJobFromPrompt_AgentMode(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	s := NewScheduler(db, &stubCompiler{}, NewScriptExecutor().WithWorkdir(t.TempDir()))
	_ = s.Init(ctx)
	// A reasoning prompt routes to agent mode; @daily satisfies the 1h frequency guard.
	job, err := s.AddJobFromPrompt(ctx, AddJobRequest{Name: "x", Schedule: "@daily", Prompt: "总结今天的科技新闻", UserID: "u"})
	if err != nil {
		t.Fatalf("agent-mode create: %v", err)
	}
	if job.Spec == nil || job.Spec.Runtime != RuntimeAgent {
		t.Errorf("reasoning prompt must produce an agent-mode job, got %+v", job.Spec)
	}
	// Frequency guard: a sub-hour schedule for an agent task is rejected.
	if _, err := s.AddJobFromPrompt(ctx, AddJobRequest{Name: "y", Schedule: "@every 5m", Prompt: "分析数据", UserID: "u"}); err == nil {
		t.Error("agent-mode job under 1h interval must be rejected")
	}
}

func TestExecuteJob_NilResultRecovery(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	s.engines["nilres"] = nilResultEngine{}
	s.executeJob(&Job{ID: "nr", Name: "n", Schedule: "@every 1h", Type: JobTypeCron, Spec: &JobSpec{Runtime: "nilres", Script: "x"}})
	var status, errMsg string
	_ = s.db.QueryRowContext(context.Background(), `SELECT status, error FROM cron_job_runs WHERE job_id='nr' ORDER BY id DESC LIMIT 1`).Scan(&status, &errMsg)
	if status != "error" || errMsg == "" {
		t.Errorf("nil result must record an error, got status=%q err=%q", status, errMsg)
	}
}

func TestRunAgentJob_NilRunner(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	r := s.runAgentJob(context.Background(), &Job{ID: "a", Spec: &JobSpec{Runtime: RuntimeAgent}})
	if r.Status != "error" {
		t.Errorf("a missing agent runner must yield an error result, got %q", r.Status)
	}
}

func TestPersistHistory_MarshalError(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	// A channel cannot be JSON-marshaled → persistHistory's data-marshal branch.
	if err := s.persistHistory(context.Background(), "j", "success", "", "", 0, time.Now(), "", "", 0, make(chan int)); err == nil {
		t.Error("unmarshalable data must make persistHistory error")
	}
}

func TestPruneHistory_OnClosedDB(t *testing.T) {
	s := newTestScheduler(t, setupTestDB(t))
	_ = s.Init(context.Background())
	_ = s.db.Close()
	s.pruneHistory(context.Background(), "j") // must not panic; logs a warn
}

func TestRecordHealAttempt_FreshScheduler(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	// First touch initializes the nil healTimes map.
	s.recordHealAttempt("brand-new")
	if !s.healQuotaOK("brand-new") {
		t.Error("one attempt must leave quota available")
	}
}

func TestExecutorRun_StatusFallbackToSuccess(t *testing.T) {
	e := newExec(t)
	// Valid JSON last line with no "status" field → status defaults to success.
	res, err := e.Run(context.Background(), &JobSpec{Runtime: "python3", Script: `print('{"data": 7}')`, TimeoutSec: 10})
	if err != nil || res.Status != "success" {
		t.Errorf("no-status JSON must default to success: err=%v status=%q", err, res.Status)
	}
}

func TestParseCron5_AllFieldErrors(t *testing.T) {
	from := time.Now()
	for _, bad := range []string{"0 99 * * *", "0 0 99 * *", "0 0 1 99 *", "0 0 1 1 99"} {
		if _, err := nextRunTime(bad, JobTypeCron, from); err == nil {
			t.Errorf("cron5 %q must error on the out-of-range field", bad)
		}
	}
}
