package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
)

func TestKnowledgeEmbeddingProviderHTTPClientCoversNonNativeLocalGateway(t *testing.T) {
	var requests atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer gateway.Close()

	client, err := newKnowledgeEmbeddingProviderHTTPClient(
		knowledgeEmbeddingPlan{Ollama: false},
		config.LLMProviderConfig{
			BaseURL:  gateway.URL + "/v1",
			Locality: config.ProviderLocalityLocal,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("non-native local compatible gateway bypassed the safe client")
	}
	resp, err := client.Get(gateway.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if requests.Load() != 1 {
		t.Fatalf("safe local gateway requests=%d, want 1", requests.Load())
	}

	nativeClient, err := newKnowledgeEmbeddingProviderHTTPClient(
		knowledgeEmbeddingPlan{Ollama: true}, config.LLMProviderConfig{},
	)
	if err != nil || nativeClient == nil {
		t.Fatalf("native Ollama compatible client=%v err=%v, want guarded native path", nativeClient, err)
	}
}

func TestNativeOllamaEmbeddingRedirectCannotReplayDocumentCrossOrigin(t *testing.T) {
	var redirectedRequests atomic.Int64
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer redirected.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirected.URL+"/exfiltrate", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := newKnowledgeEmbeddingProviderHTTPClient(
		knowledgeEmbeddingPlan{Ollama: true},
		config.LLMProviderConfig{BaseURL: origin.URL + "/v1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Post(
		origin.URL+"/v1/embeddings",
		"application/json",
		strings.NewReader(`{"input":["private document"]}`),
	)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, egress.ErrProviderEndpointPolicy) {
		t.Fatalf("native cross-origin redirect error = %v, want endpoint policy rejection", err)
	}
	if redirectedRequests.Load() != 0 {
		t.Fatalf("native 307 replay reached redirected origin %d times", redirectedRequests.Load())
	}
}

func TestKnowledgeEmbeddingProviderHTTPClientRejectsEndpointlessCustomOrLocalProvider(t *testing.T) {
	for _, tt := range []struct {
		name     string
		plan     knowledgeEmbeddingPlan
		provider config.LLMProviderConfig
	}{
		{
			name:     "custom compatible",
			plan:     knowledgeEmbeddingPlan{Provider: "my-compatible-gateway"},
			provider: config.LLMProviderConfig{Compatible: "openai"},
		},
		{
			name:     "declared local",
			plan:     knowledgeEmbeddingPlan{Provider: "openai"},
			provider: config.LLMProviderConfig{Locality: config.ProviderLocalityLocal},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, err := newKnowledgeEmbeddingProviderHTTPClient(tt.plan, tt.provider)
			if err == nil || client != nil {
				t.Fatalf("endpoint-less provider client=%v err=%v, want fail-closed", client, err)
			}
		})
	}

	client, err := newKnowledgeEmbeddingProviderHTTPClient(
		knowledgeEmbeddingPlan{Provider: "openai"},
		config.LLMProviderConfig{Locality: config.ProviderLocalityCloud},
	)
	if err != nil || client == nil {
		t.Fatalf("official OpenAI default endpoint client=%v err=%v", client, err)
	}
}

func TestKnowledgeEmbeddingProviderHTTPClientPreservesExplicitIdempotencyHeader(t *testing.T) {
	received := make(chan string, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer provider.Close()

	client, err := newKnowledgeEmbeddingProviderHTTPClient(
		knowledgeEmbeddingPlan{Ollama: false},
		config.LLMProviderConfig{
			BaseURL:  provider.URL + "/v1",
			Locality: config.ProviderLocalityLocal,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := egress.WithProviderClientRequestKey(context.Background(), "durable-batch-key")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL+"/v1/embeddings", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "caller-explicit")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := <-received; got != "caller-explicit" {
		t.Fatalf("explicit Idempotency-Key overwritten with %q", got)
	}
}

func TestKnowledgeEmbeddingProviderHTTPClientAddsOnlyBatchRequestKey(t *testing.T) {
	received := make(chan string, 2)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		received <- req.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer provider.Close()
	client, err := newKnowledgeEmbeddingProviderHTTPClient(
		knowledgeEmbeddingPlan{},
		config.LLMProviderConfig{
			BaseURL: provider.URL + "/v1", Locality: config.ProviderLocalityLocal,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	documentRequest, err := http.NewRequestWithContext(
		egress.WithProviderClientRequestKey(context.Background(), "kb-embed-stable-batch"),
		http.MethodPost, provider.URL+"/v1/embeddings", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	documentResponse, err := client.Do(documentRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = documentResponse.Body.Close()
	if got := documentRequest.Header.Get("Idempotency-Key"); got != "" {
		t.Fatalf("transport mutated caller request header to %q", got)
	}

	queryRequest, err := http.NewRequest(
		http.MethodPost, provider.URL+"/v1/embeddings", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	queryResponse, err := client.Do(queryRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = queryResponse.Body.Close()

	if got := <-received; got != "kb-embed-stable-batch" {
		t.Fatalf("document embedding idempotency key = %q", got)
	}
	if got := <-received; got != "" {
		t.Fatalf("query embedding unexpectedly carried idempotency key %q", got)
	}
}
