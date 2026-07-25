package apihttp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
)

type currentAccumulationDeriver struct {
	calls int
}

func (d *currentAccumulationDeriver) DeriveAccumulationMetadata(
	_ context.Context,
	_ string,
) (k12.AccumulationDerivedMetadata, error) {
	d.calls++
	return k12.AccumulationDerivedMetadata{
		Subject: "语文", EntryType: "好词好句",
		SubjectProvenance: k12.DerivationProvenance{
			Method: "model", Policy: "test", Version: "1",
		},
		EntryTypeProvenance: k12.DerivationProvenance{
			Method: "model", Policy: "test", Version: "1",
		},
	}, nil
}

func doCurrent(
	t *testing.T,
	h http.Handler,
	method, path, body string,
	headers map[string]string,
) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			if strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("decode response %d %q: %v", rec.Code, rec.Body.String(), err)
			}
		}
	}
	return rec, out
}

func TestCurrentAccumulationHTTPContentOnlyDetailDurableGenerationAndDelete(t *testing.T) {
	deriver := &currentAccumulationDeriver{}
	h := newServerWithSolver(t, fakeSolveExec{},
		assembly.WithAccumulationMetadataDeriver(deriver))

	rec, _ := doCurrent(t, h, http.MethodPost, "/accumulation?agent=mingming",
		`{"content":"桂花香","subject":"语文"}`,
		map[string]string{"Idempotency-Key": "accum-invalid"})
	if rec.Code != http.StatusBadRequest || deriver.calls != 0 {
		t.Fatalf("client-derived metadata must fail before derivation: status=%d calls=%d",
			rec.Code, deriver.calls)
	}

	rec, created := doCurrent(t, h, http.MethodPost, "/accumulation?agent=mingming",
		`{"content":"桂花香"}`,
		map[string]string{"Idempotency-Key": "accum-create-1"})
	if rec.Code != http.StatusOK || created["created"] != true ||
		created["record_id"] == "" || deriver.calls != 1 {
		t.Fatalf("current accumulation create: status=%d body=%v calls=%d",
			rec.Code, created, deriver.calls)
	}
	id := created["record_id"].(string)

	rec, detail := doCurrent(t, h, http.MethodGet,
		"/accumulation/"+id+"?agent=mingming", "", nil)
	if rec.Code != http.StatusOK || detail["content"] != "桂花香" ||
		detail["subject"] != "语文" || detail["entry_type"] != "好词好句" ||
		detail["version"] != float64(1) {
		t.Fatalf("current accumulation detail: status=%d body=%v", rec.Code, detail)
	}
	if _, exists := detail["status"]; exists {
		t.Fatalf("current accumulation DTO leaked legacy status: %v", detail)
	}

	rec, generated := doCurrent(t, h, http.MethodPost,
		"/accumulation/"+id+"/dictation-to-basket",
		`{"agent":"mingming"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dictation command: status=%d body=%v", rec.Code, generated)
	}
	generation, _ := generated["dictation_generation"].(map[string]any)
	if generation["status"] != k12.DictationCommitted ||
		generation["generation_id"] == "" || generation["practice_item_id"] == "" {
		t.Fatalf("dictation generation response: %v", generated)
	}
	_, refreshed := doCurrent(t, h, http.MethodGet,
		"/accumulation/"+id+"?agent=mingming", "", nil)
	if refreshedGeneration, _ := refreshed["dictation_generation"].(map[string]any); refreshedGeneration["generation_id"] != generation["generation_id"] {
		t.Fatalf("detail did not read durable generation: command=%v detail=%v",
			generation, refreshedGeneration)
	}

	headers := map[string]string{
		"If-Match": "1", "Idempotency-Key": "delete-accum-1",
	}
	rec, deleted := doCurrent(t, h, http.MethodDelete,
		"/accumulation/"+id+"?agent=mingming", "", headers)
	if rec.Code != http.StatusOK || deleted["deleted"] != true ||
		deleted["accumulation_id"] != id || deleted["version"] != float64(2) {
		t.Fatalf("delete accumulation: status=%d body=%v", rec.Code, deleted)
	}
	rec, replay := doCurrent(t, h, http.MethodDelete,
		"/accumulation/"+id+"?agent=mingming", "", headers)
	if rec.Code != http.StatusOK || replay["version"] != deleted["version"] {
		t.Fatalf("delete replay unstable: status=%d first=%v replay=%v",
			rec.Code, deleted, replay)
	}
	if rec, _ := doCurrent(t, h, http.MethodGet,
		"/accumulation/"+id+"?agent=mingming", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("tombstoned accumulation detail status=%d", rec.Code)
	}
}

func TestCurrentCreativeWorkHTTPExactDTOGenerationAndDelete(t *testing.T) {
	h := newFeedbackServer(t, func(
		context.Context, string, string, string,
	) (string, error) {
		return "桂花落在青石板上的细节清楚；建议补充一个声音细节。", nil
	})
	rec, _ := doCurrent(t, h, http.MethodPost, "/creative-works",
		`{"agent":"mingming","work_type":"writing","content_markdown":"原文","title":"猜测标题"}`,
		map[string]string{"Idempotency-Key": "invalid-work"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("current create must reject retired title: %d", rec.Code)
	}
	rec, created := doCurrent(t, h, http.MethodPost, "/creative-works",
		`{"agent":"mingming","work_type":"writing","content_markdown":"桂花落在青石板上。"}`,
		map[string]string{"Idempotency-Key": "create-work-1"})
	if rec.Code != http.StatusOK || created["work_id"] == "" ||
		created["initial_feedback_generation_id"] == "" {
		t.Fatalf("current work create: status=%d body=%v", rec.Code, created)
	}
	id := created["work_id"].(string)

	rec, detail := doCurrent(t, h, http.MethodGet,
		"/creative-works/"+id+"?agent=mingming", "", nil)
	if rec.Code != http.StatusOK || detail["work_id"] != id ||
		detail["display_name"] != "语文写作" ||
		detail["content_markdown"] != "桂花落在青石板上。" ||
		detail["row_version"] != float64(1) {
		t.Fatalf("current work detail: status=%d body=%v", rec.Code, detail)
	}
	for _, retired := range []string{"record_id", "versions", "status", "status_label", "task", "intent"} {
		if _, exists := detail[retired]; exists {
			t.Fatalf("current work DTO leaked %s: %v", retired, detail)
		}
	}
	initial, _ := detail["initial_feedback"].(map[string]any)
	if initial["status"] != k12.WorkFeedbackQueued {
		t.Fatalf("initial feedback state: %v", initial)
	}

	rec, feedback := doCurrent(t, h, http.MethodPost,
		"/creative-works/"+id+"/generate-feedback",
		`{"agent":"mingming"}`,
		map[string]string{"Idempotency-Key": "generate-work-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("generate feedback: status=%d body=%v", rec.Code, feedback)
	}
	latest, _ := feedback["latest_feedback"].(map[string]any)
	fact, _ := latest["feedback"].(map[string]any)
	if latest["status"] != k12.WorkFeedbackSucceeded ||
		fact["feedback_id"] == "" || fact["feedback_type"] != "writing" {
		t.Fatalf("canonical latest feedback missing: %v", feedback)
	}
	for _, field := range []string{
		"evidence_refs", "visible_evidence", "affirmation",
		"parent_guidance", "next_step", "source_snapshot",
	} {
		if _, exists := fact[field]; !exists {
			t.Fatalf("feedback missing %s: %v", field, fact)
		}
	}
	if rec, _ := doCurrent(t, h, http.MethodPost,
		"/creative-works/"+id+"/revision",
		`{"agent":"mingming","content_markdown":"修改稿"}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("retired revision route status=%d", rec.Code)
	}

	headers := map[string]string{
		"If-Match": "2", "Idempotency-Key": "delete-work-1",
	}
	rec, deleted := doCurrent(t, h, http.MethodDelete,
		"/creative-works/"+id+"?agent=mingming", "", headers)
	if rec.Code != http.StatusOK || deleted["deleted"] != true ||
		deleted["work_id"] != id || deleted["row_version"] != float64(3) {
		t.Fatalf("delete work: status=%d body=%v", rec.Code, deleted)
	}
}
