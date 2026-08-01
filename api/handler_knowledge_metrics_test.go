package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
