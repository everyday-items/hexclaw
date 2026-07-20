package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/knowledge"
)

func TestKnowledgeEmbeddingRuntimeProfilesProbeExactCloudModelsAndKeepCatalogReadOnly(t *testing.T) {
	models := []string{
		config.OpenRouterNemotronEmbedFreeModelID,
		config.OpenRouterVLLEmbedFreeModelID,
	}
	var embeddingRequests atomic.Int64
	var chatRequests atomic.Int64
	var mu sync.Mutex
	failedModels := map[string]bool{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/embeddings"):
			embeddingRequests.Add(1)
			var request struct {
				Model string   `json:"model"`
				Input []string `json:"input"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			mu.Lock()
			failed := failedModels[request.Model]
			mu.Unlock()
			if len(request.Input) == 0 {
				http.Error(w, "embedding input must not be empty", http.StatusBadRequest)
				return
			}
			dimension := 2048
			nonzero := true
			if request.Model == "vendor/unknown-embedding" {
				dimension = 17
				nonzero = false // Dimension can be observed, but an all-zero probe is not a usable capability.
			}
			if failed {
				dimension-- // Successful HTTP with a wrong vector contract must fail closed without transport retries.
			}
			vector := make([]float32, dimension)
			if nonzero {
				vector[0] = 1
			}
			data := make([]map[string]any, len(request.Input))
			for i := range data {
				data[i] = map[string]any{
					"object": "embedding", "index": i, "embedding": vector,
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"model":  request.Model,
				"data":   data,
				"usage":  map[string]int{"prompt_tokens": 1, "total_tokens": 1},
			})
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			chatRequests.Add(1)
			http.Error(w, "chat must not be called", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	const providerInstanceID = "pvd_v1_0123456789abcdef0123456789abcdef"
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "OpenRouter Primary"
	cfg.Knowledge.Embedding.Model = models[0]
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"OpenRouter Primary": {
			ProviderInstanceID: providerInstanceID,
			APIKey:             "test-secret",
			BaseURL:            provider.URL + "/v1",
			Locality:           config.ProviderLocalityCloud,
			ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
			Models: append(append([]string(nil), models...),
				"chat-only", "vendor/unknown-embedding"),
			ModelSpecs: []config.LLMProviderModelSpec{
				{ID: models[0], Capabilities: []string{config.LLMModelCapabilityEmbedding}},
				{ID: models[1], Capabilities: []string{config.LLMModelCapabilityEmbedding}},
				{ID: "chat-only", Capabilities: []string{config.LLMModelCapabilityText}},
				{ID: "vendor/unknown-embedding", Capabilities: []string{config.LLMModelCapabilityEmbedding}},
			},
		},
	}
	gate := newKnowledgeSemanticRuntimeGate()
	bundle := buildKnowledgeEmbeddingRuntimeProfiles(context.Background(), cfg, &egress.Policy{}, gate)
	if bundle.Resolver == nil || bundle.Registry == nil {
		t.Fatal("runtime profile builder returned an incomplete bundle")
	}
	if got := embeddingRequests.Load(); got != 3 {
		t.Fatalf("startup /embeddings probes=%d, want two trusted models plus one unknown-dimension preflight", got)
	}
	if got := chatRequests.Load(); got != 0 {
		t.Fatalf("startup chat requests=%d, want zero", got)
	}

	catalog, err := bundle.Resolver.Catalog(context.Background(), "owner", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got := embeddingRequests.Load(); got != 3 {
		t.Fatalf("catalog GET caused network traffic: /embeddings probes=%d, want 3", got)
	}
	if len(catalog.Profiles) != 2 {
		t.Fatalf("catalog profiles=%+v, want only the two exact executable models", catalog.Profiles)
	}
	sort.Slice(catalog.Profiles, func(i, j int) bool {
		return catalog.Profiles[i].ModelName < catalog.Profiles[j].ModelName
	})
	for _, profile := range catalog.Profiles {
		if profile.ProviderID != providerInstanceID || profile.Dimension != 2048 ||
			profile.Availability != knowledge.ProfileAvailabilityConnected ||
			profile.Capability != config.LLMModelCapabilityEmbedding {
			t.Fatalf("cloud profile did not preserve its verified contract: %+v", profile)
		}
		snapshot, resolveErr := bundle.Resolver.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{
			Kind: knowledge.EmbeddingSelectionProfile, ProfileID: profile.ProfileID,
		})
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		wantNormalization := "l2"
		if snapshot.Normalization != wantNormalization {
			t.Fatalf("model %q normalization=%q, want %q", profile.ModelName, snapshot.Normalization, wantNormalization)
		}
	}
	batchProfile := catalog.Profiles[1]
	batchSnapshot, err := bundle.Resolver.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{
		Kind: knowledge.EmbeddingSelectionProfile, ProfileID: batchProfile.ProfileID,
	})
	if err != nil {
		t.Fatal(err)
	}
	batchExecutor, err := bundle.Registry.ExecutorForProfile(context.Background(), batchSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := batchExecutor.EmbedForPurpose(
		context.Background(), knowledge.EmbeddingPurposeDocument, []string{"one", "two", "three"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 3 || len(vectors[0]) != 2048 || embeddingRequests.Load() != 4 {
		t.Fatalf("cloud vectors=%dx%d requests=%d, want 3x2048 after three startup probes plus one batch", len(vectors), len(vectors[0]), embeddingRequests.Load())
	}

	// Apply/Resolve is the side-effecting freshness boundary. A failed real
	// probe must revoke the cached connected projection rather than trusting
	// configuration presence or silently falling through to another executor.
	target := catalog.Profiles[0]
	targetSnapshot, err := bundle.Resolver.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{
		Kind: knowledge.EmbeddingSelectionProfile, ProfileID: target.ProfileID,
	})
	if err != nil {
		t.Fatal(err)
	}
	targetExecutor, err := bundle.Registry.ExecutorForProfile(context.Background(), targetSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	failedModels[target.ModelName] = true
	mu.Unlock()
	bundle.Resolver.profiles[target.ProfileID].probeInterval = 0
	_, err = bundle.Resolver.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{
		Kind: knowledge.EmbeddingSelectionProfile, ProfileID: target.ProfileID,
	})
	if !errors.Is(err, knowledge.ErrProfileUnavailable) {
		t.Fatalf("failed Apply probe error=%v, want ErrProfileUnavailable", err)
	}
	if got := embeddingRequests.Load(); got != 5 {
		t.Fatalf("Apply /embeddings requests=%d, want startup 3 plus batch 1 plus exact profile probe 1", got)
	}
	afterFailure, err := bundle.Resolver.Catalog(context.Background(), "owner", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got := embeddingRequests.Load(); got != 5 {
		t.Fatalf("catalog after Apply caused network traffic: requests=%d, want 5", got)
	}
	targetReadiness, ok := targetExecutor.(knowledge.ProfileEmbeddingExecutorReadiness)
	if !ok {
		t.Fatal("profile executor does not expose readiness")
	}
	if targetReadiness.EmbeddingReady(context.Background()) {
		t.Fatal("failed stale probe left the active profile data plane ready")
	}
	for attempt := 0; attempt < 3; attempt++ {
		_, embedErr := targetExecutor.EmbedForPurpose(
			context.Background(), knowledge.EmbeddingPurposeQuery, []string{"must stay lexical"},
		)
		if !errors.Is(embedErr, knowledge.ErrEmbeddingUnavailable) {
			t.Fatalf("unavailable data attempt %d error=%v, want ErrEmbeddingUnavailable", attempt, embedErr)
		}
	}
	if got := embeddingRequests.Load(); got != 5 {
		t.Fatalf("unavailable active executor made data/probe calls inside TTL: requests=%d, want 5", got)
	}
	for _, profile := range afterFailure.Profiles {
		if profile.ProfileID == target.ProfileID && profile.Availability != knowledge.ProfileAvailabilityUnavailable {
			t.Fatalf("failed real probe left profile connected: %+v", profile)
		}
	}
	if got := chatRequests.Load(); got != 0 {
		t.Fatalf("semantic runtime chat requests=%d, want zero", got)
	}

	// Renaming the editable provider map key changes only display metadata; the
	// persisted opaque provider identity keeps every logical profile ID stable.
	renamed := *cfg
	renamed.LLM = cfg.LLM
	renamed.LLM.Providers = map[string]config.LLMProviderConfig{
		"Renamed OpenRouter": cfg.LLM.Providers["OpenRouter Primary"],
	}
	renamed.Knowledge.Embedding.Provider = "Renamed OpenRouter"
	renamedBundle := buildKnowledgeEmbeddingRuntimeProfiles(context.Background(), &renamed, &egress.Policy{}, newKnowledgeSemanticRuntimeGate())
	renamedCatalog, err := renamedBundle.Resolver.Catalog(context.Background(), "owner", "default")
	if err != nil {
		t.Fatal(err)
	}
	originalIDs := make(map[string]string, len(catalog.Profiles))
	for _, profile := range catalog.Profiles {
		originalIDs[profile.ModelName] = profile.ProfileID
	}
	for _, profile := range renamedCatalog.Profiles {
		if profile.ProviderID != providerInstanceID || profile.ProfileID != originalIDs[profile.ModelName] {
			t.Fatalf("provider rename changed stable identity: %+v", profile)
		}
	}
}

func TestKnowledgeEmbeddingRuntimeProfilesUnavailableDataGateRecoversThroughControlledProbe(t *testing.T) {
	var healthy atomic.Bool
	var probeRequests atomic.Int64
	var dataRequests atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/embeddings") {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		isProbe := len(request.Input) == 1 && request.Input[0] == knowledgeEmbeddingProbeText
		if isProbe {
			probeRequests.Add(1)
		} else {
			dataRequests.Add(1)
		}
		dimension := 2
		if healthy.Load() {
			dimension = 3
		}
		vector := make([]float32, dimension)
		vector[0] = 1
		data := make([]map[string]any, len(request.Input))
		for index := range data {
			data[index] = map[string]any{"object": "embedding", "index": index, "embedding": vector}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	defer provider.Close()

	const modelID = "vendor/readiness-gated-embedding-v1"
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"Readiness Provider": {
			ProviderInstanceID: "pvd_v1_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			APIKey:             "test-secret",
			BaseURL:            provider.URL + "/v1",
			Locality:           config.ProviderLocalityCloud,
			ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
			Models:             []string{modelID},
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID: modelID, Capabilities: []string{config.LLMModelCapabilityEmbedding},
				Embedding: &config.LLMEmbeddingModelSpec{
					Protocol: config.LLMEmbeddingProtocolOpenAI, Dimension: 3, Normalization: "l2",
				},
			}},
		},
	}
	bundle := buildKnowledgeEmbeddingRuntimeProfiles(
		context.Background(), cfg, &egress.Policy{}, newKnowledgeSemanticRuntimeGate(),
		withKnowledgeEmbeddingProbeInterval(0),
	)
	if probeRequests.Load() != 1 || dataRequests.Load() != 0 {
		t.Fatalf("startup requests: probes=%d data=%d, want 1/0", probeRequests.Load(), dataRequests.Load())
	}
	if len(bundle.Resolver.profileOrder) != 1 {
		t.Fatalf("profile order=%v, want one unavailable known-dimension profile", bundle.Resolver.profileOrder)
	}
	state := bundle.Resolver.profiles[bundle.Resolver.profileOrder[0]]
	activeSnapshot := state.cachedSnapshot()
	activeSnapshot.Profile.Availability = knowledge.ProfileAvailabilityConnected
	executor, err := bundle.Registry.ExecutorForProfile(context.Background(), activeSnapshot)
	if err != nil {
		t.Fatal(err)
	}

	healthy.Store(true)
	readiness, ok := executor.(knowledge.ProfileEmbeddingExecutorReadiness)
	if !ok {
		t.Fatal("profile executor does not expose readiness")
	}
	if !readiness.EmbeddingReady(context.Background()) {
		t.Fatal("controlled recovery probe did not reopen data plane")
	}
	if probeRequests.Load() != 2 || dataRequests.Load() != 0 {
		t.Fatalf("recovery readiness requests: probes=%d data=%d, want 2/0", probeRequests.Load(), dataRequests.Load())
	}
	vectors, err := executor.EmbedForPurpose(
		context.Background(), knowledge.EmbeddingPurposeDocument, []string{"recovered"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 1 || len(vectors[0]) != 3 || probeRequests.Load() != 2 || dataRequests.Load() != 1 {
		t.Fatalf("recovered vectors=%v probes=%d data=%d", vectors, probeRequests.Load(), dataRequests.Load())
	}
}

func TestKnowledgeEmbeddingRuntimeProfilesPropagatesNoneNormalization(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/embeddings") {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(request.Input))
		for index := range data {
			data[index] = map[string]any{
				"object": "embedding", "index": index, "embedding": []float32{3, 4, 0},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	defer provider.Close()

	const modelID = "vendor/raw-embedding-v1"
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"Raw Embeddings": {
			ProviderInstanceID: "pvd_v1_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			APIKey:             "test-secret",
			BaseURL:            provider.URL + "/v1",
			Locality:           config.ProviderLocalityCloud,
			ModelSpecsMode:     config.LLMModelSpecsModeExplicit,
			Models:             []string{modelID},
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID: modelID, Capabilities: []string{config.LLMModelCapabilityEmbedding},
				Embedding: &config.LLMEmbeddingModelSpec{
					Protocol: config.LLMEmbeddingProtocolOpenAI, Dimension: 3, Normalization: "none",
				},
			}},
		},
	}
	bundle := buildKnowledgeEmbeddingRuntimeProfiles(
		context.Background(), cfg, &egress.Policy{}, newKnowledgeSemanticRuntimeGate(),
	)
	catalog, err := bundle.Resolver.Catalog(context.Background(), "owner", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Profiles) != 1 {
		t.Fatalf("catalog=%+v, want one explicit raw-vector profile", catalog.Profiles)
	}
	snapshot, err := bundle.Resolver.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{
		Kind: knowledge.EmbeddingSelectionProfile, ProfileID: catalog.Profiles[0].ProfileID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Normalization != "none" {
		t.Fatalf("normalization=%q, want none", snapshot.Normalization)
	}
	executor, err := bundle.Registry.ExecutorForProfile(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := executor.EmbedForPurpose(context.Background(), knowledge.EmbeddingPurposeDocument, []string{"raw"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 1 || len(vectors[0]) != 3 || vectors[0][0] != 3 || vectors[0][1] != 4 {
		t.Fatalf("normalization=none vector=%v, want provider vector [3 4 0] unchanged", vectors)
	}
}

func TestKnowledgeEmbeddingRuntimeProfilesKeepsLegacyLocalNomicExecutable(t *testing.T) {
	var embeddingRequests atomic.Int64
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"nomic-embed-text:latest"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/embeddings":
			embeddingRequests.Add(1)
			var request struct {
				Input []string `json:"input"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			vector := make([]float32, 768)
			vector[0] = 1
			data := make([]map[string]any, len(request.Input))
			for i := range data {
				data[i] = map[string]any{"object": "embedding", "index": i, "embedding": vector}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list", "model": "nomic-embed-text:latest",
				"data": data,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ollama.Close()

	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "Local Ollama"
	cfg.Knowledge.Embedding.Model = "nomic-embed-text:latest"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"Local Ollama": {
			BaseURL: ollama.URL + "/v1", Locality: config.ProviderLocalityLocal,
		},
	}
	bundle := buildKnowledgeEmbeddingRuntimeProfiles(
		context.Background(), cfg, &egress.Policy{}, newKnowledgeSemanticRuntimeGate(),
	)
	catalog, err := bundle.Resolver.Catalog(context.Background(), "owner", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Profiles) != 1 || catalog.Profiles[0].ModelName != "nomic-embed-text:latest" ||
		catalog.Profiles[0].Dimension != 768 ||
		catalog.Profiles[0].Availability != knowledge.ProfileAvailabilityInstalled {
		t.Fatalf("legacy local Nomic catalog=%+v", catalog.Profiles)
	}
	if embeddingRequests.Load() != 0 {
		t.Fatal("local catalog discovery unexpectedly embedded user data")
	}
	snapshot, err := bundle.Resolver.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{
		Kind: knowledge.EmbeddingSelectionProfile, ProfileID: catalog.Profiles[0].ProfileID,
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := bundle.Registry.ExecutorForProfile(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := executor.EmbedForPurpose(context.Background(), knowledge.EmbeddingPurposeDocument, []string{"one", "two", "three"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 3 || len(vectors[0]) != 768 || embeddingRequests.Load() != 1 {
		t.Fatalf("local Nomic vectors=%dx%d requests=%d, want 3x768 in one batch", len(vectors), len(vectors[0]), embeddingRequests.Load())
	}
}

func TestKnowledgeEmbeddingRuntimeProfilesDiscoversExplicitUnknownDimensionByRealPreflight(t *testing.T) {
	var embeddingRequests atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/embeddings") {
			http.NotFound(w, r)
			return
		}
		embeddingRequests.Add(1)
		var request struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		vector := make([]float32, 37)
		vector[0] = 1
		data := make([]map[string]any, len(request.Input))
		for i := range data {
			data[i] = map[string]any{"object": "embedding", "index": i, "embedding": vector}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list", "model": request.Model, "data": data,
		})
	}))
	defer provider.Close()

	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "Custom Embedding"
	cfg.Knowledge.Embedding.Model = "vendor/exact-embed-v7"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"Custom Embedding": {
			ProviderInstanceID: "pvd_v1_fedcba9876543210fedcba9876543210",
			APIKey:             "test-secret", BaseURL: provider.URL + "/v1",
			Locality: config.ProviderLocalityCloud,
			Models:   []string{"vendor/exact-embed-v7"}, ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID: "vendor/exact-embed-v7", Capabilities: []string{config.LLMModelCapabilityEmbedding},
			}},
		},
	}
	bundle := buildKnowledgeEmbeddingRuntimeProfiles(
		context.Background(), cfg, &egress.Policy{}, newKnowledgeSemanticRuntimeGate(),
	)
	if got := embeddingRequests.Load(); got != 1 {
		t.Fatalf("unknown-dimension startup preflights=%d, want exactly 1", got)
	}
	catalog, err := bundle.Resolver.Catalog(context.Background(), "owner", "default")
	if err != nil {
		t.Fatal(err)
	}
	if got := embeddingRequests.Load(); got != 1 {
		t.Fatalf("catalog retried unknown-dimension preflight: requests=%d", got)
	}
	if len(catalog.Profiles) != 1 || catalog.Profiles[0].Dimension != 37 ||
		catalog.Profiles[0].Availability != knowledge.ProfileAvailabilityConnected {
		t.Fatalf("discovered profile=%+v, want exact 37-dimensional connected contract", catalog.Profiles)
	}
	snapshot, err := bundle.Resolver.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{
		Kind: knowledge.EmbeddingSelectionProfile, ProfileID: catalog.Profiles[0].ProfileID,
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := bundle.Registry.ExecutorForProfile(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := executor.EmbedForPurpose(
		context.Background(), knowledge.EmbeddingPurposeDocument, []string{"one", "two", "three"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 3 || len(vectors[0]) != 37 || embeddingRequests.Load() != 2 {
		t.Fatalf("discovered executor vectors=%dx%d requests=%d, want 3x37 after one preflight plus one batch", len(vectors), len(vectors[0]), embeddingRequests.Load())
	}
}
