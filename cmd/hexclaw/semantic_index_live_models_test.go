package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

const (
	semanticLiveModelsGate         = "HEX_SEMANTIC_LIVE_MODELS"
	semanticLiveLocalCapacityGate  = "HEX_SEMANTIC_LIVE_LOCAL_CAPACITY"
	semanticLiveLocalRetrievalGate = "HEX_SEMANTIC_LIVE_LOCAL_RETRIEVAL"

	semanticLiveLocalProviderEnv = "HEX_SEMANTIC_LIVE_LOCAL_PROVIDER"
	semanticLiveLocalBaseURLEnv  = "HEX_SEMANTIC_LIVE_LOCAL_BASE_URL"
	semanticLiveLocalModelEnv    = "HEX_SEMANTIC_LIVE_LOCAL_MODEL"

	semanticLiveCloudProviderEnv = "HEX_SEMANTIC_LIVE_CLOUD_PROVIDER"
	semanticLiveCloudBaseURLEnv  = "HEX_SEMANTIC_LIVE_CLOUD_BASE_URL"
	semanticLiveCloudModelsEnv   = "HEX_SEMANTIC_LIVE_CLOUD_EMBEDDING_MODELS"

	semanticLiveDefaultLocalModel    = "qwen3-embedding:8b"
	semanticLiveDefaultCloudProvider = "OpenRouter"
	semanticLiveDefaultCloudBaseURL  = "https://openrouter.ai/api/v1"
	semanticLiveLaneTimeout          = 90 * time.Second

	semanticLiveLocalCapacityBatchCount = 2
	semanticLiveLocalCapacityTimeout    = 270 * time.Second
	semanticLiveLocalRetrievalTimeout   = 5 * time.Minute
)

var semanticLiveDefaultCloudModels = []string{
	config.OpenRouterNemotronEmbedFreeModelID,
	config.OpenRouterVLLEmbedFreeModelID,
}

var semanticLiveDocuments = []string{
	"A child is reading a book in the library.",
	"A student reads a storybook at the library.",
	"The turbine pressure dropped after the coolant valve opened.",
}

// TestSemanticIndexLiveModels is an explicitly authorized smoke test for the
// production semantic-index embedding path. It reads the normal desktop
// configuration only after the gate is enabled, never persists that config,
// never writes a database, and never pulls or removes a local model.
//
// Each lane runs through the production multi-profile builder, performs its
// real startup preflight, then sends exactly one three-input data batch. No
// chat completion request is part of this semantic-index harness.
func TestSemanticIndexLiveModels(t *testing.T) {
	if strings.TrimSpace(os.Getenv(semanticLiveModelsGate)) != "1" {
		t.Skip("set HEX_SEMANTIC_LIVE_MODELS=1 to run the real local/cloud semantic-index model smoke test")
	}

	cfg, err := config.Load("")
	if err != nil {
		// Loading may touch credential-bearing configuration. Keep the failure
		// deliberately opaque rather than risk echoing source material.
		t.Fatal("load saved HexClaw configuration failed (details withheld to protect credentials)")
	}

	t.Run("local_ollama", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), semanticLiveLaneTimeout)
		defer cancel()

		laneCfg, plan, provider := semanticLiveLocalPlan(t, ctx, cfg)
		semanticLiveRunEmbeddingLane(
			t, ctx, laneCfg, plan, provider,
			knowledge.ProviderLocationLocal,
			knowledge.ProfileAvailabilityInstalled,
		)
	})

	for _, model := range semanticLiveCloudModels() {
		model := model
		t.Run("cloud_openrouter/"+strings.NewReplacer("/", "_", ":", "_").Replace(model), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), semanticLiveLaneTimeout)
			defer cancel()

			laneCfg, plan, provider := semanticLiveCloudPlan(t, ctx, cfg, model)
			semanticLiveRunEmbeddingLane(
				t, ctx, laneCfg, plan, provider,
				knowledge.ProviderLocationCloud,
				knowledge.ProfileAvailabilityConnected,
			)
		})
	}
}

func TestSemanticIndexLiveLocalCapacity(t *testing.T) {
	if strings.TrimSpace(os.Getenv(semanticLiveLocalCapacityGate)) != "1" {
		t.Skip("set HEX_SEMANTIC_LIVE_LOCAL_CAPACITY=1 to run the real local semantic-index capacity test")
	}
	semanticLiveRunLocalCapacity(t)
}

