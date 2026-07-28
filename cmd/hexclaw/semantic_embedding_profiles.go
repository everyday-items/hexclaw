package main

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/knowledge"
)

const knowledgeEmbeddingProbeTimeout = 15 * time.Second

const knowledgeEmbeddingProbeText = "hexclaw-embedding-connectivity-probe-v1"

// knowledgeEmbeddingRuntimeProfiles is the production capability/data-plane
// pair. Both objects share one revocation gate and are assembled from the same
// immutable entries so the catalog can never advertise a vector space whose
// executor was built from different model metadata.
type knowledgeEmbeddingRuntimeProfiles struct {
	Resolver *knowledgeEmbeddingProfileResolver
	Registry *knowledgeEmbeddingExecutorRegistry
}

type knowledgeEmbeddingRuntimeProfileBuildConfig struct {
	observeHTTPClient func(providerKey, model string, client *http.Client)
	probeInterval     time.Duration
	probeIntervalSet  bool
}

type knowledgeEmbeddingRuntimeProfileOption func(*knowledgeEmbeddingRuntimeProfileBuildConfig)

// withKnowledgeEmbeddingHTTPClientObserver is test instrumentation around the
// same guarded production client. It cannot replace the transport or endpoint
// policy; callers may only wrap/observe the already-validated client.
func withKnowledgeEmbeddingHTTPClientObserver(
	observer func(providerKey, model string, client *http.Client),
) knowledgeEmbeddingRuntimeProfileOption {
	return func(build *knowledgeEmbeddingRuntimeProfileBuildConfig) {
		build.observeHTTPClient = observer
	}
}

// withKnowledgeEmbeddingProbeInterval keeps recovery timing deterministic in
// runtime tests. Production uses the same five-second TTL as the resolver.
func withKnowledgeEmbeddingProbeInterval(interval time.Duration) knowledgeEmbeddingRuntimeProfileOption {
	return func(build *knowledgeEmbeddingRuntimeProfileBuildConfig) {
		build.probeInterval = interval
		build.probeIntervalSet = true
	}
}

type knowledgeEmbeddingModelCandidate struct {
	providerKey   string
	provider      config.LLMProviderConfig
	model         string
	protocol      string
	dimension     int
	normalization string
	local         bool
	nativeOllama  bool
}

