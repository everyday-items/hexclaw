package apihttp_test

import (
	"net/http"
	"testing"
)

// BUG-20260726-031 Wave 4 RED:
// the problem-level source-action command is one authenticated, idempotent
// endpoint. An unsupported action is a malformed command (400), not an
// unregistered route (404).
func TestBUG_20260726_031_ProblemSourceActionRouteRejectsUnsupportedAction(t *testing.T) {
	handler := newServer(t)

	rec, out := do(
		t,
		handler,
		http.MethodPost,
		"/image-tasks/dispatch-wave4/problems/problem-wave4/source-actions",
		`{
			"action":"replace_everything",
			"structure_version":1,
			"expected_input_revision":1,
			"payload":{}
		}`,
	)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf(
			"source-actions route = %d, want 400 for unsupported action; body=%#v",
			rec.Code,
			out,
		)
	}
}

// Valid skip, durable receipt, exact replay, conflicts, resume and concurrency
// are superseded by bug_20260726_031_problem_source_action_wave4b_test.go. That
// fixture owns a real ImageTask facade plus persisted dispatch/job/problem;
// newServer(t) intentionally has neither and must not be used to fabricate a
// successful source-action command.
