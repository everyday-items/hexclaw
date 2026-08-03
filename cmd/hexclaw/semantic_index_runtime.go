package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/localinfer"
	"github.com/hexagon-codes/hexclaw/resourcegov"
)

const (
	knowledgeDesktopOwnerID                   = "desktop-user"
	knowledgeDefaultCorpusID                  = "default"
	knowledgeEmbeddingDocumentTransformEpoch  = "v1"
	knowledgeEmbeddingProtocolOpenAI          = "openai_embeddings"
	knowledgeEmbeddingExecutorContractVersion = "embedding_executor_v1"
	knowledgeEmbeddingProfileIDPrefix         = "ep_v1_"
	knowledgeEmbeddingDefaultNormalization    = "l2"
	knowledgeEmbeddingNoNormalization         = "none"
	knowledgeEmbeddingDefaultEndpointIdentity = "https://api.openai.com/v1"
)

// knowledgeEmbeddingProfileEntry is the runtime-only, secret-free assembly
// input used while the config DTO evolves independently. Provider credentials
// are bound only to the executor and never participate in profile identity.
type knowledgeEmbeddingProfileEntry struct {
	ProviderID     string
	ProviderName   string
	ModelName      string
	Protocol       string
	BaseURL        string
	Location       knowledge.ProviderLocation
	Capability     string
	Dimension      int
	Availability   knowledge.ProfileAvailability
	DisplayOrder   int
	Normalization  string
	QueryPrefix    string
	DocumentPrefix string
	Configured     bool

	// ProbeAvailability is optional and is consulted by Resolve (Apply/startup),
	// never by Catalog. This keeps GET side-effect free while still allowing a
	// stale connected/installed projection to fail closed before policy writes.
	ProbeAvailability func(context.Context) knowledge.ProfileAvailability
	ProbeInterval     time.Duration
}

type knowledgeEmbeddingProfileState struct {
	snapshot          knowledge.EmbeddingProfileSnapshot
	configured        bool
	probeAvailability func(context.Context) knowledge.ProfileAvailability
	probeInterval     time.Duration
	lastProbe         time.Time
	mu                sync.RWMutex
	probeMu           sync.Mutex
}

// knowledgeEmbeddingExecutorContract is the complete, credential-free vector
// execution contract. Its field names intentionally mirror the architecture
// schema; its canonical JSON is the only input to profile_config_hash.
type knowledgeEmbeddingExecutorContract struct {
	ContractVersion    string
	Dimension          int
	DocumentPrefix     string
	EndpointIdentity   string
	ExactModelID       string
	MaxInputRunes      int
	Normalization      string
	Protocol           string
	ProviderInstanceID string
	ProviderLocation   string
	QueryPrefix        string
	TransformEpoch     string
}

func knowledgeEmbeddingExecutorContractHash(
	contract knowledgeEmbeddingExecutorContract,
) (string, error) {
	canonical, err := knowledgeCanonicalJSONObject(map[string]any{
		"contract_version":     contract.ContractVersion,
		"dimension":            contract.Dimension,
		"document_prefix":      contract.DocumentPrefix,
		"endpoint_identity":    contract.EndpointIdentity,
		"exact_model_id":       contract.ExactModelID,
		"max_input_runes":      contract.MaxInputRunes,
		"normalization":        contract.Normalization,
		"protocol":             contract.Protocol,
		"provider_instance_id": contract.ProviderInstanceID,
		"provider_location":    contract.ProviderLocation,
		"query_prefix":         contract.QueryPrefix,
		"transform_epoch":      contract.TransformEpoch,
	})
	if err != nil {
		return "", err
	}
	return semanticDigest(string(canonical)), nil
}

func knowledgeEmbeddingLogicalProfileID(providerID, model, protocol string) (string, error) {
	canonical, err := knowledgeCanonicalJSONObject(map[string]any{
		"exact_model_id":       model,
		"protocol":             protocol,
		"provider_instance_id": providerID,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return knowledgeEmbeddingProfileIDPrefix + base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

// knowledgeCanonicalJSONObject implements the RFC 8785/JCS subset used by the
// two fixed semantic-index schemas: lexicographically sorted string keys and
// string/integer values. Avoiding encoding/json's HTML and U+2028 escaping is
// important because provider/model IDs are user-controlled Unicode strings.
func knowledgeCanonicalJSONObject(fields map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]byte, 0, len(fields)*32)
	result = append(result, '{')
	for index, key := range keys {
		if index > 0 {
			result = append(result, ',')
		}
		var err error
		result, err = appendKnowledgeCanonicalJSONString(result, key)
		if err != nil {
			return nil, err
		}
		result = append(result, ':')
		switch value := fields[key].(type) {
		case string:
			result, err = appendKnowledgeCanonicalJSONString(result, value)
		case int:
			result = strconv.AppendInt(result, int64(value), 10)
		default:
			return nil, fmt.Errorf("knowledge: unsupported canonical JSON value for %q", key)
		}
		if err != nil {
			return nil, err
		}
	}
	return append(result, '}'), nil
}

func appendKnowledgeCanonicalJSONString(dst []byte, value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("knowledge: canonical JSON contains invalid UTF-8")
	}
	const hexadecimal = "0123456789abcdef"
	dst = append(dst, '"')
	for _, current := range value {
		switch current {
		case '"', '\\':
			dst = append(dst, '\\', byte(current))
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\r':
			dst = append(dst, '\\', 'r')
		default:
			if current < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0', hexadecimal[byte(current)>>4], hexadecimal[byte(current)&0x0f])
			} else {
				dst = append(dst, string(current)...)
			}
		}
	}
	return append(dst, '"'), nil
}

func knowledgeEmbeddingNormalization(value string) (string, error) {
	normalization := strings.TrimSpace(value)
	if normalization == "" {
		normalization = knowledgeEmbeddingDefaultNormalization
	}
	switch normalization {
	case knowledgeEmbeddingDefaultNormalization, knowledgeEmbeddingNoNormalization:
		return normalization, nil
	default:
		return "", knowledge.ErrInvalidEmbeddingProfile
	}
}

