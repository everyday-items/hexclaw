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

	if strings.Contains(p, "用 requests / httpx") {
		t.Error("prompt must not steer the model toward 3rd-party HTTP libs")
	}
	for _, must := range []string{"urllib.request", "禁止", "逐字使用", "http://"} {
		if !strings.Contains(p, must) {
			t.Errorf("hardened prompt must mention %q", must)
		}
	}
}
