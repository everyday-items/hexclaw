package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func invokeOllamaDeleteForAtomicityTest(t *testing.T, s *Server, model string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/ollama/models/"+model, nil)
	req.SetPathValue("name", model)
	s.handleOllamaDelete(w, req)
	return w
}

func TestBUG20260728_015_OllamaUnloadRejectsUpstreamNon2xxBeforeAnyDelete(t *testing.T) {
	for _, upstreamStatus := range []int{http.StatusConflict, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(upstreamStatus), func(t *testing.T) {
			var deleteCalls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/generate":
					http.Error(w, "unload rejected", upstreamStatus)
				case r.Method == http.MethodDelete && r.URL.Path == "/api/delete":
					deleteCalls.Add(1)
					w.WriteHeader(http.StatusOK)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(upstream.Close)

			s := &Server{}
			setOllamaBaseURLForContractTest(t, s, upstream.URL)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/ollama/unload",
				strings.NewReader(`{"model":"qwen3-embedding:8b"}`),
			)
			s.handleOllamaUnload(w, req)

			if w.Code < http.StatusBadRequest {
				t.Fatalf(
					"sidecar status=%d body=%s, want non-2xx when Ollama unload returns %d",
					w.Code,
					w.Body.String(),
					upstreamStatus,
				)
			}
			if got := deleteCalls.Load(); got != 0 {
				t.Fatalf("Ollama /api/delete calls=%d, want 0 after failed unload", got)
			}
		})
	}
}

func TestBUG20260728_016_OllamaDeletePersistsIntentBeforeDestructiveCall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := config.DefaultConfig()
	s := &Server{cfg: cfg}
	s.SetKnowledgeEmbeddingInfo(KnowledgeEmbeddingInfo{
		Enabled:  true,
		Provider: "Ollama (local)",
		Model:    "qwen3-embedding:8b",
		Local:    true,
	})

	var persistedBeforeDelete atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/delete" {
			http.NotFound(w, r)
			return
		}
		persistedBeforeDelete.Store(s.cfg.Knowledge.Embedding.DisableAutoInstall)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	setOllamaBaseURLForContractTest(t, s, upstream.URL)

	w := invokeOllamaDeleteForAtomicityTest(t, s, "qwen3-embedding:8b")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if !persistedBeforeDelete.Load() {
		t.Fatal("DisableAutoInstall was not persisted before Ollama /api/delete")
	}
}

func TestBUG20260728_016_PersistFailureStopsBeforeDestructiveCall(t *testing.T) {
	tempDir := t.TempDir()
	homeFile := filepath.Join(tempDir, "home-is-a-file")
	if err := os.WriteFile(homeFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeFile)

	cfg := config.DefaultConfig()
	s := &Server{cfg: cfg}
	s.SetKnowledgeEmbeddingInfo(KnowledgeEmbeddingInfo{
		Enabled:  true,
		Provider: "Ollama (local)",
		Model:    "qwen3-embedding:8b",
		Local:    true,
	})

	var deleteCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/delete" {
			deleteCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)
	setOllamaBaseURLForContractTest(t, s, upstream.URL)

	w := invokeOllamaDeleteForAtomicityTest(t, s, "qwen3-embedding:8b")
	if w.Code < http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want persistence failure", w.Code, w.Body.String())
	}
	if got := deleteCalls.Load(); got != 0 {
		t.Fatalf(
			"Ollama /api/delete calls=%d, want 0 when deletion intent cannot be persisted",
			got,
		)
	}
}

func TestBUG20260728_016_AmbiguousDeleteReconcilesTagsWithoutBlindRetry(t *testing.T) {
	var deleteCalls atomic.Int64
	var tagsCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/api/delete":
			deleteCalls.Add(1)
			// Ollama applied the deletion but the response was lost.
			panic(http.ErrAbortHandler)
		case r.Method == http.MethodGet && r.URL.Path == "/api/tags":
			tagsCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	s := &Server{}
	setOllamaBaseURLForContractTest(t, s, upstream.URL)
	w := invokeOllamaDeleteForAtomicityTest(t, s, "qwen3.5:9b")

	if w.Code != http.StatusOK {
		t.Fatalf(
			"status=%d body=%s, want idempotent success after /api/tags proves model absent",
			w.Code,
			w.Body.String(),
		)
	}
	if got := deleteCalls.Load(); got != 1 {
		t.Fatalf("Ollama /api/delete calls=%d, want exactly 1", got)
	}
	if got := tagsCalls.Load(); got != 1 {
		t.Fatalf("Ollama /api/tags reconciliation calls=%d, want 1", got)
	}
}
