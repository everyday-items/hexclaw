package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

type recordingKnowledgeEmbedder struct {
	dimension int
	inputs    [][]string
}

func (e *recordingKnowledgeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	copied := append([]string(nil), texts...)
	e.inputs = append(e.inputs, copied)
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = make([]float32, e.dimension)
	}
	return vectors, nil
}

func (e *recordingKnowledgeEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (e *recordingKnowledgeEmbedder) Dimension() int { return e.dimension }

type blockingKnowledgeEmbedder struct {
	dimension int
	calls     atomic.Int64
	started   chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (e *blockingKnowledgeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.calls.Add(1)
	e.once.Do(func() { close(e.started) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.release:
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = make([]float32, e.dimension)
		}
		return vectors, nil
	}
}

func (e *blockingKnowledgeEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (e *blockingKnowledgeEmbedder) Dimension() int { return e.dimension }

type blockingReadyKnowledgeEmbedder struct {
	dimension    int
	readyStarted chan struct{}
	readyCancel  chan struct{}
	readyRelease chan struct{}
	startOnce    sync.Once
	cancelOnce   sync.Once
}

func (e *blockingReadyKnowledgeEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("unexpected embedding call")
}

func (e *blockingReadyKnowledgeEmbedder) EmbedOne(context.Context, string) ([]float32, error) {
	return nil, errors.New("unexpected embedding call")
}

func (e *blockingReadyKnowledgeEmbedder) Dimension() int { return e.dimension }

func (e *blockingReadyKnowledgeEmbedder) Ready(ctx context.Context) bool {
	e.startOnce.Do(func() { close(e.readyStarted) })
	<-ctx.Done()
	e.cancelOnce.Do(func() { close(e.readyCancel) })
	<-e.readyRelease
	return false
}

type cancellingSemanticWorkerRunner struct {
	calls  atomic.Int64
	cancel context.CancelFunc
}

type failingKnowledgeProfileResolver struct{ err error }

func (r failingKnowledgeProfileResolver) Resolve(
	context.Context,
	string,
	string,
	knowledge.EmbeddingSelection,
) (knowledge.EmbeddingProfileSnapshot, error) {
	return knowledge.EmbeddingProfileSnapshot{}, r.err
}

func (r failingKnowledgeProfileResolver) Catalog(
	context.Context,
	string,
	string,
) (knowledge.EmbeddingProfileCatalog, error) {
	return knowledge.EmbeddingProfileCatalog{}, nil
}

func (r *cancellingSemanticWorkerRunner) RunOnce(context.Context) (bool, error) {
	call := r.calls.Add(1)
	if call < 3 {
		return true, nil
	}
	r.cancel()
	return false, nil
}

func TestKnowledgeEmbeddingProfileResolverUsesOnlyExplicitEmbeddingCapability(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Knowledge.ChunkSize = 400
	cfg.Knowledge.ChunkOverlap = 80
	cfg.Knowledge.Embedding.Provider = "embedding-gateway"
	cfg.Knowledge.Embedding.Model = "text-embedding-3-small"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"embedding-gateway": {
			APIKey:     "sk-secret-must-not-leak",
			BaseURL:    "https://embedding.example.test/v1",
			Compatible: "openai",
			Model:      "chat-model-must-not-enter-catalog",
			Models:     []string{"chat-a", "chat-b"},
		},
		"chat-only": {APIKey: "sk-chat", Model: "chat-only-model"},
	}
	plan := knowledgeEmbeddingPlan{
		Provider: "embedding-gateway", Model: "text-embedding-3-small",
		Configured: true, Ready: true, ServiceAvailable: true,
	}
	resolver := newKnowledgeEmbeddingProfileResolver(cfg, plan, 1536)

	catalog, err := resolver.Catalog(context.Background(), "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Profiles) != 1 {
		t.Fatalf("catalog profiles = %+v, want only explicit embedding profile", catalog.Profiles)
	}
	profile := catalog.Profiles[0]
	if profile.ModelName != "text-embedding-3-small" || profile.Location != knowledge.ProviderLocationCloud ||
		profile.Availability != knowledge.ProfileAvailabilityConnected || profile.Dimension != 1536 {
		t.Fatalf("cloud profile = %+v", profile)
	}
	if profile.ProviderName != "OpenAI 兼容" {
		t.Fatalf("provider display name = %q", profile.ProviderName)
	}
	if profile.ModelName == "chat-model-must-not-enter-catalog" || profile.ModelName == "chat-only-model" {
		t.Fatalf("chat capability leaked into embedding catalog: %+v", profile)
	}

	snapshot, err := resolver.Resolve(context.Background(), "owner-1", "default", knowledge.EmbeddingSelection{Kind: knowledge.EmbeddingSelectionAuto})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Profile.ProfileID != profile.ProfileID || snapshot.ProfileConfigHash == "" || snapshot.ChunkConfigHash == "" {
		t.Fatalf("auto snapshot = %+v", snapshot)
	}
	if _, err := resolver.Resolve(context.Background(), "owner-1", "default", knowledge.EmbeddingSelection{
		Kind: knowledge.EmbeddingSelectionProfile, ProfileID: "unknown",
	}); !errors.Is(err, knowledge.ErrProfileUnavailable) {
		t.Fatalf("unknown profile error = %v", err)
	}
}

func TestKnowledgeEmbeddingProfileIdentitySeparatesSelectionFromVectorSpace(t *testing.T) {
	base := knowledgeEmbeddingProfileEntry{
		ProviderID: "pvd_v1_0123456789abcdef0123456789abcdef", ProviderName: "OpenRouter",
		ModelName: "nvidia/nemotron-3-embed-1b:free", Protocol: "openai_embeddings",
		BaseURL: "https://openrouter.ai/api/v1", Location: knowledge.ProviderLocationCloud,
		Capability: "embedding", Dimension: 2048,
		Availability:  knowledge.ProfileAvailabilityConnected,
		Normalization: "l2", QueryPrefix: "query: ", DocumentPrefix: "document: ",
	}
	snapshot := func(entry knowledgeEmbeddingProfileEntry) knowledge.EmbeddingProfileSnapshot {
		t.Helper()
		got, err := knowledgeEmbeddingProfileSnapshotForEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	original := snapshot(base)
	const wantProfileID = "ep_v1_grzWUZcoNrEqUKM_Do1QZJeb7yhACwn2NdNr2MTT_bY"
	const wantConfigHash = "929bbe571a14c3bafcb7ba3719fb6ce6eaeb0f11559120fb3b6462517192de7c"
	if original.Profile.ProfileID != wantProfileID {
		t.Fatalf("profile_id=%q, want JCS/SHA-256 golden %q", original.Profile.ProfileID, wantProfileID)
	}
	if original.ProfileConfigHash != wantConfigHash {
		t.Fatalf("profile_config_hash=%q, want JCS/SHA-256 golden %q", original.ProfileConfigHash, wantConfigHash)
	}

	rotated := base
	rotated.BaseURL = "https://gateway.example.test/openrouter/v1"
	rotated.Availability = knowledge.ProfileAvailabilityUnavailable
	rotatedSnapshot := snapshot(rotated)
	if rotatedSnapshot.Profile.ProfileID != original.Profile.ProfileID {
		t.Fatalf("profile_id changed across endpoint/availability rotation: original=%q rotated=%q", original.Profile.ProfileID, rotatedSnapshot.Profile.ProfileID)
	}
	if rotatedSnapshot.ProfileConfigHash == original.ProfileConfigHash {
		t.Fatal("endpoint rotation must change profile_config_hash")
	}

	for name, mutate := range map[string]func(*knowledgeEmbeddingProfileEntry){
		"provider instance": func(entry *knowledgeEmbeddingProfileEntry) {
			entry.ProviderID = "pvd_v1_fedcba9876543210fedcba9876543210"
		},
		"exact model": func(entry *knowledgeEmbeddingProfileEntry) {
			entry.ModelName = "nvidia/llama-nemotron-embed-vl-1b-v2:free"
		},
		"protocol": func(entry *knowledgeEmbeddingProfileEntry) {
			entry.Protocol = config.LLMEmbeddingProtocolOllama
		},
	} {
		t.Run("profile_identity_changes_"+name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if got := snapshot(changed).Profile.ProfileID; got == original.Profile.ProfileID {
				t.Fatalf("profile identity mutation %q left profile_id unchanged", name)
			}
		})
	}

	for name, mutate := range map[string]func(*knowledgeEmbeddingProfileEntry){
		"endpoint": func(entry *knowledgeEmbeddingProfileEntry) {
			entry.BaseURL = "https://gateway.example.test/openrouter/v1"
		},
		"provider location": func(entry *knowledgeEmbeddingProfileEntry) {
			entry.Location = knowledge.ProviderLocationLocal
		},
		"dimension": func(entry *knowledgeEmbeddingProfileEntry) { entry.Dimension = 3072 },
		"normalization": func(entry *knowledgeEmbeddingProfileEntry) {
			entry.Normalization = "none"
		},
		"query prefix": func(entry *knowledgeEmbeddingProfileEntry) {
			entry.QueryPrefix = "search_query: "
		},
		"document prefix": func(entry *knowledgeEmbeddingProfileEntry) {
			entry.DocumentPrefix = "search_document: "
		},
	} {
		t.Run("config_hash_changes_"+name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			changedSnapshot := snapshot(changed)
			if changedSnapshot.ProfileConfigHash == original.ProfileConfigHash {
				t.Fatalf("executor-contract mutation %q left config hash unchanged", name)
			}
			if changedSnapshot.Profile.ProfileID != original.Profile.ProfileID {
				t.Fatalf("executor-contract-only mutation %q changed logical profile identity", name)
			}
		})
	}

	metadataOnly := base
	metadataOnly.ProviderName = "Renamed Provider"
	metadataOnly.DisplayOrder = 999
	metadataOnly.Availability = knowledge.ProfileAvailabilityUnavailable
	metadataOnly.Configured = !base.Configured
	metadataSnapshot := snapshot(metadataOnly)
	if metadataSnapshot.Profile.ProfileID != original.Profile.ProfileID ||
		metadataSnapshot.ProfileConfigHash != original.ProfileConfigHash {
		t.Fatalf("display/dynamic metadata changed immutable identities: %+v", metadataSnapshot)
	}

	equivalentEndpoint := base
	equivalentEndpoint.BaseURL = " HTTPS://OPENROUTER.AI:443/api/v1/ "
	equivalentSnapshot := snapshot(equivalentEndpoint)
	if equivalentSnapshot.ProfileConfigHash != original.ProfileConfigHash {
		t.Fatalf("equivalent endpoint spelling changed config hash: original=%q equivalent=%q",
			original.ProfileConfigHash, equivalentSnapshot.ProfileConfigHash)
	}
}

func TestKnowledgeEmbeddingExecutorContractHashMutationMatrix(t *testing.T) {
	base := knowledgeEmbeddingExecutorContract{
		ContractVersion:    "embedding_executor_v1",
		Dimension:          2048,
		DocumentPrefix:     "document: ",
		EndpointIdentity:   "https://openrouter.ai/api/v1",
		ExactModelID:       "nvidia/nemotron-3-embed-1b:free",
		MaxInputRunes:      6000,
		Normalization:      "l2",
		Protocol:           "openai_embeddings",
		ProviderInstanceID: "pvd_v1_0123456789abcdef0123456789abcdef",
		ProviderLocation:   "cloud",
		QueryPrefix:        "query: ",
		TransformEpoch:     "v1",
	}
	want := "929bbe571a14c3bafcb7ba3719fb6ce6eaeb0f11559120fb3b6462517192de7c"
	got, err := knowledgeEmbeddingExecutorContractHash(base)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("contract hash=%q, want golden %q", got, want)
	}

	mutations := map[string]func(*knowledgeEmbeddingExecutorContract){
		"contract version":  func(contract *knowledgeEmbeddingExecutorContract) { contract.ContractVersion = "embedding_executor_v2" },
		"provider instance": func(contract *knowledgeEmbeddingExecutorContract) { contract.ProviderInstanceID += "-next" },
		"exact model":       func(contract *knowledgeEmbeddingExecutorContract) { contract.ExactModelID += ":next" },
		"protocol":          func(contract *knowledgeEmbeddingExecutorContract) { contract.Protocol = "ollama_embeddings" },
		"endpoint":          func(contract *knowledgeEmbeddingExecutorContract) { contract.EndpointIdentity += "/tenant" },
		"location":          func(contract *knowledgeEmbeddingExecutorContract) { contract.ProviderLocation = "local" },
		"dimension":         func(contract *knowledgeEmbeddingExecutorContract) { contract.Dimension++ },
		"normalization":     func(contract *knowledgeEmbeddingExecutorContract) { contract.Normalization = "none" },
		"query prefix":      func(contract *knowledgeEmbeddingExecutorContract) { contract.QueryPrefix += "next:" },
		"document prefix":   func(contract *knowledgeEmbeddingExecutorContract) { contract.DocumentPrefix += "next:" },
		"max input runes":   func(contract *knowledgeEmbeddingExecutorContract) { contract.MaxInputRunes++ },
		"transform epoch":   func(contract *knowledgeEmbeddingExecutorContract) { contract.TransformEpoch = "v2" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			changedHash, hashErr := knowledgeEmbeddingExecutorContractHash(changed)
			if hashErr != nil {
				t.Fatal(hashErr)
			}
			if changedHash == got {
				t.Fatalf("mutation %q was omitted from executor contract hash", name)
			}
		})
	}
}

