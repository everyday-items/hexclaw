package cron

// Builtin error-path coverage: a cron script passing bad input to a host builtin
// must surface a clean, attributable error (status=error with the builtin name) —
// never a panic and never a silent success. These lock the script-facing API's
// failure contract, the part most exposed to a weak compile model's output.

import (
	"context"
	"strings"
	"testing"
)

func runStar(t *testing.T, body string) *RunResult {
	t.Helper()
	res, err := NewStarlarkEngine().Execute(context.Background(),
		&JobSpec{Runtime: RuntimeStarlark, Script: body, TimeoutSec: 10})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

func TestStarlarkBuiltins_ErrorPaths(t *testing.T) {
	cases := []struct {
		name   string
		script string
		errSub string
	}{
		{"json_decode bad input", `emit({"data": json_decode("{not json")})`, "json_decode"},
		{"re_findall bad pattern", `emit({"data": re_findall("(", "x")})`, "bad pattern"},
		{"re_sub bad pattern", `emit({"data": re_sub("(", "_", "x")})`, "bad pattern"},
		{"re_findall missing arg", `emit({"data": re_findall("x")})`, "re_findall"},
		{"sha256 missing arg", `emit({"data": sha256()})`, "sha256"},
		{"url_encode wrong type", `emit({"data": url_encode([1,2])})`, "url_encode"},
		{"http_get non-http scheme", `emit({"data": http_get("ftp://x/y")})`, "only http/https"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := runStar(t, c.script)
			if res.Status != "error" {
				t.Fatalf("expected error status, got %q (err=%q)", res.Status, res.Error)
			}
			if !strings.Contains(res.Error, c.errSub) {
				t.Errorf("error %q must mention %q", res.Error, c.errSub)
			}
		})
	}
}

// builtinKBIngest's UnpackArgs guard: title/content are required positionals.
func TestStarlarkKBIngest_MissingArgsError(t *testing.T) {
	eng := NewStarlarkEngine()
	eng.SetKBIngest(func(context.Context, string, string, string) (string, error) { return "id", nil })
	res, err := eng.Execute(context.Background(),
		&JobSpec{Runtime: RuntimeStarlark, Script: `emit({"data": kb_ingest(title = "only-title")})`, TimeoutSec: 10})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != "error" || !strings.Contains(res.Error, "kb_ingest") {
		t.Errorf("missing content must error via kb_ingest, got status=%q err=%q", res.Status, res.Error)
	}
}

// StarlarkEngine identity: always-available pure-Go runtime named "starlark".
func TestStarlarkEngine_Identity(t *testing.T) {
	eng := NewStarlarkEngine()
	if eng.Name() != RuntimeStarlark {
		t.Errorf("Name = %q, want %q", eng.Name(), RuntimeStarlark)
	}
	if !eng.Available() {
		t.Error("Starlark engine must always be available (pure Go)")
	}
}