// TestSemanticIndexLiveLocalRevisionRetrieval is the real production-path
// closure gate for BUG-20260728-012. Unlike the legacy RAG real harness, it
// builds and publishes an immutable revision before querying through
// Manager + SQLiteRevisionSemanticSearcher. The test uses only a temporary
// SQLite database and a preinstalled loopback Ollama model; it never loads the
// saved desktop config, pulls/deletes a model, calls chat, or writes user data.
func TestSemanticIndexLiveLocalRevisionRetrieval(t *testing.T) {
	if strings.TrimSpace(os.Getenv(semanticLiveLocalRetrievalGate)) != "1" {
		t.Skip("set HEX_SEMANTIC_LIVE_LOCAL_RETRIEVAL=1 to run the real local revision-scoped retrieval gate")
	}

	ctx, cancel := context.WithTimeout(context.Background(), semanticLiveLocalRetrievalTimeout)
	defer cancel()

	// Force the isolated loopback plan even if another live test selected a
	// provider by name. The base URL and exact model remain explicitly
	// overridable through the existing local-only live-test variables.
	t.Setenv(semanticLiveLocalProviderEnv, "")
	laneCfg, plan, provider := semanticLiveLocalPlan(t, ctx, config.DefaultConfig())
	if plan.Model != "qwen3-embedding:8b" || !plan.Ollama {
		t.Fatalf("revision retrieval resolved provider=%q model=%q ollama=%t, want local qwen3-embedding:8b",
			plan.Provider, plan.Model, plan.Ollama)
	}

	counter := &semanticLiveEmbeddingCounter{}
	bundle := buildKnowledgeEmbeddingRuntimeProfiles(
		ctx, laneCfg, &egress.Policy{}, newKnowledgeSemanticRuntimeGate(),
		withKnowledgeEmbeddingHTTPClientObserver(func(providerKey, model string, client *http.Client) {
			if providerKey == plan.Provider && model == plan.Model {
				counter.next = client.Transport
				client.Transport = counter
			}
		}),
	)

	db, _ := newKnowledgeSemanticRuntimeTestDB(t)
	// The shared semantic-runtime helper intentionally stops at v23 for unit
	// tests. Revision search also consumes the production Knowledge provenance
	// schema introduced by the v24/v26/v27/v28 chain and v46. Apply those exact
	// migrations without the unrelated K12 migrations whose base tables do not
	// belong to this isolated Knowledge-only database.
	if err := migrate.Run(ctx, db, []migrate.Migration{
		migrate.KnowledgeIngestV24,
		migrate.KnowledgeIngestGenerationsV26,
		migrate.KnowledgeDocumentScopeV27,
		migrate.KnowledgeIngestCheckpointV28,
		migrate.KnowledgeIngestExecutionV46,
	}); err != nil {
		t.Fatalf("apply production Knowledge migrations to isolated test database: %v", err)
	}
	fixtures := []struct {
		id      string
		title   string
		content string
	}{
		{
			id:      "bug012-live-photosynthesis",
			title:   "植物怎样制造养分",
			content: "绿色植物通过叶绿素吸收太阳光，把二氧化碳和水转化为有机物，并释放氧气。",
		},
		{
			id:      "bug012-live-goroutine",
			title:   "Go 并发",
			content: "Go 语言使用 goroutine 执行并发任务，并通过 channel 在协程之间传递消息。",
		},
		{
			id:      "bug012-live-canal",
			title:   "京杭大运河",
			content: "京杭大运河贯通中国南北，历史上承担粮食运输和区域交流的重要功能。",
		},
	}
	for i, fixture := range fixtures {
		if _, err := db.ExecContext(ctx, `INSERT INTO kb_documents
			(id,title,content,source,chunk_count,status,deleted,error_message,source_type)
			VALUES(?,?,?,?,1,'indexed',0,'','manual')`,
			fixture.id, fixture.title, fixture.content, "bug012-live"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO kb_chunks
			(id,doc_id,content,chunk_index,embedding)
			VALUES(?,?,?,?,NULL)`,
			fmt.Sprintf("%s-chunk-%d", fixture.id, i), fixture.id, fixture.content, 0); err != nil {
			t.Fatal(err)
		}
	}

	runtime, err := setupKnowledgeSemanticIndex(
		ctx, db, bundle.Resolver, bundle.Registry, "bug012-live-revision-worker",
	)
	if err != nil {
		t.Fatalf("setup production semantic runtime: %v", err)
	}
	before, err := runtime.Service.GetPolicy(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ActiveRevision != nil || before.DesiredRevision == nil {
		t.Fatalf("pre-worker policy active=%+v desired=%+v, want staged revision only",
			before.ActiveRevision, before.DesiredRevision)
	}

	indexStarted := time.Now()
	processed, err := runtime.Worker.RunOnce(ctx)
	indexElapsed := time.Since(indexStarted)
	if err != nil || !processed {
		t.Fatalf("production revision worker processed=%t elapsed=%v err=%v",
			processed, indexElapsed, err)
	}

	after, err := runtime.Service.GetPolicy(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ActiveRevision == nil || after.DesiredRevision != nil ||
		after.ActiveRevision.State != knowledge.VectorIndexReady ||
		after.ActiveRevision.Profile.ModelName != plan.Model ||
		after.ActiveRevision.Profile.Dimension != 4096 ||
		after.ActiveRevision.Profile.Location != knowledge.ProviderLocationLocal {
		t.Fatalf("published policy active=%+v desired=%+v, want ready local Qwen 4096 revision",
			after.ActiveRevision, after.DesiredRevision)
	}

	var vectorCount, minDimension, maxDimension, revisionCount, profileHashCount int
	var minModel, maxModel string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(dimension), MAX(dimension),
			COUNT(DISTINCT revision_id), COUNT(DISTINCT profile_config_hash),
			MIN(model_name), MAX(model_name)
			FROM kb_revision_vectors WHERE revision_id=?`,
		after.ActiveRevision.RevisionID,
	).Scan(
		&vectorCount, &minDimension, &maxDimension,
		&revisionCount, &profileHashCount, &minModel, &maxModel,
	); err != nil {
		t.Fatal(err)
	}
	if vectorCount != len(fixtures) || minDimension != 4096 || maxDimension != 4096 ||
		revisionCount != 1 || profileHashCount != 1 ||
		minModel != plan.Model || maxModel != plan.Model {
		t.Fatalf("revision vectors count=%d dimension=%d..%d revisions=%d hashes=%d models=%q..%q",
			vectorCount, minDimension, maxDimension, revisionCount, profileHashCount, minModel, maxModel)
	}

	store := knowledge.NewSQLiteStore(db)
	hybrid := knowledge.DefaultHybridConfig()
	hybrid.ExpandEnabled = false
	hybrid.RerankEnabled = false
	hybrid.UseRRF = false
	manager := knowledge.NewManager(
		store, store, nil,
		knowledge.WithHybridConfig(hybrid),
		knowledge.WithRevisionSemanticSearcher(runtime.Searcher),
	)

	queryStarted := time.Now()
	_, hits, err := manager.QueryHits(ctx, "植物怎样利用阳光制造养分并释放氧气？", 3)
	queryElapsed := time.Since(queryStarted)
	if err != nil {
		t.Fatalf("production revision-scoped QueryHits elapsed=%v: %v", queryElapsed, err)
	}
	if len(hits) != 1 || hits[0].DocID != fixtures[0].id {
		t.Fatalf("production revision-scoped hits=%+v, want strict Top-1 %q", hits, fixtures[0].id)
	}

	records := counter.snapshot()
	documentRequests, documentInputs, queryRequests := 0, 0, 0
	for _, record := range records {
		if record.model != plan.Model || record.httpStatus != http.StatusOK {
			t.Fatalf("embedding record model=%q status=%d, want %q/200",
				record.model, record.httpStatus, plan.Model)
		}
		if len(record.inputValues) == 1 &&
			strings.HasPrefix(record.inputValues[0], bug20260728QwenQueryPrefix) {
			queryRequests++
			if !record.hasDeadline ||
				record.deadlineRemaining < 55*time.Second ||
				record.deadlineRemaining > 60*time.Second {
				t.Fatalf("query request deadline=%v, want production profile about 60s",
					record.deadlineRemaining)
			}
			continue
		}
		documentRequests++
		documentInputs += len(record.inputValues)
		if !record.hasDeadline ||
			record.deadlineRemaining < 115*time.Second ||
			record.deadlineRemaining > 120*time.Second {
			t.Fatalf("document request deadline=%v, want production profile about 120s",
				record.deadlineRemaining)
		}
		for _, input := range record.inputValues {
			if strings.HasPrefix(input, bug20260728QwenQueryPrefix) {
				t.Fatal("document embedding unexpectedly received the Qwen query instruction")
			}
		}
	}
	if documentRequests != 2 || documentInputs != len(fixtures) || queryRequests != 1 {
		t.Fatalf("embedding route documents=%d requests/%d inputs queries=%d, want 2/3/1; records=%+v",
			documentRequests, documentInputs, queryRequests, records)
	}
	if got := counter.chatRequests.Load(); got != 0 {
		t.Fatalf("chat requests=%d, want zero", got)
	}

	t.Logf(
		"BUG012 real production revision retrieval passed: provider=%q model=%q revision=%q vectors=%d dimension=4096 index_elapsed=%v query_elapsed=%v top1_doc=%q top1_score=%.4f embedding_requests=%d chat_requests=0 http_statuses=%v",
		plan.Provider, plan.Model, after.ActiveRevision.RevisionID, vectorCount,
		indexElapsed, queryElapsed, hits[0].DocID, hits[0].Score, len(records), counter.statuses(),
	)
	_ = provider // Provider is resolved only from the isolated in-memory local plan.
}

// semanticLiveRunLocalCapacity closes the gap between a three-input smoke and
// the production worker's calibrated local-Qwen batch. The repository is an
// in-memory protocol fake: the real user database and saved configuration are
// never mutated. The resolver, registry, purpose transforms, guarded provider
// transport, selected embedding model, and SemanticIndexWorker are all production objects.
func semanticLiveRunLocalCapacity(t *testing.T) {
	t.Helper()

	cfg, err := config.Load("")
	if err != nil {
		t.Fatal("load saved HexClaw configuration failed (details withheld to protect credentials)")
	}
	// This aggregate harness budget permits startup plus two sequential worker
	// calls at the calibrated model-scoped budget.
	ctx, cancel := context.WithTimeout(context.Background(), semanticLiveLocalCapacityTimeout)
	defer cancel()

	laneCfg, plan, provider := semanticLiveLocalPlan(t, ctx, cfg)
	expectedDimension := knowledgeEmbeddingDimension(plan.Model)
	if expectedDimension <= 0 {
		t.Fatalf("local capacity model %q has no trusted exact embedding dimension", plan.Model)
	}
	if _, err := newKnowledgeEmbeddingProviderHTTPClient(plan, provider); err != nil {
		t.Fatalf("provider %q production embedding transport rejected its endpoint: %v", plan.Provider, err)
	}
	executionProfile, ok := knowledge.EmbeddingExecutionProfileForModel(plan.Model)
	if !ok {
		t.Fatalf("local capacity model %q has no calibrated execution profile", plan.Model)
	}

	counter := &semanticLiveEmbeddingCounter{}
	bundle := buildKnowledgeEmbeddingRuntimeProfiles(
		ctx, laneCfg, &egress.Policy{}, newKnowledgeSemanticRuntimeGate(),
		withKnowledgeEmbeddingHTTPClientObserver(func(providerKey, model string, client *http.Client) {
			if providerKey == plan.Provider && model == plan.Model {
				counter.next = client.Transport
				client.Transport = counter
			}
		}),
	)
	catalog, err := bundle.Resolver.Catalog(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID)
	if err != nil {
		t.Fatalf("local capacity catalog resolution failed: %v", err)
	}
	if len(catalog.Profiles) != 1 {
		t.Fatalf("local capacity catalog profile count=%d, want 1", len(catalog.Profiles))
	}
	profile := catalog.Profiles[0]
	if profile.ProviderName != plan.Provider || profile.ModelName != plan.Model ||
		profile.Location != knowledge.ProviderLocationLocal ||
		profile.Availability != knowledge.ProfileAvailabilityInstalled ||
		profile.Dimension != expectedDimension || profile.Capability != "embedding" {
		t.Fatalf(
			"local capacity catalog mismatch: provider=%q model=%q location=%q availability=%q dimension=%d capability=%q",
			profile.ProviderName, profile.ModelName, profile.Location, profile.Availability,
			profile.Dimension, profile.Capability,
		)
	}
	snapshot, err := bundle.Resolver.Resolve(
		ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID,
		knowledge.EmbeddingSelection{Kind: knowledge.EmbeddingSelectionProfile, ProfileID: profile.ProfileID},
	)
	if err != nil {
		t.Fatalf("local capacity profile resolution failed: %v", err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("local capacity profile snapshot is invalid: %v", err)
	}

	inputs := semanticLiveCapacityInputs(
		executionProfile.BatchMaxCount * semanticLiveLocalCapacityBatchCount,
	)
	repository := newSemanticLiveCapacityRepository(
		snapshot, inputs, expectedDimension, executionProfile.BatchMaxCount,
	)
	worker := knowledge.NewSemanticIndexWorker(
		repository,
		bundle.Registry,
		knowledge.SemanticIndexWorkerConfig{
			OwnerID:  knowledgeDesktopOwnerID,
			CorpusID: knowledgeDefaultCorpusID,
			WorkerID: "semantic-live-local-capacity-worker",
			// Deliberately larger than the calibrated limit: the worker must
			// shape this request from the immutable model profile.
			BatchSize:     64,
			LeaseDuration: 5 * time.Minute,
			// Zero deliberately exercises the model-scoped production budget.
			EmbeddingTimeout: 0,
		},
	)
	processed, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatalf(
			"local capacity worker failed for provider %q model %q: %s",
			plan.Provider, plan.Model, semanticLiveRedactedError(err, provider.APIKey),
		)
	}
	if !processed {
		t.Fatal("local capacity worker did not claim its synthetic rebuild job")
	}

	commitSizes, embedded, completed := repository.result()
	if len(commitSizes) != semanticLiveLocalCapacityBatchCount ||
		embedded != int64(len(inputs)) || !completed {
		t.Fatalf(
			"local capacity durable boundary: commit_sizes=%v embedded=%d completed=%v, want [%d %d]/%d/true",
			commitSizes, embedded, completed,
			executionProfile.BatchMaxCount, executionProfile.BatchMaxCount, len(inputs),
		)
	}

	records := counter.snapshot()
	if len(records) != semanticLiveLocalCapacityBatchCount {
		t.Fatalf("local capacity embedding request count=%d, want %d", len(records), semanticLiveLocalCapacityBatchCount)
	}
	latencies := make([]time.Duration, 0, len(records))
	deadlineBudgets := make([]time.Duration, 0, len(records))
	for i, record := range records {
		if i > 0 && record.startedAt.Before(records[i-1].finishedAt) {
			t.Fatalf(
				"local capacity request[%d] overlapped request[%d]; worker batches must be sequential",
				i, i-1,
			)
		}
		if record.model != plan.Model || record.inputs != executionProfile.BatchMaxCount {
			t.Fatalf(
				"local capacity request[%d]: model=%q inputs=%d, want %q/%d",
				i, record.model, record.inputs, plan.Model, executionProfile.BatchMaxCount,
			)
		}
		if record.httpStatus != http.StatusOK {
			t.Fatalf("local capacity request[%d] HTTP status=%d, want 200", i, record.httpStatus)
		}
		if record.duration <= 0 {
			t.Fatalf("local capacity request[%d] did not record a positive latency", i)
		}
		if !record.hasDeadline ||
			record.deadlineRemaining < executionProfile.BatchTimeout-time.Second ||
			record.deadlineRemaining > executionProfile.BatchTimeout+time.Second {
			t.Fatalf(
				"local capacity request[%d] deadline budget=%s present=%v, want calibrated profile budget=%s±1s",
				i, record.deadlineRemaining, record.hasDeadline, executionProfile.BatchTimeout,
			)
		}
		latencies = append(latencies, record.duration.Round(time.Millisecond))
		deadlineBudgets = append(deadlineBudgets, record.deadlineRemaining.Round(time.Millisecond))
	}
	chatRequests := counter.chatRequests.Load()
	if chatRequests != 0 {
		t.Fatalf("local capacity chat completion requests=%d, want 0", chatRequests)
	}
	t.Logf(
		"semantic local capacity passed: provider=%q model=%q batches=%d batch_size=%d vectors=%d dimension=%d embedding_requests=%d chat_requests=%d latencies=%v request_deadline_budgets=%v http_statuses=%v",
		plan.Provider, plan.Model, len(records), executionProfile.BatchMaxCount,
		embedded, expectedDimension, len(records), chatRequests,
		latencies, deadlineBudgets, counter.statuses(),
	)
}

func semanticLiveCapacityInputs(count int) []knowledge.RevisionChunkInput {
	inputs := make([]knowledge.RevisionChunkInput, count)
	for i := range inputs {
		content := semanticLiveCapacityText(i)
		digest := sha256.Sum256([]byte(content))
		chunkID := fmt.Sprintf("capacity-chunk-%03d", i)
		inputs[i] = knowledge.RevisionChunkInput{
			Cursor: knowledge.RevisionChunkCursor{
				DocumentID: "capacity-document", ChunkIndex: i, ChunkID: chunkID,
			},
			DocumentID:        "capacity-document",
			ContentGeneration: 1,
			ChunkID:           chunkID,
			ChunkIndex:        i,
			Content:           content,
			ContentHash:       hex.EncodeToString(digest[:]),
		}
	}
	return inputs
}

func semanticLiveCapacityText(index int) string {
	topics := []string{
		"This school safety handbook explains how teachers account for students, keep evacuation routes clear, and report hazards before a laboratory activity begins.",
		"This history lesson compares primary sources, maps, and census records so students can distinguish direct evidence from a later interpretation of the same event.",
		"This maintenance guide describes inspection of a cooling pump, including vibration readings, valve position, seal leakage, and the shutdown criteria used by technicians.",
		"This nutrition chapter explains how serving size, fibre, protein, added sugar, and food allergies should be considered when planning a balanced weekly menu.",
	}
	var builder strings.Builder
	for paragraph := 0; len([]rune(builder.String())) < 400; paragraph++ {
		if builder.Len() > 0 {
			builder.WriteByte(' ')
		}
		fmt.Fprintf(
			&builder,
			"Document section %03d, paragraph %d. %s The section includes a concrete example, a short explanation of cause and effect, and a checklist item that a reader can verify.",
			index, paragraph+1, topics[index%len(topics)],
		)
	}
	runes := []rune(builder.String())
	return string(runes[:400])
}

type semanticLiveCapacityRepository struct {
	mu          sync.Mutex
	claimed     bool
	job         knowledge.KnowledgeJob
	plan        knowledge.JobExecutionPlan
	inputs      []knowledge.RevisionChunkInput
	batchNumber int
	inFlight    map[string]struct{}
	commitSizes []int
	embedded    int64
	completed   bool
	dimension   int
	batchSize   int
}

func newSemanticLiveCapacityRepository(
	snapshot knowledge.EmbeddingProfileSnapshot,
	inputs []knowledge.RevisionChunkInput,
	dimension int,
	batchSize int,
) *semanticLiveCapacityRepository {
	now := time.Now().UTC()
	expiresAt := now.Add(5 * time.Minute)
	return &semanticLiveCapacityRepository{
		job: knowledge.KnowledgeJob{
			JobID: "semantic-live-local-capacity-job", Kind: knowledge.KnowledgeJobEmbedDocument,
			OwnerID: knowledgeDesktopOwnerID, CorpusUID: knowledgeDefaultCorpusID,
			DocumentID: "capacity-document", TargetRevisionID: "capacity-revision",
			State: knowledge.KnowledgeJobQueued, Stage: knowledge.JobStageEmbedding,
			Attempt: 1, LeaseEpoch: 1, LeaseExpiresAt: &expiresAt,
			CreatedAt: now, UpdatedAt: now,
		},
		plan: knowledge.JobExecutionPlan{
			CorpusUID: knowledgeDefaultCorpusID, CorpusAlias: knowledgeDefaultCorpusID,
			RevisionID: "capacity-revision", PolicyVersion: 1, ContentVersion: 1,
			Snapshot: snapshot,
		},
		inputs:    append([]knowledge.RevisionChunkInput(nil), inputs...),
		inFlight:  make(map[string]struct{}),
		dimension: dimension,
		batchSize: batchSize,
	}
}

func (r *semanticLiveCapacityRepository) ClaimNextJobForCorpus(
	_ context.Context,
	ownerID, corpusID, workerID string,
	now time.Time,
	leaseDuration time.Duration,
) (knowledge.KnowledgeJob, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.claimed {
		return knowledge.KnowledgeJob{}, false, nil
	}
	if ownerID != r.job.OwnerID || corpusID != r.job.CorpusUID || strings.TrimSpace(workerID) == "" {
		return knowledge.KnowledgeJob{}, false, fmt.Errorf("capacity repository claim scope mismatch")
	}
	r.claimed = true
	r.job.State = knowledge.KnowledgeJobRunning
	r.job.LeaseOwner = workerID
	expiresAt := now.Add(leaseDuration)
	r.job.LeaseExpiresAt = &expiresAt
	return r.job, true, nil
}

func (r *semanticLiveCapacityRepository) LoadJobExecutionPlan(
	_ context.Context,
	_ knowledge.JobLease,
	_ time.Time,
) (knowledge.JobExecutionPlan, error) {
	return r.plan, nil
}

func (r *semanticLiveCapacityRepository) ListRevisionChunkInputs(
	_ context.Context,
	_ knowledge.JobLease,
	_ time.Time,
	after *knowledge.RevisionChunkCursor,
	limit int,
) ([]knowledge.RevisionChunkInput, error) {
	if limit != r.batchSize {
		return nil, fmt.Errorf("capacity worker batch size=%d, want %d", limit, r.batchSize)
	}
	start := 0
	if after != nil {
		if after.DocumentID != "capacity-document" {
			return nil, fmt.Errorf("capacity cursor document mismatch")
		}
		start = after.ChunkIndex + 1
	}
	if start >= len(r.inputs) {
		return nil, nil
	}
	end := start + limit
	if end > len(r.inputs) {
		end = len(r.inputs)
	}
	return append([]knowledge.RevisionChunkInput(nil), r.inputs[start:end]...), nil
}

func (r *semanticLiveCapacityRepository) CreateEmbeddingBatchManifest(
	_ context.Context,
	_ knowledge.JobLease,
	_ time.Time,
	manifest knowledge.EmbeddingBatchManifest,
) (knowledge.EmbeddingBatchManifest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(manifest.Chunks) != r.batchSize {
		return knowledge.EmbeddingBatchManifest{}, fmt.Errorf(
			"capacity manifest chunks=%d, want %d", len(manifest.Chunks), r.batchSize,
		)
	}
	r.batchNumber++
	manifest.BatchID = fmt.Sprintf("capacity-batch-%d", r.batchNumber)
	manifest.JobID = r.job.JobID
	manifest.RevisionID = r.plan.RevisionID
	manifest.ProfileConfigHash = r.plan.Snapshot.ProfileConfigHash
	manifest.State = knowledge.EmbeddingBatchPrepared
	manifest.LeaseEpoch = r.job.LeaseEpoch
	return manifest, nil
}

func (r *semanticLiveCapacityRepository) BeginEmbeddingBatch(
	_ context.Context,
	_ knowledge.JobLease,
	_ time.Time,
	batchID string,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, duplicate := r.inFlight[batchID]; duplicate {
		return fmt.Errorf("capacity batch %q began twice", batchID)
	}
	r.inFlight[batchID] = struct{}{}
	return nil
}

func (r *semanticLiveCapacityRepository) MarkEmbeddingBatchOutcomeUnknown(
	context.Context,
	knowledge.JobLease,
	time.Time,
	string,
	string,
) error {
	return fmt.Errorf("capacity batch outcome unexpectedly became unknown")
}

func (r *semanticLiveCapacityRepository) CommitEmbeddingBatch(
	_ context.Context,
	_ knowledge.JobLease,
	_ time.Time,
	commit knowledge.EmbeddingBatchCommit,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.inFlight[commit.BatchID]; !ok {
		return fmt.Errorf("capacity batch %q committed before begin", commit.BatchID)
	}
	delete(r.inFlight, commit.BatchID)
	if len(commit.Vectors) != r.batchSize {
		return fmt.Errorf(
			"capacity batch %q vectors=%d, want %d",
			commit.BatchID, len(commit.Vectors), r.batchSize,
		)
	}
	for i, vector := range commit.Vectors {
		if len(vector.Values) != r.dimension {
			return fmt.Errorf(
				"capacity batch %q vector[%d] dimension=%d, want %d",
				commit.BatchID, i, len(vector.Values), r.dimension,
			)
		}
		var squaredNorm float64
		for j, value := range vector.Values {
			v := float64(value)
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("capacity batch %q vector[%d][%d] is not finite", commit.BatchID, i, j)
			}
			squaredNorm += v * v
		}
		if squaredNorm <= 1e-16 {
			return fmt.Errorf("capacity batch %q vector[%d] is zero", commit.BatchID, i)
		}
	}
	r.embedded += int64(len(commit.Vectors))
	if commit.ChunksDone != r.embedded || commit.ChunksTotal != int64(len(r.inputs)) {
		return fmt.Errorf(
			"capacity progress=%d/%d, want %d/%d",
			commit.ChunksDone, commit.ChunksTotal, r.embedded, len(r.inputs),
		)
	}
	r.commitSizes = append(r.commitSizes, len(commit.Vectors))
	return nil
}

