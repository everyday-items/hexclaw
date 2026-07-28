package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

const bug20260728ProviderInstanceID = "pvd_v1_00112233445566778899aabbccddeeff"

type bug20260728ProbeProvider struct {
	err error
}

func (p *bug20260728ProbeProvider) Complete(
	_ context.Context,
	_ hexagon.CompletionRequest,
) (*hexagon.CompletionResponse, error) {
	if p.err != nil {
		return nil, p.err
	}
	return &hexagon.CompletionResponse{Content: "OK"}, nil
}

func bug20260728ProviderConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			ProviderInstanceID: bug20260728ProviderInstanceID,
			DisplayName:        "HexClaw-GPT",
			APIKey:             "sk-provider-probe-red",
			BaseURL:            "https://provider.example.test/v1",
			Model:              "gpt-5.6-sol",
			Models:             []string{"gpt-5.6-sol"},
			ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID:           "gpt-5.6-sol",
				Capabilities: []string{config.LLMModelCapabilityText},
			}},
			Compatible: "openai",
			Locality:   config.ProviderLocalityCloud,
		},
	}
	cfg.LLM.Default = "custom"
	return cfg
}

func bug20260728OpenStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	store, err := sqlitestore.New(t.TempDir() + "/provider-probe-red.db")
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	if err := store.Init(context.Background()); err != nil {
		_ = store.Close()
		t.Fatalf("init sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func bug20260728ProbeBody(includeProviderID bool) string {
	providerID := ""
	if includeProviderID {
		providerID = `,"provider_instance_id":"` + bug20260728ProviderInstanceID + `"`
	}
	return `{"provider":{"type":"custom","base_url":"https://provider.example.test/v1",` +
		`"api_key":"sk-provider-probe-red","model":"gpt-5.6-sol","locality":"cloud"` +
		providerID + `}}`
}

func bug20260728RunProbe(
	t *testing.T,
	srv *Server,
	includeProviderID bool,
) map[string]any {
	t.Helper()
	return bug20260728RunProbeBody(t, srv, bug20260728ProbeBody(includeProviderID))
}

func bug20260728RunProbeForProviderID(
	t *testing.T,
	srv *Server,
	providerID string,
) map[string]any {
	t.Helper()
	body := `{"provider":{"type":"custom","base_url":"https://provider.example.test/v1",` +
		`"api_key":"sk-provider-probe-red","model":"gpt-5.6-sol","locality":"cloud",` +
		`"provider_instance_id":"` + providerID + `"}}`
	return bug20260728RunProbeBody(t, srv, body)
}

func bug20260728RunProbeBody(
	t *testing.T,
	srv *Server,
	body string,
) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/config/llm/test",
		strings.NewReader(body),
	)
	srv.handleTestLLMConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode probe response: %v", err)
	}
	return payload
}

func bug20260728GetProvider(
	t *testing.T,
	srv *Server,
) map[string]any {
	t.Helper()
	return bug20260728GetProviderByKey(t, srv, "custom")
}

func bug20260728GetProviderByKey(
	t *testing.T,
	srv *Server,
	key string,
) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleGetLLMConfig(
		rec,
		httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("get config status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode config response: %v", err)
	}
	provider, ok := payload.Providers[key]
	if !ok {
		t.Fatalf("%s provider missing: %s", key, rec.Body.String())
	}
	return provider
}

func bug20260728AssertPersistedResponse(t *testing.T, payload map[string]any) {
	t.Helper()
	persisted, exists := payload["persisted"]
	if !exists {
		t.Errorf("probe response missing persisted receipt fact: %#v", payload)
	} else if persisted != true {
		t.Errorf("persisted=%v, want true", persisted)
	}
	testedAt, exists := payload["tested_at"]
	if !exists || testedAt == nil || testedAt == "" || testedAt == float64(0) {
		t.Errorf("probe response tested_at=%v exists=%v, want durable timestamp", testedAt, exists)
	}
}

func bug20260728AssertReceipt(
	t *testing.T,
	provider map[string]any,
	wantOutcome string,
) map[string]any {
	t.Helper()
	return bug20260728AssertReceiptForProviderID(
		t,
		provider,
		wantOutcome,
		bug20260728ProviderInstanceID,
	)
}

