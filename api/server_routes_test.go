package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestServerRoutes_AgentsRulesFallback(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/rules", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp struct {
		Rules []any `json:"rules"`
		Total int   `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Total != 0 || len(resp.Rules) != 0 {
		t.Fatalf("unexpected fallback payload: %+v", resp)
	}
}

func TestServerRoutes_LLMTestWiring(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/test", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestServerRoutes_LogsWiring(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.logCollector.Add("info", "test", "message", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs?limit=1", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
}

func TestServerRoutes_StatsWiring(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp statsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Goroutines <= 0 {
		t.Fatalf("goroutines = %d, want > 0", resp.Goroutines)
	}
}
