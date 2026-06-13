package cron

// BUG-20260613: the script sandbox passed cmd.Env = os.Environ() wholesale, so
// an LLM-generated cron script inherited every parent env var — including any
// API keys/tokens in the app's environment — and could read or exfiltrate
// them. The sandbox now runs scripts with a minimal allowlisted environment.

import (
	"context"
	"strings"
	"testing"
)

func TestBug20260613_SandboxStripsSecretEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-secret-should-not-leak")
	t.Setenv("HEXCLAW_TOKEN", "tok-secret-should-not-leak")

	e := newTestExecutor(t)
	spec := &JobSpec{
		Runtime: "python3",
		// Dump the whole environment so the test can assert secrets are absent.
		Script: `import os, json
print(json.dumps({"status": "success", "data": dict(os.environ)}))`,
		TimeoutSec: 30,
	}

	res, err := e.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(res.Stdout, "sk-secret-should-not-leak") ||
		strings.Contains(res.Stdout, "tok-secret-should-not-leak") {
		t.Errorf("sandbox must not leak parent secret env vars to the script; stdout=%s", res.Stdout)
	}
	// Sanity: PATH must still be present so python/venv resolves.
	if !strings.Contains(res.Stdout, "PATH") {
		t.Errorf("sandbox must still provide PATH, stdout=%s", res.Stdout)
	}
}
