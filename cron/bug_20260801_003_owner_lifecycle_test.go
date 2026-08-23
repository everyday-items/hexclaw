package cron

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

type bug20260801003OwnerLifecycleSnapshot struct {
	rowCount      int
	jobExists     bool
	owner         string
	name          string
	schedule      string
	status        string
	sourcePrompt  string
	specJSON      string
	runCount      int
	inMemory      bool
	memoryOwner   string
	memoryStatus  JobStatus
}

func addBUG20260801003OwnerLifecycleJob(
	t *testing.T,
	scheduler *Scheduler,
	id, owner string,
	status JobStatus,
) {
	t.Helper()
	job := &Job{
		ID:           id,
		Name:         "Owner lifecycle probe",
		Type:         JobTypeCron,
		Schedule:     "@daily",
		UserID:       owner,
		Platform:     "api",
		ChatID:       "owner-lifecycle-chat",
		Status:       status,
		SourcePrompt: "owner lifecycle prompt",
		Spec: &JobSpec{
			Runtime:    RuntimeStarlark,
			Script:     `emit("owner lifecycle")`,
			TimeoutSec: 17,
		},
		Deliver: []string{"chat"},
	}
	if err := scheduler.AddJob(context.Background(), job); err != nil {
		t.Fatalf("add owner lifecycle job: %v", err)
	}
}

