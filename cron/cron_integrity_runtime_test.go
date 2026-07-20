package cron

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openForeignKeyCronTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "cron-runtime.db")+
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestUpsertJobFromScriptMergesDuplicateEvidenceBeforeDeletingParent(t *testing.T) {
	ctx := context.Background()
	db := openForeignKeyCronTestDB(t)
	scheduler := newTestScheduler(t, db)
	if err := scheduler.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	jobs := []*Job{
		{ID: "stable-a", Name: "old-a", Schedule: "@daily", UserID: "u1", SourceKey: "agent-a/daily",
			CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			LastRunAt: time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC), RunCount: 2, Spec: minimalSpec()},
		{ID: "stable-b", Name: "old-b", Schedule: "@daily", UserID: "u1", SourceKey: "agent-a/daily",
			CreatedAt: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC),
			LastRunAt: time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC), RunCount: 5, Spec: minimalSpec()},
	}
	for _, job := range jobs {
		if err := scheduler.AddJob(ctx, job); err != nil {
			t.Fatalf("AddJob %s: %v", job.ID, err)
		}
	}
	if _, err := db.Exec(`UPDATE cron_jobs SET run_count=2,last_run_at='2026-07-03T00:00:00Z' WHERE id='stable-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE cron_jobs SET run_count=5,last_run_at='2026-07-05T00:00:00Z' WHERE id='stable-b'`); err != nil {
		t.Fatal(err)
	}
	for id, owner := range map[int]string{101: "stable-a", 102: "stable-b"} {
		if _, err := db.Exec(`INSERT INTO cron_job_runs(id,job_id,status,data_json) VALUES (?,?,'success',?)`,
			id, owner, `{"owner":"`+owner+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct{ owner, key, value string }{
		{"stable-a", "shared", "survivor-value"},
		{"stable-b", "shared", "loser-value"},
		{"stable-b", "loser-only", "must-move"},
	} {
		if _, err := db.Exec(`INSERT INTO cron_job_state(job_id,key,value) VALUES (?,?,?)`, row.owner, row.key, row.value); err != nil {
			t.Fatal(err)
		}
	}

	replacement, err := scheduler.UpsertJobFromScript(ctx, AddJobRequest{
		Name: "new-display-name", Schedule: "@daily", UserID: "u1", SourceKey: "agent-a/daily",
	}, RuntimeStarlark, `emit("new")`)
	if err != nil {
		t.Fatalf("UpsertJobFromScript: %v", err)
	}
	if replacement.ID != "stable-a" {
		t.Fatalf("survivor=%q, want stable-a", replacement.ID)
	}
	if replacement.RunCount != 7 {
		t.Fatalf("merged parent run_count=%d, want 7", replacement.RunCount)
	}
	wantLastRun := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	if !replacement.LastRunAt.Equal(wantLastRun) {
		t.Fatalf("merged parent last_run_at=%v, want %v", replacement.LastRunAt, wantLastRun)
	}
	var persistedRunCount int64
	var persistedLastRun sql.NullTime
	if err := db.QueryRow(`SELECT run_count,last_run_at FROM cron_jobs WHERE id='stable-a'`).
		Scan(&persistedRunCount, &persistedLastRun); err != nil {
		t.Fatal(err)
	}
	if persistedRunCount != 7 || !persistedLastRun.Valid || !persistedLastRun.Time.Equal(wantLastRun) {
		t.Fatalf("persisted parent stats=(%d,%v), want (7,%v)", persistedRunCount, persistedLastRun, wantLastRun)
	}
	var runCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_job_runs WHERE job_id='stable-a'`).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 2 {
		t.Fatalf("run evidence count=%d, want 2", runCount)
	}
	var shared, moved string
	if err := db.QueryRow(`SELECT value FROM cron_job_state WHERE job_id='stable-a' AND key='shared'`).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	if shared != "survivor-value" {
		t.Fatalf("state survivor value=%q", shared)
	}
	if err := db.QueryRow(`SELECT value FROM cron_job_state WHERE job_id='stable-a' AND key='loser-only'`).Scan(&moved); err != nil {
		t.Fatal(err)
	}
	if moved != "must-move" {
		t.Fatalf("loser-only state=%q", moved)
	}
	var loserRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_jobs WHERE id='stable-b'`).Scan(&loserRows); err != nil {
		t.Fatal(err)
	}
	if loserRows != 0 {
		t.Fatalf("duplicate parent rows=%d, want 0", loserRows)
	}
	var parentAudit, stateAudit int
	if err := db.QueryRow(`SELECT
		SUM(CASE WHEN event_kind='merge_parent' THEN 1 ELSE 0 END),
		SUM(CASE WHEN event_kind='state_conflict' THEN 1 ELSE 0 END)
		FROM cron_job_merge_audit WHERE survivor_job_id='stable-a' AND loser_job_id='stable-b'`).Scan(&parentAudit, &stateAudit); err != nil {
		t.Fatal(err)
	}
	if parentAudit != 1 || stateAudit != 1 {
		t.Fatalf("audit parent/state=%d/%d, want 1/1", parentAudit, stateAudit)
	}
}
