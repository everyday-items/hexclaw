package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type ollamaBaseURLSetter interface {
	SetOllamaBaseURL(string) error
}

func setOllamaBaseURLForContractTest(t *testing.T, s *Server, rawURL string) {
	t.Helper()
	setter, ok := any(s).(ollamaBaseURLSetter)
	if !ok {
		t.Fatalf("Server must provide a loopback-only Ollama base URL injection seam")
	}
	if err := setter.SetOllamaBaseURL(rawURL); err != nil {
		t.Fatalf("SetOllamaBaseURL(%q): %v", rawURL, err)
	}
}

func TestHandleOllamaPull_ForwardsOfficialModelField(t *testing.T) {
	type pullRequest struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	type observed struct {
		Request pullRequest
		Err     error
	}
	received := make(chan observed, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/pull" {
			http.NotFound(w, r)
			return
		}
		var got pullRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&got)
		received <- observed{Request: got, Err: err}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte("{\"status\":\"success\"}\n"))
	}))
	t.Cleanup(upstream.Close)

	s := &Server{}
	setOllamaBaseURLForContractTest(t, s, upstream.URL)
	w := httptest.NewRecorder()
	// Extra client metadata must remain forward-compatible; it must not turn the
	// public request schema into additionalProperties:false.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ollama/pull", strings.NewReader(
		`{"model":"nomic-embed-text:latest","client_trace_id":"contract-test"}`,
	))
	s.handleOllamaPull(w, req)

	got := <-received
	if got.Err != nil {
		t.Fatalf("Ollama rejected pull body: %v", got.Err)
	}
	if got.Request.Model != "nomic-embed-text:latest" {
		t.Fatalf("model = %q, want nomic-embed-text:latest", got.Request.Model)
	}
	if !got.Request.Stream {
		t.Fatal("stream = false, want true")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleOllamaDelete_ForwardsOfficialModelField(t *testing.T) {
	type deleteRequest struct {
		Model string `json:"model"`
	}
	type observed struct {
		Request deleteRequest
		Err     error
	}
	received := make(chan observed, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/delete" {
			http.NotFound(w, r)
			return
		}
		var got deleteRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&got)
		received <- observed{Request: got, Err: err}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	s := &Server{}
	setOllamaBaseURLForContractTest(t, s, upstream.URL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/ollama/models/nomic-embed-text:latest", nil)
	req.SetPathValue("name", "nomic-embed-text:latest")
	s.handleOllamaDelete(w, req)

	got := <-received
	if got.Err != nil {
		t.Fatalf("Ollama rejected delete body: %v", got.Err)
	}
	if got.Request.Model != "nomic-embed-text:latest" {
		t.Fatalf("model = %q, want nomic-embed-text:latest", got.Request.Model)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleOllamaPull_RejectsLegacyNameOnlyBody(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ollama/pull", strings.NewReader(
		`{"name":"nomic-embed-text:latest"}`,
	))
	s.handleOllamaPull(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model is required") {
		t.Fatalf("body = %q, want model-required error", w.Body.String())
	}
}

func TestServerSetOllamaBaseURL_RejectsNonLoopbackEndpoint(t *testing.T) {
	s := &Server{}
	setter, ok := any(s).(ollamaBaseURLSetter)
	if !ok {
		t.Fatalf("Server must provide a loopback-only Ollama base URL injection seam")
	}
	if err := setter.SetOllamaBaseURL("https://ollama.example.com"); err == nil {
		t.Fatal("non-loopback Ollama endpoint must be rejected")
	}
}
