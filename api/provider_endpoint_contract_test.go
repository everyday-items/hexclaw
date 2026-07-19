package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

type providerEndpointRoundTripper func(*http.Request) (*http.Response, error)

func (f providerEndpointRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestProviderEndpointContract_RoundTripsConfirmationAndPrivateHostAuthorization(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.DefaultConfig()
	eng := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, eng, nil, nil)
	put := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{
		"default":"corp-gateway",
		"providers":{"corp-gateway":{
			"api_key":"sk-test",
			"base_url":"http://10.0.0.8:8080/v1",
			"model":"corp-model",
			"compatible":"openai",
			"locality":"cloud",
			"locality_source":"user",
			"confirmed_endpoint_host":"10.0.0.8",
			"private_network_access":{"host":"10.0.0.8","allowed":true}
		}}
	}`))
	putRec := httptest.NewRecorder()
	srv.handleUpdateLLMConfig(putRec, put)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", putRec.Code, putRec.Body.String())
	}

	getRec := httptest.NewRecorder()
	srv.handleGetLLMConfig(getRec, httptest.NewRequest(http.MethodGet, "/api/v1/config/llm", nil))
	body := getRec.Body.String()
	for _, want := range []string{
		`"locality_source":"user"`,
		`"confirmed_endpoint_host":"10.0.0.8"`,
		`"private_network_access":{"host":"10.0.0.8","allowed":true}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET lost %s: %s", want, body)
		}
	}
}

func TestProviderEndpointContract_BlocksPrivateModelCatalogWithoutExactAuthorization(t *testing.T) {
	original := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = original })
	called := false
	http.DefaultClient = &http.Client{Transport: providerEndpointRoundTripper(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{"data":[]}`)),
			Header:     make(http.Header),
		}, nil
	})}

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/config/llm/models", strings.NewReader(`{
		"base_url":"http://10.0.0.8:8080/v1",
		"api_key":"sk-test",
		"locality":"cloud"
	}`))
	rec := httptest.NewRecorder()
	srv.handleFetchProviderModels(rec, req)

	if called {
		t.Fatal("private endpoint must be rejected before outbound transport")
	}
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden {
		t.Fatalf("expected private endpoint rejection, got %d: %s", rec.Code, rec.Body.String())
	}
}
