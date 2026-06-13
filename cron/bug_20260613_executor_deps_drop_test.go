package cron

// BUG-20260613 (review F1, M5 residual): the compile boundary forces deps
// empty, but a spec PERSISTED before that fix still carries deps, and the
// executor's `len(spec.Deps) > 0` branch drives it into the venv/pip path —
// unreliable on the stdlib-only sandbox host. The executor must ignore deps
// entirely (the AST validator already forbids non-stdlib imports), so the pip
// path is never taken regardless of what a legacy spec carries.

import (
	"context"
	"os"
	"testing"
)

func TestBug20260613_ExecutorIgnoresLegacyDeps(t *testing.T) {
	workdir := t.TempDir()
	venvCache := t.TempDir()
	e := NewScriptExecutor().WithWorkdir(workdir).WithVenvCache(venvCache)

	// A legacy-style spec: carries deps, but the script is pure stdlib.
	spec := &JobSpec{
		Runtime:    "python3",
		Script:     `import json; print(json.dumps({"status": "success", "data": "ok"}))`,
		Deps:       []string{"requests", "beautifulsoup4"},
		TimeoutSec: 30,
		Compiled:   CompileMeta{Hash: "legacyhash"},
	}

	res, err := e.Run(context.Background(), spec)

	// Core invariant: no venv was built — the pip dead path was not taken.
	entries, _ := os.ReadDir(venvCache)
	if len(entries) != 0 {
		t.Errorf("executor must NOT build a venv for a stdlib-only sandbox, found %d entries in venv cache", len(entries))
	}

	// And the script still ran to success via the system interpreter.
	if err != nil || res == nil {
		t.Fatalf("a stdlib script must run despite legacy deps, got res=%v err=%v", res, err)
	}
	if res.Status != "success" {
		t.Errorf("status must be success, got %q (err=%q)", res.Status, res.Error)
	}
}
