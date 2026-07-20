package knowledge

import (
	"context"
	"errors"
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

type requestKeyRecordingExecutor struct {
	dimension int
	failAt    int
	calls     int
	keys      []string
	found     []bool
	purposes  []EmbeddingPurpose
}

func (e *requestKeyRecordingExecutor) EmbedForPurpose(
	ctx context.Context,
	purpose EmbeddingPurpose,
	texts []string,
) ([][]float32, error) {
	e.calls++
	key, ok := EmbeddingBatchClientRequestKeyFromContext(ctx)
	e.keys = append(e.keys, key)
	e.found = append(e.found, ok)
	e.purposes = append(e.purposes, purpose)
	if e.failAt > 0 && e.calls == e.failAt {
		return nil, errors.New("scripted provider interruption")
	}
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = make([]float32, e.dimension)
		vectors[i][0] = 1
	}
	return vectors, nil
}

// canonicalManifestKeyRepository models a repository that canonicalizes the
// persisted provider key before returning the authoritative manifest. The
// worker must use the returned value, never its pre-persistence input copy.
type canonicalManifestKeyRepository struct {
	SemanticIndexWorkerRepository
}

type openAICompatibleRequestKeyExecutor struct {
	embedder hexagon.VectorEmbedder
}

func (e *openAICompatibleRequestKeyExecutor) EmbedForPurpose(
	ctx context.Context,
	purpose EmbeddingPurpose,
	texts []string,
) ([][]float32, error) {
	if purpose != EmbeddingPurposeDocument {
		return nil, errors.New("unexpected non-document embedding purpose")
	}
	return e.embedder.Embed(ctx, texts)
}

func (r *canonicalManifestKeyRepository) CreateEmbeddingBatchManifest(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	manifest EmbeddingBatchManifest,
) (EmbeddingBatchManifest, error) {
	stored, err := r.SemanticIndexWorkerRepository.CreateEmbeddingBatchManifest(ctx, lease, now, manifest)
	if err != nil {
		return EmbeddingBatchManifest{}, err
	}
	stored.ClientRequestKey = "persisted-" + stored.ChunkIDsDigest
	return stored, nil
}

func TestSemanticIndexWorkerBindsStablePersistedClientRequestKeyPerBatch(t *testing.T) {
	h := newWorkerHarness(t, "alpha chunk", "beta chunk")
	now := time.Unix(1_800_212_000, 0).UTC()
	repository := &canonicalManifestKeyRepository{SemanticIndexWorkerRepository: h.repo}

	interrupted := &requestKeyRecordingExecutor{dimension: 3, failAt: 1}
	first := NewSemanticIndexWorker(repository, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": interrupted},
	}, workerConfig(&now, "worker-before-provider-retry", 1))
	processed, err := first.RunOnce(h.ctx)
	if !processed || err == nil {
		t.Fatalf("first RunOnce: processed=%v err=%v, want retryable provider error", processed, err)
	}
	if len(interrupted.keys) != 1 || !interrupted.found[0] || interrupted.keys[0] == "" {
		t.Fatalf("first provider context key=%q found=%v, want non-empty", interrupted.keys, interrupted.found)
	}
	if !strings.HasPrefix(interrupted.keys[0], "persisted-") {
		t.Fatalf("worker used pre-persistence manifest key %q", interrupted.keys[0])
	}

	now = now.Add(30 * time.Second)
	restarted := &requestKeyRecordingExecutor{dimension: 3}
	second := NewSemanticIndexWorker(repository, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": restarted},
	}, workerConfig(&now, "worker-after-provider-retry", 1))
	processed, err = second.RunOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("restart RunOnce: processed=%v err=%v", processed, err)
	}
	if len(restarted.keys) != 2 || !restarted.found[0] || !restarted.found[1] {
		t.Fatalf("restart provider keys=%q found=%v, want two present keys", restarted.keys, restarted.found)
	}
	if interrupted.keys[0] != restarted.keys[0] {
		t.Fatalf("same batch changed key across retry/restart: first=%q retry=%q",
			interrupted.keys[0], restarted.keys[0])
	}
	if restarted.keys[0] == restarted.keys[1] {
		t.Fatalf("next batch reused provider key %q", restarted.keys[0])
	}
	for i, purpose := range append(interrupted.purposes, restarted.purposes...) {
		if purpose != EmbeddingPurposeDocument {
			t.Fatalf("provider call %d purpose=%q, want document", i, purpose)
		}
	}
}

func TestSemanticIndexWorkerForwardsPersistedClientRequestKeyToProviderHTTPRetries(t *testing.T) {
	requestKeys := make(chan string, 3)
	var attempts atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestKeys <- r.Header.Get("Idempotency-Key")
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1,0,0]}],"model":"bge-m3","usage":{"prompt_tokens":1,"total_tokens":1}}`))
	}))
	defer provider.Close()

	providerClient, err := egress.NewProviderHTTPClient(
		provider.URL+"/v1", config.ProviderPrivateNetworkAccess{},
	)
	if err != nil {
		t.Fatal(err)
	}
	openAI := hexagon.NewOpenAI(
		"test-key",
		hexagon.OpenAIWithBaseURL(provider.URL+"/v1"),
		hexagon.OpenAIWithHTTPClient(providerClient),
	)
	embedder := hexagon.NewOpenAIEmbedder(
		openAI,
		hexagon.WithEmbedderModel("bge-m3"),
		hexagon.WithEmbedderDimension(3),
	)

	h := newWorkerHarness(t, "durable batch payload")
	now := time.Unix(1_800_213_000, 0).UTC()
	repository := &canonicalManifestKeyRepository{SemanticIndexWorkerRepository: h.repo}
	worker := NewSemanticIndexWorker(repository, &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{
			"profile-a": &openAICompatibleRequestKeyExecutor{embedder: embedder},
		},
	}, workerConfig(&now, "worker-provider-http", 1))
	processed, err := worker.RunOnce(h.ctx)
	if err != nil || !processed {
		t.Fatalf("RunOnce: processed=%v err=%v", processed, err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("provider HTTP attempts=%d, want one retry", got)
	}
	first, second := <-requestKeys, <-requestKeys
	if first == "" || !strings.HasPrefix(first, "persisted-") {
		t.Fatalf("first provider HTTP Idempotency-Key=%q, want persisted batch key", first)
	}
	if second != first {
		t.Fatalf("provider HTTP retry changed Idempotency-Key: first=%q second=%q", first, second)
	}
}

func TestRevisionSemanticQueryDoesNotCarryBatchClientRequestKey(t *testing.T) {
	h := newRevisionSearchHarness(t)
	if _, err := h.service.EnsureDefaultPolicy(h.ctx, "owner-1", "default"); err != nil {
		t.Fatal(err)
	}
	executor := &requestKeyRecordingExecutor{dimension: 3}
	searcher := NewSQLiteRevisionSemanticSearcher(h.db, "owner-1", "default", &workerExecutorRegistry{
		executors: map[string]ProfileEmbeddingExecutor{"profile-a": executor},
	})
	if _, routeRan, err := searcher.Search(h.ctx, "query without batch identity", 3, Filter{}); err != nil || !routeRan {
		t.Fatalf("Search: routeRan=%v err=%v", routeRan, err)
	}
	if len(executor.keys) != 1 || executor.found[0] || executor.keys[0] != "" {
		t.Fatalf("query context leaked batch client request key: keys=%q found=%v",
			executor.keys, executor.found)
	}
	if len(executor.purposes) != 1 || executor.purposes[0] != EmbeddingPurposeQuery {
		t.Fatalf("query purposes=%v", executor.purposes)
	}
}
