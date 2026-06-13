package cron

// BUG-20260613 (audit M5): the sandbox is stdlib-only — the AST validator
// enforces a stdlib import whitelist — so any third-party dependency is
// unsatisfiable and would only drive the executor into its dead pip path
// (unreliable on the sandboxed host). The compile boundary now forces deps
// empty via normalizeCompiledSpec, so a venv build that can only fail is never
// attempted, whatever the model emitted.

import "testing"

func TestBug20260613_NormalizeForcesEmptyDeps(t *testing.T) {
	spec := &JobSpec{Runtime: "", TimeoutSec: 0, Deps: []string{"requests", "beautifulsoup4>=4.0"}}
	normalizeCompiledSpec(spec)

	if len(spec.Deps) != 0 {
		t.Errorf("deps must be forced empty (stdlib-only sandbox, pip is a dead path), got %v", spec.Deps)
	}
	if spec.Runtime != "python3" {
		t.Errorf("runtime must default to python3, got %q", spec.Runtime)
	}
	if spec.TimeoutSec != defaultScriptTimeoutSec {
		t.Errorf("zero timeout must resolve to default %d, got %d", defaultScriptTimeoutSec, spec.TimeoutSec)
	}
}

func TestBug20260613_NormalizePreservesExplicitTimeoutAndRuntime(t *testing.T) {
	spec := &JobSpec{Runtime: "python3", TimeoutSec: 120, Deps: nil}
	normalizeCompiledSpec(spec)
	if spec.TimeoutSec != 120 {
		t.Errorf("explicit timeout must be preserved, got %d", spec.TimeoutSec)
	}
	if len(spec.Deps) != 0 {
		t.Errorf("deps must stay empty, got %v", spec.Deps)
	}
}
