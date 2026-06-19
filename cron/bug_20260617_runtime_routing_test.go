package cron

import "testing"

// Bug 2026-06-17: a weak compiler model (e.g. glm-4-flash) declared runtime "python3"
// while emitting a Starlark script (emit/collect). validateCompiledScript then ran the
// python validator — which passes, because `emit(collect())` is valid python *syntax*
// (NameError is a runtime error) — so the spec persisted as python3 and execution routed
// to the python3 engine, dying with "NameError: name 'emit' is not defined".
//
// The compiler's system prompt exclusively teaches Starlark host builtins, so its output
// must pin runtime to starlark regardless of the model's declared value. looksLikeStarlark
// backstops jobs already persisted as python3 so they self-heal at execution.

func TestNormalizeCompiledSpecPinsStarlarkOverDeclaredPython3(t *testing.T) {
	spec := &JobSpec{
		Runtime: RuntimePython3,
		Script:  "def collect():\n    return []\nemit(collect())\n",
	}
	normalizeCompiledSpec(spec)
	if spec.Runtime != RuntimeStarlark {
		t.Fatalf("compiler output must pin runtime to starlark, got %q", spec.Runtime)
	}
}

func TestLooksLikeStarlarkBackstop(t *testing.T) {
	starlark := []string{
		"emit(collect())",
		"id = kb_ingest(\"t\", \"c\", \"s\")",
		"r = http_get(\"https://example.com\")",
		"data = json_decode(r[\"body\"])",
	}
	for _, s := range starlark {
		if !looksLikeStarlark(s) {
			t.Errorf("expected Starlark detection for %q", s)
		}
	}
	python := []string{
		"import os\nprint(os.getcwd())",
		"x = [1, 2, 3]\nprint(sum(x))",
	}
	for _, s := range python {
		if looksLikeStarlark(s) {
			t.Errorf("plain python must not be detected as Starlark: %q", s)
		}
	}
}
