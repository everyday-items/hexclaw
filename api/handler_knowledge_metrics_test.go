package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/localinfer"
	"github.com/hexagon-codes/hexclaw/resourcegov"
)

func TestKnowledgeRetrievalMetricsEndpoint(t *testing.T) {
	srv, _ := newKBConfigServer(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/metrics", nil)
	req.RemoteAddr = "127.0.0.1:45678"
	srv.routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics endpoint 应返回 200，得 %d body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		FTS struct {
			Calls uint64 `json:"calls"`
		} `json:"fts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeRetrievalMetricsEndpointDoesNotLeakLocalInferenceInputsOrErrors(t *testing.T) {
	governor, err := resourcegov.New(resourcegov.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	coordinator := localinfer.New(governor)
	srv, _ := newKBConfigServer(t, knowledge.WithLocalInferenceCoordinator(coordinator))

	const (
		queryCanary  = "query-canary-52f9c7a4"
		pathCanary   = "/Users/private/path-canary-0d13/lesson.pdf"
		secretCanary = "sk-secret-canary-901e"
	)
	_, lease, err := coordinator.Acquire(context.Background(), localinfer.OperationQueryEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	lease.Finish(errors.New("embedding failed query=" + queryCanary + " path=" + pathCanary + " secret=" + secretCanary))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/metrics", nil)
	req.RemoteAddr = "127.0.0.1:45678"
	srv.routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics endpoint status=%d body=%s", w.Code, w.Body.String())
	}
	for _, canary := range []string{queryCanary, pathCanary, secretCanary} {
		if strings.Contains(w.Body.String(), canary) {
			t.Fatalf("metrics response leaked private canary %q: %s", canary, w.Body.String())
		}
	}

	var payload any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	root := assertKnowledgeMetricsJSONKeys(t, payload, "fts", "like", "local_inference", "rerank", "vector")
	laneFields := []string{"calls", "empty", "errors", "fallback_rate", "fallbacks", "hit_rate", "hits", "total_latency_ms"}
	for _, lane := range []string{"fts", "like", "vector"} {
		assertKnowledgeMetricsJSONKeys(t, root[lane], laneFields...)
	}
	rerank := assertKnowledgeMetricsJSONKeys(
		t, root["rerank"], "configured", "eligible", "executed", "failed", "skipped", "succeeded",
	)
	assertKnowledgeMetricsJSONKeys(
		t, rerank["skipped"], "disabled", "empty_result", "execution_failed", "insufficient_candidates", "no_executor",
	)

	local := assertKnowledgeMetricsJSONKeys(t, root["local_inference"], "model_load_available", "operations")
	operations := assertKnowledgeMetricsJSONKeys(
		t,
		local["operations"],
		"chat",
		"document_embedding",
		"probe",
		"query_embedding",
		"rerank",
		"warmup",
	)
	operationFields := []string{
		"admitted", "attempts", "cancelled", "completed", "failed",
		"first_output_count", "first_output_max_ms", "first_output_total_ms",
		"generation_count", "generation_max_ms", "generation_total_ms",
		"queue_wait_max_ms", "queue_wait_total_ms", "total_duration_max_ms", "total_duration_ms",
	}
	for operation, value := range operations {
		metrics := assertKnowledgeMetricsJSONKeys(t, value, operationFields...)
		if operation == string(localinfer.OperationQueryEmbedding) && metrics["failed"] != float64(1) {
			t.Fatalf("query embedding failed=%v, want 1", metrics["failed"])
		}
	}
}

func assertKnowledgeMetricsJSONKeys(t *testing.T, value any, want ...string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("metrics JSON value type=%T, want object", value)
	}
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("metrics JSON keys=%v, want %v", got, want)
	}
	return object
}