func snapshotBUG20260801003OwnerLifecycle(
	t *testing.T,
	db *sql.DB,
	scheduler *Scheduler,
	jobID string,
) bug20260801003OwnerLifecycleSnapshot {
	t.Helper()
	var got bug20260801003OwnerLifecycleSnapshot
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_jobs`).Scan(&got.rowCount); err != nil {
		t.Fatalf("count cron jobs: %v", err)
	}
	err := db.QueryRow(`
		SELECT user_id, name, schedule, status, source_prompt, spec_json, run_count
		FROM cron_jobs WHERE id = ?`, jobID).Scan(
		&got.owner,
		&got.name,
		&got.schedule,
		&got.status,
		&got.sourcePrompt,
		&got.specJSON,
		&got.runCount,
	)
	switch {
	case err == nil:
		got.jobExists = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		t.Fatalf("read cron job %q: %v", jobID, err)
	}
	if job, ok := scheduler.GetJob(context.Background(), jobID); ok {
		got.inMemory = true
		got.memoryOwner = job.UserID
		got.memoryStatus = job.Status
	}
	return got
}

func TestBUG20260801003OwnerLifecycleHidesCrossOwnerAndMissingWithoutMutation(t *testing.T) {
	type lifecycleAction struct {
		name          string
		initialStatus JobStatus
		invoke        func(context.Context, *Scheduler, string, string) error
	}
	actions := []lifecycleAction{
		{
			name:          "pause",
			initialStatus: StatusActive,
			invoke: func(ctx context.Context, scheduler *Scheduler, jobID, ownerID string) error {
				return scheduler.PauseJobForOwner(ctx, jobID, ownerID)
			},
		},
		{
			name:          "resume",
			initialStatus: StatusPaused,
			invoke: func(ctx context.Context, scheduler *Scheduler, jobID, ownerID string) error {
				return scheduler.ResumeJobForOwner(ctx, jobID, ownerID)
			},
		},
		{
			name:          "remove",
			initialStatus: StatusActive,
			invoke: func(ctx context.Context, scheduler *Scheduler, jobID, ownerID string) error {
				return scheduler.RemoveJobForOwner(ctx, jobID, ownerID)
			},
		},
	}

	for _, action := range actions {
		t.Run(action.name, func(t *testing.T) {
			scheduler, db := newBUG20260801003TriggerOwnerScheduler(t)
			addBUG20260801003OwnerLifecycleJob(
				t, scheduler, "owner-b-job", "owner-b", action.initialStatus,
			)
			before := snapshotBUG20260801003OwnerLifecycle(t, db, scheduler, "owner-b-job")

			crossOwnerErr := action.invoke(
				context.Background(), scheduler, "owner-b-job", "owner-a",
			)
			if !errors.Is(crossOwnerErr, ErrCronJobNotFound) {
				t.Fatalf("cross-owner %s error=%v, want ErrCronJobNotFound", action.name, crossOwnerErr)
			}
			if after := snapshotBUG20260801003OwnerLifecycle(t, db, scheduler, "owner-b-job"); after != before {
				t.Fatalf("cross-owner %s changed job: before=%+v after=%+v", action.name, before, after)
			}

			missingErr := action.invoke(
				context.Background(), scheduler, "missing-job", "owner-a",
			)
			if !errors.Is(missingErr, ErrCronJobNotFound) {
				t.Fatalf("missing %s error=%v, want ErrCronJobNotFound", action.name, missingErr)
			}
			if crossOwnerErr.Error() != missingErr.Error() {
				t.Fatalf("%s leaks target existence: cross=%q missing=%q",
					action.name, crossOwnerErr.Error(), missingErr.Error())
			}
			if after := snapshotBUG20260801003OwnerLifecycle(t, db, scheduler, "owner-b-job"); after != before {
				t.Fatalf("missing %s changed victim job: before=%+v after=%+v", action.name, before, after)
			}
		})
	}
}

func TestBUG20260801003OwnerLifecycleAllowsSameOwner(t *testing.T) {
	t.Run("pause", func(t *testing.T) {
		scheduler, db := newBUG20260801003TriggerOwnerScheduler(t)
		addBUG20260801003OwnerLifecycleJob(t, scheduler, "pause-owned", "owner-a", StatusActive)
		if err := scheduler.PauseJobForOwner(context.Background(), "pause-owned", "owner-a"); err != nil {
			t.Fatalf("same-owner pause: %v", err)
		}
		got := snapshotBUG20260801003OwnerLifecycle(t, db, scheduler, "pause-owned")
		if !got.jobExists || got.status != string(StatusPaused) || !got.inMemory || got.memoryStatus != StatusPaused {
			t.Fatalf("paused job was not updated durably and in memory: %+v", got)
		}
	})

	t.Run("resume", func(t *testing.T) {
		scheduler, db := newBUG20260801003TriggerOwnerScheduler(t)
		addBUG20260801003OwnerLifecycleJob(t, scheduler, "resume-owned", "owner-a", StatusPaused)
		if err := scheduler.ResumeJobForOwner(context.Background(), "resume-owned", "owner-a"); err != nil {
			t.Fatalf("same-owner resume: %v", err)
		}
		got := snapshotBUG20260801003OwnerLifecycle(t, db, scheduler, "resume-owned")
		if !got.jobExists || got.status != string(StatusActive) || !got.inMemory || got.memoryStatus != StatusActive {
			t.Fatalf("resumed job was not updated durably and in memory: %+v", got)
		}
	})

	t.Run("remove", func(t *testing.T) {
		scheduler, db := newBUG20260801003TriggerOwnerScheduler(t)
		addBUG20260801003OwnerLifecycleJob(t, scheduler, "remove-owned", "owner-a", StatusActive)
		if err := scheduler.RemoveJobForOwner(context.Background(), "remove-owned", "owner-a"); err != nil {
			t.Fatalf("same-owner remove: %v", err)
		}
		got := snapshotBUG20260801003OwnerLifecycle(t, db, scheduler, "remove-owned")
		if got.rowCount != 0 || got.jobExists || got.inMemory {
			t.Fatalf("removed job still exists: %+v", got)
		}
	})
}

func TestBUG20260801003ResumeReloadsPausedJobAfterRestart(t *testing.T) {
	writer, db := newBUG20260801003TriggerOwnerScheduler(t)
	addBUG20260801003OwnerLifecycleJob(t, writer, "paused-after-restart", "owner-a", StatusPaused)

	restarted := NewScheduler(db, nil, nil)
	if err := restarted.Init(context.Background()); err != nil {
		t.Fatalf("restart scheduler: %v", err)
	}
	if _, ok := restarted.GetJob(context.Background(), "paused-after-restart"); ok {
		t.Fatal("paused job unexpectedly loaded into the active runtime map before resume")
	}

	if err := restarted.ResumeJobForOwner(
		context.Background(), "paused-after-restart", "owner-a",
	); err != nil {
		t.Fatalf("resume paused job after restart: %v", err)
	}
	reloaded, ok := restarted.GetJob(context.Background(), "paused-after-restart")
	if !ok {
		t.Fatal("resumed job was not reloaded into the runtime map")
	}
	if reloaded.ID != "paused-after-restart" ||
		reloaded.Name != "Owner lifecycle probe" ||
		reloaded.UserID != "owner-a" ||
		reloaded.Schedule != "@daily" ||
		reloaded.Status != StatusActive ||
		reloaded.SourcePrompt != "owner lifecycle prompt" ||
		reloaded.Platform != "api" ||
		reloaded.ChatID != "owner-lifecycle-chat" ||
		len(reloaded.Deliver) != 1 || reloaded.Deliver[0] != "chat" ||
		reloaded.Spec == nil || reloaded.Spec.Runtime != RuntimeStarlark ||
		reloaded.Spec.Script != `emit("owner lifecycle")` || reloaded.Spec.TimeoutSec != 17 {
		t.Fatalf("resumed job was not fully reloaded: %+v", reloaded)
	}
	var durableStatus string
	if err := db.QueryRow(
		`SELECT status FROM cron_jobs WHERE id = ?`, "paused-after-restart",
	).Scan(&durableStatus); err != nil {
		t.Fatalf("read resumed durable status: %v", err)
	}
	if durableStatus != string(StatusActive) {
		t.Fatalf("durable resumed status=%q, want %q", durableStatus, StatusActive)
	}

	triggerErr := restarted.TriggerJobForOwner(
		context.Background(), "paused-after-restart", "owner-a",
	)
	if errors.Is(triggerErr, ErrCronJobNotFound) {
		t.Fatalf("resumed job still fails the runtime lookup: %v", triggerErr)
	}
	if !errors.Is(triggerErr, ErrCronExecutorUnavailable) {
		t.Fatalf("trigger error=%v, want ErrCronExecutorUnavailable after successful runtime lookup", triggerErr)
	}
}
