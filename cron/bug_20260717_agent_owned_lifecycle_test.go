package cron

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestBug20260717_RemoveJobsBySourceKeyPrefixIsScopedAndAtomic(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}

	add := func(userID, sourceKey string) *Job {
		t.Helper()
		job, err := s.UpsertJobFromScript(ctx, AddJobRequest{
			Name:      sourceKey,
			Schedule:  "@daily",
			UserID:    userID,
			SourceKey: sourceKey,
		}, "python3", minimalSpec().Script)
		if err != nil {
			t.Fatalf("add %s: %v", sourceKey, err)
		}
		return job
	}

	ownedA := add("k12", "agent-a/daily-reminder")
	ownedB := add("another-user", "agent-a/weekly-sheet")
	unrelated := add("k12", "agent-ab/daily-reminder")

	detached, err := s.RemoveJobsBySourceKeyPrefix(ctx, "agent-a/")
	if err != nil {
		t.Fatalf("detach agent-owned jobs: %v", err)
	}
	if len(detached) != 2 {
		t.Fatalf("detached=%d, want 2", len(detached))
	}
	if _, ok := s.GetJob(ctx, ownedA.ID); ok {
		t.Fatalf("owned job %s remained in scheduler memory", ownedA.ID)
	}
	if _, ok := s.GetJob(ctx, ownedB.ID); ok {
		t.Fatalf("owned job %s remained in scheduler memory", ownedB.ID)
	}
	if _, ok := s.GetJob(ctx, unrelated.ID); !ok {
		t.Fatalf("prefix boundary deleted unrelated job %s", unrelated.ID)
	}

	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("persisted cron rows=%d, want 1", remaining)
	}
}

func TestBug20260717_RemoveJobsBySourceKeyPrefixRejectsEmptyPrefix(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	ctx := context.Background()
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveJobsBySourceKeyPrefix(ctx, "  "); err == nil {
		t.Fatal("empty prefix must fail closed instead of deleting every cron job")
	}
}

func TestRemoveJobsBySourceKeyPrefixDeletesPausedDurableJobsAfterRestart(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	active, err := s.UpsertJobFromScript(ctx, AddJobRequest{
		Name: "active", Schedule: "@daily", UserID: "desktop-user", SourceKey: "agent-a/weekly-sheet",
	}, RuntimeStarlark, `emit({"status": "success"})`)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := s.UpsertJobFromScript(ctx, AddJobRequest{
		Name: "paused", Schedule: "@daily", UserID: "legacy-owner", SourceKey: "agent-a/return-reminder", Paused: true,
	}, RuntimeStarlark, `emit({"status": "success"})`)
	if err != nil {
		t.Fatal(err)
	}

	restarted := newTestScheduler(t, db)
	if err := restarted.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.GetJob(ctx, paused.ID); ok {
		t.Fatal("fixture invalid: paused job must not be loaded into active map")
	}
	detached, err := restarted.RemoveJobsBySourceKeyPrefix(ctx, "agent-a/")
	if err != nil {
		t.Fatal(err)
	}
	if len(detached) != 2 {
		t.Fatalf("durable cleanup must detach active and paused jobs, got %+v", detached)
	}
	if _, ok := restarted.GetJob(ctx, active.ID); ok {
		t.Fatal("active map still contains deleted job")
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs WHERE id IN (?, ?)`, active.ID, paused.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("durable cleanup left %d owned rows", remaining)
	}
}

func TestRemoveJobsBySourceKeyPrefixConcurrentEnsureKeepsDurableAndActiveStateAligned(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		sourceKey := fmt.Sprintf("agent-race-%d/weekly-sheet", i)
		req := AddJobRequest{
			Name: fmt.Sprintf("race-%d", i), Schedule: "@daily", UserID: "desktop-user", SourceKey: sourceKey,
		}
		job, err := s.UpsertJobFromScript(ctx, req, RuntimeStarlark, `emit({"status": "success"})`)
		if err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func(iteration int) {
			defer wg.Done()
			<-start
			_, removeErr := s.RemoveJobsBySourceKeyPrefix(ctx, fmt.Sprintf("agent-race-%d/", iteration))
			errs <- removeErr
		}(i)
		go func() {
			defer wg.Done()
			<-start
			_, _, ensureErr := s.EnsureJobFromScriptMissingOnly(ctx, req, RuntimeStarlark, `emit({"status": "success"})`)
			errs <- ensureErr
		}()
		close(start)
		wg.Wait()
		close(errs)
		for opErr := range errs {
			if opErr != nil {
				t.Fatal(opErr)
			}
		}

		var durableCount int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs WHERE id = ?`, job.ID).Scan(&durableCount); err != nil {
			t.Fatal(err)
		}
		if durableCount != 0 && durableCount != 1 {
			t.Fatalf("durable duplicate after remove/ensure race: %d", durableCount)
		}
		_, active := s.GetJob(ctx, job.ID)
		if active != (durableCount == 1) {
			t.Fatalf("durable/active divergence after remove/ensure race: durable=%d active=%v", durableCount, active)
		}
	}
}
