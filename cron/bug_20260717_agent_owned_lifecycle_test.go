package cron

import (
	"context"
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
