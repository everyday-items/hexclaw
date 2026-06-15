package cron

// BUG-20260615: the cron API only had a "prompt -> LLM-compiled script" path
// (AddJobFromPrompt). A weak compile model (glm-4-flash) repeatedly failed to
// emit a valid script for the Baidu hot-search job, so it silently degraded to
// agent mode and then failed at run time (browser tool 20KB cap vs titles at
// ~22.7KB). There was no first-class way to admit a vetted, deterministic
// pre-authored script — exactly what mechanical fetch->parse->store jobs need.
//
// AddJobFromScript closes that gap: it runs the supplied script through the same
// validateSpec chain the compiler output goes through, then admits it as a live
// script-mode job (zero LLM at tick time). These tests lock that contract.

import (
	"context"
	"strings"
	"testing"
)

func newScriptSeam(t *testing.T) *Scheduler {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	s := NewScheduler(db, &stubCompiler{}, NewScriptExecutor().WithWorkdir(t.TempDir()))
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s
}

func TestBug20260615_AddJobFromScript_CreatesLiveScriptModeJob(t *testing.T) {
	hasPython3(t)
	s := newScriptSeam(t)
	script := "import json\n" +
		"print(json.dumps({\"status\": \"success\", \"data\": \"ok\"}))\n"

	job, err := s.AddJobFromScript(context.Background(), AddJobRequest{
		Name: "probe", Schedule: "@daily", Prompt: "collect something", UserID: "u1",
	}, RuntimePython3, script)
	if err != nil {
		t.Fatalf("AddJobFromScript: %v", err)
	}
	if job.Spec.Runtime != "python3" {
		t.Errorf("runtime = %q, want python3", job.Spec.Runtime)
	}
	if job.Spec.Script != script {
		t.Errorf("script not stored verbatim")
	}
	// Must be live in scheduler memory immediately (hot-effective, no restart).
	if got, ok := s.GetJob(context.Background(), job.ID); !ok || got.Spec.Runtime != "python3" {
		t.Errorf("job must be live in scheduler memory as script mode")
	}
}

func TestBug20260615_AddJobFromScript_RejectsUnsafeScript(t *testing.T) {
	hasPython3(t)
	s := newScriptSeam(t)
	// Forbidden call (os.system) must be rejected by validateSpec — never admitted.
	unsafe := "import os\n" +
		"os.system(\"echo pwned\")\n" +
		"import json\n" +
		"print(json.dumps({\"status\": \"success\"}))\n"

	_, err := s.AddJobFromScript(context.Background(), AddJobRequest{
		Name: "evil", Schedule: "@daily", Prompt: "p", UserID: "u1",
	}, RuntimePython3, unsafe)
	if err == nil {
		t.Fatal("unsafe script must be rejected, not admitted")
	}
	if !strings.Contains(err.Error(), "validation") && !strings.Contains(err.Error(), "禁用") {
		t.Errorf("rejection should cite validation failure, got: %v", err)
	}
}

func TestBug20260615_AddJobFromScript_RejectsEmptyScript(t *testing.T) {
	s := newScriptSeam(t)
	if _, err := s.AddJobFromScript(context.Background(), AddJobRequest{
		Name: "x", Schedule: "@daily", Prompt: "p", UserID: "u1",
	}, "", "   "); err == nil {
		t.Fatal("empty script must be rejected")
	}
}

// F-4: engineFor must return nil for an unknown runtime (not silently fall back
// to Starlark) so executeJob surfaces an explicit error instead of running a
// python script on the wrong engine.
func TestBug20260615_EngineFor_NilForUnknownRuntime(t *testing.T) {
	s := newScriptSeam(t)
	if s.engineFor("ruby") != nil {
		t.Error("unknown runtime must resolve to nil, not a silent starlark fallback")
	}
	if s.engineFor(RuntimeStarlark) == nil {
		t.Error("starlark runtime must resolve to its engine")
	}
}
