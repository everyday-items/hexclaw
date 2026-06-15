package cron

// BUG-20260613: the compile system prompt told the model to use requests/httpx
// (3rd-party deps the sandbox can't reliably install, and whose TLS stack fails
// the LibreSSL handshake on some hosts). It also let the model invent
// endpoints. The hardened prompt pins scripts to the standard library, requires
// verbatim URLs, and mandates an http:// fallback on TLS failure.

import (
	"strings"
	"testing"
)

func TestBug20260613_CompilePromptForcesStdlibOnly(t *testing.T) {
	p := buildCompileSystemPrompt(CompileHints{})

	// Output contract is emit(), not python's print(json.dumps(...)).
	if strings.Contains(p, "print(json.dumps") {
		t.Error("starlark prompt must not use the python print(json.dumps) contract")
	}
	// Pins scripts to injected host builtins + verbatim URLs + the emit contract.
	for _, must := range []string{"http_get", "json_decode", "逐字使用", "emit("} {
		if !strings.Contains(p, must) {
			t.Errorf("hardened starlark prompt must mention %q", must)
		}
	}
}
