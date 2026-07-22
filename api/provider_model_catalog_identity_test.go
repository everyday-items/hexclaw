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

func TestConfigReadAndCatalogIdentityUsePersistedSnapshotForInactiveProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer inactive-secret" {
			t.Fatalf("Authorization=%q, want persisted inactive provider credential", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"inactive-model"}]}`)
	}))
	defer upstream.Close()

	disabled := false
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"active": {
			ProviderInstanceID: "pvd_v1_ffeeddccbbaa99887766554433221100",
			APIKey:             "active-secret",
			BaseURL:            "https://active.example.test/v1",
			Model:              "active-model",
			Models:             []string{"active-model"},
		},
		"inactive": {
			ProviderInstanceID: providerModelCatalogTestInstanceID,
			APIKey:             "inactive-secret",
			BaseURL:            upstream.URL + "/v1",
			Model:              "inactive-model",
			Models:             []string{"inactive-model"},
			Enabled:            &disabled,
		},
	}
	activeOnly := cfg.LLM
	activeOnly.Providers = map[string]config.LLMProviderConfig{"active": cfg.LLM.Providers["active"]}
	srv := NewServer(cfg, &mockEngine{activeLLM: activeOnly}, nil, nil)

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil)
	getRec := httptest.NewRecorder()
	srv.handleGetLLMConfig(getRec, getReq)
	if getRec.Code != http.StatusOK || !strings.Contains(getRec.Body.String(), `"inactive"`) {
		t.Fatalf("GET status=%d body=%s, want persisted inactive provider", getRec.Code, getRec.Body.String())
	}

	catalogReq := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/models", strings.NewReader(`{
		"provider_instance_id":"`+providerModelCatalogTestInstanceID+`"
	}`))
	catalogRec := httptest.NewRecorder()
	srv.handleFetchProviderModels(catalogRec, catalogReq)
	if catalogRec.Code != http.StatusOK || !strings.Contains(catalogRec.Body.String(), `"id":"inactive-model"`) {
		t.Fatalf("catalog status=%d body=%s, want persisted inactive provider catalog", catalogRec.Code, catalogRec.Body.String())
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

func TestActiveLLMConfig_PreservesNilAndExplicitEmptyCapabilitySemantics(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"legacy": {
			Model:  "legacy-chat",
			Models: []string{"legacy-chat"},
		},
		"explicit-empty-catalog": {
			Models:     []string{"unclassified"},
			ModelSpecs: []config.LLMProviderModelSpec{},
		},
		"explicit-empty-capabilities": {
			Models: []string{"unclassified"},
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID:           "unclassified",
				Capabilities: []string{},
			}},
		},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)

	snapshot := srv.activeLLMConfig()
	legacy := snapshot.Providers["legacy"]
	if legacy.ModelSpecs != nil {
		t.Fatalf("legacy ModelSpecs=%#v, want nil so legacy text synthesis remains enabled", legacy.ModelSpecs)
	}
	if !config.ModelHasCapability(legacy, "legacy-chat", config.LLMModelCapabilityText) {
		t.Fatal("legacy text model lost text capability after snapshot clone")
	}

	emptyCatalog := snapshot.Providers["explicit-empty-catalog"]
	if emptyCatalog.ModelSpecs == nil || len(emptyCatalog.ModelSpecs) != 0 {
		t.Fatalf("explicit empty ModelSpecs=%#v, want non-nil empty slice", emptyCatalog.ModelSpecs)
	}
	if config.ModelHasCapability(emptyCatalog, "unclassified", config.LLMModelCapabilityText) {
		t.Fatal("explicit empty catalog must remain fail-closed after snapshot clone")
	}

	emptyCapabilities := snapshot.Providers["explicit-empty-capabilities"]
	if emptyCapabilities.ModelSpecs[0].Capabilities == nil || len(emptyCapabilities.ModelSpecs[0].Capabilities) != 0 {
		t.Fatalf("explicit empty capabilities=%#v, want non-nil empty slice", emptyCapabilities.ModelSpecs[0].Capabilities)
	}
	if config.ModelHasCapability(emptyCapabilities, "unclassified", config.LLMModelCapabilityText) {
		t.Fatal("explicit empty capabilities must remain fail-closed after snapshot clone")
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
	errs := make(chan string, iterations*3)
	var wg sync.WaitGroup
	for i := 0; i < iterations; i++ {
		wg.Add(3)
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
		go func() {
			defer wg.Done()
			fullRec := httptest.NewRecorder()
			srv.handleGetFullConfig(fullRec, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
			if fullRec.Code != http.StatusOK {
				errs <- "full config: " + fullRec.Body.String()
			}
			modelsRec := httptest.NewRecorder()
			srv.handleListModels(modelsRec, httptest.NewRequest(http.MethodGet, "/api/v1/models", nil))
			if modelsRec.Code != http.StatusOK {
				errs <- "models: " + modelsRec.Body.String()
			}
			_ = srv.resolveOllamaNumCtx(0)
			_ = srv.resolveOllamaKeepAlive()
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
