package cron

// BUG-20260613 (audit C2 + L2):
//   C2 — only agent jobs were delivered. Script jobs were assumed to
//        "self-deliver" by POSTing /api/v1/notify, but that endpoint was never
//        registered (a phantom), so script results reached nobody. The
//        scheduler now delivers BOTH runtimes through the job's deliver targets.
//   L2 — an IM target (feishu/...) only logged "kept in history" and never
//        sent. It now routes through an injected Deliverer.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBug20260613_DeliverableContentPrefersScriptData(t *testing.T) {
	// Script job: payload lives under Data; Stdout is the raw JSON line.
	got := deliverableContent(&RunResult{Data: "百度热搜：A、B、C", Stdout: `{"status":"success","data":"百度热搜：A、B、C"}`})
	if got != "百度热搜：A、B、C" {
		t.Errorf("script delivery must use the data field, got %q", got)
	}

	// Structured data with a human field → deliver that field, never raw JSON.
	gotMsg := deliverableContent(&RunResult{Data: map[string]any{
		"title": "百度热搜 2026-06-15", "count": 20,
		"message": "百度热搜 2026-06-15 · 收录 20 条",
	}})
	if gotMsg != "百度热搜 2026-06-15 · 收录 20 条" {
		t.Errorf("structured data must deliver its message field, got %q", gotMsg)
	}

	// Structured data with only a title (no message) → fall back to the title.
	gotTitle := deliverableContent(&RunResult{Data: map[string]any{"title": "周报 2026-06-15", "count": 7}})
	if gotTitle != "周报 2026-06-15" {
		t.Errorf("structured data should fall back to title, got %q", gotTitle)
	}

	// Structured data with no human field → suppressed, never a raw-JSON dump.
	gotBare := deliverableContent(&RunResult{Data: map[string]any{"count": 3}})
	if strings.Contains(gotBare, "{") || strings.Contains(gotBare, "count") {
		t.Errorf("opaque structured data must not be JSON-dumped to a notification, got %q", gotBare)
	}

	// Agent job: no Data, falls back to Stdout.
	gotAgent := deliverableContent(&RunResult{Stdout: "今日要点：A、B、C"})
	if gotAgent != "今日要点：A、B、C" {
		t.Errorf("agent delivery must fall back to stdout, got %q", gotAgent)
	}
}

// C2: a SCRIPT job's successful result must be delivered (desktop notify),
// where before nothing was delivered at all.
func TestBug20260613_ScriptResultIsDelivered(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var notified []string
	s.SetNotifier(func(_ *Job, _, title, body string) { notified = append(notified, title+"|"+body) })

	job := &Job{ID: "job-script", Name: "热搜", UserID: "u1", Deliver: []string{"chat"},
		Spec: &JobSpec{Runtime: "python3", Script: "x", TimeoutSec: 60}}
	result := &RunResult{Status: "success", Data: "热搜结果：A、B、C"}

	s.deliverResult(job, result)
	if len(notified) != 1 {
		t.Fatalf("script success must be delivered exactly once, got %d", len(notified))
	}
	if !strings.Contains(notified[0], "热搜结果") || !strings.Contains(notified[0], "热搜") {
		t.Errorf("delivery must carry job name + content, got %q", notified[0])
	}
}

// C2 (wiring): a SCRIPT job driven through executeJob — the real path — must
// deliver. This guards the executeJob call site itself, not just deliverResult
// in isolation (the bug was executeJob delivering only for agent runtime).
func TestBug20260613_ScriptJobDeliveredViaExecuteJob(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var notified []string
	s.SetNotifier(func(_ *Job, _, title, body string) { notified = append(notified, title+"|"+body) })

	job := &Job{
		ID: "job-exec-script", Name: "热搜采集", Type: JobTypeCron, Schedule: "@daily",
		UserID: "u1", Status: StatusActive, Deliver: []string{"chat"},
		SourcePrompt: "采集热搜",
		Spec: &JobSpec{
			Runtime:    "python3",
			Script:     `import json; print(json.dumps({"status": "success", "data": "热搜：A、B、C"}))`,
			TimeoutSec: 30,
		},
	}
	if err := s.AddJob(context.Background(), job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	s.executeJob(job)

	if len(notified) != 1 {
		t.Fatalf("a successful SCRIPT job must be delivered via executeJob, got %d notifications", len(notified))
	}
	if !strings.Contains(notified[0], "热搜：A、B、C") {
		t.Errorf("delivery must carry the script's data payload, got %q", notified[0])
	}
}

// L2: an IM deliver target routes through the injected Deliverer with the
// resolved content — not just a log line.
func TestBug20260613_IMTargetRoutesThroughDeliverer(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	type sent struct{ target, content string }
	var calls []sent
	s.SetDeliverer(func(_ *Job, target, content string) error {
		calls = append(calls, sent{target, content})
		return nil
	})

	job := &Job{ID: "job-im", Name: "日报", UserID: "u1", ChatID: "c-1", Deliver: []string{"feishu"},
		Spec: &JobSpec{Runtime: RuntimeAgent, TimeoutSec: 60}}
	s.deliverResult(job, &RunResult{Status: "success", Stdout: "今日总结完成。"})

	if len(calls) != 1 {
		t.Fatalf("IM target must route through the deliverer, got %d calls", len(calls))
	}
	if calls[0].target != "feishu" || !strings.Contains(calls[0].content, "今日总结完成") {
		t.Errorf("deliverer must receive target+content, got %+v", calls[0])
	}
}

// L2: without a wired deliverer, an IM target must NOT raise — it falls back to
// history-only and must not panic or block other targets.
func TestBug20260613_IMTargetNoDelivererIsSafe(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	_ = s.Init(context.Background())
	var notified int
	s.SetNotifier(func(_ *Job, _, _, _ string) { notified++ })

	// chat + feishu: chat delivers, feishu falls back to log (no deliverer).
	job := &Job{ID: "job-mix", Name: "混合", UserID: "u1", Deliver: []string{"chat", "feishu"},
		Spec: &JobSpec{Runtime: RuntimeAgent}}
	s.deliverResult(job, &RunResult{Status: "success", Stdout: "ok"})
	if notified != 1 {
		t.Errorf("the desktop target must still deliver even when an IM target has no deliverer, got %d", notified)
	}
}

// A deliverer error must not crash delivery of the other targets.
func TestBug20260613_DelivererErrorIsContained(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	_ = s.Init(context.Background())
	s.SetDeliverer(func(_ *Job, _, _ string) error { return errors.New("feishu down") })

	job := &Job{ID: "job-err", Name: "X", UserID: "u1", Deliver: []string{"feishu"},
		Spec: &JobSpec{Runtime: RuntimeAgent}}
	// Must not panic.
	s.deliverResult(job, &RunResult{Status: "success", Stdout: "content"})
}
