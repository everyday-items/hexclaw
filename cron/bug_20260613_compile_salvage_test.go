package cron

// BUG-20260613: when strict JSON parse AND self-correction both fail (the
// glm-4-flash failure: bare ```python fence or unescaped multi-line script in
// JSON), salvageCompiledSpec extracts the script with fixed metadata so the
// task still compiles to the script tier instead of falling back to agent.

import (
	"strings"
	"testing"
)

func TestBug20260613_SalvageFencedScript(t *testing.T) {
	raw := "Sure:\n```python\nimport urllib.request, re, json\nhtml = urllib.request.urlopen(\"http://x\").read().decode()\nprint(json.dumps({\"status\": \"success\", \"data\": re.findall(r\"<b>(.*?)</b>\", html)}))\n```"
	spec := salvageCompiledSpec(raw)
	if spec == nil {
		t.Fatal("fenced script must be salvaged")
	}
	if spec.Runtime != "python3" || !strings.Contains(spec.Script, "urllib.request") || strings.Contains(spec.Script, "```") {
		t.Errorf("salvaged spec wrong: runtime=%q script_head=%.50q", spec.Runtime, spec.Script)
	}
	// Salvaged script must still pass the security validator.
	if err := validateNoForbiddenImports(spec.Script); err != nil {
		t.Errorf("salvaged stdlib script must pass validation: %v", err)
	}
}

func TestBug20260613_SalvageBrokenJSONScript(t *testing.T) {
	broken := "{\n  \"runtime\": \"python3\",\n  \"script\": \"import json\nprint(json.dumps({'status': 'success'}))\",\n  \"deps\": []\n}"
	// Strict parse must reject it (control).
	if _, err := parseCompiledSpec(broken); err == nil {
		t.Fatal("broken JSON must fail strict parse")
	}
	spec := salvageCompiledSpec(broken)
	if spec == nil || !strings.Contains(spec.Script, "import json") {
		t.Errorf("broken-JSON script must be salvaged, got %+v", spec)
	}
}

func TestBug20260613_SalvageReturnsNilForGarbage(t *testing.T) {
	if spec := salvageCompiledSpec("totally unrelated prose with no script at all"); spec != nil {
		t.Errorf("garbage must not be salvaged, got %+v", spec)
	}
}
