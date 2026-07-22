package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

const providerModelCatalogTestInstanceID = "pvd_v1_00112233445566778899aabbccddeeff"

func TestFetchProviderModels_UsesPersistedProviderIdentityAndCredential(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.URL.Path; got != "/v1/models" {
			t.Fatalf("path=%q, want /v1/models", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer server-real-secret" {
			t.Fatalf("Authorization=%q, want persisted server credential", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"server-model"}]}`)
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"renamed-provider": {
			ProviderInstanceID: providerModelCatalogTestInstanceID,
			BaseURL:            upstream.URL + "/v1",
			APIKey:             "server-real-secret",
		},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/models", strings.NewReader(`{
		"provider_instance_id":"`+providerModelCatalogTestInstanceID+`",
		"base_url":"https://client-stale.invalid/v1",
		"api_key":"****stale"
	}`))
	rec := httptest.NewRecorder()

	srv.handleFetchProviderModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", calls.Load())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"id":"server-model"`) {
		t.Fatalf("body=%s, want persisted provider catalog", body)
	}
}

func TestFetchProviderModels_UnknownProviderIdentityFailsClosed(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"data":[{"id":"must-not-be-reached"}]}`)
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/models", strings.NewReader(`{
		"provider_instance_id":"pvd_v1_ffeeddccbbaa99887766554433221100",
		"base_url":"`+upstream.URL+`/v1",
		"api_key":"client-secret"
	}`))
	rec := httptest.NewRecorder()

	srv.handleFetchProviderModels(rec, req)

	if rec.Code < http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want fail-closed client error", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("unknown identity triggered %d upstream calls, want 0", calls.Load())
	}
	if !strings.Contains(rec.Body.String(), "provider_instance_id") {
		t.Fatalf("body=%s, want identity error", rec.Body.String())
	}
}
