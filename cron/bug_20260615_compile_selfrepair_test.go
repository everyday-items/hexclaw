package cron

// BUG-20260615 (P2.5): the compiler self-corrected only JSON-format errors, not
// Starlark VALIDATION errors. Real runs showed weak/unfamiliar models emit a
// structurally-correct script with one small slip (bad escape \., stray
// continuation, wrong field) that fails StarlarkEngine.Validate — and the
// compiler gave up instead of feeding the precise error back for a fix round.
// These lock the validation self-repair: one repair round recovers the slip; a
// still-invalid repair surfaces the error after at most one round.

import (
	"context"
	"strings"
	"testing"
)

// A Starlark script with an invalid escape sequence (\.) — exactly the
// nemotron-550b failure. Parses-as-JSON fine, but fails StarlarkEngine.Validate.
const badEscapeStarlark = "x = re_findall(\"\\.\", \"y\")\nemit({\"status\": \"success\"})\n"

const fixedStarlark = `emit({"status": "success", "data": "ok"})`

func TestBug20260615_CompileValidationSelfRepair_Recovers(t *testing.T) {
	seq := &seqProvider{responses: []string{
		`{"runtime":"starlark","script":"` + escapeJSON(badEscapeStarlark) + `"}`,
		`{"runtime":"starlark","script":"` + escapeJSON(fixedStarlark) + `"}`,
	}}
	c := NewLLMCompilerStatic(seq, "glm-4-flash")
	spec, err := c.Compile(context.Background(), "采集某榜单", CompileHints{})
	if err != nil {
		t.Fatalf("validation self-repair should recover a fixable slip, got: %v", err)
	}
	if spec.Runtime != RuntimeStarlark {
		t.Errorf("runtime = %q, want starlark", spec.Runtime)
	}
	if verr := NewStarlarkEngine().Validate(spec.Script); verr != nil {
		t.Errorf("repaired script must pass validation: %v", verr)
	}
	if len(seq.reqs) < 2 {
		t.Errorf("expected a repair round (>=2 LLM calls), got %d", len(seq.reqs))
	}
}

func TestBug20260615_CompileValidationSelfRepair_GivesUpAfterOneRound(t *testing.T) {
	// Both replies carry the same bad escape — repair must not loop forever; it
	// surfaces the validation error after exactly one repair round.
	bad := `{"runtime":"starlark","script":"` + escapeJSON(badEscapeStarlark) + `"}`
	seq := &seqProvider{responses: []string{bad, bad}}
	c := NewLLMCompilerStatic(seq, "glm-4-flash")
	_, err := c.Compile(context.Background(), "x", CompileHints{})
	if err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("a still-invalid repair must surface the validation error, got: %v", err)
	}
	if len(seq.reqs) > 2 {
		t.Errorf("repair must run at most one round (<=2 calls), got %d", len(seq.reqs))
	}
}
