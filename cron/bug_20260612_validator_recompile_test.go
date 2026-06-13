package cron

// BUG-20260612: the AST validator banned ANY attribute call named `.compile`,
// which rejected the benign and extremely common `re.compile(...)` — observed
// live when the compiler's otherwise-valid output was refused ("禁用方法调用:
// .compile"). The ban targets the code-execution builtin compile(); the regex
// method must pass. Hardening added alongside: `import builtins` is now banned
// so the builtin cannot be reached via a module alias.

import (
	"strings"
	"testing"
)

func TestBug20260612_ValidatorAllowsReCompile(t *testing.T) {
	script := `import re
import json
pattern = re.compile(r"<div>(.*?)</div>")
print(json.dumps({"status": "success", "data": pattern.findall("<div>x</div>")}))
`
	if err := validateNoForbiddenImports(script); err != nil {
		t.Fatalf("re.compile must pass validation, got: %v", err)
	}
}

func TestBug20260612_ValidatorStillBansCodeExecution(t *testing.T) {
	cases := map[string]string{
		"bare compile()":      `compile("1+1", "<s>", "eval")`,
		"builtins attr":       `__builtins__.compile("1+1", "<s>", "eval")`,
		"import builtins":     "import builtins\nbuiltins.compile('1+1', '<s>', 'eval')",
		"from builtins":       "from builtins import compile\ncompile('1+1', '<s>', 'eval')",
		"os.system attr call": "import os\nos.system('id')",
		"bare eval":           `eval("1+1")`,
		"subprocess import":   "import subprocess\nsubprocess.run(['id'])",
	}
	for name, script := range cases {
		if err := validateNoForbiddenImports(script); err == nil {
			t.Errorf("%s must be rejected, but passed", name)
		} else if !strings.Contains(err.Error(), "禁用") {
			t.Errorf("%s rejected with unexpected error: %v", name, err)
		}
	}
}
