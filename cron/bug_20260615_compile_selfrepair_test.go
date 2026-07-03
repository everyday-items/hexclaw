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
	// BUG-20260703：坏转义(\.) 现由 deterministic 预修复在首次校验失败时直接修好，
	// 不再花一轮 LLM——只需 1 次 LLM 调用（初始编译）。
	if len(seq.reqs) != 1 {
		t.Errorf("bad escape should be repaired deterministically without an LLM round (want 1 call), got %d", len(seq.reqs))
	}
}

func TestBug20260615_CompileValidationSelfRepair_GivesUpAfterOneRound(t *testing.T) {
	// Both replies carry a Python-only builtin that deterministic repair does NOT
	// handle (set()) — repair must not loop forever; it surfaces the validation
	// error after exactly one repair round.
	// (try/except is no longer a give-up example: BUG-20260703 repairs it
	// deterministically, see TestBug20260703_RepairPythonTryExceptAndIsNone.)
	badScript := "def run():\n    x = set([1, 2])\n    return {\"status\":\"success\"}\nemit(run())\n"
	bad := `{"runtime":"starlark","script":"` + escapeJSON(badScript) + `"}`
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

func TestBug20260702_CompileValidationSelfRepair_FixesRegexEscapesAfterLLMRepair(t *testing.T) {
	// 1st reply uses set() (not deterministically fixable) → forces one LLM round;
	// the LLM's 2nd reply carries bad regex escapes that the post-LLM deterministic
	// repair doubles. (try/except would now be pre-repaired, skipping the round —
	// BUG-20260703 — so use set() to still exercise the after-LLM-repair path.)
	badPythonOnly := "def run():\n    x = set([1, 2])\n    return {\"status\":\"success\"}\nemit(run())\n"
	badRegexEscape := `def run():
    data = re_findall("window\.INITIAL_STATE\s*=\s*\{(.*?)\}", "window.INITIAL_STATE = {x}")
    return {"status":"success","data": data}
run()`
	seq := &seqProvider{responses: []string{
		`{"runtime":"starlark","script":"` + escapeJSON(badPythonOnly) + `"}`,
		`{"runtime":"starlark","script":"` + escapeJSON(badRegexEscape) + `"}`,
	}}
	c := NewLLMCompilerStatic(seq, "nemotron")
	// 百度热搜采集类 prompt 自 BUG-20260704 起命中确定性模板不走 LLM，
	// 本测试对象是正则转义的确定性修复链，换用非模板数据源。
	spec, err := c.Compile(context.Background(), "采集微博热搜", CompileHints{})
	if err != nil {
		t.Fatalf("regex escape deterministic repair should recover after LLM repair: %v", err)
	}
	if err := validateStarlarkSource(spec.Script); err != nil {
		t.Fatalf("repaired script must validate: %v\n%s", err, spec.Script)
	}
	if !strings.Contains(spec.Script, `window\\.INITIAL_STATE`) || !strings.Contains(spec.Script, `\\s*`) {
		t.Fatalf("regex escapes were not doubled safely: %s", spec.Script)
	}
	if !strings.Contains(spec.Script, "emit(run())") {
		t.Fatalf("bare run() was not wrapped in emit(run()): %s", spec.Script)
	}
	if len(seq.reqs) != 2 {
		t.Fatalf("expected exactly one LLM repair round, got %d", len(seq.reqs))
	}
}
