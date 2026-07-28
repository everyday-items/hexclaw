package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/resourcegov"
)

var (
	ErrUnsupportedKnowledgeJob      = errors.New("knowledge: unsupported semantic index job")
	ErrIncompleteRevisionBuild      = errors.New("knowledge: incomplete semantic index revision build")
	ErrEmbeddingBatchOutcomeUnknown = errors.New("knowledge: embedding batch outcome unknown; reconciliation required")
)

// SemanticIndexWorkerRepository is the durable worker protocol. Every method
// that mutates work is fenced by the exact lease epoch held by the caller.
type SemanticIndexWorkerRepository interface {
	ClaimNextJobForCorpus(ctx context.Context, ownerID, corpusID, workerID string, now time.Time, leaseDuration time.Duration) (KnowledgeJob, bool, error)
	LoadJobExecutionPlan(ctx context.Context, lease JobLease, now time.Time) (JobExecutionPlan, error)
	ListRevisionChunkInputs(ctx context.Context, lease JobLease, now time.Time, after *RevisionChunkCursor, limit int) ([]RevisionChunkInput, error)
	CreateEmbeddingBatchManifest(ctx context.Context, lease JobLease, now time.Time, manifest EmbeddingBatchManifest) (EmbeddingBatchManifest, error)
	BeginEmbeddingBatch(ctx context.Context, lease JobLease, now time.Time, batchID string) error
	MarkEmbeddingBatchOutcomeUnknown(ctx context.Context, lease JobLease, now time.Time, batchID, lastError string) error
	CommitEmbeddingBatch(ctx context.Context, lease JobLease, now time.Time, commit EmbeddingBatchCommit) error
	GarbageCollectDocument(ctx context.Context, lease JobLease, now time.Time) error
	GetRevisionBuildSummary(ctx context.Context, lease JobLease, now time.Time) (RevisionBuildSummary, error)
	SaveStageCheckpoint(ctx context.Context, lease JobLease, now time.Time, checkpoint StageCheckpoint) error
	CompleteActiveRevisionJob(ctx context.Context, lease JobLease, now time.Time, expectedContentVersion int64) error
	PrepareRevisionForPublish(ctx context.Context, lease JobLease, now time.Time, preparation RevisionPublishPreparation) error
	PublishRevisionCAS(ctx context.Context, command PublishRevisionCommand) error
	RetryJob(ctx context.Context, lease JobLease, now, nextAttempt time.Time, lastError string) (KnowledgeJob, error)
	FailJob(ctx context.Context, lease JobLease, now time.Time, lastError string) (KnowledgeJob, error)
}

type semanticIndexLeaseRenewer interface {
	RenewJobLease(ctx context.Context, lease JobLease, now time.Time, leaseDuration time.Duration) (JobLease, error)
}

type runningJobCancelRegistrar interface {
	registerRunningJobCancel(jobID string, cancel context.CancelFunc) func()
}

type ingestPageCheckpointRepository interface {
	SetIngestPageTotal(context.Context, JobLease, time.Time, string, int64) error
	LoadIngestPageCheckpoints(context.Context, JobLease, time.Time, string, int64) ([]IngestPageCheckpoint, error)
	SaveIngestPageCheckpoint(context.Context, JobLease, time.Time, IngestPageCheckpoint) error
	SaveIngestSegmentPlan(context.Context, JobLease, time.Time, string, []IngestSegmentPlan) error
}

type workerIngestPageProgress struct {
	repository ingestPageCheckpointRepository
	lease      func() JobLease
	now        func() time.Time
}

func (p workerIngestPageProgress) SetPageTotal(ctx context.Context, digest string, total int64) error {
	return p.repository.SetIngestPageTotal(ctx, p.lease(), p.now(), digest, total)
}

