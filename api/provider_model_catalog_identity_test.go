package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestActiveLLMConfig_ReturnsDeepImmutableSnapshot(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"provider": {
			ProviderInstanceID: providerModelCatalogTestInstanceID,
			Models:             []string{"server-model"},
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID:           "server-model",
				Capabilities: []string{config.LLMModelCapabilityText},
			}},
		},
	}
	engine := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, engine, nil, nil)

	snapshot := srv.activeLLMConfig()
	provider := snapshot.Providers["provider"]
	provider.Models[0] = "mutated-model"
	provider.ModelSpecs[0].Capabilities[0] = config.LLMModelCapabilityVision
	delete(snapshot.Providers, "provider")

	live := engine.ActiveLLMConfig()
	if _, ok := live.Providers["provider"]; !ok {
		t.Fatal("mutating snapshot deleted the live provider")
	}
	if got := live.Providers["provider"].Models[0]; got != "server-model" {
		t.Fatalf("live model=%q after snapshot mutation", got)
	}
	if got := live.Providers["provider"].ModelSpecs[0].Capabilities[0]; got != config.LLMModelCapabilityText {
		t.Fatalf("live capability=%q after snapshot mutation", got)
	}
}

func TestFetchProviderModels_ConcurrentPUTUsesRaceFreeConfigSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"server-model"}]}`)
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.LLM.Default = ""
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"provider": {
			ProviderInstanceID: providerModelCatalogTestInstanceID,
			BaseURL:            upstream.URL + "/v1",
			APIKey:             "server-real-secret",
			Model:              "server-model",
			Models:             []string{"server-model"},
		},
	}
	engine := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, engine, nil, nil)

	const iterations = 24
	errs := make(chan string, iterations*2)
	var wg sync.WaitGroup
	for i := 0; i < iterations; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
				"providers":{"provider":{
					"provider_instance_id":"`+providerModelCatalogTestInstanceID+`",
					"api_key":"server-real-secret",
					"base_url":"`+upstream.URL+`/v1",
					"model":"server-model",
					"models":["server-model"],
					"compatible":"openai",
					"enabled":true
				}}
			}`))
			rec := httptest.NewRecorder()
			srv.handleUpdateLLMConfig(rec, req)
			if rec.Code != http.StatusOK {
				errs <- "PUT: " + rec.Body.String()
			}
		}()
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/models", strings.NewReader(`{
				"provider_instance_id":"`+providerModelCatalogTestInstanceID+`"
			}`))
			rec := httptest.NewRecorder()
			srv.handleFetchProviderModels(rec, req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"server-model"`) {
				errs <- "catalog: " + rec.Body.String()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestFetchProviderModels_UpstreamParseFailuresReturnErrorEnvelope(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		payload string
	}{
		{name: "malformed JSON", payload: `<html>bad gateway</html>`},
		{name: "no parseable model", payload: `{"data":[{"name":"missing-id"}]}`},
		{name: "empty catalog", payload: `{"data":[]}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, testCase.payload)
			}))
			defer upstream.Close()

			cfg := config.DefaultConfig()
			cfg.LLM.Providers = map[string]config.LLMProviderConfig{
				"provider": {
					ProviderInstanceID: providerModelCatalogTestInstanceID,
					BaseURL:            upstream.URL + "/v1",
					APIKey:             "server-real-secret",
				},
			}
			srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/models", strings.NewReader(`{
				"provider_instance_id":"`+providerModelCatalogTestInstanceID+`"
			}`))
			rec := httptest.NewRecorder()

			srv.handleFetchProviderModels(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, `"models":[]`) || !strings.Contains(body, `"error":`) {
				t.Fatalf("body=%s, want empty models with explicit error", body)
			}
		})
	}
}
