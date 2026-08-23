package cron

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

type bug20260801003PromptUpdateRow struct {
	id           string
	name         string
	schedule     string
	sourcePrompt string
	userID       string
	status       string
	specJSON     string
}

func newBUG20260801003PromptUpdateScheduler(
	t *testing.T,
	compiler JobSpecCompiler,
) (*Scheduler, *sql.DB) {
	t.Helper()
	db := setupTestDB(t)
	scheduler := NewScheduler(
		db,
		compiler,
		NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()),
	)
	if err := scheduler.Init(context.Background()); err != nil {
		db.Close()
		t.Fatalf("init scheduler: %v", err)
	}
	return scheduler, db
}

func addBUG20260801003PromptUpdateOldJob(t *testing.T, scheduler *Scheduler, id, owner string) *Job {
	t.Helper()
	job := &Job{
		ID:           id,
		Name:         "stable prompt job",
		Type:         JobTypeCron,
		Schedule:     "@daily",
		UserID:       owner,
		Status:       StatusActive,
		SourcePrompt: "old prompt",
		Spec:         minimalSpec(),
	}
	if err := scheduler.AddJob(context.Background(), job); err != nil {
		t.Fatalf("add old job: %v", err)
	}
	return job
}

func snapshotBUG20260801003PromptUpdateRow(
	t *testing.T,
	db *sql.DB,
	jobID string,
) bug20260801003PromptUpdateRow {
	t.Helper()
	var row bug20260801003PromptUpdateRow
	err := db.QueryRow(
		`SELECT id, name, schedule, source_prompt, user_id, status, spec_json
		 FROM cron_jobs WHERE id = ?`,
		jobID,
	).Scan(
		&row.id,
		&row.name,
		&row.schedule,
		&row.sourcePrompt,
		&row.userID,
		&row.status,
		&row.specJSON,
	)
	if err != nil {
		t.Fatalf("snapshot cron job %q: %v", jobID, err)
	}
	return row
}

func countBUG20260801003PromptUpdateRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_jobs`).Scan(&count); err != nil {
		t.Fatalf("count cron jobs: %v", err)
	}
	return count
}

func assertBUG20260801003PromptUpdateOldJobUnchanged(
	t *testing.T,
	scheduler *Scheduler,
	db *sql.DB,
	before bug20260801003PromptUpdateRow,
) {
	t.Helper()
	after := snapshotBUG20260801003PromptUpdateRow(t, db, before.id)
	if after != before {
		t.Fatalf("old cron job changed: before=%+v after=%+v", before, after)
	}
	if count := countBUG20260801003PromptUpdateRows(t, db); count != 1 {
		t.Fatalf("failed update created or removed an extra row: count=%d", count)
	}
	if got, ok := scheduler.GetJob(context.Background(), before.id); !ok || got.ID != before.id {
		t.Fatalf("old cron job missing from memory after failed update: ok=%v job=%+v", ok, got)
	}
}

func TestBUG20260801003PromptUpdateSameOwnerReplacesAtomically(t *testing.T) {
	scheduler, db := newBUG20260801003PromptUpdateScheduler(t, &stubCompiler{
		ret: &JobSpec{Runtime: RuntimeStarlark, Script: `emit("new generation")`, TimeoutSec: 30},
	})
	defer db.Close()

	old := addBUG20260801003PromptUpdateOldJob(t, scheduler, "prompt-update-old", "owner-a")
	replacement, err := scheduler.ReplaceJobForOwner(
		context.Background(),
		old.ID,
		"owner-a",
		AddJobRequest{
			Name:     "replacement prompt job",
			Schedule: "@hourly",
			Prompt:   "new prompt",
			UserID:   "forged-owner",
		},
		"",
		"",
		nil,
	)
	if err != nil {
		t.Fatalf("same-owner replacement: %v", err)
	}
	if replacement == nil || replacement.ID == "" || replacement.ID == old.ID {
		t.Fatalf("replacement identity invalid: %+v", replacement)
	}
	if replacement.UserID != "owner-a" {
		t.Fatalf("replacement owner was not bound to trusted owner: %q", replacement.UserID)
	}
	if _, ok := scheduler.GetJob(context.Background(), old.ID); ok {
		t.Fatal("old job remains in memory after successful replacement")
	}
	if got, ok := scheduler.GetJob(context.Background(), replacement.ID); !ok || got.ID != replacement.ID {
		t.Fatalf("replacement missing from memory: ok=%v job=%+v", ok, got)
	}
	if count := countBUG20260801003PromptUpdateRows(t, db); count != 1 {
		t.Fatalf("successful replacement left %d cron rows, want 1", count)
	}
	var sourcePrompt, ownerID, schedule string
	err = db.QueryRow(
		`SELECT source_prompt, user_id, schedule FROM cron_jobs WHERE id = ?`,
		replacement.ID,
	).Scan(&sourcePrompt, &ownerID, &schedule)
	if err != nil {
		t.Fatalf("read replacement row: %v", err)
	}
	if sourcePrompt != "new prompt" || ownerID != "owner-a" || schedule != "@hourly" {
		t.Fatalf("replacement row mismatch: prompt=%q owner=%q schedule=%q", sourcePrompt, ownerID, schedule)
	}
	var oldRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_jobs WHERE id = ?`, old.ID).Scan(&oldRows); err != nil {
		t.Fatalf("count old cron rows: %v", err)
	}
	if oldRows != 0 {
		t.Fatalf("old DB row remains after replacement: %d", oldRows)
	}
}