func TestKnowledgeEmbeddingProfileResolverCatalogsMultipleProfilesDeterministically(t *testing.T) {
	entries := []knowledgeEmbeddingProfileEntry{
		{
			ProviderID: "openrouter-primary", ProviderName: "OpenRouter",
			ModelName: "nvidia/llama-nemotron-embed-vl-1b-v2:free", Protocol: "openai_embeddings",
			BaseURL: "https://openrouter.ai/api/v1", Location: knowledge.ProviderLocationCloud,
			Capability: "embedding", Dimension: 2048, Availability: knowledge.ProfileAvailabilityConnected,
			DisplayOrder: 30, Configured: true,
		},
		{
			ProviderID: "ollama", ProviderName: "Ollama", ModelName: "nomic-embed-text:latest",
			Protocol: "openai_embeddings", BaseURL: "http://127.0.0.1:11434/v1",
			Location: knowledge.ProviderLocationLocal, Capability: "embedding", Dimension: 768,
			Availability: knowledge.ProfileAvailabilityInstalled, DisplayOrder: 10, Configured: true,
			QueryPrefix: "search_query: ", DocumentPrefix: "search_document: ",
		},
		{
			ProviderID: "openrouter-primary", ProviderName: "OpenRouter",
			ModelName: "nvidia/nemotron-3-embed-1b:free", Protocol: "openai_embeddings",
			BaseURL: "https://openrouter.ai/api/v1", Location: knowledge.ProviderLocationCloud,
			Capability: "embedding", Dimension: 2048, Availability: knowledge.ProfileAvailabilityConnected,
			DisplayOrder: 20, Configured: true,
		},
		// Exact duplicate must not create a second option.
		{
			ProviderID: "openrouter-primary", ProviderName: "OpenRouter",
			ModelName: "nvidia/nemotron-3-embed-1b:free", Protocol: "openai_embeddings",
			BaseURL: "https://openrouter.ai/api/v1", Location: knowledge.ProviderLocationCloud,
			Capability: "embedding", Dimension: 2048, Availability: knowledge.ProfileAvailabilityConnected,
			DisplayOrder: 20, Configured: true,
		},
		// Chat-only and unknown-dimension entries are fail-closed.
		{ProviderID: "chat", ModelName: "gpt-chat", Protocol: "openai_chat", Location: knowledge.ProviderLocationCloud, Capability: "text", Dimension: 2048, Availability: knowledge.ProfileAvailabilityConnected},
		{ProviderID: "unknown", ModelName: "vendor/unknown-embed", Protocol: "openai_embeddings", Location: knowledge.ProviderLocationCloud, Capability: "embedding", Dimension: 0, Availability: knowledge.ProfileAvailabilityConnected},
	}

	resolver := newKnowledgeEmbeddingProfileResolverFromEntries(entries)
	catalog, err := resolver.Catalog(context.Background(), "owner", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Profiles) != 3 {
		t.Fatalf("catalog profiles=%+v, want exact three executable profiles", catalog.Profiles)
	}
	wantModels := []string{
		"nomic-embed-text:latest",
		"nvidia/nemotron-3-embed-1b:free",
		"nvidia/llama-nemotron-embed-vl-1b-v2:free",
	}
	for i, want := range wantModels {
		if got := catalog.Profiles[i].ModelName; got != want {
			t.Fatalf("catalog model[%d]=%q, want %q", i, got, want)
		}
	}

	reversed := append([]knowledgeEmbeddingProfileEntry(nil), entries...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	reorderedCatalog, err := newKnowledgeEmbeddingProfileResolverFromEntries(reversed).Catalog(context.Background(), "owner", "default")
	if err != nil {
		t.Fatal(err)
	}
	if reorderedCatalog.Version != catalog.Version {
		t.Fatalf("catalog version depends on entry order: first=%d reversed=%d", catalog.Version, reorderedCatalog.Version)
	}
	for i := range catalog.Profiles {
		if reorderedCatalog.Profiles[i].ProfileID != catalog.Profiles[i].ProfileID {
			t.Fatalf("catalog order/profile ID drift at %d: first=%+v reversed=%+v", i, catalog.Profiles, reorderedCatalog.Profiles)
		}
	}

	auto, err := resolver.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{Kind: knowledge.EmbeddingSelectionAuto})
	if err != nil {
		t.Fatal(err)
	}
	if auto.Profile.ModelName != "nomic-embed-text:latest" {
		t.Fatalf("auto model=%q, want first executable profile", auto.Profile.ModelName)
	}
	fixedID := catalog.Profiles[2].ProfileID
	fixed, err := resolver.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{Kind: knowledge.EmbeddingSelectionProfile, ProfileID: fixedID})
	if err != nil {
		t.Fatal(err)
	}
	if fixed.Profile.ModelName != "nvidia/llama-nemotron-embed-vl-1b-v2:free" {
		t.Fatalf("fixed model=%q", fixed.Profile.ModelName)
	}
	if _, err := resolver.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{Kind: knowledge.EmbeddingSelectionProfile, ProfileID: "unknown"}); !errors.Is(err, knowledge.ErrProfileUnavailable) {
		t.Fatalf("unknown profile error=%v, want ErrProfileUnavailable", err)
	}
}