func (p workerIngestPageProgress) LoadCompletedPages(
	ctx context.Context,
	digest string,
	total int64,
) ([]IngestPageCheckpoint, error) {
	return p.repository.LoadIngestPageCheckpoints(ctx, p.lease(), p.now(), digest, total)
}

func (p workerIngestPageProgress) CommitPage(ctx context.Context, page IngestPageCheckpoint) error {
	return p.repository.SaveIngestPageCheckpoint(ctx, p.lease(), p.now(), page)
}

func (p workerIngestPageProgress) SaveSegmentPlan(
	ctx context.Context,
	digest string,
	segments []IngestSegmentPlan,
) error {
	return p.repository.SaveIngestSegmentPlan(ctx, p.lease(), p.now(), digest, segments)
}

type SemanticIndexWorkerConfig struct {
	OwnerID       string
	CorpusID      string
	WorkerID      string
	BatchSize     int
	LeaseDuration time.Duration
	RetryDelay    time.Duration
	MaxRetryDelay time.Duration
	MaxAttempts   int
	// EmbeddingTimeout overrides the per-batch provider budget in tests or
	// constrained deployments. Zero uses documentEmbeddingBudget(batch size).
	EmbeddingTimeout time.Duration
	Now              func() time.Time
}

// SemanticIndexWorker executes at most one durable job per RunOnce call. A
// caller-owned loop controls polling/backoff and shutdown.
type SemanticIndexWorker struct {
	repository      SemanticIndexWorkerRepository
	registry        ProfileEmbeddingExecutorRegistry
	ingestProcessor DocumentIngestProcessor
	governor        *resourcegov.Governor
	config          SemanticIndexWorkerConfig
}

type SemanticIndexWorkerOption func(*SemanticIndexWorker)

func WithSemanticWorkerResourceGovernor(governor *resourcegov.Governor) SemanticIndexWorkerOption {
	return func(worker *SemanticIndexWorker) { worker.governor = governor }
}

func NewSemanticIndexWorker(
	repository SemanticIndexWorkerRepository,
	registry ProfileEmbeddingExecutorRegistry,
	config SemanticIndexWorkerConfig,
	options ...SemanticIndexWorkerOption,
) *SemanticIndexWorker {
	if config.BatchSize <= 0 {
		config.BatchSize = 64
	}
	if config.BatchSize > 1000 {
		config.BatchSize = 1000
	}
	if config.LeaseDuration <= 0 {
		config.LeaseDuration = 5 * time.Minute
	}
	if config.RetryDelay <= 0 {
		config.RetryDelay = 30 * time.Second
	}
	if config.MaxRetryDelay <= 0 {
		config.MaxRetryDelay = 15 * time.Minute
	}
	if config.MaxRetryDelay < config.RetryDelay {
		config.MaxRetryDelay = config.RetryDelay
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 8
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	worker := &SemanticIndexWorker{repository: repository, registry: registry, config: config}
	for _, option := range options {
		if option != nil {
			option(worker)
		}
	}
	return worker
}

// SetDocumentIngestProcessor installs the off-request parser/OCR/splitter
// boundary. It is optional so semantic-index-only runtimes remain valid.
func (w *SemanticIndexWorker) SetDocumentIngestProcessor(processor DocumentIngestProcessor) {
	w.ingestProcessor = processor
}

func (w *SemanticIndexWorker) RunOnce(ctx context.Context) (bool, error) {
	if w.repository == nil || strings.TrimSpace(w.config.OwnerID) == "" ||
		strings.TrimSpace(w.config.CorpusID) == "" || strings.TrimSpace(w.config.WorkerID) == "" {
		return false, fmt.Errorf("knowledge: invalid semantic index worker configuration")
	}
	now := w.now()
	job, claimed, err := w.repository.ClaimNextJobForCorpus(
		ctx, w.config.OwnerID, w.config.CorpusID, w.config.WorkerID, now, w.config.LeaseDuration,
	)
	if err != nil || !claimed {
		return false, err
	}
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	if registrar, ok := w.repository.(runningJobCancelRegistrar); ok {
		unregister := registrar.registerRunningJobCancel(job.JobID, cancelJob)
		defer unregister()
	}
	lease := job.Lease()
	err = w.executeClaimed(jobCtx, job, &lease)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrJobFenced) {
		return true, err
	}
	failureTime := w.now()
	message := err.Error()
	transitionCtx, cancelTransition := semanticWorkerTransitionContext(ctx)
	defer cancelTransition()
	if isPermanentSemanticWorkerError(err) || job.Attempt >= w.config.MaxAttempts {
		var transitionErr error
		if structuredRepository, ok := w.repository.(interface {
			FailJobWithFailure(context.Context, JobLease, time.Time, KnowledgeJobFailure) (KnowledgeJob, error)
		}); ok && errors.Is(err, ErrVisionModelRequired) {
			_, transitionErr = structuredRepository.FailJobWithFailure(
				transitionCtx, lease, failureTime, KnowledgeJobFailureFromError(err),
			)
		} else {
			_, transitionErr = w.repository.FailJob(transitionCtx, lease, failureTime, message)
		}
		if transitionErr != nil {
			if errors.Is(transitionErr, ErrJobFenced) {
				return true, ErrJobFenced
			}
			return true, errors.Join(err, transitionErr)
		}
		return true, err
	}
	if _, transitionErr := w.repository.RetryJob(
		transitionCtx, lease, failureTime,
		failureTime.Add(cappedSemanticRetryDelay(w.config.RetryDelay, w.config.MaxRetryDelay, job.Attempt)),
		message,
	); transitionErr != nil {
		if errors.Is(transitionErr, ErrJobFenced) {
			return true, ErrJobFenced
		}
		return true, errors.Join(err, transitionErr)
	}
	return true, err
}