func TestBUG20260801003PromptUpdateWrongOwnerAndUnknownAreOpaque(t *testing.T) {
	scheduler, db := newBUG20260801003PromptUpdateScheduler(t, &stubCompiler{
		ret: &JobSpec{Runtime: RuntimeStarlark, Script: `emit("candidate")`, TimeoutSec: 30},
	})
	defer db.Close()
	old := addBUG20260801003PromptUpdateOldJob(t, scheduler, "prompt-update-owned", "owner-a")
	before := snapshotBUG20260801003PromptUpdateRow(t, db, old.ID)

	request := AddJobRequest{Name: "candidate", Schedule: "@hourly", Prompt: "candidate prompt"}
	crossOwnerErr := func() error {
		_, err := scheduler.ReplaceJobForOwner(
			context.Background(), old.ID, "owner-b", request, "", "", nil,
		)
		return err
	}()
	unknownIDErr := func() error {
		_, err := scheduler.ReplaceJobForOwner(
			context.Background(), "missing-prompt-job", "owner-a", request, "", "", nil,
		)
		return err
	}()
	if !errors.Is(crossOwnerErr, ErrCronJobNotFound) || !errors.Is(unknownIDErr, ErrCronJobNotFound) {
		t.Fatalf("wrong-owner/unknown errors are not opaque not-found: cross=%v unknown=%v", crossOwnerErr, unknownIDErr)
	}
	if crossOwnerErr.Error() != unknownIDErr.Error() {
		t.Fatalf("wrong-owner error leaks existence: cross=%q unknown=%q", crossOwnerErr, unknownIDErr)
	}
	assertBUG20260801003PromptUpdateOldJobUnchanged(t, scheduler, db, before)
}

func TestBUG20260801003PromptUpdateBuildFailureKeepsOldJob(t *testing.T) {
	scheduler, db := newBUG20260801003PromptUpdateScheduler(t, &stubCompiler{err: errStubCompile})
	defer db.Close()
	old := addBUG20260801003PromptUpdateOldJob(t, scheduler, "prompt-update-build-failure", "owner-a")
	before := snapshotBUG20260801003PromptUpdateRow(t, db, old.ID)

	_, err := scheduler.ReplaceJobForOwner(
		context.Background(), old.ID, "owner-a",
		AddJobRequest{Name: "candidate", Schedule: "@hourly", Prompt: "compile failure"},
		"", "", nil,
	)
	if !errors.Is(err, errStubCompile) {
		t.Fatalf("expected compiler error, got %v", err)
	}
	assertBUG20260801003PromptUpdateOldJobUnchanged(t, scheduler, db, before)
}