// buildKnowledgeEmbeddingRuntimeProfiles builds one executor per explicitly
// declared embedding model. The only legacy exception is a selected native
// Ollama model with a trusted exact dimension (notably Nomic), preserving the
// existing zero-config local path without guessing cloud chat capabilities.
//
// Cloud/local-compatible availability is established by a real one-input
// POST /embeddings preflight at startup and on a stale Resolve (the Apply
// boundary). Catalog is a cached projection and therefore never performs I/O.
func buildKnowledgeEmbeddingRuntimeProfiles(
	ctx context.Context,
	cfg *config.Config,
	cloudPolicy *egress.Policy,
	runtimeGate *knowledgeSemanticRuntimeGate,
	options ...knowledgeEmbeddingRuntimeProfileOption,
) knowledgeEmbeddingRuntimeProfiles {
	gate := selectKnowledgeSemanticRuntimeGate([]*knowledgeSemanticRuntimeGate{runtimeGate})
	build := &knowledgeEmbeddingRuntimeProfileBuildConfig{}
	for _, option := range options {
		if option != nil {
			option(build)
		}
	}
	profileEntries := make([]knowledgeEmbeddingProfileEntry, 0)
	executorEntries := make([]knowledgeEmbeddingExecutorEntry, 0)
	probeInterval := 5 * time.Second
	if build.probeIntervalSet {
		probeInterval = build.probeInterval
	}

	for order, candidate := range knowledgeEmbeddingModelCandidates(ctx, cfg) {
		plan := knowledgeEmbeddingPlan{
			Provider: candidate.providerKey,
			Model:    candidate.model,
			Ollama:   candidate.nativeOllama,
		}
		queryPrefix, documentPrefix := knowledgeEmbeddingPrefixes(cfg, candidate.model)
		location := knowledge.ProviderLocationCloud
		if candidate.local {
			location = knowledge.ProviderLocationLocal
		}
		entry := knowledgeEmbeddingProfileEntry{
			ProviderID:     config.EffectiveProviderInstanceID(candidate.providerKey, candidate.provider),
			ProviderName:   candidate.providerKey,
			ModelName:      candidate.model,
			Protocol:       candidate.protocol,
			BaseURL:        knowledgeEmbeddingEffectiveBaseURL(plan, candidate.provider),
			Location:       location,
			Capability:     config.LLMModelCapabilityEmbedding,
			Dimension:      candidate.dimension,
			Availability:   knowledge.ProfileAvailabilityUnavailable,
			DisplayOrder:   order + 10,
			Normalization:  candidate.normalization,
			QueryPrefix:    queryPrefix,
			DocumentPrefix: documentPrefix,
			Configured:     true,
			ProbeInterval:  probeInterval,
		}

		var embedder hexagon.VectorEmbedder
		var readiness *knowledge.ReadinessGatedEmbedder
		if candidate.nativeOllama {
			baseURL, model := entry.BaseURL, candidate.model
			entry.ProbeAvailability = func(probeCtx context.Context) knowledge.ProfileAvailability {
				if knowledge.OllamaModelInstalled(probeCtx, baseURL, model) {
					return knowledge.ProfileAvailabilityInstalled
				}
				if knowledge.OllamaServiceAvailable(probeCtx, baseURL) {
					return knowledge.ProfileAvailabilityDownloadable
				}
				return knowledge.ProfileAvailabilityUnavailable
			}
			entry.Availability = probeKnowledgeEmbeddingAvailability(ctx, gate, entry.ProbeAvailability)
		}

		client, clientErr := newKnowledgeEmbeddingProviderHTTPClient(plan, candidate.provider)
		credentialsReady := candidate.nativeOllama || strings.TrimSpace(candidate.provider.APIKey) != "" || candidate.local
		if clientErr == nil && credentialsReady {
			if build.observeHTTPClient != nil {
				build.observeHTTPClient(candidate.providerKey, candidate.model, client)
			}
			providerOptions := []hexagon.OpenAIOption{hexagon.OpenAIWithHTTPClient(client)}
			if entry.BaseURL != "" {
				providerOptions = append(providerOptions, hexagon.OpenAIWithBaseURL(entry.BaseURL))
			}
			aiProvider := hexagon.NewOpenAI(
				knowledgeEmbeddingProviderAPIKey(plan, candidate.provider),
				providerOptions...,
			)
			newGuardedEmbedder := func(dimension int) hexagon.VectorEmbedder {
				var result hexagon.VectorEmbedder = hexagon.NewOpenAIEmbedder(
					aiProvider,
					hexagon.WithEmbedderModel(candidate.model),
					hexagon.WithEmbedderDimension(dimension),
				)
				if !candidate.local {
					if cloudPolicy == nil {
						return nil
					}
					result = egress.NewCloudEmbedder(result, cloudPolicy)
				}
				return result
			}
			guarded := newGuardedEmbedder(candidate.dimension)
			if guarded != nil {
				dimensionDiscovered := false
				if candidate.dimension <= 0 {
					discoveredDimension := 0
					entry.Availability = probeKnowledgeEmbeddingAvailability(
						ctx, gate, func(probeCtx context.Context) knowledge.ProfileAvailability {
							var ok bool
							discoveredDimension, ok = knowledgeEmbeddingProbeVectorDimension(
								probeCtx, guarded, !candidate.local,
							)
							if !ok {
								return knowledge.ProfileAvailabilityUnavailable
							}
							if candidate.local {
								return knowledge.ProfileAvailabilityInstalled
							}
							return knowledge.ProfileAvailabilityConnected
						},
					)
					if discoveredDimension <= 0 ||
						entry.Availability == knowledge.ProfileAvailabilityUnavailable {
						continue
					}
					candidate.dimension = discoveredDimension
					entry.Dimension = discoveredDimension
					guarded = newGuardedEmbedder(discoveredDimension)
					dimensionDiscovered = true
				}
				if !candidate.nativeOllama {
					probeEmbedder := guarded
					availabilityOnSuccess := knowledge.ProfileAvailabilityConnected
					if candidate.local {
						availabilityOnSuccess = knowledge.ProfileAvailabilityInstalled
					}
					entry.ProbeAvailability = func(probeCtx context.Context) knowledge.ProfileAvailability {
						if knowledgeEmbeddingRealProbe(probeCtx, probeEmbedder, candidate.dimension, !candidate.local) {
							return availabilityOnSuccess
						}
						return knowledge.ProfileAvailabilityUnavailable
					}
					if !dimensionDiscovered {
						entry.Availability = probeKnowledgeEmbeddingAvailability(ctx, gate, entry.ProbeAvailability)
					}
				}
				if entry.ProbeAvailability != nil {
					availabilityProbe := entry.ProbeAvailability
					readiness = knowledge.NewReadinessGatedEmbedder(
						guarded,
						func(probeCtx context.Context) bool {
							return knowledgeEmbeddingAvailabilityExecutable(availabilityProbe(probeCtx))
						},
						knowledgeEmbeddingAvailabilityExecutable(entry.Availability),
						probeInterval,
					)
					entry.ProbeAvailability = func(probeCtx context.Context) knowledge.ProfileAvailability {
						availability := availabilityProbe(probeCtx)
						readiness.ObserveReady(knowledgeEmbeddingAvailabilityExecutable(availability))
						return availability
					}
					guarded = readiness
				}
				embedder = knowledge.NewTruncatingEmbedder(hexagon.NewCachedEmbedder(guarded), 0)
			}
		}

		snapshot, snapshotErr := knowledgeEmbeddingProfileSnapshotForEntry(entry)
		if snapshotErr != nil {
			continue
		}
		profileEntries = append(profileEntries, entry)
		if embedder != nil {
			executorEntries = append(executorEntries, knowledgeEmbeddingExecutorEntry{
				Snapshot: snapshot, Embedder: embedder, Readiness: readiness,
				QueryPrefix: queryPrefix, DocumentPrefix: documentPrefix,
			})
		}
	}

	return knowledgeEmbeddingRuntimeProfiles{
		Resolver: newKnowledgeEmbeddingProfileResolverFromEntries(profileEntries, gate),
		Registry: newKnowledgeEmbeddingExecutorRegistryFromEntries(executorEntries, gate),
	}
}

