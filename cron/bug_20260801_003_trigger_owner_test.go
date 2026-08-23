package cron

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newBUG20260801003TriggerOwnerScheduler(t *testing.T) (*Scheduler, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "trigger-owner.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	scheduler := NewScheduler(
		db,
		nil,
		NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()),
	)
	if err := scheduler.Init(context.Background()); err != nil {
		t.Fatalf("init scheduler: %v", err)
	}
	return scheduler, db
}

func TestBUG20260801003TriggerJobForOwnerIsOpaqueAndOwnerScoped(t *testing.T) {
	scheduler, db := newBUG20260801003TriggerOwnerScheduler(t)
	marker := filepath.Join(t.TempDir(), "executed.marker")
	script := fmt.Sprintf(
		"from pathlib import Path\nPath(%q).write_text('executed', encoding='utf-8')\nprint('{\"status\":\"success\"}')\n",
		marker,
	)
	job := &Job{
		ID:       "owner-b-job",
		Name:     "owner-b-job",
		Type:     JobTypeCron,
		Schedule: "@daily",
		UserID:   "owner-b",
		Spec:     &JobSpec{Runtime: RuntimePython3, Script: script},
	}
	if err := scheduler.AddJob(context.Background(), job); err != nil {
		t.Fatalf("add job: %v", err)
	}

	crossOwnerErr := scheduler.TriggerJobForOwner(context.Background(), job.ID, "owner-a")
	missingErr := scheduler.TriggerJobForOwner(context.Background(), "missing-job", "owner-a")
	if crossOwnerErr == nil || missingErr == nil {
		t.Fatalf("cross-owner/missing errors=%v/%v", crossOwnerErr, missingErr)
	}
	if crossOwnerErr.Error() != missingErr.Error() {
		t.Fatalf("cross-owner error leaks existence: cross=%q missing=%q",
			crossOwnerErr.Error(), missingErr.Error())
	}

	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("cross-owner trigger executed job; marker stat error=%v", err)
	}
	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_job_runs WHERE job_id = ?`, job.ID).Scan(&runs); err != nil {
		t.Fatalf("count cross-owner runs: %v", err)
	}
	if runs != 0 {
		t.Fatalf("cross-owner trigger persisted %d run(s)", runs)
	}

	if err := scheduler.TriggerJobForOwner(context.Background(), job.ID, "owner-b"); err != nil {
		t.Fatalf("same-owner trigger: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		markerExists := false
		if _, err := os.Stat(marker); err == nil {
			markerExists = true
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat marker: %v", err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM cron_job_runs WHERE job_id = ?`, job.ID).Scan(&runs); err != nil {
			t.Fatalf("count same-owner runs: %v", err)
		}
		if markerExists && runs == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("same-owner job did not execute: marker=%v runs=%d", markerExists, runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