func (r *semanticLiveCapacityRepository) GarbageCollectDocument(
	context.Context,
	knowledge.JobLease,
	time.Time,
) error {
	return fmt.Errorf("capacity test unexpectedly entered garbage collection")
}

func (r *semanticLiveCapacityRepository) GetRevisionBuildSummary(
	_ context.Context,
	_ knowledge.JobLease,
	_ time.Time,
) (knowledge.RevisionBuildSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return knowledge.RevisionBuildSummary{
		RevisionID: r.plan.RevisionID, ChunkSetDigest: "capacity-chunk-set-v1",
		ExpectedChunks: int64(len(r.inputs)), EmbeddedChunks: r.embedded,
	}, nil
}

func (r *semanticLiveCapacityRepository) SaveStageCheckpoint(
	_ context.Context,
	_ knowledge.JobLease,
	_ time.Time,
	checkpoint knowledge.StageCheckpoint,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.embedded != int64(len(r.inputs)) || checkpoint.State != knowledge.StageCheckpointSucceeded {
		return fmt.Errorf("capacity checkpoint written before both batches committed")
	}
	return nil
}

func (r *semanticLiveCapacityRepository) CompleteActiveRevisionJob(
	_ context.Context,
	_ knowledge.JobLease,
	_ time.Time,
	expectedContentVersion int64,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if expectedContentVersion != r.plan.ContentVersion || r.embedded != int64(len(r.inputs)) {
		return fmt.Errorf("capacity completion contract mismatch")
	}
	r.completed = true
	return nil
}

