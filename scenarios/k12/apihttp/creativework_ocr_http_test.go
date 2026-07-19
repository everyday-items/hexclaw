package apihttp_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

type httpWritingOCR struct {
	results []struct {
		raw string
		err error
	}
	calls int
}

func (f *httpWritingOCR) RecognizeWriting(context.Context, []byte) (string, error) {
	f.calls++
	idx := f.calls - 1
	if idx >= len(f.results) {
		idx = len(f.results) - 1
	}
	return f.results[idx].raw, f.results[idx].err
}

func httpWritingAsset(t *testing.T) string {
	t.Helper()
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	raw := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde,
	}
	id, err := assetstore.Save("mingming", raw)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCreativeWorkOCRHTTPConfirmedSnapshotIsRequiredByCreateAndFeedbackEvidence(t *testing.T) {
	ocr := &httpWritingOCR{results: []struct {
		raw string
		err error
	}{{raw: "柳枝象绿色丝带"}}}
	h := newServerWithSolver(t, fakeSolveExec{},
		assembly.WithCreativeWorkOCR(ocr),
		assembly.WithWorkFeedbackGenerator(func(context.Context, string, string, string) (string, error) {
			return "这句话比喻清楚；建议补充柳枝随风移动的细节。", nil
		}),
	)
	assetID := httpWritingAsset(t)

	rec, job := do(t, h, http.MethodPost, "/creative-work-ocr-jobs",
		`{"agent":"mingming","request_id":"add-writing-1","source_asset_id":"`+assetID+`"}`)
	if rec.Code != http.StatusOK || job["status"] != "awaiting_confirmation" || job["ocr_raw"] != "柳枝象绿色丝带" {
		t.Fatalf("create OCR job: status=%d body=%v", rec.Code, job)
	}
	jobID := job["job_id"].(string)
	if rec, _ := do(t, h, http.MethodGet, "/creative-work-ocr-jobs/"+jobID+"?agent=other", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign owner must see 404, got %d", rec.Code)
	}

	unconfirmedBody := `{"agent":"mingming","work_type":"writing","title":"春天的校园","task":"写景",` +
		`"source_asset_id":"` + assetID + `","content_markdown":"柳枝像绿色丝带",` +
		`"ocr_job_id":"` + jobID + `"}`
	if rec, _ := do(t, h, http.MethodPost, "/creative-works", unconfirmedBody); rec.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed OCR snapshot must be rejected, got %d", rec.Code)
	}

	rec, confirmed := do(t, h, http.MethodPost, "/creative-work-ocr-jobs/"+jobID+"/confirm",
		`{"agent":"mingming","content_markdown":"柳枝像绿色丝带"}`)
	if rec.Code != http.StatusOK || confirmed["status"] != "confirmed" || confirmed["confirmed_version"] != float64(1) {
		t.Fatalf("confirm OCR: status=%d body=%v", rec.Code, confirmed)
	}
	createBody := `{"agent":"mingming","work_type":"writing","title":"春天的校园","task":"写景",` +
		`"source_asset_id":"` + assetID + `","content_markdown":"柳枝像绿色丝带",` +
		`"ocr_job_id":"` + jobID + `","ocr_version":1,"ocr_confirmed_digest":"` +
		confirmed["confirmed_digest"].(string) + `"}`
	rec, created := do(t, h, http.MethodPost, "/creative-works", createBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("create confirmed work: status=%d body=%v", rec.Code, created)
	}
	workID := created["record_id"].(string)
	rec, feedback := do(t, h, http.MethodPost, "/creative-works/"+workID+"/generate-feedback", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate feedback: status=%d body=%v", rec.Code, feedback)
	}
	version := feedback["versions"].([]any)[0].(map[string]any)
	structured := version["structured_feedback"].(map[string]any)
	refs := structured["evidence_refs"].([]any)
	found := false
	for _, value := range refs {
		if ref, ok := value.(string); ok && len(ref) > len("ocr-confirmed:") && ref[:len("ocr-confirmed:")] == "ocr-confirmed:" {
			found = true
		}
	}
	if !found {
		t.Fatalf("confirmed OCR evidence missing: %v", refs)
	}
}

func TestCreativeWorkOCRHTTPFailureRetryAndManualPaste(t *testing.T) {
	ocr := &httpWritingOCR{results: []struct {
		raw string
		err error
	}{
		{err: errors.New("vision timeout")},
		{raw: "重试成功"},
		{err: errors.New("vision unavailable")},
	}}
	h := newServerWithSolver(t, fakeSolveExec{}, assembly.WithCreativeWorkOCR(ocr))
	assetID := httpWritingAsset(t)

	_, failed := do(t, h, http.MethodPost, "/creative-work-ocr-jobs",
		`{"agent":"mingming","request_id":"retry-one","source_asset_id":"`+assetID+`"}`)
	if failed["status"] != "failed" {
		t.Fatalf("expected durable failed status: %v", failed)
	}
	jobID := failed["job_id"].(string)
	rec, retried := do(t, h, http.MethodPost, "/creative-work-ocr-jobs/"+jobID+"/retry", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || retried["job_id"] != jobID || retried["status"] != "awaiting_confirmation" {
		t.Fatalf("same-job retry failed: status=%d body=%v", rec.Code, retried)
	}

	_, manualFailed := do(t, h, http.MethodPost, "/creative-work-ocr-jobs",
		`{"agent":"mingming","request_id":"manual-one","source_asset_id":"`+assetID+`"}`)
	manualID := manualFailed["job_id"].(string)
	rec, manual := do(t, h, http.MethodPost, "/creative-work-ocr-jobs/"+manualID+"/confirm",
		`{"agent":"mingming","content_markdown":"家长手工粘贴的原稿"}`)
	if rec.Code != http.StatusOK || manual["status"] != "confirmed" || manual["ocr_raw"] != nil {
		t.Fatalf("manual paste confirmation failed: status=%d body=%v", rec.Code, manual)
	}
}