func (w *SemanticIndexWorker) executeClaimed(ctx context.Context, job KnowledgeJob, lease *JobLease) error {
	if job.Kind == KnowledgeJobIngest {
		return w.executeIngest(ctx, job, lease)
	}
	if job.Kind == KnowledgeJobGC {
		return w.repository.GarbageCollectDocument(ctx, *lease, w.now())
	}
	switch job.Kind {
	case KnowledgeJobRebuildRevision, KnowledgeJobEmbedDocument:
		// supported below
	default:
		return fmt.Errorf("%w: kind=%s", ErrUnsupportedKnowledgeJob, job.Kind)
	}
	if w.registry == nil {
		return fmt.Errorf("knowledge: embedding executor registry is not configured")
	}
	plan, err := w.repository.LoadJobExecutionPlan(ctx, *lease, w.now())
	if err != nil {
		return err
	}
	if err := plan.Snapshot.Validate(); err != nil {
		return err
	}
	executor, err := w.registry.ExecutorForProfile(ctx, plan.Snapshot)
	if err != nil {
		return err
	}
	initial, err := w.repository.GetRevisionBuildSummary(ctx, *lease, w.now())
	if err != nil {
		return err
	}
	done, total := initial.EmbeddedChunks, initial.ExpectedChunks
	var cursor *RevisionChunkCursor
	batchSize := w.config.BatchSize
	executionProfile, profileScoped := EmbeddingExecutionProfileForModel(plan.Snapshot.Profile.ModelName)
	if profileScoped && executionProfile.BatchMaxCount > 0 &&
		batchSize > executionProfile.BatchMaxCount {
		batchSize = executionProfile.BatchMaxCount
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.renewLease(ctx, lease); err != nil {
			return err
		}
		inputs, err := w.repository.ListRevisionChunkInputs(ctx, *lease, w.now(), cursor, batchSize)
		if err != nil {
			return err
		}
		if len(inputs) == 0 {
			break
		}
		texts := make([]string, len(inputs))
		for i, input := range inputs {
			texts[i] = strings.TrimSpace(input.Content)
			if texts[i] == "" {
				return fmt.Errorf("%w: empty chunk %q", ErrInvalidEmbeddingResult, input.ChunkID)
			}
			if profileScoped {
				texts[i] = clampRunes(texts[i], executionProfile.MaxInputRunes)
			}
		}
		manifestInput := makeEmbeddingBatchManifest(job.JobID, plan, inputs, texts)
		manifest, err := w.repository.CreateEmbeddingBatchManifest(ctx, *lease, w.now(), manifestInput)
		if err != nil {
			return err
		}
		embeddingTimeout := w.config.EmbeddingTimeout
		if embeddingTimeout <= 0 {
			if profileScoped {
				embeddingTimeout = executionProfile.BatchTimeout
			} else {
				embeddingTimeout = documentEmbeddingBudget(len(texts))
			}
		}
		// Use the repository-returned manifest as the authority: a durable store
		// may resume or canonicalize an existing batch identity after restart.
		// The provider transport forwards this key as Idempotency-Key; providers
		// that ignore it remain at-least-once.
		vectors, providerBegan, err := w.invokeEmbeddingBatch(
			ctx, lease, manifest, executor, texts, embeddingTimeout,
		)
		if err != nil {
			if providerBegan && isEmbeddingBatchOutcomeUnknown(err) {
				transitionCtx, cancelTransition := semanticWorkerTransitionContext(ctx)
				transitionErr := w.repository.MarkEmbeddingBatchOutcomeUnknown(
					transitionCtx, *lease, w.now(), manifest.BatchID, err.Error(),
				)
				cancelTransition()
				if transitionErr != nil {
					return errors.Join(err, transitionErr)
				}
				return errors.Join(ErrEmbeddingBatchOutcomeUnknown, err)
			}
			return err
		}
		// Provider latency consumes lease time. Renew immediately before the
		// fenced commit so a healthy but slow batch does not expire between the
		// network response and its atomic write.
		if err := w.renewLease(ctx, lease); err != nil {
			return err
		}
		revisionVectors, err := validateProviderBatch(plan, inputs, vectors)
		if err != nil {
			return err
		}
		done += int64(len(revisionVectors))
		if done > total {
			return fmt.Errorf("%w: progress %d/%d", ErrIncompleteRevisionBuild, done, total)
		}
		if err := w.repository.CommitEmbeddingBatch(ctx, *lease, w.now(), EmbeddingBatchCommit{
			BatchID: manifest.BatchID, Vectors: revisionVectors,
			ChunksDone: done, ChunksTotal: total,
		}); err != nil {
			return err
		}
		last := inputs[len(inputs)-1].Cursor
		cursor = &last
	}

	summary, err := w.repository.GetRevisionBuildSummary(ctx, *lease, w.now())
	if err != nil {
		return err
	}
	if summary.ExpectedChunks != summary.EmbeddedChunks || summary.FailedChunks != 0 ||
		summary.ExpectedChunks != total {
		return fmt.Errorf("%w: expected=%d embedded=%d failed=%d",
			ErrIncompleteRevisionBuild, summary.ExpectedChunks, summary.EmbeddedChunks, summary.FailedChunks)
	}
	checkpoint := StageCheckpoint{
		Stage:            JobStageEmbedding,
		InputFingerprint: semanticWorkerFingerprint(plan),
		ArtifactRef:      "revision://" + plan.RevisionID,
		ArtifactDigest:   summary.ChunkSetDigest,
		State:            StageCheckpointSucceeded,
	}
	if err := w.repository.SaveStageCheckpoint(ctx, *lease, w.now(), checkpoint); err != nil {
		return err
	}
	if job.Kind == KnowledgeJobEmbedDocument {
		return w.repository.CompleteActiveRevisionJob(ctx, *lease, w.now(), plan.ContentVersion)
	}
	if err := w.repository.PrepareRevisionForPublish(ctx, *lease, w.now(), RevisionPublishPreparation{
		IndexedThroughVersion: plan.ContentVersion,
		ChunkSetDigest:        summary.ChunkSetDigest,
		ExpectedChunks:        summary.ExpectedChunks,
	}); err != nil {
		return err
	}
	return w.repository.PublishRevisionCAS(ctx, PublishRevisionCommand{
		Lease: *lease, Now: w.now(), OwnerID: w.config.OwnerID, CorpusID: w.config.CorpusID,
		RevisionID: plan.RevisionID, ExpectedPolicyVersion: plan.PolicyVersion,
		ExpectedActiveRevisionID: plan.PreviousActiveRevisionID,
		ExpectedContentVersion:   plan.ContentVersion,
	})
}