func (r *semanticLiveCapacityRepository) PrepareRevisionForPublish(
	context.Context,
	knowledge.JobLease,
	time.Time,
	knowledge.RevisionPublishPreparation,
) error {
	return fmt.Errorf("capacity embed-document job unexpectedly prepared a rebuild publication")
}

func (r *semanticLiveCapacityRepository) PublishRevisionCAS(
	context.Context,
	knowledge.PublishRevisionCommand,
) error {
	return fmt.Errorf("capacity embed-document job unexpectedly published a rebuild revision")
}

func (r *semanticLiveCapacityRepository) RetryJob(
	_ context.Context,
	_ knowledge.JobLease,
	_ time.Time,
	_ time.Time,
	lastError string,
) (knowledge.KnowledgeJob, error) {
	return r.job, fmt.Errorf("capacity worker attempted retry: %s", lastError)
}

func (r *semanticLiveCapacityRepository) FailJob(
	_ context.Context,
	_ knowledge.JobLease,
	_ time.Time,
	lastError string,
) (knowledge.KnowledgeJob, error) {
	return r.job, fmt.Errorf("capacity worker attempted permanent failure: %s", lastError)
}

func (r *semanticLiveCapacityRepository) RenewJobLease(
	_ context.Context,
	lease knowledge.JobLease,
	now time.Time,
	leaseDuration time.Duration,
) (knowledge.JobLease, error) {
	lease.ExpiresAt = now.Add(leaseDuration)
	return lease, nil
}