func knowledgeEmbeddingNormalizedEndpointIdentity(raw string) (string, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		endpoint = knowledgeEmbeddingDefaultEndpointIdentity
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.ForceQuery || parsed.Fragment != "" {
		return "", knowledge.ErrInvalidEmbeddingProfile
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", knowledge.ErrInvalidEmbeddingProfile
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	port := parsed.Port()
	if parsed.Scheme == "https" && port == "443" || parsed.Scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		parsed.Host = "[" + hostname + "]"
	} else {
		parsed.Host = hostname
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = strings.TrimRight(parsed.RawPath, "/")
	return parsed.String(), nil
}

func knowledgeEmbeddingProfileSnapshotForEntry(
	entry knowledgeEmbeddingProfileEntry,
) (knowledge.EmbeddingProfileSnapshot, error) {
	providerID := strings.TrimSpace(entry.ProviderID)
	model := strings.TrimSpace(entry.ModelName)
	maxInputRunes := knowledge.DefaultEmbedMaxRunes
	if profile, ok := knowledge.EmbeddingExecutionProfileForModel(model); ok {
		entry.Dimension = profile.Dimension
		maxInputRunes = profile.MaxInputRunes
	}
	protocol := strings.TrimSpace(entry.Protocol)
	if protocol == "" {
		protocol = knowledgeEmbeddingProtocolOpenAI
	}
	capability := strings.TrimSpace(entry.Capability)
	if providerID == "" || model == "" || capability != "embedding" || entry.Dimension <= 0 {
		return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrInvalidEmbeddingProfile
	}
	normalization, err := knowledgeEmbeddingNormalization(entry.Normalization)
	if err != nil {
		return knowledge.EmbeddingProfileSnapshot{}, err
	}
	endpoint, err := knowledgeEmbeddingNormalizedEndpointIdentity(entry.BaseURL)
	if err != nil {
		return knowledge.EmbeddingProfileSnapshot{}, err
	}
	providerName := strings.TrimSpace(entry.ProviderName)
	if providerName == "" {
		providerName = providerID
	}

	// profile_id is the durable logical selection identity. Endpoint rotation,
	// credentials and transient availability deliberately do not change it.
	profileID, err := knowledgeEmbeddingLogicalProfileID(providerID, model, protocol)
	if err != nil {
		return knowledge.EmbeddingProfileSnapshot{}, err
	}
	documentTransformHash := knowledgeEmbeddingDocumentTransformHash(
		knowledgeEmbeddingDocumentTransformEpoch,
		entry.DocumentPrefix,
		maxInputRunes,
	)
	configHash, err := knowledgeEmbeddingExecutorContractHash(knowledgeEmbeddingExecutorContract{
		ContractVersion:    knowledgeEmbeddingExecutorContractVersion,
		Dimension:          entry.Dimension,
		DocumentPrefix:     entry.DocumentPrefix,
		EndpointIdentity:   endpoint,
		ExactModelID:       model,
		MaxInputRunes:      maxInputRunes,
		Normalization:      normalization,
		Protocol:           protocol,
		ProviderInstanceID: providerID,
		ProviderLocation:   string(entry.Location),
		QueryPrefix:        entry.QueryPrefix,
		TransformEpoch:     knowledgeEmbeddingDocumentTransformEpoch,
	})
	if err != nil {
		return knowledge.EmbeddingProfileSnapshot{}, err
	}
	snapshot := knowledge.EmbeddingProfileSnapshot{
		Profile: knowledge.EmbeddingProfile{
			ProfileID: profileID, ModelName: model, ProviderID: providerID,
			ProviderName: providerName, Location: entry.Location, Capability: capability,
			Dimension: entry.Dimension, Availability: entry.Availability,
			DisplayOrder: entry.DisplayOrder,
		},
		Normalization: normalization, ChunkConfigHash: documentTransformHash,
		ProfileConfigHash: configHash,
	}
	if err := snapshot.Validate(); err != nil {
		return knowledge.EmbeddingProfileSnapshot{}, err
	}
	return snapshot, nil
}

// knowledgeEmbeddingProfileResolver projects only the embedding capability
// explicitly selected by resolveKnowledgeEmbeddingPlan. It intentionally does
// not enumerate cfg.LLM chat models: a chat-compatible endpoint is not proof
// that a model supports embeddings.
type knowledgeEmbeddingProfileResolver struct {
	multi               bool
	profiles            map[string]*knowledgeEmbeddingProfileState
	profileOrder        []string
	snapshot            knowledge.EmbeddingProfileSnapshot
	configured          bool
	runtimeGate         *knowledgeSemanticRuntimeGate
	probeAvailability   func(context.Context) knowledge.ProfileAvailability
	probeInterval       time.Duration
	lastProbe           time.Time
	availabilityMu      sync.RWMutex
	availabilityProbeMu sync.Mutex
}

func newKnowledgeEmbeddingProfileResolver(
	cfg *config.Config,
	plan knowledgeEmbeddingPlan,
	dimension int,
	runtimeGates ...*knowledgeSemanticRuntimeGate,
) *knowledgeEmbeddingProfileResolver {
	providerCfg := config.LLMProviderConfig{}
	if cfg != nil {
		providerCfg = cfg.LLM.Providers[plan.Provider]
	}
	location := knowledge.ProviderLocationCloud
	providerName := semanticProviderDisplayName(plan.Provider, providerCfg, plan.Ollama)
	availability := knowledge.ProfileAvailabilityUnavailable
	switch {
	case plan.Ollama:
		location = knowledge.ProviderLocationLocal
		switch {
		case plan.Ready:
			availability = knowledge.ProfileAvailabilityInstalled
		case plan.ServiceAvailable:
			availability = knowledge.ProfileAvailabilityDownloadable
		}
	case plan.Ready:
		availability = knowledge.ProfileAvailabilityConnected
	}

	queryPrefix, docPrefix := knowledgeEmbeddingPrefixes(cfg, plan.Model)
	snapshot, _ := knowledgeEmbeddingProfileSnapshotForEntry(knowledgeEmbeddingProfileEntry{
		ProviderID: config.EffectiveProviderInstanceID(plan.Provider, providerCfg), ProviderName: providerName, ModelName: plan.Model,
		Protocol: knowledgeEmbeddingProtocolOpenAI,
		BaseURL:  knowledgeEmbeddingEffectiveBaseURL(plan, providerCfg),
		Location: location, Capability: "embedding", Dimension: dimension,
		Availability: availability, DisplayOrder: 10,
		Normalization: knowledgeEmbeddingDefaultNormalization,
		QueryPrefix:   queryPrefix, DocumentPrefix: docPrefix, Configured: plan.Configured,
	})

	resolver := &knowledgeEmbeddingProfileResolver{
		snapshot: snapshot, configured: plan.Configured,
		runtimeGate:   selectKnowledgeSemanticRuntimeGate(runtimeGates),
		probeInterval: 5 * time.Second, lastProbe: time.Now(),
	}
	if plan.Ollama {
		baseURL, model := knowledgeEmbeddingEffectiveBaseURL(plan, providerCfg), plan.Model
		resolver.probeAvailability = func(ctx context.Context) knowledge.ProfileAvailability {
			if knowledge.OllamaModelInstalled(ctx, baseURL, model) {
				return knowledge.ProfileAvailabilityInstalled
			}
			if knowledge.OllamaServiceAvailable(ctx, baseURL) {
				return knowledge.ProfileAvailabilityDownloadable
			}
			return knowledge.ProfileAvailabilityUnavailable
		}
	}
	return resolver
}

func newKnowledgeEmbeddingProfileResolverFromEntries(
	entries []knowledgeEmbeddingProfileEntry,
	runtimeGates ...*knowledgeSemanticRuntimeGate,
) *knowledgeEmbeddingProfileResolver {
	resolver := &knowledgeEmbeddingProfileResolver{
		multi: true, profiles: make(map[string]*knowledgeEmbeddingProfileState),
		runtimeGate: selectKnowledgeSemanticRuntimeGate(runtimeGates),
	}
	type candidate struct {
		entry    knowledgeEmbeddingProfileEntry
		snapshot knowledge.EmbeddingProfileSnapshot
	}
	candidates := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		snapshot, err := knowledgeEmbeddingProfileSnapshotForEntry(entry)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{entry: entry, snapshot: snapshot})
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].snapshot.Profile, candidates[j].snapshot.Profile
		if a.DisplayOrder != b.DisplayOrder {
			return a.DisplayOrder < b.DisplayOrder
		}
		if a.ProviderName != b.ProviderName {
			return a.ProviderName < b.ProviderName
		}
		if a.ModelName != b.ModelName {
			return a.ModelName < b.ModelName
		}
		if a.ProviderID != b.ProviderID {
			return a.ProviderID < b.ProviderID
		}
		if a.ProfileID != b.ProfileID {
			return a.ProfileID < b.ProfileID
		}
		return candidates[i].snapshot.ProfileConfigHash < candidates[j].snapshot.ProfileConfigHash
	})
	for _, candidate := range candidates {
		profileID := candidate.snapshot.Profile.ProfileID
		if _, exists := resolver.profiles[profileID]; exists {
			continue
		}
		probeInterval := candidate.entry.ProbeInterval
		if probeInterval == 0 {
			probeInterval = 5 * time.Second
		}
		state := &knowledgeEmbeddingProfileState{
			snapshot: candidate.snapshot, configured: candidate.entry.Configured,
			probeAvailability: candidate.entry.ProbeAvailability,
			probeInterval:     probeInterval, lastProbe: time.Now(),
		}
		resolver.profiles[profileID] = state
		resolver.profileOrder = append(resolver.profileOrder, profileID)
	}
	if len(resolver.profileOrder) > 0 {
		first := resolver.profiles[resolver.profileOrder[0]]
		resolver.snapshot = first.snapshot
		resolver.configured = first.configured
	}
	return resolver
}

