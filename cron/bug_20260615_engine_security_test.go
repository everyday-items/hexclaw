package cron

// BUG-20260615 (#1/#2 + builtins): StarlarkEngine hardening — an execution-step
// cap (kill infinite loops) and enhanced builtins (html_unescape/re_sub/
// url_encode/sha256) for HTML/monitor tasks. KB ingest goes through the
// in-process kb_ingest builtin (F-3).

import (
	"context"
	"testing"
)

func TestBug20260615_StarlarkStepLimit_KillsInfiniteLoop(t *testing.T) {
	eng := NewStarlarkEngine()
	script := "def run():\n    x = 0\n    for i in range(100000000000):\n        x = x + 1\n    return {\"status\": \"success\"}\nemit(run())"
	res, err := eng.Execute(context.Background(), &JobSpec{Runtime: RuntimeStarlark, Script: script, TimeoutSec: 30})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != "error" && res.Status != "timeout" {
		t.Errorf("infinite loop must be stopped by the step cap, got status=%q", res.Status)
	}
}

func TestBug20260615_StarlarkEnhancedBuiltins(t *testing.T) {
	eng := NewStarlarkEngine()
	script := `
def run():
    return {
        "status": "success",
        "data": {
            "unescaped": html_unescape("a &amp; b &lt;c&gt;"),
            "subbed": re_sub("\\s+", "_", "a  b   c"),
            "encoded": url_encode("a b&c"),
            "hash": sha256("hello"),
        },
    }
emit(run())`
	res, err := eng.Execute(context.Background(), &JobSpec{Runtime: RuntimeStarlark, Script: script, TimeoutSec: 10})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("status=%q err=%q", res.Status, res.Error)
	}
	d, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("data not a map: %T", res.Data)
	}
	if d["unescaped"] != "a & b <c>" {
		t.Errorf("html_unescape = %q", d["unescaped"])
	}
	if d["subbed"] != "a_b_c" {
		t.Errorf("re_sub = %q", d["subbed"])
	}
	if d["encoded"] != "a+b%26c" {
		t.Errorf("url_encode = %q", d["encoded"])
	}
	if d["hash"] != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf("sha256(\"hello\") = %q", d["hash"])
	}
}