func (r *semanticLiveCapacityRepository) result() ([]int, int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.commitSizes...), r.embedded, r.completed
}

func semanticLiveLocalPlan(
	t *testing.T,
	ctx context.Context,
	source *config.Config,
) (*config.Config, knowledgeEmbeddingPlan, config.LLMProviderConfig) {
	t.Helper()

	cfg := semanticLiveCloneConfig(source)
	model := semanticLiveEnv(semanticLiveLocalModelEnv, semanticLiveDefaultLocalModel)
	requestedProvider := strings.TrimSpace(os.Getenv(semanticLiveLocalProviderEnv))
	requestedBaseURL := strings.TrimSpace(os.Getenv(semanticLiveLocalBaseURLEnv))

	if requestedProvider != "" {
		providerName, provider, ok := semanticLiveFindProvider(cfg, requestedProvider)
		if !ok {
			t.Fatalf("local provider %q is not present in the saved configuration", requestedProvider)
		}
		if requestedBaseURL != "" {
			provider.BaseURL = requestedBaseURL
		}
		cfg.LLM.Providers[providerName] = provider
		cfg.Knowledge.Embedding.Provider = providerName
		cfg.Knowledge.Embedding.Model = model
	} else if requestedBaseURL != "" {
		const providerName = "semantic-live-ollama-loopback"
		cfg.LLM.Providers[providerName] = config.LLMProviderConfig{
			BaseURL:  requestedBaseURL,
			Locality: config.ProviderLocalityLocal,
		}
		cfg.Knowledge.Embedding.Provider = providerName
		cfg.Knowledge.Embedding.Model = model
	} else {
		// Let production discovery inspect every configured native Ollama
		// provider deterministically and prefer one with the exact model ready.
		cfg.Knowledge.Embedding.Provider = ""
		cfg.Knowledge.Embedding.Model = model
	}

	plan := resolveKnowledgeEmbeddingPlan(ctx, cfg)
	if plan.Provider == "" ||
		(!plan.Ready && requestedProvider == "" && requestedBaseURL == "") {
		// Zero-config fallback is still production's guarded loopback Ollama
		// contract. It is also tried when configured candidates exist but none
		// has the requested model ready. Adding it only to this in-memory copy
		// cannot alter user data.
		const providerName = "semantic-live-ollama-loopback"
		cfg.LLM.Providers[providerName] = config.LLMProviderConfig{
			BaseURL:  defaultKnowledgeOllamaEmbeddingBaseURL,
			Locality: config.ProviderLocalityLocal,
		}
		cfg.Knowledge.Embedding.Provider = providerName
		plan = resolveKnowledgeEmbeddingPlan(ctx, cfg)
	}

	provider, ok := cfg.LLM.Providers[plan.Provider]
	if !ok || !plan.Ollama {
		t.Fatalf("local provider %q did not resolve through the native loopback Ollama path", plan.Provider)
	}
	if plan.Model != model {
		t.Fatalf("local model resolved to %q, want the explicitly selected %q", plan.Model, model)
	}
	if !plan.Configured || !plan.ServiceAvailable {
		t.Fatalf("local Ollama provider %q is not reachable; the live test never downloads models", plan.Provider)
	}
	if !plan.Ready {
		t.Fatalf("local Ollama model %q is not installed; install it separately because this test never pulls models", model)
	}
	provider.Model = ""
	provider.Models = []string{model}
	provider.ModelSpecsMode = config.LLMModelSpecsModeExplicit
	provider.ModelSpecs = []config.LLMProviderModelSpec{{
		ID: model, Capabilities: []string{config.LLMModelCapabilityEmbedding},
		Embedding: &config.LLMEmbeddingModelSpec{
			Protocol: config.LLMEmbeddingProtocolOllama, Dimension: knowledgeEmbeddingDimension(model), Normalization: "l2",
		},
	}}
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{plan.Provider: provider}
	return cfg, plan, provider
}

