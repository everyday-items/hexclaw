package main

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/api"
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

const defaultKnowledgeOllamaEmbeddingBaseURL = "http://localhost:11434/v1"
const defaultKnowledgeOllamaEmbeddingModel = "qwen3-embedding:8b"
const knowledgeOllamaEmbeddingDummyAPIKey = "ollama"
const knowledgeLocalEmbeddingDummyAPIKey = "local"

type knowledgeEmbeddingRuntimeProvider struct {
	local            bool
	nativeOllama     bool
	credentialsReady bool
}

// classifyKnowledgeEmbeddingRuntimeProvider keeps provider locality separate
// from the much narrower native-Ollama management capability. Locality is the
// authoritative deployment declaration; native Ollama additionally requires a
// resolver-attested native endpoint. Declared-local OpenAI-compatible servers
// therefore receive local admission/warmup without gaining tags or pull access.
func classifyKnowledgeEmbeddingRuntimeProvider(
	name string,
	provider config.LLMProviderConfig,
	plan knowledgeEmbeddingPlan,
) knowledgeEmbeddingRuntimeProvider {
	local := config.IsLocalLLMProviderNamed(name, provider)
	nativeOllama := local && plan.Ollama
	return knowledgeEmbeddingRuntimeProvider{
		local:            local,
		nativeOllama:     nativeOllama,
		credentialsReady: nativeOllama || local || strings.TrimSpace(provider.APIKey) != "",
	}
}

// knowledgeEmbeddingLegacyAPILocal preserves the existing external contract:
// KnowledgeEmbeddingInfo.Local historically means “native Ollama model can be
// probed/managed”, not general provider locality. Keep it native-only until a
// separately approved API/UI contract can expose both concepts.
func knowledgeEmbeddingLegacyAPILocal(nativeOllama bool) bool {
	return nativeOllama
}

// knowledgeEmbeddingEffectiveBaseURL keeps discovery, provider construction,
// readiness probes and native model management on the same endpoint. The
// endpoint-less legacy Ollama entry means the built-in localhost service; an
// endpoint-less official OpenAI provider keeps the SDK's OpenAI default.
func knowledgeEmbeddingEffectiveBaseURL(
	plan knowledgeEmbeddingPlan,
	provider config.LLMProviderConfig,
) string {
	baseURL := strings.TrimSpace(provider.BaseURL)
	if plan.Ollama {
		if baseURL == "" {
			return defaultKnowledgeOllamaEmbeddingBaseURL
		}
		baseURL = strings.TrimRight(baseURL, "/")
		if !strings.HasSuffix(baseURL, "/v1") {
			baseURL += "/v1"
		}
	}
	return baseURL
}

func knowledgeEmbeddingProviderAPIKey(
	plan knowledgeEmbeddingPlan,
	provider config.LLMProviderConfig,
) string {
	if plan.Ollama {
		return knowledgeOllamaEmbeddingDummyAPIKey
	}
	if strings.TrimSpace(provider.APIKey) == "" &&
		classifyKnowledgeEmbeddingRuntimeProvider(plan.Provider, provider, plan).local {
		// ai-core treats an empty key as permission to read OPENAI_API_KEY from
		// the process environment. Use a non-secret local sentinel so an ambient
		// cloud credential can never be forwarded to a local compatible server.
		return knowledgeLocalEmbeddingDummyAPIKey
	}
	return provider.APIKey
}

func validateKnowledgeEmbeddingEndpoint(
	plan knowledgeEmbeddingPlan,
	provider config.LLMProviderConfig,
) error {
	if strings.TrimSpace(provider.BaseURL) != "" || plan.Ollama {
		return nil
	}
	// An empty URL is meaningful only for the canonical OpenAI provider. A
	// custom OpenAI-compatible or explicitly local provider without an endpoint
	// must never silently inherit api.openai.com and exfiltrate its input.
	providerID := strings.ToLower(strings.TrimSpace(plan.Provider))
	compatible := strings.ToLower(strings.TrimSpace(provider.Compatible))
	locality := strings.ToLower(strings.TrimSpace(provider.Locality))
	if providerID == "openai" && locality != config.ProviderLocalityLocal &&
		(compatible == "" || compatible == "openai") {
		return nil
	}
	return fmt.Errorf("knowledge: provider %q requires an explicit embedding base URL", plan.Provider)
}