func (w *SemanticIndexWorker) invokeEmbeddingBatch(
	ctx context.Context,
	lease *JobLease,
	manifest EmbeddingBatchManifest,
	executor ProfileEmbeddingExecutor,
	texts []string,
	timeout time.Duration,
) (vectors [][]float32, providerBegan bool, err error) {
	var permit *resourcegov.Permit
	if w.governor != nil {
		permit, err = w.governor.Acquire(
			ctx, resourcegov.ResourceAccelerator, resourcegov.PriorityBackground,
		)
		if err != nil {
			return nil, false, err
		}
		defer permit.Release()
		// Resource wait may consume most of a lease. Fence again before the
		// durable provider boundary so a stale worker never starts a call.
		if err = w.renewLease(ctx, lease); err != nil {
			return nil, false, err
		}
	}
	if err = w.repository.BeginEmbeddingBatch(ctx, *lease, w.now(), manifest.BatchID); err != nil {
		return nil, false, err
	}
	providerBegan = true
	embedRequestCtx := withEmbeddingBatchClientRequestKey(
		ragEmbedContext(ctx), manifest.ClientRequestKey,
	)
	embedCtx, cancelEmbed := context.WithTimeout(embedRequestCtx, timeout)
	defer cancelEmbed()
	vectors, err = executor.EmbedForPurpose(embedCtx, EmbeddingPurposeDocument, texts)
	return vectors, providerBegan, err
}

