package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexagon/rag"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
)

func TestSafeCohereRerankerUsesGuardedOriginAndMapsScores(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/rerank" {
			t.Fatalf("path = %q, want /v1/rerank", req.URL.Path)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q", got)
		}
		var body struct {
			Query           string   `json:"query"`
			Documents       []string `json:"documents"`
			Model           string   `json:"model"`
			TopN            int      `json:"top_n"`
			ReturnDocuments bool     `json:"return_documents"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Query != "needle" || body.Model != "rerank-v1" || body.TopN != 2 || body.ReturnDocuments {
			t.Fatalf("unexpected request: %+v", body)
		}
		if len(body.Documents) != 2 || body.Documents[0] != "first" || body.Documents[1] != "second" {
			t.Fatalf("documents = %#v", body.Documents)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.4}]}`))
	}))
	defer server.Close()

	rr, err := newSafeCohereReranker(
		server.URL+"/v1", "secret", "rerank-v1", 2, config.ProviderPrivateNetworkAccess{},
	)
	if err != nil {
		t.Fatal(err)
	}
	docs := []rag.Document{{ID: "a", Content: "first"}, {ID: "b", Content: "second"}}
	got, err := rr.Rerank(context.Background(), "needle", docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "b" || got[0].Score != 0.9 || got[1].ID != "a" || got[1].Score != 0.4 {
		t.Fatalf("reranked docs = %#v", got)
	}
}

func TestSafeCohereRerankerBlocksCrossOriginRedirectWithoutLeakingCredentials(t *testing.T) {
	var targetCalls atomic.Int64
	var targetAuthorization atomic.Value
	targetAuthorization.Store("")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		targetCalls.Add(1)
		targetAuthorization.Store(req.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	rr, err := newSafeCohereReranker(
		origin.URL+"/v1", "secret", "rerank-v1", 1, config.ProviderPrivateNetworkAccess{},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rr.Rerank(context.Background(), "needle", []rag.Document{{ID: "a", Content: "first"}})
	if !errors.Is(err, egress.ErrProviderEndpointPolicy) {
		t.Fatalf("redirect error = %v, want endpoint policy rejection", err)
	}
	if targetCalls.Load() != 0 || targetAuthorization.Load().(string) != "" {
		t.Fatalf("redirect target calls=%d authorization=%q", targetCalls.Load(), targetAuthorization.Load())
	}
}

func TestSafeCohereRerankerRejectsInvalidProviderResponses(t *testing.T) {
	for name, response := range map[string]string{
		"out of range index": `{"results":[{"index":2,"relevance_score":0.5}]}`,
		"duplicate index":    `{"results":[{"index":0,"relevance_score":0.5},{"index":0,"relevance_score":0.4}]}`,
		"invalid score":      `{"results":[{"index":0,"relevance_score":-0.1}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(response))
			}))
			defer server.Close()
			rr, err := newSafeCohereReranker(
				server.URL+"/v1", "secret", "rerank-v1", 2, config.ProviderPrivateNetworkAccess{},
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rr.Rerank(context.Background(), "q", []rag.Document{{ID: "a", Content: "a"}}); err == nil {
				t.Fatal("invalid provider response was accepted")
			}
		})
	}
}

func TestSafeCohereRerankerRejectsMissingOrPublicPlaintextEndpoint(t *testing.T) {
	for name, endpoint := range map[string]string{
		"missing":          "",
		"public plaintext": "http://api.example.com/v1",
	} {
		t.Run(name, func(t *testing.T) {
			rr, err := newSafeCohereReranker(
				endpoint, "secret", "rerank-v1", 2, config.ProviderPrivateNetworkAccess{},
			)
			if rr != nil || err == nil {
				t.Fatalf("reranker=%v err=%v, want fail closed", rr, err)
			}
		})
	}
}
