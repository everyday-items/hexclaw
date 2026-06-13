package cron

// BUG-20260613 (audit H2): cron_job_runs grew without bound — a job ticking
// every hour writes a history row forever and nothing ever trimmed the table.
// persistHistory now prunes to the most recent maxHistoryRowsPerJob rows per
// job on every write, backed by the (job_id, id) index.

import (
	"context"
	"testing"
	"time"
)

func TestBug20260613_HistoryPrunedToCap(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx := context.Background()

	// Write well past the cap; each write should keep the table bounded.
	total := maxHistoryRowsPerJob + 73
	for i := 0; i < total; i++ {
		if err := s.persistHistory(ctx, "job-h2", "success", "", "", int64(i), time.Now(), "", "", 0, nil); err != nil {
			t.Fatalf("persistHistory #%d: %v", i, err)
		}
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_job_runs WHERE job_id = ?`, "job-h2").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != maxHistoryRowsPerJob {
		t.Errorf("history must be capped at %d, got %d", maxHistoryRowsPerJob, count)
	}

	// The rows kept must be the NEWEST ones: duration_ms was written as the
	// loop index, so the minimum retained must be total-cap (the oldest kept).
	var minDur int
	if err := db.QueryRowContext(ctx, `SELECT MIN(duration_ms) FROM cron_job_runs WHERE job_id = ?`, "job-h2").Scan(&minDur); err != nil {
		t.Fatalf("min: %v", err)
	}
	if minDur != total-maxHistoryRowsPerJob {
		t.Errorf("prune must drop the OLDEST rows: expected oldest kept duration %d, got %d", total-maxHistoryRowsPerJob, minDur)
	}
}

// Pruning is per-job: one job's overflow must not evict another job's rows.
func TestBug20260613_HistoryPruneIsPerJob(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		_ = s.persistHistory(ctx, "job-small", "success", "", "", 0, time.Now(), "", "", 0, nil)
	}
	for i := 0; i < maxHistoryRowsPerJob+20; i++ {
		_ = s.persistHistory(ctx, "job-big", "success", "", "", 0, time.Now(), "", "", 0, nil)
	}

	var small int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_job_runs WHERE job_id = ?`, "job-small").Scan(&small)
	if small != 10 {
		t.Errorf("a job under the cap must keep all its rows, got %d", small)
	}
}
