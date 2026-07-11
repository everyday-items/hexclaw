package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
)

func newBlockingWarmupServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			_, _ = w.Write([]byte(`{"models":[]}`))
			return
		}
		once.Do(func() { close(started) })
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv, started
}

func TestStartLocalWarmup_CancelIsWaitable(t *testing.T) {
	srv, started := newBlockingWarmupServer(t)
	eng := newWarmupTestEngine(t, map[string]config.LLMProviderConfig{
		"Ollama (本地)": {BaseURL: srv.URL + "/v1", Model: "m"},
	}, "Ollama (本地)")

	handle := eng.StartLocalWarmup(context.Background(), time.Minute)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("warmup request did not start")
	}
	handle.Cancel()
	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := handle.Wait(waitCtx); err == nil {
		t.Fatal("canceled warmup must report cancellation")
	}
}

func TestStartLocalWarmup_TimeoutIsWaitable(t *testing.T) {
	srv, _ := newBlockingWarmupServer(t)
	eng := newWarmupTestEngine(t, map[string]config.LLMProviderConfig{
		"Ollama (本地)": {BaseURL: srv.URL + "/v1", Model: "m"},
	}, "Ollama (本地)")

	handle := eng.StartLocalWarmup(context.Background(), 30*time.Millisecond)
	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := handle.Wait(waitCtx)
	if err == nil || (!errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline")) {
		t.Fatalf("timed warmup must report deadline, got %v", err)
	}
}

func TestEngineStop_CancelsInFlightWarmup(t *testing.T) {
	srv, started := newBlockingWarmupServer(t)
	eng := newWarmupTestEngine(t, map[string]config.LLMProviderConfig{
		"Ollama (本地)": {BaseURL: srv.URL + "/v1", Model: "m"},
	}, "Ollama (本地)")
	handle := eng.StartLocalWarmup(context.Background(), time.Minute)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("warmup request did not start")
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := handle.Wait(waitCtx); errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("engine Stop returned while warmup was still running")
	}
}
