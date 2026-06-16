package cron

// BUG-20260615: boundary/exception coverage for the cron "script run → KB write"
// main chain. Happy paths live in bug_20260615_f3_kb_ingest_test.go; here we pin
// the edges:
//   - kb_ingest empty/whitespace title, empty/whitespace content, oversize content
//   - a script that emits status="error" must NOT ingest, and surfaces the error
//   - deliverableContent regressions for the just-changed object-data handling:
//       object data with a readable field (message/summary/title/text) is delivered;
//       object data with no readable field is suppressed (never a raw-JSON dump).
// No source changes — these lock the existing engine + agent_mode contract.

import (
	"context"
	"strings"
	"testing"
)

// recordingKBIngest is a test KB writer that records every call and returns a
// fixed id. The bool reports whether it was ever invoked, so a test can assert
// a rejected ingest never reached the writer.
type recordingKBIngest struct {
	called  int
	title   string
	content string
	source  string
}

func (r *recordingKBIngest) fn(_ context.Context, title, content, source string) (string, error) {
	r.called++
	r.title, r.content, r.source = title, content, source
	return "doc-rec", nil
}

func runStarlark(t *testing.T, eng *StarlarkEngine, script string) *RunResult {
	t.Helper()
	res, err := eng.Execute(context.Background(), &JobSpec{Runtime: RuntimeStarlark, Script: script, TimeoutSec: 10})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

// ── kb_ingest content boundaries ──────────────────────────────────────────

// An empty title is allowed by kb_ingest (the title?/upsert layer tolerates it:
// an empty title always inserts). The writer must still receive the content.
func TestBug20260615_KBIngest_EmptyTitleStillIngests(t *testing.T) {
	rec := &recordingKBIngest{}
	eng := NewStarlarkEngine()
	eng.SetKBIngest(rec.fn)
	script := `
def run():
    res = kb_ingest(title = "", content = "body text", source = "cron-x")
    return {"status": "success", "data": {"id": res["id"]}}
emit(run())`
	res := runStarlark(t, eng, script)
	if res.Status != "success" {
		t.Fatalf("empty title must still ingest, status=%q err=%q", res.Status, res.Error)
	}
	if rec.called != 1 || rec.title != "" || rec.content != "body text" {
		t.Errorf("writer got called=%d title=%q content=%q", rec.called, rec.title, rec.content)
	}
}

// Whitespace-only content must be rejected at the builtin BEFORE the writer is
// called — content must not be empty is the engine's own guard.
func TestBug20260615_KBIngest_WhitespaceContentRejectedBeforeWriter(t *testing.T) {
	rec := &recordingKBIngest{}
	eng := NewStarlarkEngine()
	eng.SetKBIngest(rec.fn)
	script := `
def run():
    return {"status": "success", "data": kb_ingest(title = "T", content = "\n\t  ", source = "s")}
emit(run())`
	res := runStarlark(t, eng, script)
	if res.Status != "error" || !strings.Contains(res.Error, "content must not be empty") {
		t.Errorf("whitespace content must error, status=%q err=%q", res.Status, res.Error)
	}
	if rec.called != 0 {
		t.Errorf("writer must NOT be called for empty content, called=%d", rec.called)
	}
}

// Oversize content (1 MiB) must pass straight through — the engine imposes no
// length cap, so a large collected page reaches the writer intact. This guards
// against a silent truncation regression.
func TestBug20260615_KBIngest_OversizeContentPassesThrough(t *testing.T) {
	rec := &recordingKBIngest{}
	eng := NewStarlarkEngine()
	eng.SetKBIngest(rec.fn)
	// Build a ~1 MiB string inside Starlark via repeated concatenation in a loop.
	script := `
def run():
    block = "x" * 1024
    big = ""
    for i in range(1024):
        big += block
    res = kb_ingest(title = "big", content = big, source = "s")
    return {"status": "success", "data": {"id": res["id"], "n": len(big)}}
emit(run())`
	res := runStarlark(t, eng, script)
	if res.Status != "success" {
		t.Fatalf("oversize content must ingest, status=%q err=%q", res.Status, res.Error)
	}
	if rec.called != 1 {
		t.Fatalf("writer must be called once, called=%d", rec.called)
	}
	if len(rec.content) != 1024*1024 {
		t.Errorf("content must reach the writer un-truncated, got %d bytes want %d", len(rec.content), 1024*1024)
	}
}

// ── emit error status: no ingest, error surfaced ──────────────────────────

// A script that hits an error path can choose to skip kb_ingest and emit
// status="error" with a reason. The writer is never called and the RunResult
// carries the error verbatim (so the scheduler records a failure, not success).
func TestBug20260615_ScriptEmitsErrorStatus_NoIngest(t *testing.T) {
	rec := &recordingKBIngest{}
	eng := NewStarlarkEngine()
	eng.SetKBIngest(rec.fn)
	script := `
def run():
    resp = {"status": 500}
    if resp["status"] != 200:
        return {"status": "error", "error": "upstream returned %d" % resp["status"]}
    kb_ingest(title = "T", content = "never reached", source = "s")
    return {"status": "success"}
emit(run())`
	res := runStarlark(t, eng, script)
	if res.Status != "error" {
		t.Fatalf("emit status=error must map to error, got status=%q", res.Status)
	}
	if !strings.Contains(res.Error, "upstream returned 500") {
		t.Errorf("error reason must be surfaced, got %q", res.Error)
	}
	if rec.called != 0 {
		t.Errorf("an error-status run must not ingest, writer called=%d", rec.called)
	}
}

// A runtime exception inside the script (e.g. KeyError) must also avoid any
// partial ingest already committed BEFORE the throw is not our concern here;
// this case throws before kb_ingest, so the writer is never called and the run
// is an error carrying the backtrace.
func TestBug20260615_ScriptRuntimeError_NoIngest(t *testing.T) {
	rec := &recordingKBIngest{}
	eng := NewStarlarkEngine()
	eng.SetKBIngest(rec.fn)
	script := `
def run():
    d = {"a": 1}
    boom = d["missing_key"]   # raises before any ingest
    kb_ingest(title = "T", content = "unreached", source = "s")
    return {"status": "success"}
emit(run())`
	res := runStarlark(t, eng, script)
	if res.Status != "error" || res.Error == "" {
		t.Fatalf("runtime error must map to error with a message, status=%q err=%q", res.Status, res.Error)
	}
	if rec.called != 0 {
		t.Errorf("a throwing run must not ingest, writer called=%d", rec.called)
	}
}

// ── deliverableContent regression: object data field selection ────────────

// ③ Object data carrying a "message" field → deliverableContent returns the
// message (regression for this session's deliverableContent change). Also
// verifies the field priority order message > summary > title > text.
func TestBug20260615_DeliverableContent_ObjectDataReadableFields(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
		want string
	}{
		{"message wins", map[string]any{"message": "M", "summary": "S", "title": "Ti", "text": "Te"}, "M"},
		{"summary when no message", map[string]any{"summary": "S", "title": "Ti", "text": "Te"}, "S"},
		{"title when no message/summary", map[string]any{"title": "Ti", "text": "Te"}, "Ti"},
		{"text last resort", map[string]any{"text": "Te"}, "Te"},
		{"message trimmed", map[string]any{"message": "  spaced  "}, "spaced"},
		// A blank message must be skipped, falling through to the next readable field.
		{"blank message falls through to summary", map[string]any{"message": "   ", "summary": "S"}, "S"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deliverableContent(&RunResult{Data: c.data})
			if got != c.want {
				t.Errorf("deliverableContent(%v) = %q, want %q", c.data, got, c.want)
			}
		})
	}
}