func (w *SemanticIndexWorker) executeIngest(ctx context.Context, job KnowledgeJob, lease *JobLease) error {
	repository, ok := w.repository.(documentIngestWorkerRepository)
	if !ok || w.ingestProcessor == nil || job.DocumentID == "" {
		return fmt.Errorf("%w: ingest processor unavailable", ErrUnsupportedKnowledgeJob)
	}
	var source PersistedIngestDocument
	var err error
	if jobRepository, ok := w.repository.(jobScopedDocumentIngestRepository); ok {
		source, err = jobRepository.GetIngestDocumentForJob(
			ctx, job.OwnerID, job.CorpusUID, job.DocumentID, job.JobID,
		)
	} else {
		source, err = repository.GetIngestDocumentForCorpusUID(
			ctx, job.OwnerID, job.CorpusUID, job.DocumentID,
		)
	}
	if err != nil {
		return err
	}
	if source.CorpusUID != job.CorpusUID || source.ContentGeneration != job.DocumentGeneration {
		return ErrJobFenced
	}
	if err := repository.SaveJobProgress(ctx, *lease, w.now(), JobProgressUpdate{Stage: JobStageExtracting}); err != nil {
		return err
	}

	prepared, err := w.prepareIngestWithHeartbeat(ctx, lease, source)
	if err != nil {
		return err
	}
	pages := prepared.PageCount
	if err := repository.SaveJobProgress(ctx, *lease, w.now(), JobProgressUpdate{
		Stage: JobStageChunking, PagesDone: &pages, PagesTotal: &pages,
	}); err != nil {
		return err
	}
	if err := w.renewLease(ctx, lease); err != nil {
		return err
	}
	return repository.CompleteIngestDocument(ctx, *lease, w.now(), prepared)
}

