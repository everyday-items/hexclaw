package cron

// Coverage completion for cron.go scheduler internals: executeJob runtime
// branches (nil spec / unknown / unavailable / agent / once / script success),
// TriggerJob, history prune, DB-error paths (via a closed DB), scanJobRow on a
// corrupt row, legacy migration, and the schedule-format parser arms.

import (
	"context"
	"testing"
	"time"
)

// unavailEngine is a registered-but-unavailable engine, to drive executeJob's
// "runtime not available on this host" branch deterministically.
type unavailEngine struct{}

func (unavailEngine) Name() string                                   { return "fake-unavail" }
func (unavailEngine) Available() bool                                { return false }
func (unavailEngine) Validate(string) error                          { return nil }
func (unavailEngine) Execute(context.Context, *JobSpec) (*RunResult, error) {
	return &RunResult{Status: "success"}, nil
}

func freshScheduler(t *testing.T) (*Scheduler, func()) {
	t.Helper()
	db := setupTestDB(t)
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s, func() { _ = db.Close() }
}

func starlarkSuccessSpec() *JobSpec {
	return &JobSpec{Runtime: RuntimeStarlark, Script: `emit({"status":"success","data":1})`, TimeoutSec: 10}
}

func TestExecuteJob_RuntimeBranches(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	ctx := context.Background()

	// nil spec
	s.executeJob(&Job{ID: "j-nil", Name: "n", Schedule: "@every 1h", Spec: nil})
	// unknown runtime → engineFor nil
	s.executeJob(&Job{ID: "j-unknown", Name: "n", Schedule: "@every 1h", Spec: &JobSpec{Runtime: "ruby", Script: "x"}})
	// registered but unavailable runtime
	s.engines["fake-unavail"] = unavailEngine{}
	s.executeJob(&Job{ID: "j-unavail", Name: "n", Schedule: "@every 1h", Spec: &JobSpec{Runtime: "fake-unavail", Script: "x"}})
	// starlark success (cron type) → deliverResult path
	s.executeJob(&Job{ID: "j-ok", Name: "n", Schedule: "@every 1h", Type: JobTypeCron, Spec: starlarkSuccessSpec()})
	// once type → status done branch
	s.executeJob(&Job{ID: "j-once", Name: "n", Schedule: "2026-01-01T00:00:00", Type: JobTypeOnce, Spec: starlarkSuccessSpec()})

	// each run persisted a history row
	for _, id := range []string{"j-nil", "j-unknown", "j-unavail", "j-ok", "j-once"} {
		var n int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_job_runs WHERE job_id=?`, id).Scan(&n)
		if n == 0 {
			t.Errorf("%s: expected a history row", id)
		}
	}
}

func TestExecuteJob_AgentRuntime(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	s.SetAgentRunner(func(context.Context, *Job) (AgentResult, error) {
		return AgentResult{Content: "all done\nTASK_STATUS: done"}, nil
	})
	// A prompt with no ingest requirement → success without a knowledge_ingest call.
	s.executeJob(&Job{ID: "j-agent", Name: "ping job", Schedule: "@every 1h", Type: JobTypeCron,
		SourcePrompt: "ping the status page", Spec: &JobSpec{Runtime: RuntimeAgent, TimeoutSec: 60}})
	var status string
	_ = s.db.QueryRowContext(context.Background(), `SELECT status FROM cron_job_runs WHERE job_id=? ORDER BY id DESC LIMIT 1`, "j-agent").Scan(&status)
	if status != "success" {
		t.Errorf("agent job status = %q, want success", status)
	}
}

func TestTriggerJob_Branches(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	ctx := context.Background()
	// not found
	if err := s.TriggerJob(ctx, "missing"); err == nil {
		t.Error("triggering a missing job must error")
	}
	// success path: add a starlark job then trigger
	job := &Job{Name: "t", Schedule: "@every 1h", Type: JobTypeCron, UserID: "u", Status: StatusActive, Spec: starlarkSuccessSpec()}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := s.TriggerJob(ctx, job.ID); err != nil {
		t.Errorf("TriggerJob: %v", err)
	}
	// no scriptExec wired → "executor not ready"
	s2 := NewScheduler(setupTestDB(t), nil, nil)
	_ = s2.Init(ctx)
	j2 := &Job{Name: "t", Schedule: "@every 1h", Type: JobTypeCron, UserID: "u", Status: StatusActive, Spec: starlarkSuccessSpec()}
	_ = s2.AddJob(ctx, j2)
	if err := s2.TriggerJob(ctx, j2.ID); err == nil {
		t.Error("nil scriptExec must make TriggerJob error")
	}
}

func TestPruneHistory_CapsRows(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	ctx := context.Background()
	for i := range maxHistoryRowsPerJob + 15 {
		_ = s.persistHistory(ctx, "j-prune", "success", "", "", 1, time.Now(), "out", "", 0, map[string]any{"i": i})
	}
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_job_runs WHERE job_id=?`, "j-prune").Scan(&n)
	if n != maxHistoryRowsPerJob {
		t.Errorf("history rows = %d, want capped at %d", n, maxHistoryRowsPerJob)
	}
}