func semanticLiveCloudPlan(
	t *testing.T,
	ctx context.Context,
	source *config.Config,
	model string,
) (*config.Config, knowledgeEmbeddingPlan, config.LLMProviderConfig) {
	t.Helper()

	cfg := semanticLiveCloneConfig(source)
	requestedProvider := semanticLiveEnv(semanticLiveCloudProviderEnv, semanticLiveDefaultCloudProvider)
	providerName, provider, ok := semanticLiveFindProvider(cfg, requestedProvider)
	if !ok {
		t.Fatalf("cloud provider %q is not present in the saved configuration", requestedProvider)
	}
	if provider.Enabled != nil && !*provider.Enabled {
		t.Fatalf("cloud provider %q is disabled", providerName)
	}
	if strings.TrimSpace(provider.APIKey) == "" {
		t.Fatalf("cloud provider %q has no saved API key", providerName)
	}
	if !strings.EqualFold(strings.TrimSpace(provider.Locality), config.ProviderLocalityCloud) {
		t.Fatalf("cloud provider %q locality is not explicitly %q", providerName, config.ProviderLocalityCloud)
	}

	model = strings.TrimSpace(model)
	dimension := knowledgeEmbeddingDimension(model)
	if dimension <= 0 {
		t.Fatalf("cloud model %q has no trusted exact embedding dimension", model)
	}
	provider.BaseURL = semanticLiveCloudBaseURL(provider.BaseURL)
	provider.Model = ""
	provider.Models = []string{model}
	provider.ModelSpecsMode = config.LLMModelSpecsModeExplicit
	provider.ModelSpecs = []config.LLMProviderModelSpec{{
		ID: model, Capabilities: []string{config.LLMModelCapabilityEmbedding},
		Embedding: &config.LLMEmbeddingModelSpec{
			Protocol: config.LLMEmbeddingProtocolOpenAI, Dimension: dimension, Normalization: "l2",
		},
	}}
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{providerName: provider}
	cfg.Knowledge.Embedding.Provider = providerName
	cfg.Knowledge.Embedding.Model = model

	plan := resolveKnowledgeEmbeddingPlan(ctx, cfg)
	if plan.Ollama || !plan.Configured || !plan.Ready || !plan.ServiceAvailable {
		t.Fatalf("cloud provider %q did not resolve as a ready explicit embedding capability", providerName)
	}
	if plan.Model != model {
		t.Fatalf("cloud model resolved to %q, want %q", plan.Model, model)
	}
	return cfg, plan, provider
}

