package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
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

func TestProviderEndpointContract_ModelCatalogBlocksCrossOriginRedirectBeforeForwardingCredentials(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			original := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = original })
			http.DefaultClient = &http.Client{}

			var targetCalls atomic.Int64
			var authorizationCalls atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				targetCalls.Add(1)
				if req.Header.Get("Authorization") != "" {
					authorizationCalls.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":[]}`)
			}))
			defer target.Close()

			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				http.Redirect(w, req, target.URL+"/credential-target", status)
			}))
			defer origin.Close()

			srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/config/llm/models", strings.NewReader(fmt.Sprintf(`{
				"base_url":%q,
				"api_key":"sk-model-catalog-secret",
				"locality":"local"
			}`, origin.URL+"/v1")))
			rec := httptest.NewRecorder()
			srv.handleFetchProviderModels(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want response contract HTTP 200", rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, `"models":[]`) || !strings.Contains(body, `"error":`) {
				t.Fatalf("body=%s, want empty models and error response contract", body)
			}
			if targetCalls.Load() != 0 {
				t.Fatalf("cross-origin redirect target calls=%d, want 0", targetCalls.Load())
			}
			if authorizationCalls.Load() != 0 {
				t.Fatalf("cross-origin redirect received Authorization %d times, want 0", authorizationCalls.Load())
			}
		})
	}
}

func TestLLMTestProviderFactory_ForwardsPrivateNetworkAuthorization(t *testing.T) {
	const privateHost = "10.255.255.1"
	provider := llmTestProviderFactory(llmConnectionTestProvider{
		Type:    "openai",
		BaseURL: "http://" + privateHost + ":1/v1",
		APIKey:  "sk-lan-provider",
		Model:   "lan-model",
		PrivateNetworkAccess: config.ProviderPrivateNetworkAccess{
			Host:    privateHost,
			Allowed: true,
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := provider.Complete(ctx, hexagon.CompletionRequest{
		Messages:  []hexagon.Message{{Role: "user", Content: "probe"}},
		MaxTokens: 1,
	})
	if err == nil {
		t.Fatal("private LAN probe unexpectedly succeeded")
	}
	if errors.Is(err, egress.ErrProviderEndpointPolicy) {
		t.Fatalf("authorized private LAN probe was rejected by endpoint policy: %v", err)
	}
}