func knowledgeEmbeddingAvailabilityExecutable(availability knowledge.ProfileAvailability) bool {
	return availability == knowledge.ProfileAvailabilityInstalled ||
		availability == knowledge.ProfileAvailabilityConnected
}

func knowledgeEmbeddingModelCandidates(ctx context.Context, cfg *config.Config) []knowledgeEmbeddingModelCandidate {
	if cfg == nil {
		return nil
	}
	providerKeys := make([]string, 0, len(cfg.LLM.Providers))
	for providerKey := range cfg.LLM.Providers {
		providerKeys = append(providerKeys, providerKey)
	}
	sort.Strings(providerKeys)

	candidates := make([]knowledgeEmbeddingModelCandidate, 0)
	seen := make(map[string]struct{})
	add := func(candidate knowledgeEmbeddingModelCandidate) {
		candidate.model = strings.TrimSpace(candidate.model)
		candidate.protocol = strings.TrimSpace(candidate.protocol)
		candidate.normalization = strings.TrimSpace(candidate.normalization)
		if candidate.protocol == "" {
			if candidate.nativeOllama {
				candidate.protocol = config.LLMEmbeddingProtocolOllama
			} else {
				candidate.protocol = config.LLMEmbeddingProtocolOpenAI
			}
		}
		if candidate.model == "" {
			return
		}
		if candidate.normalization == "" {
			candidate.normalization = knowledgeEmbeddingDefaultNormalization
		}
		if candidate.normalization != knowledgeEmbeddingDefaultNormalization &&
			candidate.normalization != knowledgeEmbeddingNoNormalization {
			return
		}
		if candidate.protocol != config.LLMEmbeddingProtocolOpenAI &&
			candidate.protocol != config.LLMEmbeddingProtocolOllama {
			return
		}
		key := config.EffectiveProviderInstanceID(candidate.providerKey, candidate.provider) + "\x00" +
			candidate.model + "\x00" + candidate.protocol
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, candidate)
	}

	for _, providerKey := range providerKeys {
		provider := cfg.LLM.Providers[providerKey]
		if provider.Enabled != nil && !*provider.Enabled {
			continue
		}
		local := isLocalEmbeddingProvider(providerKey, provider)
		nativeOllama := isOllamaEmbeddingCandidate(providerKey, provider)
		_, specs := config.NormalizeProviderModelSpecs(provider)
		for _, spec := range specs {
			if !knowledgeEmbeddingSpecHasCapability(spec, config.LLMModelCapabilityEmbedding) {
				continue
			}
			dimension := 0
			protocol := ""
			normalization := ""
			if spec.Embedding != nil {
				dimension = spec.Embedding.Dimension
				protocol = spec.Embedding.Protocol
				normalization = spec.Embedding.Normalization
			}
			// Exact trusted catalog values are the only fallback when the UI did
			// not persist a dimension. Arbitrary unknown models stay fail-closed.
			if dimension <= 0 {
				dimension = knowledgeEmbeddingDimension(spec.ID)
			}
			add(knowledgeEmbeddingModelCandidate{
				providerKey: providerKey, provider: provider, model: spec.ID,
				protocol: protocol, dimension: dimension, normalization: normalization, local: local,
				nativeOllama: nativeOllama,
			})
		}
	}

	// Legacy trusted local embeddings may be configured outside model_specs.
	// Keep their exact built-in dimension contract during migration; no
	// analogous cloud inference exists.
	legacyPlan := resolveKnowledgeEmbeddingPlan(ctx, cfg)
	if legacyPlan.Ollama && knowledgeEmbeddingDimension(legacyPlan.Model) > 0 &&
		knowledge.IsEmbeddingModelName(legacyPlan.Model) {
		if provider, ok := cfg.LLM.Providers[legacyPlan.Provider]; ok {
			add(knowledgeEmbeddingModelCandidate{
				providerKey: legacyPlan.Provider, provider: provider, model: legacyPlan.Model,
				protocol:  config.LLMEmbeddingProtocolOllama,
				dimension: knowledgeEmbeddingDimension(legacyPlan.Model), local: true, nativeOllama: true,
			})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].local != candidates[j].local {
			return candidates[i].local
		}
		if candidates[i].providerKey != candidates[j].providerKey {
			return candidates[i].providerKey < candidates[j].providerKey
		}
		if candidates[i].model != candidates[j].model {
			return candidates[i].model < candidates[j].model
		}
		return candidates[i].protocol < candidates[j].protocol
	})
	return candidates
}

