package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestFetchProviderModels_ReasoningMetadataPreservesValidExactContract(t *testing.T) {
	const upstreamModel = `{
		"id":"gpt-5.6-sol",
		"reasoning_support":"supported",
		"reasoning_control":{
			"dialect":"reasoning_effort",
			"on":"high",
			"off":"none",
			"allowed_efforts":["low","medium","high","xhigh","max"]
		}
	}`

	model := fetchProviderModelCatalogReasoningEntry(t, upstreamModel)
	if got := decodeCatalogReasoningString(t, model, "reasoning_support"); got != config.LLMReasoningSupportSupported {
		t.Fatalf("reasoning_support=%q, want %q", got, config.LLMReasoningSupportSupported)
	}

	wantControl := map[string]any{
		"dialect":         config.LLMReasoningDialectEffort,
		"on":              "high",
		"off":             "none",
		"allowed_efforts": []any{"low", "medium", "high", "xhigh", "max"},
	}
	var gotControl map[string]any
	control, ok := model["reasoning_control"]
	if !ok {
		t.Fatal("reasoning_control is missing from the normalized catalog entry")
	}
	if err := json.Unmarshal(control, &gotControl); err != nil {
		t.Fatalf("decode reasoning_control: %v", err)
	}
	if !reflect.DeepEqual(gotControl, wantControl) {
		t.Fatalf("reasoning_control=%#v, want exact upstream contract %#v", gotControl, wantControl)
	}
}

func TestFetchProviderModels_ReasoningMetadataMissingRemainsUnspecified(t *testing.T) {
	model := fetchProviderModelCatalogReasoningEntry(t, `{"id":"legacy-chat-model"}`)
	if _, exists := model["reasoning_support"]; exists {
		t.Fatalf("missing upstream reasoning_support became %s, want field omitted", model["reasoning_support"])
	}
	if _, exists := model["reasoning_control"]; exists {
		t.Fatalf("missing upstream reasoning_control became %s, want field omitted", model["reasoning_control"])
	}
}

func TestFetchProviderModels_ReasoningMetadataInvalidDeclarationsFailClosed(t *testing.T) {
	validControl := `{"dialect":"think","on":true,"off":false}`
	testCases := []struct {
		name     string
		metadata string
	}{
		{
			name:     "unknown support value",
			metadata: `"reasoning_support":"enabled"`,
		},
		{
			name:     "supported without control",
			metadata: `"reasoning_support":"supported"`,
		},
		{
			name:     "unsupported with control",
			metadata: `"reasoning_support":"unsupported","reasoning_control":` + validControl,
		},
		{
			name:     "supported with invalid dialect",
			metadata: `"reasoning_support":"supported","reasoning_control":{"dialect":"auto","on":true,"off":false}`,
		},
		{
			name:     "effort control with invalid allowed effort",
			metadata: `"reasoning_support":"supported","reasoning_control":{"dialect":"reasoning_effort","on":"ultra","off":"none","allowed_efforts":["high","ultra"]}`,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			model := fetchProviderModelCatalogReasoningEntry(
				t,
				`{"id":"invalid-reasoning-model",`+testCase.metadata+`}`,
			)
			if got := decodeCatalogReasoningString(t, model, "reasoning_support"); got != config.LLMReasoningSupportUnknown {
				t.Fatalf("reasoning_support=%q, want fail-closed %q", got, config.LLMReasoningSupportUnknown)
			}
			if control, exists := model["reasoning_control"]; exists {
				t.Fatalf("invalid declaration leaked reasoning_control=%s", control)
			}
		})
	}
}

func fetchProviderModelCatalogReasoningEntry(t *testing.T, upstreamModel string) map[string]json.RawMessage {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/models" {
			t.Errorf("upstream path=%q, want /v1/models", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[`+upstreamModel+`]}`)
	}))
	t.Cleanup(upstream.Close)

	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"catalog-reasoning-provider": {
			ProviderInstanceID: providerModelCatalogTestInstanceID,
			BaseURL:            upstream.URL + "/v1",
			APIKey:             "fake-catalog-key",
		},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/config/llm/models",
		strings.NewReader(`{"provider_instance_id":"`+providerModelCatalogTestInstanceID+`"}`),
	)
	rec := httptest.NewRecorder()

	srv.handleFetchProviderModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Models []map[string]json.RawMessage `json:"models"`
		Error  string                       `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response body %q: %v", rec.Body.String(), err)
	}
	if response.Error != "" {
		t.Fatalf("catalog error=%q", response.Error)
	}
	if len(response.Models) != 1 {
		t.Fatalf("models=%d body=%s, want one normalized model", len(response.Models), rec.Body.String())
	}
	if got := decodeCatalogReasoningString(t, response.Models[0], "id"); got == "" {
		t.Fatal("normalized catalog model id is empty")
	}
	return response.Models[0]
}

func decodeCatalogReasoningString(t *testing.T, model map[string]json.RawMessage, field string) string {
	t.Helper()
	raw, ok := model[field]
	if !ok {
		t.Fatalf("catalog model is missing %s: %#v", field, model)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %s=%s: %v", field, raw, err)
	}
	return value
}