// ④ Object data with NO readable field must be suppressed — the scheduler must
// never dump a raw JSON object as a user notification (regression). Even when
// Stdout holds the raw contract line, the result is "" (suppressed).
func TestBug20260615_DeliverableContent_OpaqueObjectSuppressed(t *testing.T) {
	cases := []map[string]any{
		{"count": 3, "ok": true},                        // no string fields at all
		{"message": "", "summary": "   ", "title": ""},  // all readable fields blank
		{"msg": "wrong key", "body": "also wrong"},      // non-contract keys only
		{"items": []any{"a", "b"}},                      // nested, no readable scalar
	}
	for i, data := range cases {
		got := deliverableContent(&RunResult{
			Data:   data,
			Stdout: `{"status":"success","data":{"count":3}}`,
		})
		if got != "" {
			t.Errorf("case %d: opaque object data must be suppressed, got %q", i, got)
		}
		if strings.Contains(got, "{") || strings.Contains(got, "count") {
			t.Errorf("case %d: must never JSON-dump object data, got %q", i, got)
		}
	}
}

// A readable field whose value is the wrong type (number, not string) must not
// crash and must not be delivered — readableField only accepts string values.
func TestBug20260615_DeliverableContent_NonStringReadableFieldIgnored(t *testing.T) {
	got := deliverableContent(&RunResult{Data: map[string]any{"message": 42, "title": "fallback"}})
	if got != "fallback" {
		t.Errorf("non-string message must be ignored and fall through to title, got %q", got)
	}
}

// ⑥ A non-text payload (bare contract JSON line, no human-facing data) must not
// be delivered to any target — neither a desktop notify nor an IM deliverer.
// This is the end-to-end guard for deliverResult, not deliverableContent alone:
// an opaque object whose Stdout is the contract line produces zero deliveries.
func TestBug20260615_OpaquePayloadNotDeliveredEndToEnd(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var notified int
	var imCalls int
	s.SetNotifier(func(_ *Job, _, _, _ string) { notified++ })
	s.SetDeliverer(func(_ *Job, _, _ string) error { imCalls++; return nil })

	// chat + feishu targets, but the payload is opaque (count only) with a raw
	// contract Stdout line: nothing readable, so neither target fires.
	job := &Job{ID: "job-opaque", Name: "采集", UserID: "u1", Deliver: []string{"chat", "feishu"},
		Spec: &JobSpec{Runtime: RuntimeStarlark}}
	s.deliverResult(job, &RunResult{
		Status: "success",
		Data:   map[string]any{"count": 3},
		Stdout: `{"status":"success","data":{"count":3}}`,
	})

	if notified != 0 {
		t.Errorf("opaque payload must not produce a desktop notification, got %d", notified)
	}
	if imCalls != 0 {
		t.Errorf("opaque payload must not reach the IM deliverer, got %d", imCalls)
	}
}