func semanticLiveRunEmbeddingLane(
	t *testing.T,
	ctx context.Context,
	cfg *config.Config,
	plan knowledgeEmbeddingPlan,
	provider config.LLMProviderConfig,
	wantLocation knowledge.ProviderLocation,
	wantAvailability knowledge.ProfileAvailability,
) {
	t.Helper()

	dimension := knowledgeEmbeddingDimension(plan.Model)
	if _, err := newKnowledgeEmbeddingProviderHTTPClient(plan, provider); err != nil {
		t.Fatalf("provider %q production embedding transport rejected its endpoint: %v", plan.Provider, err)
	}
	counter := &semanticLiveEmbeddingCounter{}
	bundle := buildKnowledgeEmbeddingRuntimeProfiles(
		ctx, cfg, &egress.Policy{}, newKnowledgeSemanticRuntimeGate(),
		withKnowledgeEmbeddingHTTPClientObserver(func(providerKey, model string, client *http.Client) {
			if providerKey == plan.Provider && model == plan.Model {
				counter.next = client.Transport
				client.Transport = counter
			}
		}),
	)
	catalog, err := bundle.Resolver.Catalog(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID)
	if err != nil {
		t.Fatalf("provider %q catalog resolution failed: %v", plan.Provider, err)
	}
	if len(catalog.Profiles) != 1 {
		t.Fatalf("provider %q catalog profile count=%d, want 1", plan.Provider, len(catalog.Profiles))
	}
	profile := catalog.Profiles[0]
	if profile.ProviderID != config.EffectiveProviderInstanceID(plan.Provider, provider) ||
		profile.ProviderName != plan.Provider || profile.ModelName != plan.Model ||
		profile.Location != wantLocation || profile.Availability != wantAvailability ||
		profile.Dimension != dimension || profile.Capability != "embedding" {
		t.Fatalf(
			"provider %q catalog profile mismatch: model=%q location=%q availability=%q dimension=%d capability=%q observed_embedding_requests=%d observed_http_statuses=%v",
			plan.Provider, profile.ModelName, profile.Location, profile.Availability,
			profile.Dimension, profile.Capability, len(counter.snapshot()), counter.statuses(),
		)
	}

	snapshot, err := bundle.Resolver.Resolve(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID,
		knowledge.EmbeddingSelection{Kind: knowledge.EmbeddingSelectionProfile, ProfileID: profile.ProfileID})
	if err != nil {
		t.Fatalf("provider %q snapshot resolution failed: %v", plan.Provider, err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("provider %q produced an invalid profile snapshot", plan.Provider)
	}
	if snapshot.Normalization != "l2" || snapshot.Profile.ProfileID != profile.ProfileID {
		t.Fatalf("provider %q snapshot contract mismatch: normalization=%q", plan.Provider, snapshot.Normalization)
	}

	executor, err := bundle.Registry.ExecutorForProfile(ctx, snapshot)
	if err != nil {
		t.Fatalf("provider %q registry rejected its resolved snapshot: %v", plan.Provider, err)
	}

	vectors, err := executor.EmbedForPurpose(
		ctx,
		knowledge.EmbeddingPurposeDocument,
		semanticLiveDocuments,
	)
	if err != nil {
		t.Fatalf(
			"provider %q model %q real /embeddings request failed: %s",
			plan.Provider, plan.Model, semanticLiveRedactedError(err, provider.APIKey),
		)
	}
	records := counter.snapshot()
	dataBatches, probeBatches, chatRequests := 0, 0, counter.chatRequests.Load()
	for _, record := range records {
		if record.model != plan.Model {
			t.Fatalf("provider %q sent embedding request to model %q, want exact %q", plan.Provider, record.model, plan.Model)
		}
		switch record.inputs {
		case len(semanticLiveDocuments):
			dataBatches++
		case 1:
			probeBatches++
		default:
			t.Fatalf("provider %q unexpected embedding batch input count=%d", plan.Provider, record.inputs)
		}
	}
	wantProbeBatches := 0
	if wantLocation == knowledge.ProviderLocationCloud {
		wantProbeBatches = 1
	}
	if dataBatches != 1 || probeBatches != wantProbeBatches || chatRequests != 0 {
		t.Fatalf(
			"provider %q request contract: data_batches=%d probe_batches=%d chat_requests=%d, want 1/%d/0",
			plan.Provider, dataBatches, probeBatches, chatRequests, wantProbeBatches,
		)
	}

	metrics := semanticLiveValidateVectors(t, plan, vectors, dimension)
	t.Logf(
		"semantic live lane passed: provider=%q model=%q location=%q vectors=%d dimension=%d similar_cosine=%.4f max_unrelated_cosine=%.4f data_batches=%d probe_batches=%d chat_requests=%d http_statuses=%v",
		plan.Provider, plan.Model, wantLocation, len(vectors), dimension,
		metrics.similarCosine, metrics.maxUnrelatedCosine,
		dataBatches, probeBatches, chatRequests, counter.statuses(),
	)
}

type semanticLiveEmbeddingCounter struct {
	next         http.RoundTripper
	chatRequests atomic.Int64
	mu           sync.Mutex
	records      []semanticLiveEmbeddingRequest
}

type semanticLiveEmbeddingRequest struct {
	model             string
	inputs            int
	inputValues       []string
	httpStatus        int
	startedAt         time.Time
	finishedAt        time.Time
	duration          time.Duration
	hasDeadline       bool
	deadlineRemaining time.Duration
}

func (t *semanticLiveEmbeddingCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/chat/completions") {
		t.chatRequests.Add(1)
	}
	recordIndex := -1
	startedAt := time.Now()
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/embeddings") {
		record := semanticLiveEmbeddingRequest{inputs: -1, startedAt: startedAt}
		if deadline, ok := req.Context().Deadline(); ok {
			record.hasDeadline = true
			record.deadlineRemaining = deadline.Sub(startedAt)
		}
		if req.Body != nil {
			body, err := io.ReadAll(req.Body)
			if err == nil {
				req.Body = io.NopCloser(bytes.NewReader(body))
				var payload struct {
					Model string          `json:"model"`
					Input json.RawMessage `json:"input"`
				}
				if json.Unmarshal(body, &payload) == nil {
					record.model = payload.Model
					var inputs []string
					if json.Unmarshal(payload.Input, &inputs) == nil {
						record.inputs = len(inputs)
						record.inputValues = append([]string(nil), inputs...)
					}
				}
			}
		}
		t.mu.Lock()
		t.records = append(t.records, record)
		recordIndex = len(t.records) - 1
		t.mu.Unlock()
	}
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	resp, err := next.RoundTrip(req)
	if recordIndex >= 0 {
		t.mu.Lock()
		t.records[recordIndex].finishedAt = time.Now()
		t.records[recordIndex].duration = t.records[recordIndex].finishedAt.Sub(startedAt)
		if resp != nil {
			t.records[recordIndex].httpStatus = resp.StatusCode
		}
		t.mu.Unlock()
	}
	return resp, err
}

