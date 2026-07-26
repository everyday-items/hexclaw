package apihttp_test

import (
	"net/http"
	"testing"
)

// API-013 / K12-INV-074: the synchronous preview/approval path is retired.
func TestReviewRetryRouteIsNotRegistered(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, http.MethodPost, "/review/retry", `{}`)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("retired /review/retry status=%d want 404/405", rec.Code)
	}
}