func TestScanJobRow_CorruptSpecJSON(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id,name,type,schedule,spec_json,source_prompt,user_id,status,next_run_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		"bad", "bad", "cron", "@every 1h", "{not json", "p", "u", "active", time.Now())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.ListJobs(ctx, "u"); err == nil {
		t.Error("corrupt spec_json must surface a scan error")
	}
}

func TestListJobs_PausedFilter(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	ctx := context.Background()
	active := &Job{Name: "a", Schedule: "@every 1h", UserID: "u", Status: StatusActive, Spec: starlarkSuccessSpec()}
	paused := &Job{Name: "p", Schedule: "@every 1h", UserID: "u", Status: StatusActive, Spec: starlarkSuccessSpec()}
	_ = s.AddJob(ctx, active)
	_ = s.AddJob(ctx, paused)
	_ = s.PauseJob(ctx, paused.ID)
	jobs, err := s.ListJobs(ctx, "u")
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Errorf("ListJobs returns all statuses, got %d", len(jobs))
	}
}

func TestDBErrorPaths_OnClosedDB(t *testing.T) {
	s := newTestScheduler(t, setupTestDB(t))
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	job := &Job{Name: "x", Schedule: "@every 1h", UserID: "u", Status: StatusActive, Spec: starlarkSuccessSpec()}
	_ = s.AddJob(ctx, job)
	_ = s.db.Close() // force every subsequent query to fail

	if _, err := s.ListJobs(ctx, "u"); err == nil {
		t.Error("ListJobs on closed DB must error")
	}
	if err := s.RemoveJob(ctx, job.ID); err == nil {
		t.Error("RemoveJob on closed DB must error")
	}
	if err := s.PauseJob(ctx, job.ID); err == nil {
		t.Error("PauseJob (updateJobStatus) on closed DB must error")
	}
	if _, err := s.GetJobHistory(ctx, job.ID); err == nil {
		t.Error("GetJobHistory on closed DB must error")
	}
	if err := s.loadJobs(ctx); err == nil {
		t.Error("loadJobs on closed DB must error")
	}
}

func TestGetJobHistory_NotFound(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	if _, err := s.GetJobHistory(context.Background(), "nope"); err == nil {
		t.Error("history for an unknown job must error")
	}
}

func TestAddJob_InvalidSchedule(t *testing.T) {
	s, done := freshScheduler(t)
	defer done()
	err := s.AddJob(context.Background(), &Job{Name: "x", Schedule: "@every nonsense", UserID: "u", Spec: starlarkSuccessSpec()})
	if err == nil {
		t.Error("invalid schedule must be rejected by AddJob")
	}
}

func TestSerializeAndParseJobMeta_Deliver(t *testing.T) {
	j := &Job{Deliver: []string{"feishu", "push"}}
	meta := serializeJobMeta(j)
	back := &Job{}
	parseJobMeta(back, meta)
	if len(back.Deliver) != 2 || back.Deliver[0] != "feishu" {
		t.Errorf("deliver round-trip failed: %v", back.Deliver)
	}
	// empty / blank meta is a no-op
	parseJobMeta(back, "")
	if len(back.Deliver) != 2 {
		t.Error("blank meta must not clobber deliver")
	}
}

func TestNextRunTime_AllFormats(t *testing.T) {
	from := time.Date(2026, 6, 15, 14, 30, 0, 0, time.Local)
	for _, sch := range []string{"@hourly", "@weekly", "@daily", "@every 30s"} {
		if _, err := nextRunTime(sch, JobTypeCron, from); err != nil {
			t.Errorf("%s must parse: %v", sch, err)
		}
	}
	// @weekly when from is already Monday-at-0 path + general weekly
	if _, err := nextRunTime("@weekly", JobTypeCron, time.Date(2026, 6, 15, 0, 0, 0, 0, time.Local)); err != nil {
		t.Errorf("@weekly monday: %v", err)
	}
	// bad @every
	if _, err := nextRunTime("@every nope", JobTypeCron, from); err == nil {
		t.Error("bad @every must error")
	}
	// non-positive @every
	if _, err := nextRunTime("@every 0s", JobTypeCron, from); err == nil {
		t.Error("@every 0s must error")
	}
	// once: RFC3339, then the two fallback layouts, then invalid
	for _, ts := range []string{"2026-07-01T10:00:00Z", "2026-07-01T10:00:00", "2026-07-01 10:00"} {
		if _, err := nextRunTime(ts, JobTypeOnce, from); err != nil {
			t.Errorf("once %q must parse: %v", ts, err)
		}
	}
	if _, err := nextRunTime("not-a-time", JobTypeOnce, from); err == nil {
		t.Error("invalid once time must error")
	}
	// cron5 errors: wrong field count + out-of-range + non-numeric
	for _, bad := range []string{"1 2 3", "99 * * * *", "x * * * *"} {
		if _, err := nextRunTime(bad, JobTypeCron, from); err == nil {
			t.Errorf("cron5 %q must error", bad)
		}
	}
}
