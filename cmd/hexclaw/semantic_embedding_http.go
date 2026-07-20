package main

import (
	"net/http"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
)

const knowledgeNativeEmbeddingResponseHeaderTimeout = 10 * time.Minute

// newKnowledgeEmbeddingProviderHTTPClient installs destination policy and the
// shared provider request contract for every OpenAI-compatible endpoint,
// independent of its declared compute locality. Native Ollama is managed by
// its stricter loopback-only endpoint contract and retains its long-running
// local client.
func newKnowledgeEmbeddingProviderHTTPClient(
	plan knowledgeEmbeddingPlan,
	provider config.LLMProviderConfig,
) (*http.Client, error) {
	if err := validateKnowledgeEmbeddingEndpoint(plan, provider); err != nil {
		return nil, err
	}
	options := []egress.ProviderHTTPClientOption{}
	if plan.Ollama {
		options = append(options,
			egress.WithProviderResponseHeaderTimeout(knowledgeNativeEmbeddingResponseHeaderTimeout),
		)
	}
	return egress.NewProviderHTTPClient(
		knowledgeEmbeddingEffectiveBaseURL(plan, provider),
		provider.PrivateNetworkAccess,
		options...,
	)
}