func (w *SemanticIndexWorker) prepareIngestWithHeartbeat(
	ctx context.Context,
	lease *JobLease,
	source PersistedIngestDocument,
) (PreparedIngestDocument, error) {
	interval := w.config.LeaseDuration / 3
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	prepareCtx, cancelPrepare := context.WithCancel(ctx)
	defer cancelPrepare()
	if source.VisionRoute != nil {
		prepareCtx = WithVisionRouteSnapshot(prepareCtx, *source.VisionRoute)
	}

	type leaseState struct {
		sync.Mutex
		lease JobLease
	}
	state := &leaseState{lease: *lease}
	heartbeatDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-prepareCtx.Done():
				heartbeatDone <- nil
				return
			case <-ticker.C:
				state.Lock()
				current := state.lease
				state.Unlock()
				renewer, ok := w.repository.(semanticIndexLeaseRenewer)
				if !ok {
					continue
				}
				renewed, err := renewer.RenewJobLease(prepareCtx, current, w.now(), w.config.LeaseDuration)
				if err != nil {
					if prepareCtx.Err() != nil {
						heartbeatDone <- nil
						return
					}
					heartbeatDone <- err
					cancelPrepare()
					return
				}
				state.Lock()
				state.lease = renewed
				state.Unlock()
			}
		}
	}()
	var prepared PreparedIngestDocument
	var prepareErr error
	if resumable, ok := w.ingestProcessor.(ResumableDocumentIngestProcessor); ok {
		pageRepository, repositoryOK := w.repository.(ingestPageCheckpointRepository)
		if !repositoryOK {
			prepareErr = fmt.Errorf("%w: page checkpoint repository unavailable", ErrUnsupportedKnowledgeJob)
		} else {
			progress := workerIngestPageProgress{
				repository: pageRepository,
				lease: func() JobLease {
					state.Lock()
					defer state.Unlock()
					return state.lease
				},
				now: w.now,
			}
			prepared, prepareErr = resumable.PrepareResumable(prepareCtx, source, progress)
		}
	} else {
		prepared, prepareErr = w.ingestProcessor.Prepare(prepareCtx, source)
	}
	cancelPrepare()
	heartbeatErr := <-heartbeatDone
	state.Lock()
	*lease = state.lease
	state.Unlock()
	if heartbeatErr != nil {
		return PreparedIngestDocument{}, heartbeatErr
	}
	return prepared, prepareErr
}

func (w *SemanticIndexWorker) renewLease(ctx context.Context, lease *JobLease) error {
	renewer, ok := w.repository.(semanticIndexLeaseRenewer)
	if !ok {
		return nil
	}
	renewed, err := renewer.RenewJobLease(ctx, *lease, w.now(), w.config.LeaseDuration)
	if err != nil {
		return err
	}
	*lease = renewed
	return nil
}

func (w *SemanticIndexWorker) now() time.Time { return w.config.Now().UTC() }

func isPermanentSemanticWorkerError(err error) bool {
	return errors.Is(err, ErrUnsupportedKnowledgeJob) ||
		errors.Is(err, ErrVisionModelRequired) ||
		errors.Is(err, ErrEmbeddingBatchOutcomeUnknown) ||
		errors.Is(err, ErrInvalidDocumentUpload) ||
		errors.Is(err, ErrProfileUnavailable) ||
		errors.Is(err, ErrInvalidEmbeddingResult) ||
		errors.Is(err, ErrInvalidRevisionVector) ||
		errors.Is(err, ErrIncompleteRevisionBuild)
}

func isEmbeddingBatchOutcomeUnknown(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

const semanticWorkerTransitionTimeout = 5 * time.Second

func semanticWorkerTransitionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	// Request cancellation (including the narrow race immediately after an
	// Err() check) must not strand a durable running lease until full expiry.
	// Detach only the short, repository-local fenced transition; it cannot
	// perform provider work or outlive this fixed budget.
	return context.WithTimeout(context.WithoutCancel(ctx), semanticWorkerTransitionTimeout)
}

