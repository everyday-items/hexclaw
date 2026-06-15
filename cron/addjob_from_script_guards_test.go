package cron

// AddJobFromScript admission guards that reject before any DB write — these
// complete the guard coverage alongside the unsafe/empty-script cases: a missing
// schedule and an unknown runtime must fail loudly, not admit a dead job.

import (
	"context"
	"strings"
	"testing"
)

func TestAddJobFromScript_RejectsEmptySchedule(t *testing.T) {
	s := NewScheduler(nil, nil, nil) // guard returns before AddJob/DB
	_, err := s.AddJobFromScript(context.Background(),
		AddJobRequest{Name: "x"}, RuntimeStarlark, `emit({"status":"success"})`)
	if err == nil || !strings.Contains(err.Error(), "schedule") {
		t.Fatalf("empty schedule must be rejected, got: %v", err)
	}
}

func TestAddJobFromScript_RejectsUnknownRuntime(t *testing.T) {
	s := NewScheduler(nil, nil, nil) // only starlark installed (scriptExec nil)
	_, err := s.AddJobFromScript(context.Background(),
		AddJobRequest{Name: "x", Schedule: "@every 1h"}, "ruby", `emit({"status":"success"})`)
	if err == nil || !strings.Contains(err.Error(), "unknown or unavailable runtime") {
		t.Fatalf("unknown runtime must be rejected, got: %v", err)
	}
	// python3 is also absent when scriptExec is nil — same guard.
	_, err = s.AddJobFromScript(context.Background(),
		AddJobRequest{Name: "x", Schedule: "@every 1h"}, RuntimePython3, `emit({"status":"success"})`)
	if err == nil || !strings.Contains(err.Error(), "unknown or unavailable runtime") {
		t.Fatalf("absent python3 runtime must be rejected, got: %v", err)
	}
}
