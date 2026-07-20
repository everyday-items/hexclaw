package apihttp_test

import (
	"net/http"
	"testing"
)

func TestGenericPrintJobHTTPUsesSharedRecoveryRoutes(t *testing.T) {
	h := newServer(t)
	rec, out := do(t, h, http.MethodPost, "/print-jobs", `{
		"agent":"mingming","idempotency_key":"prep-click-1","source_kind":"prep_card",
		"source_ref":"submission:s1","title":"这份作业的辅导要点",
		"canonical_markdown":"# 辅导要点\n\n小数点对齐"
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("prepare status=%d body=%s", rec.Code, rec.Body.String())
	}
	job := out["print_job"].(map[string]any)
	jobID := job["print_job_id"].(string)
	if job["status"] != "preparing" || job["source_kind"] != "prep_card" || job["source_digest"] == "" {
		t.Fatalf("generic prepare missing durable facts: %#v", job)
	}
	rec, paper := do(t, h, http.MethodGet, "/print-jobs/"+jobID+"/paper?agent=mingming", "")
	if rec.Code != http.StatusOK || paper["markdown"] != "# 辅导要点\n\n小数点对齐" || paper["source_ref"] != "submission:s1" {
		t.Fatalf("shared paper route code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, _ = do(t, h, http.MethodGet, "/print-jobs/"+jobID+"?agent=other", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner query code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, recovered := do(t, h, http.MethodPost, "/print-jobs", `{
		"agent":"mingming","idempotency_key":"fresh-key-after-reload","source_kind":"prep_card",
		"source_ref":"submission:s1","title":"这份作业的辅导要点",
		"canonical_markdown":"# 辅导要点\n\n小数点对齐"
	}`)
	if rec.Code != http.StatusOK || recovered["replayed"] != true || recovered["print_job"].(map[string]any)["print_job_id"] != jobID {
		t.Fatalf("fresh key must recover unresolved job: code=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, body := range []string{
		`{"agent":"mingming","status":"dialog_open"}`,
		`{"agent":"mingming","status":"submitted","native_job_id":"native-1"}`,
		`{"agent":"mingming","status":"printed","native_job_id":"native-1","native_receipt_id":"receipt-1","printer_snapshot":{"printer":"Office","paper":"A4"}}`,
	} {
		rec, out = do(t, h, http.MethodPost, "/print-jobs/"+jobID+"/events", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("event body=%s code=%d response=%s", body, rec.Code, rec.Body.String())
		}
	}
	if out["print_job"].(map[string]any)["status"] != "printed" {
		t.Fatalf("printed status missing: %#v", out)
	}
	rec, replay := do(t, h, http.MethodPost, "/print-jobs", `{
		"agent":"mingming","idempotency_key":"prep-click-1","source_kind":"prep_card",
		"source_ref":"submission:s1","title":"这份作业的辅导要点",
		"canonical_markdown":"# 辅导要点\n\n小数点对齐"
	}`)
	if rec.Code != http.StatusOK || replay["replayed"] != true || replay["print_job"].(map[string]any)["print_job_id"] != jobID {
		t.Fatalf("replay code=%d body=%s", rec.Code, rec.Body.String())
	}
}
