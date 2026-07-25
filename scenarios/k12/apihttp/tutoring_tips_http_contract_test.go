package apihttp_test

import (
	"net/http"
	"testing"
)

func TestTutoringTipsRequestAcceptsOnlyOwnerAndDispatchIdentity(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	create, out := do(t, fixture.handler, http.MethodPost, "/image-tasks",
		createImageTaskBody(fixture.assetID, "tips-artwork"))
	if create.Code != http.StatusOK {
		t.Fatalf("create image dispatch: %d %v", create.Code, out)
	}
	dispatchID := out["dispatch"].(map[string]any)["dispatch_id"].(string)

	rec, _ := do(t, fixture.handler, http.MethodPost, "/tutoring-tips",
		`{"agent":"mingming","grading_job_id":"internal","grade":"五年级下"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("internal identity/client fact must be rejected: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, _ = do(t, fixture.handler, http.MethodPost, "/tutoring-tips",
		`{"agent":"mingming","dispatch_id":"`+dispatchID+`","grade":"五年级下"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("client-derived fact must be rejected before lookup: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, _ = do(t, fixture.handler, http.MethodPost, "/tutoring-tips",
		`{"agent":"gege","dispatch_id":"`+dispatchID+`"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner dispatch must be hidden: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, _ = do(t, fixture.handler, http.MethodPost, "/tutoring-tips",
		`{"agent":"mingming","dispatch_id":"`+dispatchID+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("non-homework dispatch must fail closed: status=%d body=%s", rec.Code, rec.Body.String())
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
