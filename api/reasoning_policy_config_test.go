package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestLLMDefaultReasoningPolicyGetUpdateAndYAMLRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)

	get := httptest.NewRecorder()
	srv.handleGetLLMConfig(get, httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"default_reasoning_policy":{"mode":"auto"}`) {
		t.Fatalf("default GET status=%d body=%s", get.Code, get.Body.String())
	}

	put := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(
		`{"default_reasoning_policy":{"mode":"effort","effort":"xhigh"}}`,
	))
	srv.handleUpdateLLMConfig(put, request)
	if put.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", put.Code, put.Body.String())
	}
	want := config.ReasoningPolicy{Mode: config.ReasoningPolicyModeEffort, Effort: config.ReasoningEffortXHigh}
	if srv.cfg.LLM.DefaultReasoningPolicy != want {
		t.Fatalf("in-memory policy = %+v, want %+v", srv.cfg.LLM.DefaultReasoningPolicy, want)
	}

	loaded, err := config.Load(filepath.Join(home, ".hexclaw", "hexclaw.yaml"))
	if err != nil {
		t.Fatalf("reload YAML: %v", err)
	}
	if loaded.LLM.DefaultReasoningPolicy != want {
		t.Fatalf("reloaded policy = %+v, want %+v", loaded.LLM.DefaultReasoningPolicy, want)
	}

	omitted := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(omitted, httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{}`)))
	if omitted.Code != http.StatusOK {
		t.Fatalf("omitted policy PUT status=%d body=%s", omitted.Code, omitted.Body.String())
	}
	if srv.cfg.LLM.DefaultReasoningPolicy != want {
		t.Fatalf("omitted policy update changed value to %+v", srv.cfg.LLM.DefaultReasoningPolicy)
	}

	get = httptest.NewRecorder()
	srv.handleGetLLMConfig(get, httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"default_reasoning_policy":{"mode":"effort","effort":"xhigh"}`) {
		t.Fatalf("updated GET status=%d body=%s", get.Code, get.Body.String())
	}
}

func TestLLMDefaultReasoningPolicyRejectsInvalidCombinations(t *testing.T) {
	tests := []string{
		`{"default_reasoning_policy":{"mode":"inherit"}}`,
		`{"default_reasoning_policy":{"mode":"effort"}}`,
		`{"default_reasoning_policy":{"mode":"on","effort":"high"}}`,
		`{"default_reasoning_policy":{"mode":"effort","effort":"extreme"}}`,
		`{"default_reasoning_policy":{"mode":"sometimes"}}`,
	}

	for _, body := range tests {
		t.Run(body, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			cfg := config.DefaultConfig()
			srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
			w := httptest.NewRecorder()
			srv.handleUpdateLLMConfig(w, httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("invalid policy status=%d body=%s", w.Code, w.Body.String())
			}
			if got := srv.cfg.LLM.DefaultReasoningPolicy; got != (config.ReasoningPolicy{Mode: config.ReasoningPolicyModeAuto}) {
				t.Fatalf("invalid update mutated policy to %+v", got)
			}
		})
	}
}
