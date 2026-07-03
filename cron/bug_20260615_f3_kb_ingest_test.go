package cron

// BUG-20260615 F-3: a Starlark collector ingests into the local KB via the
// in-process kb_ingest builtin instead of an HTTP round-trip. These lock:
// (1) kb_ingest forwards to the injected writer and returns its id, (2) an
// unset writer errors loudly instead of silently dropping content, and (3) the
// Scheduler forwards its writer down to the Starlark engine.

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestBug20260615_F3_ArchivedScripts_UseKBIngest locks the migrated collectors:
// every archived .star must pass the real engine validator, ingest via kb_ingest,
// and never http_post to a loopback KB endpoint. Note: loopback is not blocked
// (Starlark http has no SSRF guard by design); kb_ingest is preferred because it
// is in-process and auth-free, not because a POST would be refused.
func TestBug20260615_F3_ArchivedScripts_UseKBIngest(t *testing.T) {
	for _, path := range []string{"scripts/baidu_hotsearch.star", "scripts/github_go_stars.star"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := string(b)
		if err := NewStarlarkEngine().Validate(src); err != nil {
			t.Errorf("%s must pass StarlarkEngine.Validate: %v", path, err)
		}
		if !strings.Contains(src, "kb_ingest(") {
			t.Errorf("%s must ingest via kb_ingest", path)
		}
		if strings.Contains(src, "127.0.0.1") || strings.Contains(src, "localhost") {
			t.Errorf("%s must not POST to a loopback KB endpoint (F-3)", path)
		}
	}
}

func TestBug20260615_F3_KBIngest_ForwardsAndReturnsID(t *testing.T) {
	var gotTitle, gotContent, gotSource string
	eng := NewStarlarkEngine()
	eng.SetKBIngest(func(_ context.Context, title, content, source string) (string, error) {
		gotTitle, gotContent, gotSource = title, content, source
		return "doc-123", nil
	})
	script := `
def run():
    res = kb_ingest(title = "T", content = "hello world", source = "cron-x")
    return {"status": "success", "data": {"id": res["id"]}}
emit(run())`
	res, err := eng.Execute(context.Background(), &JobSpec{Runtime: RuntimeStarlark, Script: script, TimeoutSec: 10})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("status=%q err=%q", res.Status, res.Error)
	}
	if gotTitle != "T" || gotContent != "hello world" || gotSource != "cron-x" {
		t.Errorf("kb_ingest got (%q, %q, %q)", gotTitle, gotContent, gotSource)
	}
	d, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("data not a map: %T", res.Data)
	}
	if d["id"] != "doc-123" {
		t.Errorf("returned id = %v, want doc-123", d["id"])
	}
}

func TestBug20260615_F3_KBIngest_UnsetErrorsLoudly(t *testing.T) {
	eng := NewStarlarkEngine() // no SetKBIngest
	script := `
def run():
    return {"status": "success", "data": kb_ingest(title = "T", content = "c", source = "s")}
emit(run())`
	res, err := eng.Execute(context.Background(), &JobSpec{Runtime: RuntimeStarlark, Script: script, TimeoutSec: 10})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != "error" || !strings.Contains(res.Error, "knowledge base is not enabled") {
		t.Errorf("unset kb_ingest must error, got status=%q err=%q", res.Status, res.Error)
	}
}

func TestBug20260615_F3_KBIngest_RejectsEmptyContent(t *testing.T) {
	eng := NewStarlarkEngine()
	eng.SetKBIngest(func(context.Context, string, string, string) (string, error) {
		t.Fatal("writer must not be called with empty content")
		return "", nil
	})
	script := `
def run():
    return {"status": "success", "data": kb_ingest(title = "T", content = "   ", source = "s")}
emit(run())`
	res, err := eng.Execute(context.Background(), &JobSpec{Runtime: RuntimeStarlark, Script: script, TimeoutSec: 10})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != "error" || !strings.Contains(res.Error, "content must not be empty") {
		t.Errorf("empty content must error, got status=%q err=%q", res.Status, res.Error)
	}
}

func TestBug20260615_F3_SchedulerForwardsKBIngest(t *testing.T) {
	s := NewScheduler(nil, nil, nil) // db/compiler/exec unused here
	called := false
	s.SetKBIngest(func(context.Context, string, string, string) (string, error) {
		called = true
		return "id", nil
	})
	eng, ok := s.engines[RuntimeStarlark].(*StarlarkEngine)
	if !ok {
		t.Fatal("scheduler is missing the Starlark engine")
	}
	if eng.kbIngest == nil {
		t.Fatal("scheduler did not forward kb_ingest to the Starlark engine")
	}
	if _, err := eng.kbIngest(context.Background(), "t", "c", "s"); err != nil {
		t.Fatalf("forwarded writer returned error: %v", err)
	}
	if !called {
		t.Fatal("forwarded writer was not the one we set")
	}
}