func knowledgeEmbeddingDocumentTransformHash(transformEpoch, documentPrefix string, maxRunes int) string {
	return semanticDigest(fmt.Sprintf(
		"embedding-document-transform:%s:doc-prefix=%q:max-runes=%d",
		transformEpoch, documentPrefix, maxRunes,
	))
}

// knowledgeEmbeddingPrefixes returns the effective query/document transforms
// that form part of one immutable vector space. Nomic's task prefixes are
// defaults, not runtime guesses, so they must participate in snapshot hashes.
func knowledgeEmbeddingPrefixes(cfg *config.Config, model string) (string, string) {
	if profile, ok := knowledge.EmbeddingExecutionProfileForModel(model); ok {
		return profile.QueryPrefix, profile.DocumentPrefix
	}
	queryPrefix, documentPrefix := "", ""
	if cfg != nil {
		queryPrefix = cfg.Knowledge.Embedding.QueryPrefix
		documentPrefix = cfg.Knowledge.Embedding.DocPrefix
	}
	if queryPrefix == "" && documentPrefix == "" && strings.Contains(strings.ToLower(model), "nomic") {
		return "search_query: ", "search_document: "
	}
	return queryPrefix, documentPrefix
}

type knowledgeEmbeddingExecutorEntry struct {
	Snapshot       knowledge.EmbeddingProfileSnapshot
	Embedder       hexagon.VectorEmbedder
	Readiness      interface{ Ready(context.Context) bool }
	QueryPrefix    string
	DocumentPrefix string
}

type knowledgeEmbeddingExecutorState struct {
	snapshot       knowledge.EmbeddingProfileSnapshot
	embedder       hexagon.VectorEmbedder
	readiness      interface{ Ready(context.Context) bool }
	queryPrefix    string
	documentPrefix string
}

// knowledgeEmbeddingExecutorRegistry keeps every immutable vector-space
// executor addressable by profile_config_hash. A missing hash is a hard
// failure: revisions must never fall through to the current/default model.
type knowledgeEmbeddingExecutorRegistry struct {
	executors   map[string]knowledgeEmbeddingExecutorState
	runtimeGate *knowledgeSemanticRuntimeGate
}

type knowledgeEmbeddingRuntimeHolderState struct {
	resolver *knowledgeEmbeddingProfileResolver
	registry *knowledgeEmbeddingExecutorRegistry
	gate     *knowledgeSemanticRuntimeGate
}

// knowledgeEmbeddingRuntimeHolder is the stable dependency installed into
// the semantic service/searcher/worker. Provider hot reload builds a complete
// immutable successor off to the side, drains the prior gate, then atomically
// publishes the new resolver+registry pair. No caller can observe a resolver
// from one generation with an executor registry from another.
type knowledgeEmbeddingRuntimeHolder struct {
	current   atomic.Pointer[knowledgeEmbeddingRuntimeHolderState]
	replaceMu sync.Mutex
}

func newKnowledgeEmbeddingRuntimeHolder(
	bundle knowledgeEmbeddingRuntimeProfiles,
) (*knowledgeEmbeddingRuntimeHolder, error) {
	state, err := knowledgeEmbeddingRuntimeHolderStateFor(bundle)
	if err != nil {
		return nil, err
	}
	holder := &knowledgeEmbeddingRuntimeHolder{}
	holder.current.Store(state)
	return holder, nil
}

