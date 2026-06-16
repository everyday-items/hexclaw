package cron

// BUG-20260616: three cron defects.
//   C1 Deliver targets were written to cron_jobs.meta but never read back:
//      scanJobRow's SELECTs omitted the meta column and never called
//      parseJobMeta, so IM delivery silently reverted to ["chat"] after restart.
//   C2 @hourly used from.Truncate(time.Hour) (UTC-aligned), firing at HH:30 in
//      half-hour-offset zones (IST/Iran) instead of the local top-of-hour.
//   C3 5-field cron ANDed day-of-month and day-of-week; POSIX requires OR when
//      both are restricted.

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestBug20260616_DeliverSurvivesReload(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	s := NewScheduler(db, nil, nil)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	job := &Job{
		ID: "j1", Name: "deliver-job", Type: JobTypeCron, Schedule: "@daily",
		Spec:      &JobSpec{Runtime: "python3", Script: "x"},
		UserID:    "u1",
		Status:    StatusActive,
		Deliver:   []string{"feishu", "discord"},
		NextRunAt: time.Now().Add(time.Hour),
		CreatedAt: time.Now(),
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("addjob: %v", err)
	}

	// Simulate a restart: a fresh scheduler loads jobs from the same DB.
	s2 := NewScheduler(db, nil, nil)
	if err := s2.loadJobs(ctx); err != nil {
		t.Fatalf("loadJobs: %v", err)
	}
	loaded, ok := s2.GetJob(ctx, "j1")
	if !ok {
		t.Fatal("job not loaded after restart")
	}
	got := EffectiveDeliver(loaded)
	want := []string{"feishu", "discord"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Deliver lost across reload: want %v got %v", want, got)
	}
}

func TestBug20260616_HourlyHalfHourTimezone(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+30*60) // UTC+5:30
	from := time.Date(2026, 6, 16, 10, 15, 0, 0, ist)
	next, err := nextRunTime("@hourly", JobTypeCron, from)
	if err != nil {
		t.Fatalf("nextRunTime: %v", err)
	}
	if next.Minute() != 0 || next.Second() != 0 {
		t.Fatalf("@hourly must land on the local top-of-hour, got %s", next.Format("15:04:05"))
	}
	if !next.After(from) {
		t.Fatalf("next (%s) must be after from (%s)", next, from)
	}
}

func TestBug20260616_Cron5DomDowIsOr(t *testing.T) {
	// "0 0 13 * 5" = 13th OR Friday (POSIX). A Friday recurs every week, so the
	// next run must be within 7 days — not the next "13th that is also a Friday".
	from := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	next, err := nextRunTime("0 0 13 * 5", JobTypeCron, from)
	if err != nil {
		t.Fatalf("nextRunTime: %v", err)
	}
	if next.Sub(from) >= 7*24*time.Hour {
		t.Fatalf("DoM+DoW must be OR: expected a run within a week, got %s (%v away)",
			next.Format("2006-01-02"), next.Sub(from))
	}
	if next.Day() != 13 && next.Weekday() != time.Friday {
		t.Fatalf("matched day is neither the 13th nor a Friday: %s", next.Format("2006-01-02 Mon"))
	}
}
