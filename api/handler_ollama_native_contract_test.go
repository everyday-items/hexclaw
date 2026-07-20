package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
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

func TestHandleOllamaPullRejects307BeforeReplayingBodyCrossOrigin(t *testing.T) {
	var redirectedRequests atomic.Int64
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte("{\"status\":\"success\"}\n"))
	}))
	defer redirected.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirected.URL+"/exfiltrate", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	s := &Server{}
	setOllamaBaseURLForContractTest(t, s, origin.URL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ollama/pull", strings.NewReader(
		`{"model":"private-model-name"}`,
	))
	s.handleOllamaPull(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want redirect rejection", w.Code, w.Body.String())
	}
	if redirectedRequests.Load() != 0 {
		t.Fatalf("Ollama pull replay reached foreign origin %d times", redirectedRequests.Load())
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
	for _, baseURL := range []string{
		"https://ollama.example.com",
		"http://192.168.10.20:11434/v1",
		"http://ollama.local:11434/v1",
		"http://host.docker.internal:11434/v1",
	} {
		t.Run(baseURL, func(t *testing.T) {
			s := &Server{}
			setter, ok := any(s).(ollamaBaseURLSetter)
			if !ok {
				t.Fatalf("Server must provide a loopback-only Ollama base URL injection seam")
			}
			if err := setter.SetOllamaBaseURL(baseURL); err == nil {
				t.Fatalf("non-loopback Ollama endpoint %q must be rejected", baseURL)
			}
		})
	}
}

func TestHandleOllamaPullNotifiesInstalledModelOnlyAfterSuccess(t *testing.T) {
	for _, tt := range []struct {
		name       string
		upstream   string
		wantCalled bool
	}{
		{name: "success", upstream: "{\"status\":\"pulling manifest\"}\n{\"status\":\"success\"}\n", wantCalled: true},
		{name: "upstream error event", upstream: "{\"status\":\"error\",\"error\":\"disk full\"}\n", wantCalled: false},
		{name: "later error overrides success", upstream: "{\"status\":\"success\"}\n{\"status\":\"error\",\"error\":\"checksum mismatch\"}\n", wantCalled: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte(tt.upstream))
			}))
			defer upstream.Close()

			s := &Server{}
			setOllamaBaseURLForContractTest(t, s, upstream.URL)
			called := make(chan string, 1)
			s.SetOllamaModelInstalledCallback(func(_ context.Context, model string) {
				called <- model
			})
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/ollama/pull", strings.NewReader(
				`{"model":"nomic-embed-text:latest"}`,
			))
			s.handleOllamaPull(w, req)

			select {
			case model := <-called:
				if !tt.wantCalled {
					t.Fatalf("unexpected installed callback for %q", model)
				}
				if model != "nomic-embed-text:latest" {
					t.Fatalf("callback model=%q", model)
				}
			default:
				if tt.wantCalled {
					t.Fatal("successful pull did not notify installed model")
				}
			}
		})
	}
}

func TestHandleOllamaPullSurvivesRequestCancelButStopsWithServerLifecycle(t *testing.T) {
	upstreamStarted := make(chan struct{}, 1)
	upstreamCanceled := make(chan error, 1)
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamStarted <- struct{}{}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			upstreamCanceled <- r.Context().Err()
		case <-releaseUpstream:
		}
	}))
	defer upstream.Close()
	defer close(releaseUpstream)

	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	defer cancelLifecycle()
	s := &Server{cfg: config.DefaultConfig()}
	setOllamaBaseURLForContractTest(t, s, upstream.URL)
	_ = s.buildHTTPServer(lifecycleCtx)

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ollama/pull", strings.NewReader(
		`{"model":"nomic-embed-text:latest"}`,
	)).WithContext(requestCtx)
	done := make(chan struct{})
	go func() {
		s.handleOllamaPull(httptest.NewRecorder(), req)
		close(done)
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Ollama pull did not reach upstream")
	}

	cancelRequest()
	select {
	case err := <-upstreamCanceled:
		t.Fatalf("SSE request cancellation leaked into detached Ollama pull: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancelLifecycle()
	select {
	case err := <-upstreamCanceled:
		if err == nil {
			t.Fatal("upstream cancellation reason is nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server lifecycle cancellation did not stop detached Ollama pull")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Ollama pull handler did not exit after server lifecycle cancellation")
	}
}

func TestServerStopWithDrainClosesListenerBeforeCancelingRuntime(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	releaseRequest := make(chan struct{})
	s := &Server{}
	s.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestStarted <- struct{}{}
		<-releaseRequest
		w.WriteHeader(http.StatusNoContent)
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.server.Serve(listener) }()

	requestDone := make(chan error, 1)
	go func() {
		resp, requestErr := http.Get("http://" + listener.Addr().String())
		if resp != nil {
			_ = resp.Body.Close()
		}
		requestDone <- requestErr
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("test request did not reach server")
	}

	listenerWasClosed := make(chan bool, 1)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShutdown()
	err = s.StopWithDrain(shutdownCtx, func() {
		conn, dialErr := net.DialTimeout("tcp", listener.Addr().String(), 100*time.Millisecond)
		if conn != nil {
			_ = conn.Close()
		}
		listenerWasClosed <- dialErr != nil
		close(releaseRequest)
	})
	if err != nil {
		t.Fatalf("StopWithDrain: %v", err)
	}
	if closed := <-listenerWasClosed; !closed {
		t.Fatal("runtime drain began while HTTP listener was still accepting connections")
	}
	select {
	case requestErr := <-requestDone:
		if requestErr != nil {
			t.Fatalf("in-flight request did not drain cleanly: %v", requestErr)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight request did not finish during shutdown")
	}
	select {
	case serveErr := <-serveDone:
		if serveErr != nil && serveErr != http.ErrServerClosed {
			t.Fatalf("Serve returned %v", serveErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not exit after shutdown")
	}
}