func cappedSemanticRetryDelay(base, maximum time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	if maximum < base {
		maximum = base
	}
	delay := base
	for i := 1; i < attempt; i++ {
		if delay >= maximum || delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func makeEmbeddingBatchManifest(
	jobID string,
	plan JobExecutionPlan,
	inputs []RevisionChunkInput,
	texts []string,
) EmbeddingBatchManifest {
	chunkHasher := sha256.New()
	payloadHasher := sha256.New()
	chunks := make([]EmbeddingBatchChunk, len(inputs))
	for i, input := range inputs {
		// A staged rebuild keeps its durable job ID when corpus content changes.
		// Include the immutable document generation in both digests so a new
		// generation can never replay a succeeded manifest from superseded input,
		// even when the ingestion layer reuses the same chunk ID and text.
		_, _ = fmt.Fprintf(chunkHasher, "%d\x00%s\x00%d\x00%s\x00%d\x00%s\n",
			i, input.DocumentID, input.ContentGeneration, input.ChunkID, input.ChunkIndex, input.ContentHash)
		_, _ = fmt.Fprintf(payloadHasher, "%d\x00%s\x00%d\x00%s\x00%d\x00%s\x00%s\n",
			i, input.DocumentID, input.ContentGeneration, input.ChunkID, input.ChunkIndex, input.ContentHash, texts[i])
		chunks[i] = EmbeddingBatchChunk{Ordinal: i, ChunkID: input.ChunkID, ContentHash: input.ContentHash}
	}
	chunkDigest := hex.EncodeToString(chunkHasher.Sum(nil))
	payloadDigest := hex.EncodeToString(payloadHasher.Sum(nil))
	requestHash := sha256.Sum256([]byte(strings.Join([]string{
		jobID, plan.RevisionID, plan.Snapshot.ProfileConfigHash, chunkDigest, payloadDigest,
	}, "\x00")))
	return EmbeddingBatchManifest{
		ChunkIDsDigest:   chunkDigest,
		PayloadDigest:    payloadDigest,
		ClientRequestKey: "kb-embed-" + hex.EncodeToString(requestHash[:]),
		Chunks:           chunks,
	}
}

func validateProviderBatch(
	plan JobExecutionPlan,
	inputs []RevisionChunkInput,
	vectors [][]float32,
) ([]RevisionVector, error) {
	if len(vectors) != len(inputs) {
		return nil, fmt.Errorf("%w: vectors=%d inputs=%d", ErrInvalidEmbeddingResult, len(vectors), len(inputs))
	}
	result := make([]RevisionVector, len(inputs))
	for i, vector := range vectors {
		if len(vector) != plan.Snapshot.Profile.Dimension {
			return nil, fmt.Errorf("%w: chunk=%q dimension=%d want=%d",
				ErrInvalidEmbeddingResult, inputs[i].ChunkID, len(vector), plan.Snapshot.Profile.Dimension)
		}
		for _, value := range vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return nil, fmt.Errorf("%w: chunk=%q contains non-finite value",
					ErrInvalidEmbeddingResult, inputs[i].ChunkID)
			}
		}
		result[i] = RevisionVector{
			DocumentID: inputs[i].DocumentID, ContentGeneration: inputs[i].ContentGeneration,
			ChunkID: inputs[i].ChunkID, ChunkIndex: inputs[i].ChunkIndex,
			ContentHash: inputs[i].ContentHash, Values: vector,
		}
	}
	return result, nil
}

func semanticWorkerFingerprint(plan JobExecutionPlan) string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s",
		plan.RevisionID, plan.Snapshot.ProfileConfigHash, plan.ContentVersion, plan.Snapshot.ChunkConfigHash)))
	return hex.EncodeToString(hash[:])
}