func knowledgeEmbeddingRuntimeHolderStateFor(
	bundle knowledgeEmbeddingRuntimeProfiles,
) (*knowledgeEmbeddingRuntimeHolderState, error) {
	if bundle.Resolver == nil || bundle.Registry == nil || bundle.Resolver.runtimeGate == nil ||
		bundle.Registry.runtimeGate == nil || bundle.Resolver.runtimeGate != bundle.Registry.runtimeGate ||
		!bundle.Resolver.runtimeGate.Available() {
		return nil, fmt.Errorf("knowledge: invalid embedding runtime replacement")
	}
	return &knowledgeEmbeddingRuntimeHolderState{
		resolver: bundle.Resolver, registry: bundle.Registry, gate: bundle.Resolver.runtimeGate,
	}, nil
}

func (h *knowledgeEmbeddingRuntimeHolder) Replace(
	ctx context.Context,
	bundle knowledgeEmbeddingRuntimeProfiles,
) error {
	next, err := knowledgeEmbeddingRuntimeHolderStateFor(bundle)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil {
		return knowledge.ErrProfileUnavailable
	}
	h.replaceMu.Lock()
	defer h.replaceMu.Unlock()
	previous := h.current.Load()
	if previous == nil {
		h.current.Store(next)
		return nil
	}
	if err := previous.gate.Revoke(ctx); err != nil {
		// The successor was never published. Revoke its independent gate as a
		// hygiene boundary; the persisted restart target semantics are owned by
		// the configuration transaction.
		_ = next.gate.Revoke(context.Background())
		return err
	}
	h.current.Store(next)
	return nil
}

func (h *knowledgeEmbeddingRuntimeHolder) Resolve(
	ctx context.Context,
	ownerID, corpusID string,
	selection knowledge.EmbeddingSelection,
) (knowledge.EmbeddingProfileSnapshot, error) {
	if h == nil {
		return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrProfileUnavailable
	}
	state := h.current.Load()
	if state == nil {
		return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrProfileUnavailable
	}
	return state.resolver.Resolve(ctx, ownerID, corpusID, selection)
}

func (h *knowledgeEmbeddingRuntimeHolder) Catalog(
	ctx context.Context,
	ownerID, corpusID string,
) (knowledge.EmbeddingProfileCatalog, error) {
	if h == nil {
		return knowledge.EmbeddingProfileCatalog{}, knowledge.ErrProfileUnavailable
	}
	state := h.current.Load()
	if state == nil {
		return knowledge.EmbeddingProfileCatalog{}, knowledge.ErrProfileUnavailable
	}
	return state.resolver.Catalog(ctx, ownerID, corpusID)
}

func (h *knowledgeEmbeddingRuntimeHolder) ExecutorForProfile(
	ctx context.Context,
	snapshot knowledge.EmbeddingProfileSnapshot,
) (knowledge.ProfileEmbeddingExecutor, error) {
	if h == nil {
		return nil, knowledge.ErrProfileUnavailable
	}
	state := h.current.Load()
	if state == nil {
		return nil, knowledge.ErrProfileUnavailable
	}
	return state.registry.ExecutorForProfile(ctx, snapshot)
}

func (h *knowledgeEmbeddingRuntimeHolder) invalidateAvailability() {
	if h == nil {
		return
	}
	if state := h.current.Load(); state != nil {
		state.resolver.invalidateAvailability()
	}
}

func (h *knowledgeEmbeddingRuntimeHolder) RevokeCurrent(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.replaceMu.Lock()
	defer h.replaceMu.Unlock()
	if state := h.current.Load(); state != nil {
		return state.gate.Revoke(ctx)
	}
	return nil
}

func newKnowledgeEmbeddingExecutorRegistry(
	snapshot knowledge.EmbeddingProfileSnapshot,
	embedder hexagon.VectorEmbedder,
	queryPrefix, documentPrefix string,
	runtimeGates ...*knowledgeSemanticRuntimeGate,
) *knowledgeEmbeddingExecutorRegistry {
	return newKnowledgeEmbeddingExecutorRegistryFromEntries(
		[]knowledgeEmbeddingExecutorEntry{{
			Snapshot: snapshot, Embedder: embedder,
			QueryPrefix: queryPrefix, DocumentPrefix: documentPrefix,
		}},
		runtimeGates...,
	)
}

func newKnowledgeEmbeddingExecutorRegistryFromEntries(
	entries []knowledgeEmbeddingExecutorEntry,
	runtimeGates ...*knowledgeSemanticRuntimeGate,
) *knowledgeEmbeddingExecutorRegistry {
	registry := &knowledgeEmbeddingExecutorRegistry{
		executors:   make(map[string]knowledgeEmbeddingExecutorState),
		runtimeGate: selectKnowledgeSemanticRuntimeGate(runtimeGates),
	}
	for _, entry := range entries {
		hash := strings.TrimSpace(entry.Snapshot.ProfileConfigHash)
		if hash == "" || entry.Embedder == nil || entry.Snapshot.Validate() != nil ||
			entry.Embedder.Dimension() != entry.Snapshot.Profile.Dimension {
			continue
		}
		if _, exists := registry.executors[hash]; exists {
			continue
		}
		embedder := entry.Embedder
		if profile, ok := knowledge.EmbeddingExecutionProfileForModel(entry.Snapshot.Profile.ModelName); ok {
			embedder = knowledge.NewTruncatingEmbedder(embedder, profile.MaxInputRunes)
		}
		registry.executors[hash] = knowledgeEmbeddingExecutorState{
			snapshot: entry.Snapshot, embedder: embedder,
			readiness:   entry.Readiness,
			queryPrefix: entry.QueryPrefix, documentPrefix: entry.DocumentPrefix,
		}
	}
	return registry
}

func (r *knowledgeEmbeddingExecutorRegistry) ExecutorForProfile(
	ctx context.Context,
	snapshot knowledge.EmbeddingProfileSnapshot,
) (knowledge.ProfileEmbeddingExecutor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || !r.runtimeGate.Available() || snapshot.Validate() != nil {
		return nil, knowledge.ErrProfileUnavailable
	}
	entry, ok := r.executors[snapshot.ProfileConfigHash]
	if !ok || entry.embedder == nil || !sameKnowledgeEmbeddingSpace(entry.snapshot, snapshot) ||
		entry.embedder.Dimension() != snapshot.Profile.Dimension {
		return nil, knowledge.ErrProfileUnavailable
	}
	return &knowledgePurposeEmbedder{
		embedder: entry.embedder, queryPrefix: entry.queryPrefix, documentPrefix: entry.documentPrefix,
		readiness: entry.readiness, location: entry.snapshot.Profile.Location, runtimeGate: r.runtimeGate,
	}, nil
}