func bug20260728AssertReceiptForProviderID(
	t *testing.T,
	provider map[string]any,
	wantOutcome string,
	wantProviderID string,
) map[string]any {
	t.Helper()
	raw, exists := provider["probe_receipt"]
	if !exists {
		t.Errorf("GET provider missing probe_receipt: %#v", provider)
		return nil
	}
	receipt, ok := raw.(map[string]any)
	if !ok {
		t.Errorf("probe_receipt type=%T, want object", raw)
		return nil
	}
	if got := receipt["outcome"]; got != wantOutcome {
		t.Errorf("probe_receipt.outcome=%v, want %q", got, wantOutcome)
	}
	if got := receipt["provider_instance_id"]; got != wantProviderID {
		t.Errorf("probe_receipt.provider_instance_id=%v, want %q", got, wantProviderID)
	}
	return receipt
}

func TestBUG20260728ProviderProbeReceipt_RestoresPassedAndFailedAfterServerRecreation(t *testing.T) {
	tests := []struct {
		name        string
		probeErr    error
		wantOutcome string
	}{
		{name: "passed", wantOutcome: "passed"},
		{name: "failed", probeErr: errors.New("upstream unavailable"), wantOutcome: "failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := bug20260728OpenStore(t)
			cfg := bug20260728ProviderConfig()
			oldFactory := llmTestProviderFactory
			llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider {
				return &bug20260728ProbeProvider{err: tt.probeErr}
			}
			defer func() { llmTestProviderFactory = oldFactory }()

			first := NewServer(cfg, &mockEngine{}, nil, store)
			probe := bug20260728RunProbe(t, first, true)
			bug20260728AssertPersistedResponse(t, probe)

			restarted := NewServer(cfg, &mockEngine{}, nil, store)
			provider := bug20260728GetProvider(t, restarted)
			bug20260728AssertReceipt(t, provider, tt.wantOutcome)
		})
	}
}