func (t *semanticLiveEmbeddingCounter) snapshot() []semanticLiveEmbeddingRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]semanticLiveEmbeddingRequest(nil), t.records...)
}

func (t *semanticLiveEmbeddingCounter) statuses() []int {
	records := t.snapshot()
	statuses := make([]int, 0, len(records))
	for _, record := range records {
		statuses = append(statuses, record.httpStatus)
	}
	return statuses
}

type semanticLiveVectorMetrics struct {
	similarCosine      float64
	maxUnrelatedCosine float64
}

func semanticLiveValidateVectors(
	t *testing.T,
	plan knowledgeEmbeddingPlan,
	vectors [][]float32,
	dimension int,
) semanticLiveVectorMetrics {
	t.Helper()

	if len(vectors) != len(semanticLiveDocuments) {
		t.Fatalf("provider %q vector count=%d, want %d", plan.Provider, len(vectors), len(semanticLiveDocuments))
	}
	norms := make([]float64, len(vectors))
	for i, vector := range vectors {
		if len(vector) != dimension {
			t.Fatalf("provider %q vector[%d] dimension=%d, want %d", plan.Provider, i, len(vector), dimension)
		}
		var squaredNorm float64
		for j, value := range vector {
			v := float64(value)
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("provider %q vector[%d][%d] is not finite", plan.Provider, i, j)
			}
			squaredNorm += v * v
		}
		norms[i] = math.Sqrt(squaredNorm)
		if norms[i] <= 1e-8 {
			t.Fatalf("provider %q vector[%d] is zero", plan.Provider, i)
		}
		if math.Abs(norms[i]-1) > 0.02 {
			t.Fatalf("provider %q vector[%d] L2 norm=%.6f, want 1±0.02", plan.Provider, i, norms[i])
		}
	}

	similar := semanticLiveCosine(vectors[0], vectors[1], norms[0], norms[1])
	unrelatedA := semanticLiveCosine(vectors[0], vectors[2], norms[0], norms[2])
	unrelatedB := semanticLiveCosine(vectors[1], vectors[2], norms[1], norms[2])
	maxUnrelated := math.Max(unrelatedA, unrelatedB)
	if math.IsNaN(similar) || math.IsInf(similar, 0) || similar <= maxUnrelated {
		t.Fatalf(
			"provider %q cosine relationship invalid: similar=%.6f max_unrelated=%.6f",
			plan.Provider, similar, maxUnrelated,
		)
	}
	return semanticLiveVectorMetrics{
		similarCosine: similar, maxUnrelatedCosine: maxUnrelated,
	}
}

func semanticLiveCosine(a, b []float32, normA, normB float64) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot / (normA * normB)
}

func semanticLiveCloneConfig(source *config.Config) *config.Config {
	cloned := *source
	cloned.LLM = source.LLM
	cloned.Knowledge = source.Knowledge
	cloned.LLM.Providers = make(map[string]config.LLMProviderConfig, len(source.LLM.Providers))
	for name, provider := range source.LLM.Providers {
		cloned.LLM.Providers[name] = provider
	}
	return &cloned
}

func semanticLiveFindProvider(
	cfg *config.Config,
	requested string,
) (string, config.LLMProviderConfig, bool) {
	if provider, ok := cfg.LLM.Providers[requested]; ok {
		return requested, provider, true
	}
	names := make([]string, 0, len(cfg.LLM.Providers))
	for name := range cfg.LLM.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(requested)) {
			return name, cfg.LLM.Providers[name], true
		}
	}
	return "", config.LLMProviderConfig{}, false
}

func semanticLiveEnv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func semanticLiveCloudModels() []string {
	raw := strings.TrimSpace(os.Getenv(semanticLiveCloudModelsEnv))
	if raw == "" {
		return append([]string(nil), semanticLiveDefaultCloudModels...)
	}
	models := make([]string, 0, 2)
	seen := make(map[string]struct{})
	for _, value := range strings.Split(raw, ",") {
		model := strings.TrimSpace(value)
		if model == "" {
			continue
		}
		if _, duplicate := seen[model]; duplicate {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	return models
}

func semanticLiveCloudBaseURL(configured string) string {
	if explicit := strings.TrimSpace(os.Getenv(semanticLiveCloudBaseURLEnv)); explicit != "" {
		return explicit
	}
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	return semanticLiveDefaultCloudBaseURL
}

func semanticLiveRedactedError(err error, secret string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if secret != "" {
		message = strings.ReplaceAll(message, secret, "<redacted>")
	}
	if strings.TrimSpace(message) == "" {
		return fmt.Sprintf("%T", err)
	}
	return message
}