func sameKnowledgeEmbeddingSpace(a, b knowledge.EmbeddingProfileSnapshot) bool {
	return a.Profile.ProfileID == b.Profile.ProfileID &&
		a.Profile.ProviderID == b.Profile.ProviderID &&
		a.Profile.ModelName == b.Profile.ModelName &&
		a.Profile.Location == b.Profile.Location &&
		a.Profile.Capability == b.Profile.Capability &&
		a.Profile.Dimension == b.Profile.Dimension &&
		a.Normalization == b.Normalization &&
		a.ChunkConfigHash == b.ChunkConfigHash &&
		a.ProfileConfigHash == b.ProfileConfigHash
}

type knowledgePurposeEmbedder struct {
	embedder       hexagon.VectorEmbedder
	readiness      interface{ Ready(context.Context) bool }
	queryPrefix    string
	documentPrefix string
	location       knowledge.ProviderLocation
	runtimeGate    *knowledgeSemanticRuntimeGate
}

func (e *knowledgePurposeEmbedder) LocalInferenceAdmissionAtProviderBoundary() bool {
	if e == nil || e.embedder == nil {
		return false
	}
	marker, ok := e.embedder.(interface {
		LocalInferenceAdmissionAtProviderBoundary() bool
	})
	return ok && marker.LocalInferenceAdmissionAtProviderBoundary()
}

func (e *knowledgePurposeEmbedder) EmbeddingReady(ctx context.Context) bool {
	if e == nil || e.embedder == nil {
		return false
	}
	callCtx, release, err := e.runtimeGate.Bind(ctx)
	if err != nil {
		return false
	}
	defer release()
	ready := knowledge.EmbeddingReady(callCtx, e.embedder)
	if e.readiness != nil {
		ready = e.readiness.Ready(callCtx)
	}
	return ready && callCtx.Err() == nil
}

func (e *knowledgePurposeEmbedder) EmbedForPurpose(
	ctx context.Context,
	purpose knowledge.EmbeddingPurpose,
	texts []string,
) ([][]float32, error) {
	if e == nil || e.embedder == nil {
		return nil, knowledge.ErrProfileUnavailable
	}
	callCtx, release, err := e.runtimeGate.Bind(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	prefix := ""
	switch purpose {
	case knowledge.EmbeddingPurposeQuery:
		prefix = e.queryPrefix
		callCtx = localinfer.WithOperation(callCtx, localinfer.OperationQueryEmbedding)
	case knowledge.EmbeddingPurposeDocument:
		prefix = e.documentPrefix
		callCtx = localinfer.WithOperation(callCtx, localinfer.OperationDocumentEmbedding)
	default:
		return nil, fmt.Errorf("%w: unknown embedding purpose %q", knowledge.ErrInvalidEmbeddingProfile, purpose)
	}
	inputs := make([]string, len(texts))
	for i, input := range texts {
		inputs[i] = prefix + input
	}
	if err := callCtx.Err(); err != nil {
		return nil, err
	}
	if e.location == knowledge.ProviderLocationCloud {
		callCtx = egress.WithRequest(callCtx, egress.PurposeRAGEmbed, "", egress.ClassDocument)
	}
	return e.embedder.Embed(callCtx, inputs)
}

// knowledgeSemanticRuntimeGate is a one-way revocation boundary shared by the
// profile resolver, registry and every executor already handed to callers.
// Revoke permanently rejects future calls, synchronously cancels contexts
// already inside the boundary, and waits (under the caller's deadline) for
// those calls to leave before a configuration endpoint may report success.
type knowledgeSemanticRuntimeGate struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	revoked bool
	active  int
	drained chan struct{}
}

func newKnowledgeSemanticRuntimeGate() *knowledgeSemanticRuntimeGate {
	ctx, cancel := context.WithCancel(context.Background())
	return &knowledgeSemanticRuntimeGate{ctx: ctx, cancel: cancel}
}

func selectKnowledgeSemanticRuntimeGate(gates []*knowledgeSemanticRuntimeGate) *knowledgeSemanticRuntimeGate {
	if len(gates) > 0 && gates[0] != nil {
		return gates[0]
	}
	return newKnowledgeSemanticRuntimeGate()
}

func (g *knowledgeSemanticRuntimeGate) Available() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.revoked
}

