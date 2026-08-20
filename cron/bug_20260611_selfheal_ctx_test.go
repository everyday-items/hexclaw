package cron

// BUG-20260611 (install-test finding #6): the self-heal bridge never fired in
// production despite 4 consecutive failures — zero heal_failed rows.
//
// Root cause: executeJob hands maybeSelfHeal its 10-second dbCtx (sized for
// DB writes). The heal recompile derives its 2-minute timeout FROM that ctx,
// so the effective deadline is whatever is left of the 10s — a real LLM
// compile (12-15s) always times out. Worse, the heal_failed history row is
// then written with the same expired ctx, so the failure leaves no trace at
// all (double annihilation). Same family as the earlier ">10s task history
// lost to expired dbCtx" fix, which patched the execution path but missed
// this degraded branch.
//
// Contract: self-healing must run on its own context, independent of the
// caller's DB-write deadline; a successful compile is only a candidate until
// a later real execution verifies it.

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// ctxSensitiveCompiler succeeds only if the compile ctx is still alive —
// mirrors a real LLM call, which fails immediately on an expired ctx.
type ctxSensitiveCompiler struct {
	compiled int
}

func (c *ctxSensitiveCompiler) Compile(ctx context.Context, _ string, _ CompileHints) (*JobSpec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.compiled++
	return minimalSpec(), nil
}

func insertFailedRun(t *testing.T, db *sql.DB, jobID string, at time.Time) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO cron_job_runs (job_id, status, result, error, duration_ms, run_at, stdout, stderr, exit_code, data_json)
		 VALUES (?, 'error', '', 'Expecting value: line 2 column 9', 1300, ?, '', '', 1, 'null')`,
		jobID, at); err != nil {
		t.Fatalf("failed to insert run: %v", err)
	}
}

func TestBug20260611_SelfHealSurvivesExpiredCallerCtx(t *testing.T) {
	db := setupTestDB(t)
	comp := &ctxSensitiveCompiler{}
	exec := NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir())
	s := NewScheduler(db, comp, exec)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}

	job := &Job{
		Name: "heal-probe", Type: JobTypeCron, Schedule: "@daily",
		UserID: "u1", Status: StatusActive, SourcePrompt: "采集百度热搜",
		Spec: minimalSpec(),
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("add job: %v", err)
	}
	now := time.Now()
	for i := 3; i >= 1; i-- {
		insertFailedRun(t, db, job.ID, now.Add(-time.Duration(i)*time.Minute))
	}

	// Mirror production: the caller's ctx is the 10s dbCtx, here nearly spent.
	expiringCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond) // ensure it is already expired

	s.maybeSelfHeal(expiringCtx, job, &RunResult{Status: "error", Error: "Expecting value: line 2 column 9"})

	if comp.compiled != 1 {
		t.Fatalf("[BUG-20260611] heal recompile must not inherit the caller's expired ctx (compiled=%d)", comp.compiled)
	}

	// The candidate history row must also survive the expired caller ctx.
	if handled := s.resolveSelfHealVerification(context.Background(), job, &RunResult{Status: "success"}); !handled {
		t.Fatal("self-heal candidate verification should be handled")
	}

	// The verified healed history row must survive the expired caller ctx.
	var healed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_job_runs WHERE job_id = ? AND status = 'healed'`, job.ID).Scan(&healed); err != nil {
		t.Fatalf("query healed rows: %v", err)
	}
	if healed != 1 {
		t.Fatalf("[BUG-20260611] healed history row missing (got %d) — the no-trace double annihilation", healed)
	}
}