func TestKnowledgeEmbeddingProfileCatalogDoesNotWaitForResolveProbe(t *testing.T) {
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var probeCalls atomic.Int64
	resolver := newKnowledgeEmbeddingProfileResolverFromEntries([]knowledgeEmbeddingProfileEntry{{
		ProviderID: "pvd_v1_0123456789abcdef0123456789abcdef", ProviderName: "OpenRouter",
		ModelName: config.OpenRouterNemotronEmbedFreeModelID, Protocol: config.LLMEmbeddingProtocolOpenAI,
		BaseURL: "https://openrouter.ai/api/v1", Location: knowledge.ProviderLocationCloud,
		Capability: config.LLMModelCapabilityEmbedding, Dimension: 2048,
		Availability: knowledge.ProfileAvailabilityConnected, Configured: true, ProbeInterval: 0,
		ProbeAvailability: func(context.Context) knowledge.ProfileAvailability {
			probeCalls.Add(1)
			close(probeStarted)
			<-releaseProbe
			return knowledge.ProfileAvailabilityUnavailable
		},
	}})
	initial, err := resolver.Catalog(context.Background(), "owner", "default")
	if err != nil || len(initial.Profiles) != 1 {
		t.Fatalf("initial catalog=%+v err=%v", initial, err)
	}
	resolver.invalidateAvailability()

	resolveDone := make(chan error, 1)
	go func() {
		_, resolveErr := resolver.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{
			Kind: knowledge.EmbeddingSelectionProfile, ProfileID: initial.Profiles[0].ProfileID,
		})
		resolveDone <- resolveErr
	}()
	<-probeStarted

	catalogDone := make(chan knowledge.EmbeddingProfileCatalog, 1)
	go func() {
		catalog, catalogErr := resolver.Catalog(context.Background(), "owner", "default")
		if catalogErr == nil {
			catalogDone <- catalog
		}
	}()
	select {
	case catalog := <-catalogDone:
		if len(catalog.Profiles) != 1 || catalog.Profiles[0].Availability != knowledge.ProfileAvailabilityConnected {
			t.Fatalf("in-flight probe changed cached catalog early: %+v", catalog.Profiles)
		}
		if got := probeCalls.Load(); got != 1 {
			t.Fatalf("catalog started a second provider probe: calls=%d", got)
		}
	case <-time.After(250 * time.Millisecond):
		close(releaseProbe)
		<-resolveDone
		t.Fatal("catalog blocked behind an in-flight provider probe")
	}

	close(releaseProbe)
	if resolveErr := <-resolveDone; !errors.Is(resolveErr, knowledge.ErrProfileUnavailable) {
		t.Fatalf("resolve error=%v, want unavailable after failed probe", resolveErr)
	}
	after, err := resolver.Catalog(context.Background(), "owner", "default")
	if err != nil || after.Profiles[0].Availability != knowledge.ProfileAvailabilityUnavailable {
		t.Fatalf("catalog did not publish completed probe: %+v err=%v", after.Profiles, err)
	}
}

func TestKnowledgeEmbeddingProfileResolverProjectsOllamaInstallationState(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "Ollama (local)"
	cfg.Knowledge.Embedding.Model = "nomic-embed-text"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"Ollama (local)": {BaseURL: "http://127.0.0.1:11434/v1"},
	}

	for _, tt := range []struct {
		name         string
		ready        bool
		service      bool
		availability knowledge.ProfileAvailability
	}{
		{name: "installed", ready: true, service: true, availability: knowledge.ProfileAvailabilityInstalled},
		{name: "downloadable", ready: false, service: true, availability: knowledge.ProfileAvailabilityDownloadable},
		{name: "unavailable", ready: false, service: false, availability: knowledge.ProfileAvailabilityUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resolver := newKnowledgeEmbeddingProfileResolver(cfg, knowledgeEmbeddingPlan{
				Provider: "Ollama (local)", Model: "nomic-embed-text", Configured: true,
				Ollama: true, Ready: tt.ready, ServiceAvailable: tt.service,
			}, 768)
			resolver.probeAvailability = func(context.Context) knowledge.ProfileAvailability {
				return tt.availability
			}
			resolver.probeInterval = 0
			catalog, err := resolver.Catalog(context.Background(), "owner-1", "default")
			if err != nil {
				t.Fatal(err)
			}
			if len(catalog.Profiles) != 1 || catalog.Profiles[0].Location != knowledge.ProviderLocationLocal ||
				catalog.Profiles[0].Availability != tt.availability || catalog.Profiles[0].ProviderName != "Ollama" {
				t.Fatalf("local catalog = %+v", catalog)
			}
			_, resolveErr := resolver.Resolve(context.Background(), "owner-1", "default", knowledge.EmbeddingSelection{
				Kind: knowledge.EmbeddingSelectionProfile, ProfileID: catalog.Profiles[0].ProfileID,
			})
			if tt.availability == knowledge.ProfileAvailabilityInstalled {
				if resolveErr != nil {
					t.Fatalf("installed profile resolve error = %v", resolveErr)
				}
			} else if !errors.Is(resolveErr, knowledge.ErrProfileUnavailable) {
				t.Fatalf("non-executable profile resolve error = %v, want ErrProfileUnavailable", resolveErr)
			}
		})
	}
}

