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

	"github.com/hexagon-codes/hexclaw/localinfer"
	"github.com/hexagon-codes/hexclaw/resourcegov"
	"github.com/hexagon-codes/toolkit/util/logger"
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
	repository  ingestPageCheckpointRepository
	invocations OCRPageInvocationProgress
	source      PersistedIngestDocument
	lease       func() JobLease
	now         func() time.Time
}

func (p workerIngestPageProgress) SetPageTotal(ctx context.Context, digest string, total int64) error {
	lease := p.lease()
	if err := p.repository.SetIngestPageTotal(ctx, lease, p.now(), digest, total); err != nil {
		return err
	}
	logger.Info("[knowledge] job stage changed",
		"job_id", lease.JobID,
		"owner_id", lease.OwnerID,
		"corpus_uid", lease.CorpusUID,
		"document_id", p.source.DocumentID,
		"document_generation", p.source.ContentGeneration,
		"corpus_alias", p.source.CorpusAlias,
		"filename", p.source.Filename,
		"storage_path", p.source.StoragePath,
		"media_type", p.source.MediaType,
		"size_bytes", p.source.SizeBytes,
		"sha256", p.source.SHA256,
		"agent_id", p.source.AgentID,
		"learner_id", p.source.LearnerID,
		"subject", p.source.Subject,
		"grade", p.source.Grade,
		"vision_route", p.source.VisionRoute,
		"stage", JobStageOCR,
		"page_digest", digest,
		"pages_total", total,
	)
	return nil
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

func (p workerIngestPageProgress) ClaimOCRPageInvocation(
	ctx context.Context,
	_lease JobLease,
	_now time.Time,
	claim OCRPageInvocationClaim,
) (OCRPageInvocation, error) {
	if p.invocations == nil {
		return OCRPageInvocation{}, ErrOCRPageInvocationLedgerUnavailable
	}
	return p.invocations.ClaimOCRPageInvocation(ctx, p.lease(), p.now(), claim)
}

func (p workerIngestPageProgress) ClaimOCRPageInvocationContext(
	ctx context.Context,
	claim OCRPageInvocationClaim,
) (OCRPageInvocation, error) {
	return p.ClaimOCRPageInvocation(ctx, p.lease(), p.now(), claim)
}

func (p workerIngestPageProgress) SaveOCRPageInvocation(
	ctx context.Context,
	_lease JobLease,
	_now time.Time,
	invocation OCRPageInvocation,
	result OCRPageInvocationResult,
) error {
	if p.invocations == nil {
		return ErrOCRPageInvocationLedgerUnavailable
	}
	return p.invocations.SaveOCRPageInvocation(ctx, p.lease(), p.now(), invocation, result)
}

func (p workerIngestPageProgress) SaveOCRPageInvocationContext(
	ctx context.Context,
	invocation OCRPageInvocation,
	result OCRPageInvocationResult,
) error {
	return p.SaveOCRPageInvocation(ctx, p.lease(), p.now(), invocation, result)
}

func (p workerIngestPageProgress) MarkOCRPageInvocationOutcomeUnknown(
	ctx context.Context,
	_lease JobLease,
	_now time.Time,
	invocation OCRPageInvocation,
	lastError string,
) error {
	marker, ok := p.invocations.(OCRPageInvocationOutcomeMarker)
	if !ok {
		return ErrOCRPageInvocationLedgerUnavailable
	}
	return marker.MarkOCRPageInvocationOutcomeUnknown(ctx, p.lease(), p.now(), invocation, lastError)
}

func (p workerIngestPageProgress) MarkOCRPageInvocationOutcomeUnknownContext(
	ctx context.Context,
	invocation OCRPageInvocation,
	lastError string,
) error {
	return p.MarkOCRPageInvocationOutcomeUnknown(ctx, p.lease(), p.now(), invocation, lastError)
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
	localInference  *localinfer.Coordinator
	config          SemanticIndexWorkerConfig
}

type SemanticIndexWorkerOption func(*SemanticIndexWorker)

func WithSemanticWorkerResourceGovernor(governor *resourcegov.Governor) SemanticIndexWorkerOption {
	return func(worker *SemanticIndexWorker) { worker.governor = governor }
}

func WithSemanticWorkerLocalInferenceCoordinator(coordinator *localinfer.Coordinator) SemanticIndexWorkerOption {
	return func(worker *SemanticIndexWorker) { worker.localInference = coordinator }
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
	if err != nil {
		logger.Error("[knowledge] job claim failed",
			"owner_id", w.config.OwnerID,
			"corpus_id", w.config.CorpusID,
			"worker_id", w.config.WorkerID,
			"error", err,
		)
		return false, err
	}
	if !claimed {
		return false, err
	}
	startedAt := time.Now()
	logger.Info("[knowledge] job claimed",
		"job_id", job.JobID,
		"parent_job_id", job.ParentJobID,
		"job_kind", job.Kind,
		"owner_id", job.OwnerID,
		"corpus_uid", job.CorpusUID,
		"corpus_id", w.config.CorpusID,
		"document_id", job.DocumentID,
		"document_generation", job.DocumentGeneration,
		"revision_id", job.TargetRevisionID,
		"stage", job.Stage,
		"attempt", job.Attempt,
		"next_attempt_at", job.NextAttemptAt,
		"worker_id", job.LeaseOwner,
		"lease_epoch", job.LeaseEpoch,
		"lease_expires_at", job.LeaseExpiresAt,
		"heartbeat_at", job.HeartbeatAt,
		"created_at", job.CreatedAt,
		"updated_at", job.UpdatedAt,
	)
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	if registrar, ok := w.repository.(runningJobCancelRegistrar); ok {
		unregister := registrar.registerRunningJobCancel(job.JobID, cancelJob)
		defer unregister()
	}
	lease := job.Lease()
	err = w.executeClaimed(jobCtx, job, &lease)
	if err == nil {
		var terminal KnowledgeJob
		terminalRead := false
		var terminalReadErr error
		if jobReader, ok := w.repository.(interface {
			GetJob(context.Context, string, string) (KnowledgeJob, error)
		}); ok {
			readCtx, cancelRead := context.WithTimeout(ctx, time.Second)
			persisted, readErr := jobReader.GetJob(readCtx, job.OwnerID, job.JobID)
			cancelRead()
			terminalReadErr = readErr
			if readErr == nil {
				terminal = persisted
				terminalRead = true
			}
		}
		if !terminalRead {
			logger.Info("[knowledge] job completed",
				"job_id", job.JobID,
				"parent_job_id", job.ParentJobID,
				"owner_id", job.OwnerID,
				"corpus_uid", job.CorpusUID,
				"document_id", job.DocumentID,
				"document_generation", job.DocumentGeneration,
				"revision_id", job.TargetRevisionID,
				"elapsed_ms", time.Since(startedAt).Milliseconds(),
				"terminal_read_error", terminalReadErr,
			)
			return true, nil
		}
		var pagesDone, pagesTotal, chunksDone, chunksTotal any
		if terminal.PagesDone != nil {
			pagesDone = *terminal.PagesDone
		}
		if terminal.PagesTotal != nil {
			pagesTotal = *terminal.PagesTotal
		}
		if terminal.ChunksDone != nil {
			chunksDone = *terminal.ChunksDone
		}
		if terminal.ChunksTotal != nil {
			chunksTotal = *terminal.ChunksTotal
		}
		logger.Info("[knowledge] job completed",
			"job_id", terminal.JobID,
			"parent_job_id", terminal.ParentJobID,
			"job_kind", terminal.Kind,
			"owner_id", terminal.OwnerID,
			"corpus_uid", terminal.CorpusUID,
			"document_id", terminal.DocumentID,
			"document_generation", terminal.DocumentGeneration,
			"revision_id", terminal.TargetRevisionID,
			"state", terminal.State,
			"stage", terminal.Stage,
			"elapsed_ms", time.Since(startedAt).Milliseconds(),
			"pages_done", pagesDone,
			"pages_total", pagesTotal,
			"chunks_done", chunksDone,
			"chunks_total", chunksTotal,
			"attempt", terminal.Attempt,
			"next_attempt_at", terminal.NextAttemptAt,
			"last_error", terminal.LastError,
			"failure", terminal.Failure,
			"created_at", terminal.CreatedAt,
			"updated_at", terminal.UpdatedAt,
		)
		return true, nil
	}
	if errors.Is(err, ErrJobFenced) {
		logger.Warn("[knowledge] job lease fenced",
			"job_id", job.JobID,
			"parent_job_id", job.ParentJobID,
			"job_kind", job.Kind,
			"owner_id", job.OwnerID,
			"corpus_uid", job.CorpusUID,
			"document_id", job.DocumentID,
			"document_generation", job.DocumentGeneration,
			"revision_id", job.TargetRevisionID,
			"stage", job.Stage,
			"attempt", job.Attempt,
			"elapsed_ms", time.Since(startedAt).Milliseconds(),
			"error", err,
		)
		return true, err
	}
	failureTime := w.now()
	message := err.Error()
	transitionCtx, cancelTransition := semanticWorkerTransitionContext(ctx)
	defer cancelTransition()
	if isPermanentSemanticWorkerError(err) || job.Attempt >= w.config.MaxAttempts {
		var failed KnowledgeJob
		var transitionErr error
		if structuredRepository, ok := w.repository.(interface {
			FailJobWithFailure(context.Context, JobLease, time.Time, KnowledgeJobFailure) (KnowledgeJob, error)
		}); ok && errors.Is(err, ErrVisionModelRequired) {
			failed, transitionErr = structuredRepository.FailJobWithFailure(
				transitionCtx, lease, failureTime, KnowledgeJobFailureFromError(err),
			)
		} else {
			failed, transitionErr = w.repository.FailJob(transitionCtx, lease, failureTime, message)
		}
		if transitionErr != nil {
			logger.Error("[knowledge] job failure transition failed",
				"job_id", job.JobID,
				"parent_job_id", job.ParentJobID,
				"owner_id", job.OwnerID,
				"corpus_uid", job.CorpusUID,
				"document_id", job.DocumentID,
				"document_generation", job.DocumentGeneration,
				"revision_id", job.TargetRevisionID,
				"job_error", err,
				"transition_error", transitionErr,
			)
			if errors.Is(transitionErr, ErrJobFenced) {
				return true, ErrJobFenced
			}
			return true, errors.Join(err, transitionErr)
		}
		failureCode := "job_failed"
		if failed.Failure != nil && failed.Failure.Code != "" {
			failureCode = failed.Failure.Code
		}
		logger.Warn("[knowledge] job failed",
			"job_id", failed.JobID,
			"parent_job_id", failed.ParentJobID,
			"job_kind", failed.Kind,
			"owner_id", failed.OwnerID,
			"corpus_uid", failed.CorpusUID,
			"document_id", failed.DocumentID,
			"document_generation", failed.DocumentGeneration,
			"revision_id", failed.TargetRevisionID,
			"state", failed.State,
			"stage", failed.Stage,
			"attempt", failed.Attempt,
			"failure_code", failureCode,
			"last_error", failed.LastError,
			"failure", failed.Failure,
			"elapsed_ms", time.Since(startedAt).Milliseconds(),
			"error", err,
		)
		return true, err
	}
	retried, transitionErr := w.repository.RetryJob(
		transitionCtx, lease, failureTime,
		failureTime.Add(cappedSemanticRetryDelay(w.config.RetryDelay, w.config.MaxRetryDelay, job.Attempt)),
		message,
	)
	if transitionErr != nil {
		logger.Error("[knowledge] job retry transition failed",
			"job_id", job.JobID,
			"parent_job_id", job.ParentJobID,
			"owner_id", job.OwnerID,
			"corpus_uid", job.CorpusUID,
			"document_id", job.DocumentID,
			"document_generation", job.DocumentGeneration,
			"revision_id", job.TargetRevisionID,
			"job_error", err,
			"transition_error", transitionErr,
		)
		if errors.Is(transitionErr, ErrJobFenced) {
			return true, ErrJobFenced
		}
		return true, errors.Join(err, transitionErr)
	}
	logger.Warn("[knowledge] job retry scheduled",
		"job_id", retried.JobID,
		"parent_job_id", retried.ParentJobID,
		"job_kind", retried.Kind,
		"owner_id", retried.OwnerID,
		"corpus_uid", retried.CorpusUID,
		"document_id", retried.DocumentID,
		"document_generation", retried.DocumentGeneration,
		"revision_id", retried.TargetRevisionID,
		"state", retried.State,
		"stage", retried.Stage,
		"attempt", retried.Attempt,
		"next_attempt_at", retried.NextAttemptAt,
		"last_error", retried.LastError,
		"elapsed_ms", time.Since(startedAt).Milliseconds(),
		"error", err,
	)
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
	logger.Info("[knowledge] job stage changed",
		"job_id", job.JobID,
		"parent_job_id", job.ParentJobID,
		"job_kind", job.Kind,
		"owner_id", job.OwnerID,
		"corpus_uid", plan.CorpusUID,
		"corpus_alias", plan.CorpusAlias,
		"document_id", job.DocumentID,
		"document_generation", job.DocumentGeneration,
		"revision_id", plan.RevisionID,
		"previous_active_revision_id", plan.PreviousActiveRevisionID,
		"policy_version", plan.PolicyVersion,
		"base_content_version", plan.BaseContentVersion,
		"content_version", plan.ContentVersion,
		"embedding_snapshot_id", plan.Snapshot.SnapshotID,
		"embedding_profile_id", plan.Snapshot.Profile.ProfileID,
		"embedding_provider_id", plan.Snapshot.Profile.ProviderID,
		"embedding_provider_name", plan.Snapshot.Profile.ProviderName,
		"embedding_model", plan.Snapshot.Profile.ModelName,
		"embedding_location", plan.Snapshot.Profile.Location,
		"embedding_dimension", plan.Snapshot.Profile.Dimension,
		"profile_config_hash", plan.Snapshot.ProfileConfigHash,
		"chunk_config_hash", plan.Snapshot.ChunkConfigHash,
		"stage", JobStageEmbedding,
		"chunks_done", done,
		"chunks_total", total,
	)
	var progressLogMu sync.Mutex
	activeBatchID := ""
	heartbeatStartedAt := time.Now()
	lastProgressLogAt := heartbeatStartedAt
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	heartbeatStopped := make(chan struct{})
	go func() {
		defer close(heartbeatStopped)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				jobReader, ok := w.repository.(interface {
					GetJob(context.Context, string, string) (KnowledgeJob, error)
				})
				if !ok {
					continue
				}
				readCtx, cancelRead := context.WithTimeout(heartbeatCtx, time.Second)
				persisted, readErr := jobReader.GetJob(readCtx, job.OwnerID, job.JobID)
				cancelRead()
				if readErr != nil {
					logger.Warn("[knowledge] job heartbeat read failed",
						"job_id", job.JobID,
						"parent_job_id", job.ParentJobID,
						"owner_id", job.OwnerID,
						"corpus_uid", plan.CorpusUID,
						"document_id", job.DocumentID,
						"document_generation", job.DocumentGeneration,
						"revision_id", plan.RevisionID,
						"error", readErr,
					)
					continue
				}
				logNow := time.Now()
				progressLogMu.Lock()
				if logNow.Sub(lastProgressLogAt) < 30*time.Second {
					progressLogMu.Unlock()
					continue
				}
				batchID := activeBatchID
				lastProgressLogAt = logNow
				progressLogMu.Unlock()
				var chunksDone, chunksTotal any
				if persisted.ChunksDone != nil {
					chunksDone = *persisted.ChunksDone
				}
				if persisted.ChunksTotal != nil {
					chunksTotal = *persisted.ChunksTotal
				}
				logger.Info("[knowledge] job heartbeat",
					"job_id", persisted.JobID,
					"parent_job_id", persisted.ParentJobID,
					"job_kind", persisted.Kind,
					"owner_id", persisted.OwnerID,
					"corpus_uid", plan.CorpusUID,
					"corpus_alias", plan.CorpusAlias,
					"document_id", persisted.DocumentID,
					"document_generation", persisted.DocumentGeneration,
					"revision_id", persisted.TargetRevisionID,
					"batch_id", batchID,
					"embedding_profile_id", plan.Snapshot.Profile.ProfileID,
					"embedding_provider_id", plan.Snapshot.Profile.ProviderID,
					"embedding_model", plan.Snapshot.Profile.ModelName,
					"stage", persisted.Stage,
					"elapsed_ms", time.Since(heartbeatStartedAt).Milliseconds(),
					"chunks_done", chunksDone,
					"chunks_total", chunksTotal,
				)
			}
		}
	}()
	defer func() {
		cancelHeartbeat()
		<-heartbeatStopped
	}()
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
		progressLogMu.Lock()
		activeBatchID = manifest.BatchID
		progressLogMu.Unlock()
		vectors, providerBegan, err := w.invokeEmbeddingBatch(
			ctx, lease, manifest, executor, texts, embeddingTimeout,
			plan.Snapshot.Profile.Location == ProviderLocationLocal,
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
		logNow := time.Now()
		progressLogMu.Lock()
		logBatch := done == total || logNow.Sub(lastProgressLogAt) >= 30*time.Second
		if logBatch {
			lastProgressLogAt = logNow
		}
		progressLogMu.Unlock()
		if logBatch {
			logger.Info("[knowledge] embedding batch committed",
				"job_id", job.JobID,
				"parent_job_id", job.ParentJobID,
				"job_kind", job.Kind,
				"owner_id", job.OwnerID,
				"corpus_uid", plan.CorpusUID,
				"corpus_alias", plan.CorpusAlias,
				"document_id", job.DocumentID,
				"document_generation", job.DocumentGeneration,
				"revision_id", plan.RevisionID,
				"batch_id", manifest.BatchID,
				"batch_state", manifest.State,
				"batch_attempts", manifest.Attempts,
				"batch_lease_epoch", manifest.LeaseEpoch,
				"client_request_key", manifest.ClientRequestKey,
				"provider_request_id", manifest.ProviderRequestID,
				"profile_config_hash", manifest.ProfileConfigHash,
				"chunk_ids_digest", manifest.ChunkIDsDigest,
				"payload_digest", manifest.PayloadDigest,
				"batch_chunks", manifest.Chunks,
				"embedding_provider_id", plan.Snapshot.Profile.ProviderID,
				"embedding_model", plan.Snapshot.Profile.ModelName,
				"stage", JobStageEmbedding,
				"elapsed_ms", time.Since(heartbeatStartedAt).Milliseconds(),
				"chunks_done", done,
				"chunks_total", total,
			)
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
	logger.Info("[knowledge] job stage changed",
		"job_id", job.JobID,
		"parent_job_id", job.ParentJobID,
		"job_kind", job.Kind,
		"owner_id", job.OwnerID,
		"corpus_uid", plan.CorpusUID,
		"corpus_alias", plan.CorpusAlias,
		"document_id", job.DocumentID,
		"document_generation", job.DocumentGeneration,
		"revision_id", plan.RevisionID,
		"previous_active_revision_id", plan.PreviousActiveRevisionID,
		"policy_version", plan.PolicyVersion,
		"content_version", plan.ContentVersion,
		"chunk_set_digest", summary.ChunkSetDigest,
		"indexed_through_version", summary.IndexedThroughVersion,
		"stage", JobStagePublishing,
		"chunks_done", summary.EmbeddedChunks,
		"chunks_total", summary.ExpectedChunks,
	)
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
	local bool,
) (vectors [][]float32, providerBegan bool, err error) {
	// Readiness may perform its own physical probe. Complete that call before
	// creating the provider prelease, whose single borrow belongs exclusively to
	// the real embedding request after the durable Begin boundary.
	if readiness, ok := executor.(ProfileEmbeddingExecutorReadiness); ok {
		readinessCtx := ctx
		if local {
			readinessCtx = localinfer.WithOperation(readinessCtx, localinfer.OperationProbe)
		}
		if !readiness.EmbeddingReady(readinessCtx) {
			return nil, false, ErrEmbeddingUnavailable
		}
	}
	var (
		permit         *resourcegov.Permit
		inferenceLease *localinfer.Lease
	)
	if w.localInference != nil && local {
		ctx, inferenceLease, err = w.localInference.Acquire(
			localinfer.WithOperation(ctx, localinfer.OperationDocumentEmbedding),
			localinfer.OperationDocumentEmbedding,
		)
		if err != nil {
			return nil, false, err
		}
		defer func() {
			if len(vectors) > 0 {
				inferenceLease.MarkFirstOutput()
			}
			inferenceLease.Finish(err)
		}()
	} else if w.localInference == nil && w.governor != nil && local {
		permit, err = w.governor.Acquire(
			ctx, resourcegov.ResourceAccelerator, resourcegov.PriorityBackground,
		)
		if err != nil {
			return nil, false, err
		}
		defer permit.Release()
	}
	// Admission can consume most of a lease, while cloud calls intentionally
	// bypass local admission. Both paths must refresh the same durable fence
	// after manifest preparation and immediately before BeginEmbeddingBatch.
	if err = w.renewLease(ctx, lease); err != nil {
		return nil, false, err
	}
	if err = w.repository.BeginEmbeddingBatch(ctx, *lease, w.now(), manifest.BatchID); err != nil {
		return nil, false, err
	}
	providerBegan = true
	embedRequestCtx := withEmbeddingBatchClientRequestKey(
		localinfer.WithOperation(ragEmbedContext(ctx), localinfer.OperationDocumentEmbedding),
		manifest.ClientRequestKey,
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
	logger.Info("[knowledge] job stage changed",
		"job_id", job.JobID,
		"parent_job_id", job.ParentJobID,
		"job_kind", job.Kind,
		"owner_id", source.OwnerID,
		"corpus_uid", source.CorpusUID,
		"corpus_alias", source.CorpusAlias,
		"document_id", job.DocumentID,
		"document_generation", source.ContentGeneration,
		"filename", source.Filename,
		"extension", source.Extension,
		"storage_path", source.StoragePath,
		"media_type", source.MediaType,
		"size_bytes", source.SizeBytes,
		"sha256", source.SHA256,
		"agent_id", source.AgentID,
		"learner_id", source.LearnerID,
		"subject", source.Subject,
		"grade", source.Grade,
		"vision_route", source.VisionRoute,
		"stage", JobStageExtracting,
	)

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
	logger.Info("[knowledge] job stage changed",
		"job_id", job.JobID,
		"parent_job_id", job.ParentJobID,
		"job_kind", job.Kind,
		"owner_id", source.OwnerID,
		"corpus_uid", source.CorpusUID,
		"corpus_alias", source.CorpusAlias,
		"document_id", job.DocumentID,
		"document_generation", source.ContentGeneration,
		"filename", source.Filename,
		"storage_path", source.StoragePath,
		"media_type", source.MediaType,
		"sha256", source.SHA256,
		"agent_id", source.AgentID,
		"learner_id", source.LearnerID,
		"subject", source.Subject,
		"grade", source.Grade,
		"stage", JobStageChunking,
		"pages_done", pages,
		"pages_total", pages,
	)
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
	heartbeatStartedAt := time.Now()
	heartbeatDone := make(chan error, 1)
	go func() {
		leaseTicker := time.NewTicker(interval)
		defer leaseTicker.Stop()
		for {
			select {
			case <-prepareCtx.Done():
				heartbeatDone <- nil
				return
			case <-leaseTicker.C:
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
	logCtx, cancelLog := context.WithCancel(prepareCtx)
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		logTicker := time.NewTicker(30 * time.Second)
		defer logTicker.Stop()
		for {
			select {
			case <-logCtx.Done():
				return
			case <-logTicker.C:
				state.Lock()
				current := state.lease
				state.Unlock()
				jobReader, ok := w.repository.(interface {
					GetJob(context.Context, string, string) (KnowledgeJob, error)
				})
				if !ok {
					continue
				}
				readCtx, cancelRead := context.WithTimeout(logCtx, time.Second)
				persisted, readErr := jobReader.GetJob(readCtx, current.OwnerID, current.JobID)
				cancelRead()
				if readErr != nil {
					logger.Warn("[knowledge] job heartbeat read failed",
						"job_id", current.JobID,
						"owner_id", current.OwnerID,
						"corpus_uid", source.CorpusUID,
						"document_id", source.DocumentID,
						"document_generation", source.ContentGeneration,
						"filename", source.Filename,
						"storage_path", source.StoragePath,
						"error", readErr,
					)
					continue
				}
				var pagesDone, pagesTotal, chunksDone, chunksTotal any
				if persisted.PagesDone != nil {
					pagesDone = *persisted.PagesDone
				}
				if persisted.PagesTotal != nil {
					pagesTotal = *persisted.PagesTotal
				}
				if persisted.ChunksDone != nil {
					chunksDone = *persisted.ChunksDone
				}
				if persisted.ChunksTotal != nil {
					chunksTotal = *persisted.ChunksTotal
				}
				logger.Info("[knowledge] job heartbeat",
					"job_id", persisted.JobID,
					"parent_job_id", persisted.ParentJobID,
					"job_kind", persisted.Kind,
					"owner_id", persisted.OwnerID,
					"corpus_uid", source.CorpusUID,
					"corpus_alias", source.CorpusAlias,
					"document_id", persisted.DocumentID,
					"document_generation", source.ContentGeneration,
					"filename", source.Filename,
					"extension", source.Extension,
					"storage_path", source.StoragePath,
					"media_type", source.MediaType,
					"size_bytes", source.SizeBytes,
					"sha256", source.SHA256,
					"agent_id", source.AgentID,
					"learner_id", source.LearnerID,
					"subject", source.Subject,
					"grade", source.Grade,
					"vision_route", source.VisionRoute,
					"revision_id", persisted.TargetRevisionID,
					"stage", persisted.Stage,
					"elapsed_ms", time.Since(heartbeatStartedAt).Milliseconds(),
					"pages_done", pagesDone,
					"pages_total", pagesTotal,
					"chunks_done", chunksDone,
					"chunks_total", chunksTotal,
				)
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
				source:     source,
				invocations: func() OCRPageInvocationProgress {
					value, _ := w.repository.(OCRPageInvocationProgress)
					return value
				}(),
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
	cancelLog()
	cancelPrepare()
	<-logDone
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
		errors.Is(err, ErrOCRPageInvocationOutcomeUnknown) ||
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
