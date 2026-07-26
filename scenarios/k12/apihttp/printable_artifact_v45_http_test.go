package apihttp_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const printableArtifactV45Body = `{
	"agent":"mingming","source_kind":"tutoring_tips","source_ref":"submission:http-v45",
	"title":"这份作业的辅导要点","canonical_markdown":"# 辅导要点\n\n小数点对齐"
}`

func TestPrintableArtifactV45HTTPReusesFrozenPDFWithoutPrintReceipt(t *testing.T) {
	h := newServer(t)
	rec, artifactOut := do(t, h, http.MethodPost, "/print-artifacts", printableArtifactV45Body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("artifact prepare status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, hasJob := artifactOut["print_job"]; hasJob {
		t.Fatalf("artifact-only response exposed a PrintJob: %#v", artifactOut)
	}
	artifact := artifactOut["artifact"].(map[string]any)
	artifactID := artifact["artifact_id"].(string)
	byteDigest := artifact["byte_digest"].(string)

	rec, replay := do(t, h, http.MethodPost, "/print-artifacts", printableArtifactV45Body)
	if rec.Code != http.StatusOK || replay["replayed"] != true ||
		replay["artifact"].(map[string]any)["artifact_id"] != artifactID {
		t.Fatalf("artifact replay status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec, printOut := do(t, h, http.MethodPost, "/print-jobs", `{
		"agent":"mingming","idempotency_key":"http-v45-print",
		"source_kind":"tutoring_tips","source_ref":"submission:http-v45",
		"title":"这份作业的辅导要点","canonical_markdown":"# 辅导要点\n\n小数点对齐"
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("print prepare status=%d body=%s", rec.Code, rec.Body.String())
	}
	job := printOut["print_job"].(map[string]any)
	if job["artifact_id"] != artifactID {
		t.Fatalf("print artifact=%v artifact-only=%s", job["artifact_id"], artifactID)
	}

	rawReq := httptest.NewRequest(http.MethodGet,
		"/print-artifacts/"+artifactID+"/content?agent=mingming", nil)
	rawRec := httptest.NewRecorder()
	h.ServeHTTP(rawRec, rawReq)
	if rawRec.Code != http.StatusOK || rawRec.Header().Get("Content-Type") != "application/pdf" ||
		rawRec.Header().Get("X-Content-SHA256") != byteDigest ||
		rawRec.Body.String() != "%PDF-1.7\nfixed-http-render" {
		t.Fatalf("content status=%d headers=%v body=%q", rawRec.Code, rawRec.Header(), rawRec.Body.String())
	}

	paperRec, paper := do(t, h, http.MethodGet,
		"/print-jobs/"+job["print_job_id"].(string)+"/paper?agent=mingming", "")
	if paperRec.Code != http.StatusOK || paper["markdown"] != "# 辅导要点\n\n小数点对齐" {
		t.Fatalf("legacy Markdown paper changed: status=%d body=%s", paperRec.Code, paperRec.Body.String())
	}

	crossReq := httptest.NewRequest(http.MethodGet,
		"/print-artifacts/"+artifactID+"/content?agent=other", nil)
	crossRec := httptest.NewRecorder()
	h.ServeHTTP(crossRec, crossReq)
	if crossRec.Code != http.StatusNotFound {
		t.Fatalf("cross-agent content status=%d body=%s", crossRec.Code, crossRec.Body.String())
	}
}