func knowledgeEmbeddingSpecHasCapability(spec config.LLMProviderModelSpec, capability string) bool {
	for _, candidate := range spec.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

func probeKnowledgeEmbeddingAvailability(
	ctx context.Context,
	gate *knowledgeSemanticRuntimeGate,
	probe func(context.Context) knowledge.ProfileAvailability,
) knowledge.ProfileAvailability {
	if probe == nil {
		return knowledge.ProfileAvailabilityUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, knowledgeEmbeddingProbeTimeout)
	defer cancel()
	boundCtx, release, err := gate.Bind(probeCtx)
	if err != nil {
		return knowledge.ProfileAvailabilityUnavailable
	}
	defer release()
	availability := probe(boundCtx)
	if boundCtx.Err() != nil {
		return knowledge.ProfileAvailabilityUnavailable
	}
	return availability
}

func knowledgeEmbeddingRealProbe(
	ctx context.Context,
	embedder hexagon.VectorEmbedder,
	dimension int,
	cloud bool,
) bool {
	discovered, ok := knowledgeEmbeddingProbeVectorDimension(ctx, embedder, cloud)
	return ok && discovered == dimension
}

func knowledgeEmbeddingProbeVectorDimension(
	ctx context.Context,
	embedder hexagon.VectorEmbedder,
	cloud bool,
) (int, bool) {
	if embedder == nil {
		return 0, false
	}
	if cloud {
		ctx = egress.WithRequest(ctx, egress.PurposeProviderProbe, "", egress.ClassGeneral)
	}
	vectors, err := embedder.Embed(ctx, []string{knowledgeEmbeddingProbeText})
	if err != nil || len(vectors) != 1 || len(vectors[0]) == 0 || len(vectors[0]) > 65536 {
		return 0, false
	}
	nonzero := false
	for _, value := range vectors[0] {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return 0, false
		}
		if value != 0 {
			nonzero = true
		}
	}
	if !nonzero {
		return 0, false
	}
	return len(vectors[0]), true
}
