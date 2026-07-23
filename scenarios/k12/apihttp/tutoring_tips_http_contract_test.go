package apihttp_test

import (
	"net/http"
	"testing"
)

func TestTutoringTipsRequestAcceptsOnlyOwnerAndJobIdentity(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, http.MethodPost, "/tutoring-tips",
		`{"agent":"mingming","grading_job_id":"missing","grade":"五年级下"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("client-derived fact must be rejected before lookup: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, _ = do(t, h, http.MethodPost, "/tutoring-tips",
		`{"agent":"mingming","grading_job_id":"missing"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("exact request should reach owner-scoped job lookup: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTutoringTipsRoutesAcceptOnlyPost(t *testing.T) {
	h := newServer(t)
	for _, path := range []string{"/tutoring-tips", "/tutoring-tips/send"} {
		for _, method := range []string{
			http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete,
		} {
			rec, _ := do(t, h, method, path, `{}`)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s status=%d want 405 body=%s", method, path, rec.Code, rec.Body.String())
			}
		}
	}
}