// knowledgeEmbeddingDimension is part of the immutable vector-space contract.
// ai-core's OpenAI compatibility helper intentionally defaults unknown models
// to 1536, which is unsafe for common Ollama embedding models whose native
// dimensions differ. Keep the explicitly supported catalog models exact and
// require every other model to obtain an explicit or trusted exact dimension.
func knowledgeEmbeddingDimension(model string) int {
	name := strings.ToLower(strings.TrimSpace(model))
	switch name {
	case "text-embedding-3-small":
		return 1536
	case "text-embedding-3-large":
		return 3072
	case "nvidia/nemotron-3-embed-1b",
		"nvidia/nemotron-3-embed-1b:free",
		"nvidia/llama-nemotron-embed-vl-1b-v2",
		"nvidia/llama-nemotron-embed-vl-1b-v2:free":
		return 2048
	case "nomic-embed-text", "nomic-embed-text:latest", "nomic-embed-text:v1.5":
		return 768
	case "qwen3-embedding", "qwen3-embedding:latest", "qwen3-embedding:8b",
		"qwen3-embedding:8b-q4_k_m":
		return 4096
	case "mxbai-embed-large", "mxbai-embed-large:latest",
		"baai/bge-m3", "baai/bge-m3:latest", "bge-m3", "bge-m3:latest":
		return 1024
	case "all-minilm", "all-minilm:latest", "all-minilm:l6-v2", "all-minilm:l12-v2":
		return 384
	default:
		return 0
	}
}

func knowledgeEmbeddingDimensionForProvider(provider config.LLMProviderConfig, model string) int {
	_, specs := config.NormalizeProviderModelSpecs(provider)
	for _, spec := range specs {
		if spec.ID != model || !knowledgeEmbeddingSpecHasCapability(spec, config.LLMModelCapabilityEmbedding) {
			continue
		}
		if spec.Embedding != nil && spec.Embedding.Dimension > 0 {
			return spec.Embedding.Dimension
		}
		break
	}
	return knowledgeEmbeddingDimension(model)
}

func isOllamaEmbeddingCandidate(name string, provider config.LLMProviderConfig) bool {
	// Native Ollama management is a local-only capability. Provider names are
	// merely legacy hints and must never override the authoritative locality /
	// endpoint classification (for example, "Ollama Cloud" on a public host).
	if !config.IsLocalLLMProviderNamed(name, provider) {
		return false
	}
	nameIsOllama := strings.Contains(strings.ToLower(strings.TrimSpace(name)), "ollama")
	baseURL := strings.TrimSpace(provider.BaseURL)
	// An endpoint-less legacy Ollama provider intentionally means the built-in
	// localhost:11434 default. Every configured endpoint must pass exactly the
	// same loopback/path policy as the side-effecting management API.
	if baseURL == "" {
		return nameIsOllama
	}
	if api.ValidateNativeOllamaBaseURL(baseURL) != nil {
		return false
	}
	if nameIsOllama {
		return true
	}
	u, err := url.Parse(baseURL)
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
					requestedModel = defaultKnowledgeOllamaEmbeddingModel
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
		// Compatible embeddings require an explicit model and endpoint contract.
		// Cloud routes additionally require a key; declared-local routes do not.
		plan := knowledgeEmbeddingPlan{
			Provider: requestedProvider,
			Model:    requestedModel,
		}
		runtimeProvider := classifyKnowledgeEmbeddingRuntimeProvider(requestedProvider, provider, plan)
		configured := requestedModel != "" && runtimeProvider.credentialsReady &&
			validateKnowledgeEmbeddingEndpoint(plan, provider) == nil
		plan.Configured = configured
		plan.Ready = configured
		plan.ServiceAvailable = configured
		return plan
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
			model = defaultKnowledgeOllamaEmbeddingModel
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
