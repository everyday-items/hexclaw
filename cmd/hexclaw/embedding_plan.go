package main

import (
	"context"
	"net/url"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
)

type knowledgeEmbeddingPlan struct {
	Provider         string
	Model            string
	Configured       bool
	Ollama           bool
	Ready            bool
	ServiceAvailable bool
}

func isOllamaEmbeddingCandidate(name string, provider config.LLMProviderConfig) bool {
	if strings.Contains(strings.ToLower(strings.TrimSpace(name)), "ollama") {
		return true
	}
	u, err := url.Parse(strings.TrimSpace(provider.BaseURL))
	if err != nil {
		return false
	}
	return u.Port() == "11434"
}

// resolveKnowledgeEmbeddingPlan keeps capability discovery deterministic:
// explicit configuration wins; auto mode may discover a configured Ollama
// endpoint, but never assumes that an arbitrary chat-compatible cloud endpoint
// also implements a particular embedding model.
func resolveKnowledgeEmbeddingPlan(ctx context.Context, cfg *config.Config) knowledgeEmbeddingPlan {
	if cfg == nil {
		return knowledgeEmbeddingPlan{}
	}
	requestedProvider := strings.TrimSpace(cfg.Knowledge.Embedding.Provider)
	requestedModel := strings.TrimSpace(cfg.Knowledge.Embedding.Model)

	if requestedProvider != "" {
		provider, ok := cfg.LLM.Providers[requestedProvider]
		if !ok || (provider.Enabled != nil && !*provider.Enabled) {
			return knowledgeEmbeddingPlan{Provider: requestedProvider, Model: requestedModel}
		}
		isOllama := isOllamaEmbeddingCandidate(requestedProvider, provider)
		if isOllama {
			detected, available := knowledge.InspectOllamaEmbedding(ctx, provider.BaseURL)
			if requestedModel == "" {
				requestedModel = detected
				if requestedModel == "" {
					requestedModel = "nomic-embed-text"
				}
			}
			return knowledgeEmbeddingPlan{
				Provider:         requestedProvider,
				Model:            requestedModel,
				Configured:       requestedModel != "",
				Ollama:           true,
				Ready:            available && knowledge.OllamaModelInstalled(ctx, provider.BaseURL, requestedModel),
				ServiceAvailable: available,
			}
		}
		// Cloud/custom embedding is an explicit paid capability. A provider name
		// alone is insufficient evidence that text-embedding-3-small exists.
		configured := requestedModel != "" && provider.APIKey != ""
		return knowledgeEmbeddingPlan{
			Provider:         requestedProvider,
			Model:            requestedModel,
			Configured:       configured,
			Ready:            configured,
			ServiceAvailable: configured,
		}
	}

	names := make([]string, 0, len(cfg.LLM.Providers))
	for name := range cfg.LLM.Providers {
		names = append(names, name)
	}
	sort.Strings(names)

	var standby knowledgeEmbeddingPlan
	for _, name := range names {
		provider := cfg.LLM.Providers[name]
		if provider.Enabled != nil && !*provider.Enabled {
			continue
		}
		if !isOllamaEmbeddingCandidate(name, provider) {
			continue
		}
		detected, available := knowledge.InspectOllamaEmbedding(ctx, provider.BaseURL)
		model := requestedModel
		if model == "" {
			model = detected
		}
		if model == "" {
			model = "nomic-embed-text"
		}
		plan := knowledgeEmbeddingPlan{
			Provider:         name,
			Model:            model,
			Configured:       true,
			Ollama:           true,
			Ready:            available && knowledge.OllamaModelInstalled(ctx, provider.BaseURL, model),
			ServiceAvailable: available,
		}
		if plan.Ready {
			return plan
		}
		if standby.Provider == "" {
			standby = plan
		}
	}
	return standby
}