func (g *knowledgeSemanticRuntimeGate) Bind(ctx context.Context) (context.Context, func(), error) {
	if g == nil {
		return nil, nil, knowledge.ErrProfileUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	if g.revoked {
		g.mu.Unlock()
		return nil, nil, knowledge.ErrProfileUnavailable
	}
	g.active++
	callCtx, cancel := context.WithCancel(ctx)
	stopGate := context.AfterFunc(g.ctx, cancel)
	g.mu.Unlock()
	if err := callCtx.Err(); err != nil {
		stopGate()
		cancel()
		g.release()
		return nil, nil, err
	}
	var once sync.Once
	return callCtx, func() {
		once.Do(func() {
			stopGate()
			cancel()
			g.release()
		})
	}, nil
}

func (g *knowledgeSemanticRuntimeGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.revoked && g.active == 0 && g.drained != nil {
		close(g.drained)
		g.drained = nil
	}
}

func (g *knowledgeSemanticRuntimeGate) Revoke(ctx context.Context) error {
	if g == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	if !g.revoked {
		g.revoked = true
		g.cancel()
	}
	if g.active == 0 {
		g.mu.Unlock()
		return nil
	}
	if g.drained == nil {
		g.drained = make(chan struct{})
	}
	drained := g.drained
	g.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *knowledgeEmbeddingProfileState) cachedSnapshot() knowledge.EmbeddingProfileSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

func (s *knowledgeEmbeddingProfileState) snapshotForResolve(
	ctx context.Context,
	gate *knowledgeSemanticRuntimeGate,
) (knowledge.EmbeddingProfileSnapshot, error) {
	s.mu.RLock()
	snapshot := s.snapshot
	probeAvailability := s.probeAvailability
	probeInterval, lastProbe := s.probeInterval, s.lastProbe
	s.mu.RUnlock()
	if probeAvailability == nil || knowledgeEmbeddingProbeFresh(probeInterval, lastProbe) {
		return snapshot, nil
	}

	// Resolve calls serialize their provider I/O, but Catalog never acquires
	// probeMu and can keep serving the last completed cached projection.
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	s.mu.RLock()
	snapshot = s.snapshot
	probeAvailability = s.probeAvailability
	probeInterval, lastProbe = s.probeInterval, s.lastProbe
	s.mu.RUnlock()
	if probeAvailability == nil || knowledgeEmbeddingProbeFresh(probeInterval, lastProbe) {
		return snapshot, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	boundedCtx, cancel := context.WithTimeout(ctx, knowledgeEmbeddingProbeTimeout)
	defer cancel()
	probeCtx, release, err := gate.Bind(boundedCtx)
	if err != nil {
		return knowledge.EmbeddingProfileSnapshot{}, err
	}
	defer release()
	snapshot.Profile.Availability = probeAvailability(probeCtx)
	if err := probeCtx.Err(); err != nil {
		return knowledge.EmbeddingProfileSnapshot{}, err
	}
	s.mu.Lock()
	s.snapshot.Profile.Availability = snapshot.Profile.Availability
	s.lastProbe = time.Now()
	s.mu.Unlock()
	return snapshot, nil
}

func knowledgeEmbeddingProbeFresh(interval time.Duration, lastProbe time.Time) bool {
	return interval > 0 && !lastProbe.IsZero() && time.Since(lastProbe) < interval
}

func knowledgeEmbeddingSnapshotExecutable(snapshot knowledge.EmbeddingProfileSnapshot) bool {
	return snapshot.Profile.Availability == knowledge.ProfileAvailabilityInstalled ||
		snapshot.Profile.Availability == knowledge.ProfileAvailabilityConnected
}

func (r *knowledgeEmbeddingProfileResolver) resolveMulti(
	ctx context.Context,
	selection knowledge.EmbeddingSelection,
) (knowledge.EmbeddingProfileSnapshot, error) {
	resolveState := func(state *knowledgeEmbeddingProfileState) (knowledge.EmbeddingProfileSnapshot, error) {
		snapshot, err := state.snapshotForResolve(ctx, r.runtimeGate)
		if err != nil {
			return knowledge.EmbeddingProfileSnapshot{}, err
		}
		if snapshot.Validate() != nil || !knowledgeEmbeddingSnapshotExecutable(snapshot) {
			return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrProfileUnavailable
		}
		return snapshot, nil
	}

	if selection.Kind == knowledge.EmbeddingSelectionProfile {
		state, ok := r.profiles[strings.TrimSpace(selection.ProfileID)]
		if !ok {
			return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrProfileUnavailable
		}
		return resolveState(state)
	}
	for _, profileID := range r.profileOrder {
		snapshot, err := resolveState(r.profiles[profileID])
		if err == nil {
			return snapshot, nil
		}
		if !errors.Is(err, knowledge.ErrProfileUnavailable) {
			return knowledge.EmbeddingProfileSnapshot{}, err
		}
	}
	return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrProfileUnavailable
}

func (r *knowledgeEmbeddingProfileResolver) catalogMulti() knowledge.EmbeddingProfileCatalog {
	profiles := make([]knowledge.EmbeddingProfile, 0, len(r.profileOrder))
	var recommendation *knowledge.EmbeddingRecommendation
	configured := false
	for _, profileID := range r.profileOrder {
		state := r.profiles[profileID]
		snapshot := state.cachedSnapshot()
		profiles = append(profiles, snapshot.Profile)
		configured = configured || state.configured
		if recommendation == nil && state.configured && knowledgeEmbeddingSnapshotExecutable(snapshot) {
			id := snapshot.Profile.ProfileID
			recommendation = &knowledge.EmbeddingRecommendation{
				ProfileID: &id, ReasonCode: "configured_embedding",
				ReasonText: "优先使用当前已配置的索引模型。",
			}
		}
	}
	if recommendation == nil && configured {
		recommendation = &knowledge.EmbeddingRecommendation{
			ProfileID: nil, ReasonCode: "embedding_unavailable",
			ReasonText: "当前配置的索引模型不可用，关键词检索仍可使用。",
		}
	}
	return knowledge.EmbeddingProfileCatalog{
		Profiles: profiles, Recommendation: recommendation,
		Version: semanticCatalogVersionForProfiles(profiles),
	}
}

func (r *knowledgeEmbeddingProfileResolver) currentSnapshot(ctx context.Context) (knowledge.EmbeddingProfileSnapshot, error) {
	r.availabilityMu.RLock()
	snapshot := r.snapshot
	probeAvailability := r.probeAvailability
	probeInterval, lastProbe := r.probeInterval, r.lastProbe
	r.availabilityMu.RUnlock()
	if probeAvailability == nil || knowledgeEmbeddingProbeFresh(probeInterval, lastProbe) {
		return snapshot, nil
	}

	r.availabilityProbeMu.Lock()
	defer r.availabilityProbeMu.Unlock()
	r.availabilityMu.RLock()
	snapshot = r.snapshot
	probeAvailability = r.probeAvailability
	probeInterval, lastProbe = r.probeInterval, r.lastProbe
	r.availabilityMu.RUnlock()
	if probeAvailability == nil || knowledgeEmbeddingProbeFresh(probeInterval, lastProbe) {
		return snapshot, nil
	}
	// A live availability probe is provider I/O owned by this semantic runtime,
	// so it participates in the same one-way revoke/cancel/drain boundary as an
	// embedding call. Cached and static snapshot reads remain lock-only.
	if ctx == nil {
		ctx = context.Background()
	}
	boundedCtx, cancel := context.WithTimeout(ctx, knowledgeEmbeddingProbeTimeout)
	defer cancel()
	probeCtx, release, err := r.runtimeGate.Bind(boundedCtx)
	if err != nil {
		return knowledge.EmbeddingProfileSnapshot{}, err
	}
	defer release()
	snapshot.Profile.Availability = probeAvailability(probeCtx)
	if err := probeCtx.Err(); err != nil {
		return knowledge.EmbeddingProfileSnapshot{}, err
	}
	r.availabilityMu.Lock()
	r.snapshot.Profile.Availability = snapshot.Profile.Availability
	r.lastProbe = time.Now()
	r.availabilityMu.Unlock()
	return snapshot, nil
}

func (r *knowledgeEmbeddingProfileResolver) cachedSingleSnapshot() knowledge.EmbeddingProfileSnapshot {
	r.availabilityMu.RLock()
	defer r.availabilityMu.RUnlock()
	return r.snapshot
}

func (r *knowledgeEmbeddingProfileResolver) invalidateAvailability() {
	if r == nil {
		return
	}
	if r.multi {
		for _, state := range r.profiles {
			state.probeMu.Lock()
			state.mu.Lock()
			state.lastProbe = time.Time{}
			state.mu.Unlock()
			state.probeMu.Unlock()
		}
		return
	}
	r.availabilityProbeMu.Lock()
	r.availabilityMu.Lock()
	r.lastProbe = time.Time{}
	r.availabilityMu.Unlock()
	r.availabilityProbeMu.Unlock()
}

func (r *knowledgeEmbeddingProfileResolver) currentRecommendation(
	snapshot knowledge.EmbeddingProfileSnapshot,
) *knowledge.EmbeddingRecommendation {
	if !r.configured {
		return nil
	}
	if snapshot.Profile.Availability == knowledge.ProfileAvailabilityUnavailable {
		return &knowledge.EmbeddingRecommendation{
			ProfileID: nil, ReasonCode: "embedding_unavailable",
			ReasonText: "当前配置的索引模型不可用，关键词检索仍可使用。",
		}
	}
	profileID := snapshot.Profile.ProfileID
	reasonCode, reasonText := "configured_embedding", "优先使用当前已配置的索引模型。"
	if snapshot.Profile.Availability == knowledge.ProfileAvailabilityDownloadable {
		reasonCode, reasonText = "local_model_download", "下载完成后可使用该本地模型构建语义索引。"
	}
	return &knowledge.EmbeddingRecommendation{
		ProfileID: &profileID, ReasonCode: reasonCode, ReasonText: reasonText,
	}
}

func (r *knowledgeEmbeddingProfileResolver) Resolve(
	ctx context.Context,
	_, _ string,
	selection knowledge.EmbeddingSelection,
) (knowledge.EmbeddingProfileSnapshot, error) {
	if r == nil || !r.runtimeGate.Available() {
		return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrProfileUnavailable
	}
	if err := selection.Validate(); err != nil {
		return knowledge.EmbeddingProfileSnapshot{}, err
	}
	if selection.Kind == knowledge.EmbeddingSelectionDisabled {
		return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrInvalidSelection
	}
	if r.multi {
		snapshot, err := r.resolveMulti(ctx, selection)
		if err != nil {
			return knowledge.EmbeddingProfileSnapshot{}, err
		}
		if !r.runtimeGate.Available() {
			return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrProfileUnavailable
		}
		return snapshot, nil
	}
	snapshot, err := r.currentSnapshot(ctx)
	if err != nil {
		return knowledge.EmbeddingProfileSnapshot{}, err
	}
	if !r.runtimeGate.Available() {
		return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrProfileUnavailable
	}
	if selection.Kind == knowledge.EmbeddingSelectionProfile && selection.ProfileID != snapshot.Profile.ProfileID {
		return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrProfileUnavailable
	}
	if err := snapshot.Validate(); err != nil {
		return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrProfileUnavailable
	}
	if snapshot.Profile.Availability != knowledge.ProfileAvailabilityInstalled &&
		snapshot.Profile.Availability != knowledge.ProfileAvailabilityConnected {
		// Download is a separate catalog action in the approved prototype. A
		// policy apply may only freeze a profile that can execute immediately;
		// it must never create an unconsumable download/rebuild job.
		return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrProfileUnavailable
	}
	return snapshot, nil
}

func (r *knowledgeEmbeddingProfileResolver) Catalog(
	ctx context.Context,
	_, _ string,
) (knowledge.EmbeddingProfileCatalog, error) {
	if r == nil || !r.runtimeGate.Available() {
		return knowledge.EmbeddingProfileCatalog{}, knowledge.ErrProfileUnavailable
	}
	if r.multi {
		catalog := r.catalogMulti()
		if !r.runtimeGate.Available() {
			return knowledge.EmbeddingProfileCatalog{}, knowledge.ErrProfileUnavailable
		}
		return catalog, nil
	}
	// Catalog is a read-only cached projection. Provider I/O is restricted to
	// startup and Resolve/Apply so GET latency and availability are isolated
	// from slow or failed upstream probes.
	snapshot := r.cachedSingleSnapshot()
	if !r.runtimeGate.Available() {
		return knowledge.EmbeddingProfileCatalog{}, knowledge.ErrProfileUnavailable
	}
	profiles := []knowledge.EmbeddingProfile{}
	if snapshot.Profile.ProfileID != "" && snapshot.Profile.ModelName != "" && snapshot.Profile.Dimension > 0 {
		profiles = append(profiles, snapshot.Profile)
	}
	return knowledge.EmbeddingProfileCatalog{
		Profiles: profiles, Recommendation: r.currentRecommendation(snapshot), Version: semanticCatalogVersion(snapshot.Profile),
	}, nil
}

func semanticProviderDisplayName(providerID string, provider config.LLMProviderConfig, ollama bool) string {
	if ollama {
		return "Ollama"
	}
	lowerID := strings.ToLower(strings.TrimSpace(providerID))
	if strings.EqualFold(strings.TrimSpace(provider.Compatible), "openai") || strings.Contains(lowerID, "openai") {
		return "OpenAI 兼容"
	}
	if name := strings.TrimSpace(providerID); name != "" {
		return name
	}
	return "Provider"
}

func semanticDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func semanticCatalogVersion(profile knowledge.EmbeddingProfile) int64 {
	return semanticCatalogVersionForProfiles([]knowledge.EmbeddingProfile{profile})
}

func semanticCatalogVersionForProfiles(profiles []knowledge.EmbeddingProfile) int64 {
	canonical := append([]knowledge.EmbeddingProfile(nil), profiles...)
	sort.Slice(canonical, func(i, j int) bool {
		a, b := canonical[i], canonical[j]
		if a.DisplayOrder != b.DisplayOrder {
			return a.DisplayOrder < b.DisplayOrder
		}
		if a.ProviderName != b.ProviderName {
			return a.ProviderName < b.ProviderName
		}
		if a.ModelName != b.ModelName {
			return a.ModelName < b.ModelName
		}
		if a.ProviderID != b.ProviderID {
			return a.ProviderID < b.ProviderID
		}
		return a.ProfileID < b.ProfileID
	})
	var value strings.Builder
	for _, profile := range canonical {
		_, _ = fmt.Fprintf(&value, "%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00%d\x00%s\x01",
			profile.ProfileID, profile.ProviderID, profile.ProviderName, profile.ModelName,
			profile.Location, profile.Dimension, profile.Availability, profile.DisplayOrder, profile.Capability)
	}
	sum := sha256.Sum256([]byte(value.String()))
	version := int64(binary.BigEndian.Uint64(sum[:8]) & uint64(^uint64(0)>>1))
	if version == 0 {
		return 1
	}
	return version
}

// knowledgeSemanticIndexRuntime is the production control/data-plane bundle.
// Keeping these objects together makes it difficult to accidentally expose the
// policy API without also installing revision-bound search and the durable
// worker that can advance its jobs.
type knowledgeSemanticIndexRuntime struct {
	Repository *knowledge.SQLiteSemanticIndexRepository
	Service    *knowledge.SemanticIndexService
	Searcher   *knowledge.SQLiteRevisionSemanticSearcher
	Worker     *knowledge.SemanticIndexWorker
	Gate       *knowledgeSemanticRuntimeGate
	Profiles   *knowledgeEmbeddingRuntimeHolder
}

type knowledgeSemanticRuntimeAssembly struct {
	gate           *knowledgeSemanticRuntimeGate
	governor       *resourcegov.Governor
	localInference *localinfer.Coordinator
}

type knowledgeSemanticRuntimeOption func(*knowledgeSemanticRuntimeAssembly)

func withKnowledgeSemanticRuntimeGate(gate *knowledgeSemanticRuntimeGate) knowledgeSemanticRuntimeOption {
	return func(assembly *knowledgeSemanticRuntimeAssembly) { assembly.gate = gate }
}

func withKnowledgeSemanticResourceGovernor(governor *resourcegov.Governor) knowledgeSemanticRuntimeOption {
	return func(assembly *knowledgeSemanticRuntimeAssembly) { assembly.governor = governor }
}

func withKnowledgeSemanticLocalInferenceCoordinator(
	coordinator *localinfer.Coordinator,
) knowledgeSemanticRuntimeOption {
	return func(assembly *knowledgeSemanticRuntimeAssembly) { assembly.localInference = coordinator }
}

func (r *knowledgeSemanticIndexRuntime) Revoke(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if r.Profiles != nil {
		return r.Profiles.RevokeCurrent(ctx)
	}
	return r.Gate.Revoke(ctx)
}

// activateInstalledKnowledgeSemanticIndex refreshes the volatile local-model
// capability and retries the one-time default policy bootstrap. It is shared
// by automatic provisioning and the manual Ollama pull completion hook so both
// paths transition a previously text-only corpus without requiring a restart.
func activateInstalledKnowledgeSemanticIndex(
	ctx context.Context,
	runtime *knowledgeSemanticIndexRuntime,
	resolver interface{ invalidateAvailability() },
) error {
	if runtime == nil || runtime.Service == nil || resolver == nil {
		return fmt.Errorf("knowledge: invalid installed-model activation runtime")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	resolver.invalidateAvailability()
	_, err := runtime.Service.EnsureDefaultPolicy(
		ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID,
	)
	return err
}

func setupKnowledgeSemanticIndex(
	ctx context.Context,
	db *sql.DB,
	resolver knowledge.EmbeddingProfileResolver,
	registry knowledge.ProfileEmbeddingExecutorRegistry,
	workerID string,
	options ...knowledgeSemanticRuntimeOption,
) (*knowledgeSemanticIndexRuntime, error) {
	if db == nil || resolver == nil || registry == nil || strings.TrimSpace(workerID) == "" {
		return nil, fmt.Errorf("knowledge: invalid semantic index runtime configuration")
	}
	assembly := &knowledgeSemanticRuntimeAssembly{}
	for _, option := range options {
		if option != nil {
			option(assembly)
		}
	}
	repository := knowledge.NewSQLiteSemanticIndexRepository(db)
	if _, err := repository.BindLegacyDefaultCorpus(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID); err != nil {
		return nil, fmt.Errorf("knowledge: bind desktop corpus: %w", err)
	}
	service := knowledge.NewSemanticIndexService(repository, resolver)
	searchOptions := []knowledge.RevisionSearchOption{}
	workerOptions := []knowledge.SemanticIndexWorkerOption{}
	if assembly.localInference != nil {
		searchOptions = append(searchOptions,
			knowledge.WithRevisionSearchLocalInferenceCoordinator(assembly.localInference))
		workerOptions = append(workerOptions,
			knowledge.WithSemanticWorkerLocalInferenceCoordinator(assembly.localInference))
	} else {
		searchOptions = append(searchOptions,
			knowledge.WithRevisionSearchResourceGovernor(assembly.governor))
		workerOptions = append(workerOptions,
			knowledge.WithSemanticWorkerResourceGovernor(assembly.governor))
	}
	searcher := knowledge.NewSQLiteRevisionSemanticSearcher(
		db, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID, registry, searchOptions...,
	)
	worker := knowledge.NewSemanticIndexWorker(repository, registry, knowledge.SemanticIndexWorkerConfig{
		OwnerID: knowledgeDesktopOwnerID, CorpusID: knowledgeDefaultCorpusID,
		WorkerID: workerID, BatchSize: 64, LeaseDuration: 5 * time.Minute,
		RetryDelay: 30 * time.Second,
	}, workerOptions...)
	runtime := &knowledgeSemanticIndexRuntime{
		Repository: repository, Service: service, Searcher: searcher, Worker: worker,
		Gate: selectKnowledgeSemanticRuntimeGate([]*knowledgeSemanticRuntimeGate{assembly.gate}),
	}
	if holder, ok := resolver.(*knowledgeEmbeddingRuntimeHolder); ok {
		runtime.Profiles = holder
	}
	if _, err := service.EnsureDefaultPolicy(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID); err != nil &&
		!errors.Is(err, knowledge.ErrProfileUnavailable) {
		// BindLegacyDefaultCorpus has already committed an owner/generation
		// boundary. Return the usable text-only semantic runtime alongside the
		// bootstrap error; callers must never fall back to physical legacy CRUD.
		return runtime, fmt.Errorf("knowledge: bootstrap embedding policy: %w", err)
	}
	return runtime, nil
}

type knowledgeSemanticWorkerRunner interface {
	RunOnce(context.Context) (bool, error)
}

// runKnowledgeSemanticIndexWorker drains durable work immediately and only
// backs off when the queue is empty. The caller owns ctx and can wait on its
// goroutine before closing SQLite, preventing shutdown-time lease/write races.
func runKnowledgeSemanticIndexWorker(
	ctx context.Context,
	runner knowledgeSemanticWorkerRunner,
	idleDelay time.Duration,
	onError func(error),
) {
	if runner == nil {
		return
	}
	if idleDelay <= 0 {
		idleDelay = 500 * time.Millisecond
	}
	for {
		processed, err := runner.RunOnce(ctx)
		if err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
		if ctx.Err() != nil {
			return
		}
		if processed {
			continue
		}
		timer := time.NewTimer(idleDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}
