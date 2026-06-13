package cron

// BUG-20260613 (audit C3): generated scripts wrapped the knowledge-base POST in
// try/except: pass, swallowed a failed write, and still printed status=success —
// so an ingest could silently store nothing while reporting done. And the prompt
// advertised a phantom POST /api/v1/notify endpoint (C2). The hardened prompt
// forbids swallowing, mandates non-2xx → status=error with a receipt check, and
// no longer mentions the phantom endpoint.

import (
	"strings"
	"testing"
)

func TestBug20260613_CompilePromptForbidsSwallowingWriteErrors(t *testing.T) {
	p := buildCompileSystemPrompt(CompileHints{LocalAPIBase: "http://127.0.0.1:8080"})

	// Must forbid the try/except: pass + print success anti-pattern.
	if !strings.Contains(p, "try/except: pass") {
		t.Error("prompt must explicitly forbid the try/except: pass swallow pattern")
	}
	// Must mandate non-2xx → error.
	for _, must := range []string{"非 2xx", "status=error"} {
		if !strings.Contains(p, must) {
			t.Errorf("prompt must mandate %q on a failed HTTP write", must)
		}
	}
	// Must require a write receipt (id) check.
	if !strings.Contains(p, "回执") {
		t.Error("prompt must require a write-receipt check (missing receipt => error)")
	}
}

// C2: the phantom /api/v1/notify endpoint must no longer be advertised — scripts
// deliver via the output contract, the scheduler routes the result.
func TestBug20260613_CompilePromptDropsPhantomNotifyEndpoint(t *testing.T) {
	p := buildCompileSystemPrompt(CompileHints{LocalAPIBase: "http://127.0.0.1:8080"})
	if strings.Contains(p, "/api/v1/notify") {
		t.Error("prompt must not advertise the unregistered /api/v1/notify endpoint")
	}
	// The real KB endpoint stays.
	if !strings.Contains(p, "/api/v1/knowledge/documents") {
		t.Error("prompt must still advertise the real knowledge-write endpoint")
	}
}