func TestBUG20260728ProviderProbeReceipt_DisplayNamePreservesButFingerprintChangesInvalidate(t *testing.T) {
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider {
		return &bug20260728ProbeProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	t.Run("display name is not part of connectivity fingerprint", func(t *testing.T) {
		store := bug20260728OpenStore(t)
		cfg := bug20260728ProviderConfig()
		srv := NewServer(cfg, &mockEngine{}, nil, store)
		bug20260728RunProbe(t, srv, true)
		bug20260728AssertReceipt(t, bug20260728GetProvider(t, srv), "passed")

		provider := cfg.LLM.Providers["custom"]
		provider.DisplayName = "Renamed display only"
		cfg.LLM.Providers["custom"] = provider
		bug20260728AssertReceipt(t, bug20260728GetProvider(t, srv), "passed")
	})

	tests := []struct {
		name   string
		mutate func(config.LLMProviderConfig) config.LLMProviderConfig
	}{
		{
			name: "api key",
			mutate: func(p config.LLMProviderConfig) config.LLMProviderConfig {
				p.APIKey = "sk-rotated"
				return p
			},
		},
		{
			name: "base url",
			mutate: func(p config.LLMProviderConfig) config.LLMProviderConfig {
				p.BaseURL = "https://changed.example.test/v1"
				return p
			},
		},
		{
			name: "selected model",
			mutate: func(p config.LLMProviderConfig) config.LLMProviderConfig {
				p.Model = "gpt-5.6-terra"
				return p
			},
		},
		{
			name: "locality",
			mutate: func(p config.LLMProviderConfig) config.LLMProviderConfig {
				p.Locality = config.ProviderLocalityLocal
				return p
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name+" invalidates", func(t *testing.T) {
			store := bug20260728OpenStore(t)
			cfg := bug20260728ProviderConfig()
			srv := NewServer(cfg, &mockEngine{}, nil, store)
			bug20260728RunProbe(t, srv, true)
			bug20260728AssertReceipt(t, bug20260728GetProvider(t, srv), "passed")

			cfg.LLM.Providers["custom"] = tt.mutate(cfg.LLM.Providers["custom"])
			if _, exists := bug20260728GetProvider(t, srv)["probe_receipt"]; exists {
				t.Errorf("stale receipt remained visible after %s changed", tt.name)
			}
		})
	}
}

func TestBUG20260728ProviderProbeReceipt_MissingProviderIDRemainsStateless(t *testing.T) {
	store := bug20260728OpenStore(t)
	cfg := bug20260728ProviderConfig()
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider {
		return &bug20260728ProbeProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	srv := NewServer(cfg, &mockEngine{}, nil, store)
	payload := bug20260728RunProbe(t, srv, false)
	persisted, exists := payload["persisted"]
	if !exists {
		t.Errorf("stateless probe response must explicitly return persisted=false: %#v", payload)
	} else if persisted != false {
		t.Errorf("stateless persisted=%v, want false", persisted)
	}
	if _, exists := bug20260728GetProvider(t, srv)["probe_receipt"]; exists {
		t.Error("probe without provider_instance_id must not create a durable receipt")
	}
}

func TestBUG20260728ProviderProbeReceipt_StableIdentityIsIndependentFromProviderMapKey(t *testing.T) {
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider {
		return &bug20260728ProbeProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	store := bug20260728OpenStore(t)
	cfg := bug20260728ProviderConfig()
	provider := cfg.LLM.Providers["custom"]
	delete(cfg.LLM.Providers, "custom")
	cfg.LLM.Providers["HexClaw-GPT"] = provider
	cfg.LLM.Default = "HexClaw-GPT"

	srv := NewServer(cfg, &mockEngine{}, nil, store)
	payload := bug20260728RunProbe(t, srv, true)
	bug20260728AssertPersistedResponse(t, payload)
	bug20260728AssertReceipt(
		t,
		bug20260728GetProviderByKey(t, srv, "HexClaw-GPT"),
		"passed",
	)
}

func TestBUG20260728ProviderProbeReceipt_LegacyEffectiveIdentityCanPersistFirstReceipt(t *testing.T) {
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider {
		return &bug20260728ProbeProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	store := bug20260728OpenStore(t)
	cfg := bug20260728ProviderConfig()
	provider := cfg.LLM.Providers["custom"]
	provider.ProviderInstanceID = ""
	cfg.LLM.Providers["custom"] = provider
	effectiveID := config.EffectiveProviderInstanceID("custom", provider)
	if effectiveID == "" {
		t.Fatal("legacy provider must expose a deterministic effective identity")
	}

	srv := NewServer(cfg, &mockEngine{}, nil, store)
	payload := bug20260728RunProbeForProviderID(t, srv, effectiveID)
	bug20260728AssertPersistedResponse(t, payload)
	bug20260728AssertReceiptForProviderID(
		t,
		bug20260728GetProvider(t, srv),
		"passed",
		effectiveID,
	)
}

func TestBUG20260728ProviderProbeReceipt_MapKeyRenamePreservesCanonicalReceipt(t *testing.T) {
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider {
		return &bug20260728ProbeProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	store := bug20260728OpenStore(t)
	cfg := bug20260728ProviderConfig()
	srv := NewServer(cfg, &mockEngine{}, nil, store)
	bug20260728RunProbe(t, srv, true)
	bug20260728AssertReceipt(t, bug20260728GetProvider(t, srv), "passed")

	provider := cfg.LLM.Providers["custom"]
	delete(cfg.LLM.Providers, "custom")
	cfg.LLM.Providers["HexClaw-GPT"] = provider
	cfg.LLM.Default = "HexClaw-GPT"
	bug20260728AssertReceipt(
		t,
		bug20260728GetProviderByKey(t, srv, "HexClaw-GPT"),
		"passed",
	)
}

func TestBUG20260728ProviderProbeReceipt_APIProjectionOmitsInternalFingerprint(t *testing.T) {
	oldFactory := llmTestProviderFactory
	llmTestProviderFactory = func(llmConnectionTestProvider) completionProvider {
		return &bug20260728ProbeProvider{}
	}
	defer func() { llmTestProviderFactory = oldFactory }()

	store := bug20260728OpenStore(t)
	cfg := bug20260728ProviderConfig()
	srv := NewServer(cfg, &mockEngine{}, nil, store)
	bug20260728RunProbe(t, srv, true)
	receipt := bug20260728AssertReceipt(t, bug20260728GetProvider(t, srv), "passed")
	if _, exposed := receipt["config_fingerprint"]; exposed {
		t.Error("GET config must not expose the server-internal connection fingerprint")
	}
}