func TestKnowledgeEmbeddingProfileResolverRefreshesOllamaAvailability(t *testing.T) {
	var installed atomic.Bool
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if installed.Load() {
			_, _ = w.Write([]byte(`{"models":[{"name":"nomic-embed-text:latest"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer ollama.Close()

	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "ollama"
	cfg.Knowledge.Embedding.Model = "nomic-embed-text"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"ollama": {BaseURL: ollama.URL + "/v1"},
	}
	resolver := newKnowledgeEmbeddingProfileResolver(cfg, knowledgeEmbeddingPlan{
		Provider: "ollama", Model: "nomic-embed-text", Configured: true,
		Ollama: true, ServiceAvailable: true,
	}, 768)
	resolver.probeInterval = time.Hour

	before, err := resolver.Catalog(context.Background(), "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if before.Profiles[0].Availability != knowledge.ProfileAvailabilityDownloadable {
		t.Fatalf("before availability=%q", before.Profiles[0].Availability)
	}
	installed.Store(true)
	cached, err := resolver.Catalog(context.Background(), "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if cached.Profiles[0].Availability != knowledge.ProfileAvailabilityDownloadable {
		t.Fatalf("cached availability=%q, want pre-invalidation state", cached.Profiles[0].Availability)
	}
	resolver.invalidateAvailability()
	snapshot, err := resolver.Resolve(context.Background(), "owner-1", "default", knowledge.EmbeddingSelection{Kind: knowledge.EmbeddingSelectionAuto})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Profile.Availability != knowledge.ProfileAvailabilityInstalled {
		t.Fatalf("resolved availability=%q", snapshot.Profile.Availability)
	}
	after, err := resolver.Catalog(context.Background(), "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	if after.Profiles[0].Availability != knowledge.ProfileAvailabilityInstalled {
		t.Fatalf("after availability=%q", after.Profiles[0].Availability)
	}
	if after.Version == before.Version {
		t.Fatal("catalog version did not change with availability")
	}
}

func TestActivateInstalledKnowledgeSemanticIndexBootstrapsDeferredAutoPolicy(t *testing.T) {
	db, ctx := newKnowledgeSemanticRuntimeTestDB(t)
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "ollama"
	cfg.Knowledge.Embedding.Model = "nomic-embed-text"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"ollama": {BaseURL: "http://127.0.0.1:11434/v1"},
	}
	resolver := newKnowledgeEmbeddingProfileResolver(cfg, knowledgeEmbeddingPlan{
		Provider: "ollama", Model: "nomic-embed-text", Configured: true,
		Ollama: true, ServiceAvailable: true,
	}, 768)
	var installed atomic.Bool
	resolver.probeAvailability = func(context.Context) knowledge.ProfileAvailability {
		if installed.Load() {
			return knowledge.ProfileAvailabilityInstalled
		}
		return knowledge.ProfileAvailabilityDownloadable
	}
	resolver.probeInterval = time.Hour
	registry := newKnowledgeEmbeddingExecutorRegistry(
		resolver.snapshot, &recordingKnowledgeEmbedder{dimension: 768},
		"search_query: ", "search_document: ",
	)

	runtime, err := setupKnowledgeSemanticIndex(ctx, db, resolver, registry, "worker-test")
	if err != nil {
		t.Fatal(err)
	}
	before, err := runtime.Service.GetPolicy(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Selection.Kind != knowledge.EmbeddingSelectionDisabled || before.ActiveRevision != nil {
		t.Fatalf("before install policy = %+v, want text-only", before)
	}

	installed.Store(true)
	if err := activateInstalledKnowledgeSemanticIndex(ctx, runtime, resolver); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.Service.GetPolicy(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Selection.Kind != knowledge.EmbeddingSelectionAuto || after.ActiveRevision == nil ||
		after.ActiveRevision.State != knowledge.VectorIndexReady || after.DesiredRevision != nil {
		t.Fatalf("after install policy = %+v, want immediately active auto revision", after)
	}
}

func TestRetryInstalledKnowledgeSemanticIndexActivation(t *testing.T) {
	t.Run("retries transient profile visibility", func(t *testing.T) {
		attempts := 0
		err := retryInstalledKnowledgeSemanticIndexActivation(
			context.Background(), 4, 0,
			func(context.Context) error {
				attempts++
				if attempts < 3 {
					return knowledge.ErrProfileUnavailable
				}
				return nil
			},
		)
		if err != nil || attempts != 3 {
			t.Fatalf("activation err=%v attempts=%d, want success on attempt 3", err, attempts)
		}
	})

	t.Run("does not retry permanent activation failure", func(t *testing.T) {
		permanentErr := errors.New("database unavailable")
		attempts := 0
		err := retryInstalledKnowledgeSemanticIndexActivation(
			context.Background(), 4, 0,
			func(context.Context) error {
				attempts++
				return permanentErr
			},
		)
		if !errors.Is(err, permanentErr) || attempts != 1 {
			t.Fatalf("activation err=%v attempts=%d, want one permanent failure", err, attempts)
		}
	})

	t.Run("lifecycle cancellation stops retry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attempts := 0
		err := retryInstalledKnowledgeSemanticIndexActivation(ctx, 4, time.Hour, func(context.Context) error {
			attempts++
			cancel()
			return knowledge.ErrProfileUnavailable
		})
		if !errors.Is(err, context.Canceled) || attempts != 1 {
			t.Fatalf("activation err=%v attempts=%d, want immediate lifecycle cancellation", err, attempts)
		}
	})
}

func TestKnowledgeOllamaInstallLifecycleContracts(t *testing.T) {
	if knowledgeOllamaInstallTimeout != 4*time.Hour {
		t.Fatalf("automatic Ollama install timeout = %s, want 4h", knowledgeOllamaInstallTimeout)
	}
	for _, tt := range []struct {
		installed  string
		configured string
		want       bool
	}{
		{installed: "nomic-embed-text:latest", configured: "nomic-embed-text", want: true},
		{installed: "NOMIC-EMBED-TEXT", configured: "nomic-embed-text:latest", want: false},
		{installed: "nomic-embed-text:v1.5", configured: "nomic-embed-text", want: false},
		{installed: "nomic-embed-text:latest", configured: "nomic-embed-text:v1.5", want: false},
		{installed: "nomic-embed-text:v1.5", configured: "nomic-embed-text:v1.5", want: true},
		{installed: "qwen3:8b", configured: "nomic-embed-text", want: false},
		{installed: "", configured: "nomic-embed-text", want: false},
	} {
		if got := sameOllamaModel(tt.installed, tt.configured); got != tt.want {
			t.Fatalf("sameOllamaModel(%q, %q)=%t, want %t", tt.installed, tt.configured, got, tt.want)
		}
	}
}

func TestKnowledgeEmbeddingProfileSnapshotIncludesEffectiveNomicPrefixes(t *testing.T) {
	base := config.DefaultConfig()
	base.Knowledge.Embedding.Provider = "ollama"
	base.Knowledge.Embedding.Model = "nomic-embed-text"
	base.LLM.Providers = map[string]config.LLMProviderConfig{
		"ollama": {BaseURL: "http://127.0.0.1:11434/v1"},
	}
	plan := knowledgeEmbeddingPlan{
		Provider: "ollama", Model: "nomic-embed-text", Configured: true,
		Ollama: true, Ready: true, ServiceAvailable: true,
	}
	implicit := newKnowledgeEmbeddingProfileResolver(base, plan, 768).snapshot

	explicitCfg := *base
	explicitCfg.Knowledge.Embedding.QueryPrefix = "search_query: "
	explicitCfg.Knowledge.Embedding.DocPrefix = "search_document: "
	explicit := newKnowledgeEmbeddingProfileResolver(&explicitCfg, plan, 768).snapshot

	if implicit.ProfileConfigHash != explicit.ProfileConfigHash || implicit.ChunkConfigHash != explicit.ChunkConfigHash {
		t.Fatalf("implicit nomic defaults must be immutable snapshot facts: implicit=%+v explicit=%+v", implicit, explicit)
	}
}

func TestKnowledgeEmbeddingProfileSnapshotHashesOnlyExecutorSpaceTransforms(t *testing.T) {
	base := config.DefaultConfig()
	base.Knowledge.Embedding.Provider = "embedding-gateway"
	base.Knowledge.Embedding.Model = "text-embedding-3-small"
	base.LLM.Providers = map[string]config.LLMProviderConfig{
		"embedding-gateway": {BaseURL: "https://embedding.example.test/v1"},
	}
	plan := knowledgeEmbeddingPlan{
		Provider: "embedding-gateway", Model: "text-embedding-3-small",
		Configured: true, Ready: true, ServiceAvailable: true,
	}
	snapshot := func(cfg config.Config) knowledge.EmbeddingProfileSnapshot {
		return newKnowledgeEmbeddingProfileResolver(&cfg, plan, 1536).snapshot
	}

	original := snapshot(*base)
	materializedChunkConfig := *base
	materializedChunkConfig.Knowledge.Contextual = !base.Knowledge.Contextual
	materializedChunkConfig.Knowledge.ChunkSize = base.Knowledge.ChunkSize + 127
	materializedChunkConfig.Knowledge.ChunkOverlap = base.Knowledge.ChunkOverlap + 13
	materialized := snapshot(materializedChunkConfig)
	if materialized.ChunkConfigHash != original.ChunkConfigHash ||
		materialized.ProfileConfigHash != original.ProfileConfigHash {
		t.Fatalf("materialized chunk settings changed executor-space hashes: original=%+v changed=%+v", original, materialized)
	}

	documentPrefixConfig := *base
	documentPrefixConfig.Knowledge.Embedding.DocPrefix = "document: "
	documentPrefix := snapshot(documentPrefixConfig)
	if documentPrefix.ChunkConfigHash == original.ChunkConfigHash ||
		documentPrefix.ProfileConfigHash == original.ProfileConfigHash {
		t.Fatalf("document prefix must change document-transform and profile hashes: original=%+v changed=%+v", original, documentPrefix)
	}

	queryPrefixConfig := *base
	queryPrefixConfig.Knowledge.Embedding.QueryPrefix = "query: "
	queryPrefix := snapshot(queryPrefixConfig)
	if queryPrefix.ChunkConfigHash != original.ChunkConfigHash ||
		queryPrefix.ProfileConfigHash == original.ProfileConfigHash {
		t.Fatalf("query prefix must change only profile hash: original=%+v changed=%+v", original, queryPrefix)
	}
}

func TestKnowledgeEmbeddingDocumentTransformHashIncludesTruncationLimit(t *testing.T) {
	current := knowledgeEmbeddingDocumentTransformHash("v1", "document: ", knowledge.DefaultEmbedMaxRunes)
	changedEpoch := knowledgeEmbeddingDocumentTransformHash("v2", "document: ", knowledge.DefaultEmbedMaxRunes)
	changedLimit := knowledgeEmbeddingDocumentTransformHash("v1", "document: ", knowledge.DefaultEmbedMaxRunes+1)
	changedPrefix := knowledgeEmbeddingDocumentTransformHash("v1", "passage: ", knowledge.DefaultEmbedMaxRunes)
	if current == changedEpoch {
		t.Fatal("changing the provider-input transform epoch must change the document-transform hash")
	}
	if current == changedLimit {
		t.Fatal("changing the provider-input truncation limit must change the document-transform hash")
	}
	if current == changedPrefix {
		t.Fatal("changing the document prefix must change the document-transform hash")
	}
}

func TestKnowledgeEmbeddingExecutorRegistryAppliesPurposeExactlyOnce(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "ollama"
	cfg.Knowledge.Embedding.Model = "nomic-embed-text"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"ollama": {BaseURL: "http://127.0.0.1:11434/v1"},
	}
	plan := knowledgeEmbeddingPlan{
		Provider: "ollama", Model: "nomic-embed-text", Configured: true,
		Ollama: true, Ready: true, ServiceAvailable: true,
	}
	resolver := newKnowledgeEmbeddingProfileResolver(cfg, plan, 768)
	embedder := &recordingKnowledgeEmbedder{dimension: 768}
	queryPrefix, documentPrefix := knowledgeEmbeddingPrefixes(cfg, plan.Model)
	registry := newKnowledgeEmbeddingExecutorRegistry(
		resolver.snapshot, embedder, queryPrefix, documentPrefix,
	)
	executor, err := registry.ExecutorForProfile(context.Background(), resolver.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.EmbedForPurpose(context.Background(), knowledge.EmbeddingPurposeQuery, []string{"hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.EmbedForPurpose(context.Background(), knowledge.EmbeddingPurposeDocument, []string{"world"}); err != nil {
		t.Fatal(err)
	}
	if len(embedder.inputs) != 2 || len(embedder.inputs[0]) != 1 || len(embedder.inputs[1]) != 1 {
		t.Fatalf("embed calls = %#v", embedder.inputs)
	}
	if got := embedder.inputs[0][0]; got != "search_query: hello" {
		t.Fatalf("query input = %q", got)
	}
	if got := embedder.inputs[1][0]; got != "search_document: world" {
		t.Fatalf("document input = %q", got)
	}
}

func TestKnowledgeEmbeddingExecutorRegistryRejectsSnapshotDrift(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "cloud"
	cfg.Knowledge.Embedding.Model = "text-embedding-3-small"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud": {APIKey: "sk-test", BaseURL: "https://embedding.example.test/v1"},
	}
	plan := knowledgeEmbeddingPlan{
		Provider: "cloud", Model: "text-embedding-3-small", Configured: true,
		Ready: true, ServiceAvailable: true,
	}
	resolver := newKnowledgeEmbeddingProfileResolver(cfg, plan, 1536)
	registry := newKnowledgeEmbeddingExecutorRegistry(
		resolver.snapshot, &recordingKnowledgeEmbedder{dimension: 1536}, "", "",
	)
	drifted := resolver.snapshot
	drifted.ProfileConfigHash = "different-vector-space"
	if _, err := registry.ExecutorForProfile(context.Background(), drifted); !errors.Is(err, knowledge.ErrProfileUnavailable) {
		t.Fatalf("snapshot drift error = %v, want ErrProfileUnavailable", err)
	}
}

func TestKnowledgeEmbeddingExecutorRegistryRoutesByProfileConfigHash(t *testing.T) {
	entry := func(providerID, model, endpoint string, dimension int) knowledgeEmbeddingProfileEntry {
		return knowledgeEmbeddingProfileEntry{
			ProviderID: providerID, ProviderName: providerID, ModelName: model, Protocol: "openai_embeddings",
			BaseURL: endpoint, Location: knowledge.ProviderLocationCloud, Capability: "embedding",
			Dimension: dimension, Availability: knowledge.ProfileAvailabilityConnected, Configured: true,
		}
	}
	snapshot := func(profile knowledgeEmbeddingProfileEntry) knowledge.EmbeddingProfileSnapshot {
		t.Helper()
		got, err := knowledgeEmbeddingProfileSnapshotForEntry(profile)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := snapshot(entry("openrouter-primary", "nvidia/nemotron-3-embed-1b:free", "https://openrouter.ai/api/v1", 2048))
	rotated := snapshot(entry("openrouter-primary", "nvidia/nemotron-3-embed-1b:free", "https://gateway.example.test/v1", 2048))
	localEntry := entry("ollama", "nomic-embed-text:latest", "http://127.0.0.1:11434/v1", 768)
	localEntry.Location = knowledge.ProviderLocationLocal
	localEntry.Availability = knowledge.ProfileAvailabilityInstalled
	localEntry.QueryPrefix = "search_query: "
	localEntry.DocumentPrefix = "search_document: "
	local := snapshot(localEntry)

	firstEmbedder := &recordingKnowledgeEmbedder{dimension: 2048}
	rotatedEmbedder := &recordingKnowledgeEmbedder{dimension: 2048}
	localEmbedder := &recordingKnowledgeEmbedder{dimension: 768}
	registry := newKnowledgeEmbeddingExecutorRegistryFromEntries([]knowledgeEmbeddingExecutorEntry{
		{Snapshot: rotated, Embedder: rotatedEmbedder},
		{Snapshot: local, Embedder: localEmbedder, QueryPrefix: "search_query: ", DocumentPrefix: "search_document: "},
		{Snapshot: first, Embedder: firstEmbedder},
	})

	for _, tt := range []struct {
		name     string
		snapshot knowledge.EmbeddingProfileSnapshot
		embedder *recordingKnowledgeEmbedder
	}{
		{name: "original cloud", snapshot: first, embedder: firstEmbedder},
		{name: "rotated same profile", snapshot: rotated, embedder: rotatedEmbedder},
		{name: "local", snapshot: local, embedder: localEmbedder},
	} {
		t.Run(tt.name, func(t *testing.T) {
			executor, err := registry.ExecutorForProfile(context.Background(), tt.snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := executor.EmbedForPurpose(context.Background(), knowledge.EmbeddingPurposeQuery, []string{tt.name}); err != nil {
				t.Fatal(err)
			}
			if len(tt.embedder.inputs) != 1 {
				t.Fatalf("embedder calls=%d, want 1", len(tt.embedder.inputs))
			}
		})
	}
	missing := first
	missing.ProfileConfigHash = "not-registered"
	if _, err := registry.ExecutorForProfile(context.Background(), missing); !errors.Is(err, knowledge.ErrProfileUnavailable) {
		t.Fatalf("registry miss error=%v, want ErrProfileUnavailable", err)
	}
}

func TestKnowledgeEmbeddingRuntimeHolderReloadsProfilesAndFailsClosedOnDeletion(t *testing.T) {
	entry := func(providerID, model string, dimension int) knowledgeEmbeddingProfileEntry {
		return knowledgeEmbeddingProfileEntry{
			ProviderID: providerID, ProviderName: providerID, ModelName: model,
			Protocol: config.LLMEmbeddingProtocolOpenAI, BaseURL: "https://embedding.example.test/v1",
			Location: knowledge.ProviderLocationCloud, Capability: config.LLMModelCapabilityEmbedding,
			Dimension: dimension, Availability: knowledge.ProfileAvailabilityConnected, Configured: true,
		}
	}
	bundle := func(profile knowledgeEmbeddingProfileEntry, embedder *recordingKnowledgeEmbedder) knowledgeEmbeddingRuntimeProfiles {
		snapshot, err := knowledgeEmbeddingProfileSnapshotForEntry(profile)
		if err != nil {
			t.Fatal(err)
		}
		gate := newKnowledgeSemanticRuntimeGate()
		return knowledgeEmbeddingRuntimeProfiles{
			Resolver: newKnowledgeEmbeddingProfileResolverFromEntries([]knowledgeEmbeddingProfileEntry{profile}, gate),
			Registry: newKnowledgeEmbeddingExecutorRegistryFromEntries([]knowledgeEmbeddingExecutorEntry{{
				Snapshot: snapshot, Embedder: embedder,
			}}, gate),
		}
	}

	oldEntry := entry("pvd_v1_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "vendor/embed-v1", 3)
	oldBundle := bundle(oldEntry, &recordingKnowledgeEmbedder{dimension: 3})
	holder, err := newKnowledgeEmbeddingRuntimeHolder(oldBundle)
	if err != nil {
		t.Fatal(err)
	}
	oldCatalog, err := holder.Catalog(context.Background(), "owner", "default")
	if err != nil || len(oldCatalog.Profiles) != 1 {
		t.Fatalf("old catalog=%+v err=%v", oldCatalog, err)
	}
	oldSnapshot, err := holder.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{
		Kind: knowledge.EmbeddingSelectionProfile, ProfileID: oldCatalog.Profiles[0].ProfileID,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldExecutor, err := holder.ExecutorForProfile(context.Background(), oldSnapshot)
	if err != nil {
		t.Fatal(err)
	}

	if err := holder.Replace(context.Background(), knowledgeEmbeddingRuntimeProfiles{}); err == nil {
		t.Fatal("invalid replacement succeeded")
	}
	if _, err := holder.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{
		Kind: knowledge.EmbeddingSelectionProfile, ProfileID: oldCatalog.Profiles[0].ProfileID,
	}); err != nil {
		t.Fatalf("failed preflight replacement revoked old runtime: %v", err)
	}

	newEntry := entry("pvd_v1_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "vendor/embed-v2", 5)
	newBundle := bundle(newEntry, &recordingKnowledgeEmbedder{dimension: 5})
	if err := holder.Replace(context.Background(), newBundle); err != nil {
		t.Fatal(err)
	}
	if _, err := oldExecutor.EmbedForPurpose(context.Background(), knowledge.EmbeddingPurposeQuery, []string{"old"}); !errors.Is(err, knowledge.ErrProfileUnavailable) {
		t.Fatalf("previously issued old executor error=%v, want revoked", err)
	}
	newCatalog, err := holder.Catalog(context.Background(), "owner", "default")
	if err != nil || len(newCatalog.Profiles) != 1 || newCatalog.Profiles[0].ModelName != "vendor/embed-v2" {
		t.Fatalf("new catalog=%+v err=%v", newCatalog, err)
	}
	if _, err := holder.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{
		Kind: knowledge.EmbeddingSelectionProfile, ProfileID: oldCatalog.Profiles[0].ProfileID,
	}); !errors.Is(err, knowledge.ErrProfileUnavailable) {
		t.Fatalf("deleted old profile resolved after swap: %v", err)
	}

	db, runtimeCtx := newKnowledgeSemanticRuntimeTestDB(t)
	runtime, err := setupKnowledgeSemanticIndex(runtimeCtx, db, holder, holder, "reload-holder-worker")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := runtime.Service.GetPolicy(runtimeCtx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID)
	if err != nil || len(policy.AvailableProfiles) != 1 || policy.AvailableProfiles[0].ProfileID != newCatalog.Profiles[0].ProfileID {
		t.Fatalf("stable service did not expose replacement catalog: policy=%+v err=%v", policy, err)
	}
	if _, err := runtime.Service.ApplyPolicy(
		runtimeCtx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID, policy.PolicyVersion,
		knowledge.EmbeddingSelection{Kind: knowledge.EmbeddingSelectionProfile, ProfileID: newCatalog.Profiles[0].ProfileID},
	); err != nil {
		t.Fatalf("stable service could not Apply replacement profile: %v", err)
	}

	emptyGate := newKnowledgeSemanticRuntimeGate()
	emptyBundle := knowledgeEmbeddingRuntimeProfiles{
		Resolver: newKnowledgeEmbeddingProfileResolverFromEntries(nil, emptyGate),
		Registry: newKnowledgeEmbeddingExecutorRegistryFromEntries(nil, emptyGate),
	}
	if err := holder.Replace(context.Background(), emptyBundle); err != nil {
		t.Fatal(err)
	}
	emptyCatalog, err := holder.Catalog(context.Background(), "owner", "default")
	if err != nil || len(emptyCatalog.Profiles) != 0 {
		t.Fatalf("catalog after provider deletion=%+v err=%v", emptyCatalog, err)
	}
	deletedPolicy, err := runtime.Service.GetPolicy(runtimeCtx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID)
	if err != nil || len(deletedPolicy.AvailableProfiles) != 0 {
		t.Fatalf("stable service retained deleted catalog profiles: policy=%+v err=%v", deletedPolicy, err)
	}
	if _, err := holder.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{
		Kind: knowledge.EmbeddingSelectionProfile, ProfileID: newCatalog.Profiles[0].ProfileID,
	}); !errors.Is(err, knowledge.ErrProfileUnavailable) {
		t.Fatalf("deleted replacement profile resolved: %v", err)
	}
}

func TestKnowledgeSemanticRuntimeGateRevokesPreviouslyAcquiredExecutor(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "cloud-a"
	cfg.Knowledge.Embedding.Model = "text-embedding-3-small"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud-a": {APIKey: "sk-a", BaseURL: "https://a.example.test/v1"},
	}
	plan := knowledgeEmbeddingPlan{
		Provider: "cloud-a", Model: "text-embedding-3-small", Configured: true,
		Ready: true, ServiceAvailable: true,
	}
	gate := newKnowledgeSemanticRuntimeGate()
	resolver := newKnowledgeEmbeddingProfileResolver(cfg, plan, 1536, gate)
	recorder := &recordingKnowledgeEmbedder{dimension: 1536}
	registry := newKnowledgeEmbeddingExecutorRegistry(resolver.snapshot, recorder, "", "", gate)
	executor, err := registry.ExecutorForProfile(context.Background(), resolver.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.EmbedForPurpose(context.Background(), knowledge.EmbeddingPurposeQuery, []string{"before"}); err != nil {
		t.Fatal(err)
	}

	if err := gate.Revoke(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, err := executor.EmbedForPurpose(context.Background(), knowledge.EmbeddingPurposeQuery, []string{"after"}); !errors.Is(err, knowledge.ErrProfileUnavailable) {
		t.Fatalf("revoked executor error = %v, want ErrProfileUnavailable", err)
	}
	if len(recorder.inputs) != 1 {
		t.Fatalf("provider A calls after revoke = %d, want exactly the pre-revoke call", len(recorder.inputs))
	}
	if _, err := registry.ExecutorForProfile(context.Background(), resolver.snapshot); !errors.Is(err, knowledge.ErrProfileUnavailable) {
		t.Fatalf("revoked registry error = %v, want ErrProfileUnavailable", err)
	}
	if _, err := resolver.Resolve(context.Background(), "owner", "default", knowledge.EmbeddingSelection{Kind: knowledge.EmbeddingSelectionAuto}); !errors.Is(err, knowledge.ErrProfileUnavailable) {
		t.Fatalf("revoked resolver error = %v, want ErrProfileUnavailable", err)
	}
	if _, err := resolver.Catalog(context.Background(), "owner", "default"); !errors.Is(err, knowledge.ErrProfileUnavailable) {
		t.Fatalf("revoked catalog error = %v, want ErrProfileUnavailable", err)
	}
}

func TestKnowledgeSemanticRuntimeGateCancelsInflightWithoutBlockingRevoke(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "cloud-a"
	cfg.Knowledge.Embedding.Model = "text-embedding-3-small"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud-a": {APIKey: "sk-a", BaseURL: "https://a.example.test/v1"},
	}
	plan := knowledgeEmbeddingPlan{
		Provider: "cloud-a", Model: "text-embedding-3-small", Configured: true,
		Ready: true, ServiceAvailable: true,
	}
	gate := newKnowledgeSemanticRuntimeGate()
	resolver := newKnowledgeEmbeddingProfileResolver(cfg, plan, 1536, gate)
	blocking := &blockingKnowledgeEmbedder{
		dimension: 1536,
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	registry := newKnowledgeEmbeddingExecutorRegistry(resolver.snapshot, blocking, "", "", gate)
	executor, err := registry.ExecutorForProfile(context.Background(), resolver.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, callErr := executor.EmbedForPurpose(context.Background(), knowledge.EmbeddingPurposeDocument, []string{"in-flight"})
		result <- callErr
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("provider call did not start")
	}

	revoked := make(chan error, 1)
	go func() {
		revoked <- gate.Revoke(context.Background())
	}()
	select {
	case revokeErr := <-revoked:
		if revokeErr != nil {
			t.Fatal(revokeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("revoke blocked on an in-flight provider")
	}
	select {
	case callErr := <-result:
		if !errors.Is(callErr, context.Canceled) {
			t.Fatalf("in-flight error = %v, want context cancellation", callErr)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight provider did not observe runtime cancellation")
	}
	if got := blocking.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestKnowledgeSemanticRuntimeGateDoesNotReportDrainedUntilBoundCallExits(t *testing.T) {
	gate := newKnowledgeSemanticRuntimeGate()
	type requestKey struct{}
	requestCtx := context.WithValue(context.Background(), requestKey{}, "egress-and-idempotency-values")
	callCtx, release, err := gate.Bind(requestCtx)
	if err != nil {
		t.Fatal(err)
	}
	if got := callCtx.Value(requestKey{}); got != "egress-and-idempotency-values" {
		t.Fatalf("bound call lost request context value: %v", got)
	}
	revokeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := gate.Revoke(revokeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("revoke with a bound pre-provider call error = %v, want bounded timeout", err)
	}
	select {
	case <-callCtx.Done():
	default:
		t.Fatal("runtime revoke did not synchronously cancel the bound call context")
	}
	release()
	if err := gate.Revoke(context.Background()); err != nil {
		t.Fatalf("revoke after bound call exit: %v", err)
	}
}

func TestKnowledgeSemanticRuntimeGatePreservesRequestCancellation(t *testing.T) {
	gate := newKnowledgeSemanticRuntimeGate()
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	callCtx, release, err := gate.Bind(requestCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cancelRequest()
	select {
	case <-callCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("bound call did not preserve request cancellation")
	}
}

func TestKnowledgeSemanticRuntimeGateDrainsAvailabilityProbe(t *testing.T) {
	gate := newKnowledgeSemanticRuntimeGate()
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "ollama"
	cfg.Knowledge.Embedding.Model = "nomic-embed-text"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"ollama": {BaseURL: "http://127.0.0.1:11434/v1"},
	}
	resolver := newKnowledgeEmbeddingProfileResolver(cfg, knowledgeEmbeddingPlan{
		Provider: "ollama", Model: "nomic-embed-text", Configured: true,
		Ollama: true, ServiceAvailable: true,
	}, 768, gate)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	resolver.probeInterval = 0
	resolver.probeAvailability = func(ctx context.Context) knowledge.ProfileAvailability {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return knowledge.ProfileAvailabilityUnavailable
	}
	resolveDone := make(chan error, 1)
	go func() {
		_, err := resolver.Resolve(
			context.Background(), "owner", "default",
			knowledge.EmbeddingSelection{Kind: knowledge.EmbeddingSelectionAuto},
		)
		resolveDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("availability probe did not start")
	}
	revokeDone := make(chan error, 1)
	go func() { revokeDone <- gate.Revoke(context.Background()) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("availability probe did not observe runtime cancellation")
	}
	select {
	case err := <-revokeDone:
		t.Fatalf("revoke returned before availability probe exited: %v", err)
	default:
	}
	close(release)
	if err := <-revokeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-resolveDone; !errors.Is(err, context.Canceled) && !errors.Is(err, knowledge.ErrProfileUnavailable) {
		t.Fatalf("resolve error after runtime revoke = %v", err)
	}
}

func TestKnowledgeSemanticRuntimeGateDrainsEmbeddingReadinessProbe(t *testing.T) {
	gate := newKnowledgeSemanticRuntimeGate()
	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "ollama"
	cfg.Knowledge.Embedding.Model = "nomic-embed-text"
	plan := knowledgeEmbeddingPlan{
		Provider: "ollama", Model: "nomic-embed-text", Configured: true,
		Ollama: true, Ready: true, ServiceAvailable: true,
	}
	resolver := newKnowledgeEmbeddingProfileResolver(cfg, plan, 768, gate)
	blocking := &blockingReadyKnowledgeEmbedder{
		dimension: 768, readyStarted: make(chan struct{}),
		readyCancel: make(chan struct{}), readyRelease: make(chan struct{}),
	}
	registry := newKnowledgeEmbeddingExecutorRegistry(resolver.snapshot, blocking, "", "", gate)
	executor, err := registry.ExecutorForProfile(context.Background(), resolver.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	readyDone := make(chan bool, 1)
	go func() {
		readyDone <- executor.(knowledge.ProfileEmbeddingExecutorReadiness).EmbeddingReady(context.Background())
	}()
	select {
	case <-blocking.readyStarted:
	case <-time.After(time.Second):
		t.Fatal("embedding readiness probe did not start")
	}
	revokeDone := make(chan error, 1)
	go func() { revokeDone <- gate.Revoke(context.Background()) }()
	select {
	case <-blocking.readyCancel:
	case <-time.After(time.Second):
		t.Fatal("embedding readiness probe did not observe runtime cancellation")
	}
	select {
	case err := <-revokeDone:
		t.Fatalf("revoke returned before embedding readiness probe exited: %v", err)
	default:
	}
	close(blocking.readyRelease)
	if err := <-revokeDone; err != nil {
		t.Fatal(err)
	}
	if ready := <-readyDone; ready {
		t.Fatal("cancelled readiness probe reported ready")
	}
}

func newKnowledgeSemanticRuntimeTestDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "semantic-runtime.db") +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	store := knowledge.NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, db, []migrate.Migration{migrate.KnowledgeIndexV23}); err != nil {
		t.Fatal(err)
	}
	return db, ctx
}

func TestSetupKnowledgeSemanticIndexKeepsTextOnlyPolicyWhenProfileCannotExecute(t *testing.T) {
	db, ctx := newKnowledgeSemanticRuntimeTestDB(t)
	cfg := config.DefaultConfig()
	resolver := newKnowledgeEmbeddingProfileResolver(cfg, knowledgeEmbeddingPlan{}, 1536)
	registry := newKnowledgeEmbeddingExecutorRegistry(resolver.snapshot, nil, "", "")

	runtime, err := setupKnowledgeSemanticIndex(ctx, db, resolver, registry, "worker-test")
	if err != nil {
		t.Fatal(err)
	}
	projection, err := runtime.Service.GetPolicy(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Selection.Kind != knowledge.EmbeddingSelectionDisabled ||
		projection.ActiveRevision != nil || projection.DesiredRevision != nil {
		t.Fatalf("unavailable bootstrap = %+v, want durable text-only policy", projection)
	}
}

func TestSetupKnowledgeSemanticIndexNeverFallsBackToLegacyAfterBinding(t *testing.T) {
	db, ctx := newKnowledgeSemanticRuntimeTestDB(t)
	bootstrapErr := errors.New("injected policy bootstrap failure")
	registry := newKnowledgeEmbeddingExecutorRegistry(
		knowledge.EmbeddingProfileSnapshot{}, nil, "", "",
	)
	runtime, err := setupKnowledgeSemanticIndex(
		ctx, db, failingKnowledgeProfileResolver{err: bootstrapErr}, registry, "worker-test",
	)
	if runtime == nil || !errors.Is(err, bootstrapErr) {
		t.Fatalf("runtime=%+v err=%v, want bound semantic runtime plus bootstrap error", runtime, err)
	}

	store := knowledge.NewSQLiteStore(db,
		knowledge.WithSQLiteSemanticMutations(knowledgeDesktopOwnerID, knowledgeDefaultCorpusID))
	now := time.Unix(1_800_500_000, 0).UTC()
	doc := &knowledge.Document{
		ID: "doc-after-bootstrap-error", Title: "Safe fallback", Content: "text",
		Source: "manual", SourceType: "manual", ChunkCount: 1, Status: "indexed",
		CreatedAt: now, UpdatedAt: now,
	}
	chunks := []*knowledge.Chunk{{
		ID: "doc-after-bootstrap-error-chunk-0", DocID: doc.ID,
		Content: "text", Index: 0, CreatedAt: now,
	}}
	if err := store.Add(ctx, doc, chunks); err != nil {
		t.Fatalf("semantic-scoped text write after bootstrap error: %v", err)
	}
	if err := store.Delete(ctx, doc.ID); err != nil {
		t.Fatalf("semantic tombstone after bootstrap error: %v", err)
	}
	var deleted int
	if err := db.QueryRowContext(ctx, `SELECT deleted FROM kb_documents WHERE id=?`, doc.ID).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d, want semantic tombstone", deleted)
	}
}

func TestSetupKnowledgeSemanticIndexBackfillsLegacyCorpusThroughWorker(t *testing.T) {
	db, ctx := newKnowledgeSemanticRuntimeTestDB(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_documents
		(id,title,content,source,chunk_count,status,deleted,error_message,source_type)
		VALUES('doc-1','Legacy','legacy body','manual',1,'indexed',0,'','manual')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_chunks(id,doc_id,content,chunk_index)
		VALUES('chunk-1','doc-1','legacy body',0)`); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	cfg.Knowledge.Embedding.Provider = "cloud"
	cfg.Knowledge.Embedding.Model = "text-embedding-3-small"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud": {APIKey: "sk-test", BaseURL: "https://embedding.example.test/v1"},
	}
	plan := knowledgeEmbeddingPlan{
		Provider: "cloud", Model: "text-embedding-3-small", Configured: true,
		Ready: true, ServiceAvailable: true,
	}
	resolver := newKnowledgeEmbeddingProfileResolver(cfg, plan, 1536)
	registry := newKnowledgeEmbeddingExecutorRegistry(
		resolver.snapshot, &recordingKnowledgeEmbedder{dimension: 1536}, "", "",
	)

	runtime, err := setupKnowledgeSemanticIndex(ctx, db, resolver, registry, "worker-test")
	if err != nil {
		t.Fatal(err)
	}
	before, err := runtime.Service.GetPolicy(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ActiveRevision != nil || before.DesiredRevision == nil || before.DesiredRevision.JobID == nil {
		t.Fatalf("legacy bootstrap = %+v, want staged durable rebuild", before)
	}
	processed, err := runtime.Worker.RunOnce(ctx)
	if err != nil || !processed {
		t.Fatalf("worker RunOnce processed=%t err=%v", processed, err)
	}
	after, err := runtime.Service.GetPolicy(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ActiveRevision == nil || after.DesiredRevision != nil ||
		after.ActiveRevision.State != knowledge.VectorIndexReady {
		t.Fatalf("published projection = %+v", after)
	}
}

func TestRunKnowledgeSemanticIndexWorkerDrainsJobsAndStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &cancellingSemanticWorkerRunner{cancel: cancel}
	done := make(chan struct{})
	go func() {
		runKnowledgeSemanticIndexWorker(ctx, runner, 0, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("semantic worker loop did not stop with context")
	}
	if got := runner.calls.Load(); got != 3 {
		t.Fatalf("RunOnce calls=%d, want immediate drain before idle", got)
	}
}
