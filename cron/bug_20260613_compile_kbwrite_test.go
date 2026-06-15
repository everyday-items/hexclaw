package cron

// BUG-20260613 (audit C3): generated scripts wrapped an HTTP write in
// try/except: pass, swallowed a failed write, and still printed status=success —
// so a request could silently fail while reporting done. The hardened prompt
// forbids swallowing and mandates non-2xx → status=error for external http_get/
// http_post. C2: the prompt advertised a phantom POST /api/v1/notify endpoint.
// F-3 update: knowledge-base ingest moved to the in-process kb_ingest builtin, so
// the prompt no longer advertises the loopback /api/v1/knowledge/documents POST.

import (
	"strings"
	"testing"
)

func TestBug20260613_CompilePromptForbidsSwallowingWriteErrors(t *testing.T) {
	p := buildCompileSystemPrompt(CompileHints{LocalAPIBase: "http://127.0.0.1:8080"})

	// Must mandate non-2xx → status=error on a failed HTTP write.
	for _, must := range []string{"非 2xx", "status=error"} {
		if !strings.Contains(p, must) {
			t.Errorf("prompt must mandate %q on a failed HTTP write", must)
		}
	}
	// Must forbid treating a fire-and-forget POST as success.
	if !strings.Contains(p, "发了就当成功") {
		t.Error("prompt must forbid treating a fire-and-forget write as success")
	}
}

// C2: the phantom /api/v1/notify endpoint must no longer be advertised — scripts
// deliver via the output contract, the scheduler routes the result.
func TestBug20260613_CompilePromptDropsPhantomNotifyEndpoint(t *testing.T) {
	p := buildCompileSystemPrompt(CompileHints{LocalAPIBase: "http://127.0.0.1:8080"})
	if strings.Contains(p, "/api/v1/notify") {
		t.Error("prompt must not advertise the unregistered /api/v1/notify endpoint")
	}
	// F-3: KB ingest is the in-process kb_ingest builtin; the loopback HTTP
	// endpoint is no longer advertised (and is now SSRF-blocked).
	if !strings.Contains(p, "kb_ingest") {
		t.Error("prompt must advertise the in-process kb_ingest builtin")
	}
	if strings.Contains(p, "/api/v1/knowledge/documents") {
		t.Error("prompt must not advertise the loopback KB endpoint (F-3: use kb_ingest)")
	}
}
