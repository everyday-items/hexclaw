package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Covers the pre-network input-validation guards on the Ollama management
// endpoints (model-name required). The localhost:11434 and exec-based restart
// paths are intentionally left out: they are non-hermetic and side-effecting.

func TestHandleOllamaModelGuards_RejectMissingModel(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		body    string
		wantErr string
	}{
		{"load empty model", s.handleOllamaLoad, `{"model":""}`, "model is required"},
		{"load bad json", s.handleOllamaLoad, `{bad`, "model is required"},
		{"unload empty model", s.handleOllamaUnload, `{"model":""}`, "model is required"},
		{"unload bad json", s.handleOllamaUnload, `{bad`, "model is required"},
		{"pull empty model", s.handleOllamaPull, `{"model":""}`, "model is required"},
		{"pull bad json", s.handleOllamaPull, `{bad`, "model is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.handler(w, httptest.NewRequest(http.MethodPost, "/ollama", strings.NewReader(tc.body)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantErr) {
				t.Errorf("body = %q, want it to contain %q", w.Body.String(), tc.wantErr)
			}
		})
	}
}

func TestHandleOllamaDelete_RejectsMissingName(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	// No path value set, so PathValue("name") is empty.
	s.handleOllamaDelete(w, httptest.NewRequest(http.MethodDelete, "/ollama/models/", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "model name required") {
		t.Errorf("body = %q, want it to contain %q", w.Body.String(), "model name required")
	}
}
