package apihttp_test

import (
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
)

func TestBUG20260725014AccumulationDictationHTTPProjectsReAddAndRejoinsOnce(t *testing.T) {
	h := newServerWithSolver(t, fakeSolveExec{},
		assembly.WithAccumulationMetadataDeriver(&currentAccumulationDeriver{}))
	rec, created := doCurrent(t, h, http.MethodPost, "/accumulation?agent=mingming",
		`{"content":"桂花香"}`,
		map[string]string{"Idempotency-Key": "accumulation-readd-create"})
	if rec.Code != http.StatusOK || created["record_id"] == "" {
		t.Fatalf("create accumulation: status=%d body=%v", rec.Code, created)
	}
	accumulationID := created["record_id"].(string)

	rec, firstResponse := doCurrent(t, h, http.MethodPost,
		"/accumulation/"+accumulationID+"/dictation-to-basket",
		`{"agent":"mingming"}`, nil)
	first := firstResponse["dictation_generation"].(map[string]any)
	if rec.Code != http.StatusOK || first["status"] != "committed" ||
		first["practice_item_id"] == "" {
		t.Fatalf("first dictation: status=%d body=%v", rec.Code, firstResponse)
	}
	firstItemID := first["practice_item_id"].(string)

	rec, sets := doCurrent(t, h, http.MethodGet,
		"/practice-sets?agent=mingming&status=draft", "", nil)
	items := sets["items"].([]any)
	if rec.Code != http.StatusOK || len(items) != 1 {
		t.Fatalf("draft basket list: status=%d body=%v", rec.Code, sets)
	}
	basketID := items[0].(map[string]any)["record_id"].(string)
	rec, removed := doCurrent(t, h, http.MethodPost,
		"/practice-sets/"+basketID+"/items/remove",
		`{"agent":"mingming","item_id":"`+firstItemID+`"}`, nil)
	if rec.Code != http.StatusOK || len(removed["items"].([]any)) != 0 {
		t.Fatalf("remove dictation: status=%d body=%v", rec.Code, removed)
	}

	rec, detail := doCurrent(t, h, http.MethodGet,
		"/accumulation/"+accumulationID+"?agent=mingming", "", nil)
	projected := detail["dictation_generation"].(map[string]any)
	if rec.Code != http.StatusOK || projected["status"] != "re_add" {
		t.Fatalf("removed projection: status=%d generation=%v", rec.Code, projected)
	}
	if _, exists := projected["practice_item_id"]; exists {
		t.Fatalf("re_add projection retained removed item: %v", projected)
	}

	rec, secondResponse := doCurrent(t, h, http.MethodPost,
		"/accumulation/"+accumulationID+"/dictation-to-basket",
		`{"agent":"mingming"}`, nil)
	second := secondResponse["dictation_generation"].(map[string]any)
	if rec.Code != http.StatusOK || second["status"] != "committed" ||
		second["practice_item_id"] == "" || second["practice_item_id"] == firstItemID {
		t.Fatalf("second dictation: status=%d first=%v second=%v",
			rec.Code, first, second)
	}
	secondItemID := second["practice_item_id"]

	rec, replayResponse := doCurrent(t, h, http.MethodPost,
		"/accumulation/"+accumulationID+"/dictation-to-basket",
		`{"agent":"mingming"}`, nil)
	replay := replayResponse["dictation_generation"].(map[string]any)
	if rec.Code != http.StatusOK || replay["generation_id"] != second["generation_id"] ||
		replay["practice_item_id"] != secondItemID {
		t.Fatalf("second command replay: status=%d second=%v replay=%v",
			rec.Code, second, replay)
	}

	rec, basket := doCurrent(t, h, http.MethodGet,
		"/practice-sets/"+basketID+"?agent=mingming", "", nil)
	basketItems := basket["items"].([]any)
	if rec.Code != http.StatusOK || len(basketItems) != 1 ||
		basketItems[0].(map[string]any)["item_id"] != secondItemID {
		t.Fatalf("re-added basket exact-set: status=%d body=%v", rec.Code, basket)
	}
}
