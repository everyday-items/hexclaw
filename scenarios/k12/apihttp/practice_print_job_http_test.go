package apihttp_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPracticePrintJobHTTPContract(t *testing.T) {
	h := newServer(t)
	_, seeded := do(t, h, http.MethodPost, "/practice-sets/basket/items", `{"agent":"mingming","source_session":"s1",
		"item":{"item_id":"q1","subject":"数学","added_via":"weekly","question_markdown":"2+2=?","expected_answer_markdown":"4","verification_status":"verified","verification_evidence":"独立验算"}}`)
	setID := seeded["record_id"].(string)

	rec, out := do(t, h, http.MethodPost, "/practice-sets/"+setID+"/print-jobs",
		`{"agent":"mingming","idempotency_key":"desktop-click-1","artifact_kind":"question"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("prepare status=%d body=%s", rec.Code, rec.Body.String())
	}
	job := out["print_job"].(map[string]any)
	jobID := job["print_job_id"].(string)
	paperNo := job["paper_no"].(string)
	if !strings.HasPrefix(paperNo, "P-") || job["status"] != "preparing" || job["source_digest"] == "" {
		t.Fatalf("prepare response lacks durable reservation: %#v", out)
	}

	rec, set := do(t, h, http.MethodGet, "/practice-sets/"+setID+"?agent=mingming", "")
	if rec.Code != http.StatusOK || set["status"] != "draft" {
		t.Fatalf("prepare finalized set: code=%d %#v", rec.Code, set)
	}
	rec, paper := do(t, h, http.MethodGet, "/print-jobs/"+jobID+"/paper?agent=mingming&kind=question", "")
	if rec.Code != http.StatusOK || paper["paper_no"] != paperNo || paper["source_digest"] != job["source_digest"] {
		t.Fatalf("print paper contract drifted: code=%d %#v", rec.Code, paper)
	}
	if paper["artifact_id"] != job["artifact_id"] {
		t.Fatalf("practice paper artifact identity drifted: paper=%#v job=%#v", paper, job)
	}
	contentReq := httptest.NewRequest(http.MethodGet,
		"/print-artifacts/"+paper["artifact_id"].(string)+"/content?agent=mingming", nil)
	contentRec := httptest.NewRecorder()
	h.ServeHTTP(contentRec, contentReq)
	if contentRec.Code != http.StatusOK || contentRec.Header().Get("Content-Type") != "application/pdf" ||
		!strings.HasPrefix(contentRec.Body.String(), "%PDF-") {
		t.Fatalf("practice paper canonical PDF unavailable: status=%d headers=%v body=%q",
			contentRec.Code, contentRec.Header(), contentRec.Body.String())
	}

	for _, event := range []struct {
		body string
		want string
	}{
		{`{"agent":"mingming","status":"dialog_open"}`, "dialog_open"},
		{`{"agent":"mingming","status":"submitted","native_job_id":"native-1"}`, "submitted"},
	} {
		rec, out = do(t, h, http.MethodPost, "/print-jobs/"+jobID+"/events", event.body)
		if rec.Code != http.StatusOK || out["print_job"].(map[string]any)["status"] != event.want {
			t.Fatalf("event %s code=%d body=%s", event.want, rec.Code, rec.Body.String())
		}
	}
	rec, out = do(t, h, http.MethodPost, "/print-jobs/"+jobID+"/events",
		`{"agent":"mingming","status":"printed","native_job_id":"native-1","native_receipt_id":"receipt-1","printer_snapshot":{"printer":"Office","paper":"A4","copies":1}}`)
	if rec.Code != http.StatusOK || out["print_job"].(map[string]any)["status"] != "printed" {
		t.Fatalf("printed receipt commit code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, set = do(t, h, http.MethodGet, "/practice-sets/"+setID+"?agent=mingming", "")
	if rec.Code != http.StatusOK || set["status"] != "assigned" || set["paper_no"] != paperNo {
		t.Fatalf("printed receipt did not finalize set: code=%d %#v", rec.Code, set)
	}
}

func TestPracticePrintJobHTTPUnknownHasNoOrdinaryRetry(t *testing.T) {
	h := newServer(t)
	_, seeded := do(t, h, http.MethodPost, "/practice-sets/basket/items", `{"agent":"mingming",
		"item":{"item_id":"q1","subject":"数学","question_markdown":"1+1=?","expected_answer_markdown":"2","verification_status":"verified","verification_evidence":"验算"}}`)
	setID := seeded["record_id"].(string)
	_, out := do(t, h, http.MethodPost, "/practice-sets/"+setID+"/print-jobs",
		`{"agent":"mingming","idempotency_key":"unknown-1","artifact_kind":"question"}`)
	jobID := out["print_job"].(map[string]any)["print_job_id"].(string)

	rec, _ := do(t, h, http.MethodPost, "/print-jobs/"+jobID+"/events",
		`{"agent":"mingming","status":"outcome_unknown","failure_kind":"receipt_lost"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, _ = do(t, h, http.MethodPost, "/print-jobs/"+jobID+"/retry", `{"agent":"mingming"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("unknown ordinary retry must be 409, got %d body=%s", rec.Code, rec.Body.String())
	}
	rec, out = do(t, h, http.MethodGet, "/print-jobs/"+jobID+"?agent=mingming", "")
	if rec.Code != http.StatusOK || out["print_job"].(map[string]any)["status"] != "outcome_unknown" {
		t.Fatalf("unknown must remain queryable: code=%d %#v", rec.Code, out)
	}
}

func TestPracticePrintJobHTTPCommitReceiptAtomicallyFinalizesSetAndReplays(t *testing.T) {
	h := newServer(t)
	_, seeded := do(t, h, http.MethodPost, "/practice-sets/basket/items", `{"agent":"mingming",
		"item":{"item_id":"q1","subject":"数学","question_markdown":"1+1=?","expected_answer_markdown":"2","verification_status":"verified","verification_evidence":"验算"}}`)
	setID := seeded["record_id"].(string)
	_, out := do(t, h, http.MethodPost, "/practice-sets/"+setID+"/print-jobs",
		`{"agent":"mingming","idempotency_key":"atomic-commit-1","artifact_kind":"question"}`)
	jobID := out["print_job"].(map[string]any)["print_job_id"].(string)
	receipt := `{"agent":"mingming","native_job_id":"native-practice-1","native_receipt_id":"receipt-practice-1","printer_snapshot":{"printer":"Office","paper":"A4"}}`
	rec, _ := do(t, h, http.MethodPost, "/print-jobs/"+jobID+"/commit", receipt)
	if rec.Code != http.StatusConflict {
		t.Fatalf("preparing must not bypass dialog_open, got %d body=%s", rec.Code, rec.Body.String())
	}
	rec, _ = do(t, h, http.MethodPost, "/print-jobs/"+jobID+"/events",
		`{"agent":"mingming","status":"dialog_open"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("dialog_open status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec, out = do(t, h, http.MethodPost, "/print-jobs/"+jobID+"/commit", receipt)
	if rec.Code != http.StatusOK || out["print_job"].(map[string]any)["status"] != "printed" {
		t.Fatalf("atomic commit status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, set := do(t, h, http.MethodGet, "/practice-sets/"+setID+"?agent=mingming", "")
	if rec.Code != http.StatusOK || set["status"] != "assigned" {
		t.Fatalf("commit did not atomically finalize practice set: code=%d body=%s", rec.Code, rec.Body.String())
	}

	rec, out = do(t, h, http.MethodPost, "/print-jobs/"+jobID+"/commit", receipt)
	if rec.Code != http.StatusOK || out["print_job"].(map[string]any)["native_receipt_id"] != "receipt-practice-1" {
		t.Fatalf("idempotent replay status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, _ = do(t, h, http.MethodPost, "/print-jobs/"+jobID+"/commit",
		`{"agent":"mingming","native_job_id":"native-practice-1","native_receipt_id":"receipt-practice-1","printer_snapshot":{"paper":"A4","printer":"Office"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("semantically identical snapshot replay status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, _ = do(t, h, http.MethodPost, "/print-jobs/"+jobID+"/commit",
		`{"agent":"mingming","native_job_id":"native-practice-1","native_receipt_id":"receipt-practice-1","printer_snapshot":{"printer":"Office","paper":"Letter"}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("same receipt IDs with conflicting snapshot must be 409, got %d body=%s", rec.Code, rec.Body.String())
	}
}
