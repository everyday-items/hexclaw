package knowledge

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// EmbeddingProfileResolver is the side-effect-free boundary between policy
// intent and an immutable executable profile. Implementations may inspect local
// installation/configuration state, but must not invoke embedding from here.
type EmbeddingProfileResolver interface {
	Resolve(ctx context.Context, ownerID, corpusID string, selection EmbeddingSelection) (EmbeddingProfileSnapshot, error)
	Catalog(ctx context.Context, ownerID, corpusID string) (EmbeddingProfileCatalog, error)
}

type VisionRouteSnapshotResolver interface {
	FreezeDefaultVisionRoute(context.Context) (VisionRouteSnapshot, error)
}

type VisionRouteSnapshotResolverFunc func(context.Context) (VisionRouteSnapshot, error)

func (f VisionRouteSnapshotResolverFunc) FreezeDefaultVisionRoute(ctx context.Context) (VisionRouteSnapshot, error) {
	return f(ctx)
}

// SemanticIndexRepository owns all atomic policy, revision and job transitions.
type SemanticIndexRepository interface {
	GetPolicy(ctx context.Context, ownerID, corpusID string) (EmbeddingPolicyProjection, error)
	EnsureDefaultPolicy(ctx context.Context, ownerID, corpusID string, profile EmbeddingProfileSnapshot) (ApplyPolicyResult, error)
	ApplyPolicy(ctx context.Context, ownerID, corpusID string, expectedVersion int64, selection EmbeddingSelection, profile *EmbeddingProfileSnapshot) (ApplyPolicyResult, error)
	GetJob(ctx context.Context, ownerID, jobID string) (KnowledgeJob, error)
	CancelJob(ctx context.Context, ownerID, jobID string) (KnowledgeJob, error)
}

type corpusScopedSemanticJobRepository interface {
	GetJobForCorpus(ctx context.Context, ownerID, corpusID, jobID string) (KnowledgeJob, error)
	CancelJobForCorpus(ctx context.Context, ownerID, corpusID, jobID string) (KnowledgeJob, error)
}

// SemanticIndexService is the stable facade used by future HTTP handlers.
// Worker-specific lease/checkpoint methods intentionally remain on the concrete
// repository rather than leaking into this API-facing surface.
type SemanticIndexService struct {
	repository          SemanticIndexRepository
	resolver            EmbeddingProfileResolver
	ingestRepo          DocumentIngestRepository
	blobStore           *localIngestBlobStore
	visionRouteResolver VisionRouteSnapshotResolver
}

func (s *SemanticIndexService) ConfigureVisionRouteResolver(resolver VisionRouteSnapshotResolver) {
	s.visionRouteResolver = resolver
}

func (s *SemanticIndexService) freezeVisionRoute(ctx context.Context) (*VisionRouteSnapshot, error) {
	if s.visionRouteResolver == nil {
		return nil, nil
	}
	snapshot, err := s.visionRouteResolver.FreezeDefaultVisionRoute(ctx)
	if err != nil {
		return nil, err
	}
	snapshot = snapshot.Canonical()
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func NewSemanticIndexService(repository SemanticIndexRepository, resolver EmbeddingProfileResolver) *SemanticIndexService {
	service := &SemanticIndexService{repository: repository, resolver: resolver}
	if ingestRepo, ok := repository.(DocumentIngestRepository); ok {
		service.ingestRepo = ingestRepo
	}
	return service
}

// ConfigureDocumentIngest installs the local content-addressed object root.
// Assembly calls this before serving HTTP; keeping it explicit prevents tests
// or non-desktop runtimes from silently writing into a process working dir.
func (s *SemanticIndexService) ConfigureDocumentIngest(root string) error {
	store, err := newLocalIngestBlobStore(root)
	if err != nil {
		return err
	}
	if s.ingestRepo == nil {
		return ErrDocumentIngestUnavailable
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if reconciler, ok := s.ingestRepo.(uploadOperationStartupReconciler); ok {
		if err := reconciler.CancelOrphanedReceivingUploadOperations(
			cleanupCtx, time.Now().UTC(),
		); err != nil {
			return err
		}
	}
	referenced, err := s.ingestRepo.ListIngestBlobPaths(cleanupCtx)
	if err != nil {
		return err
	}
	if err := store.PruneOrphans(referenced); err != nil {
		return err
	}
	if repository, ok := s.ingestRepo.(*SQLiteSemanticIndexRepository); ok {
		repository.ingestBlobStore = store
	}
	s.blobStore = store
	return nil
}

// CreateDocument streams the original bytes to durable local storage before
// atomically creating the owner/corpus-scoped Document and ingest Job.
func (s *SemanticIndexService) CreateDocument(
	ctx context.Context,
	ownerID, corpusID string,
	input CreateDocumentInput,
) (CreateDocumentResult, error) {
	if s.ingestRepo == nil || s.blobStore == nil {
		return CreateDocumentResult{}, ErrDocumentIngestUnavailable
	}
	uploads, ok := s.ingestRepo.(uploadOperationRepository)
	if !ok {
		return CreateDocumentResult{}, ErrDocumentIngestUnavailable
	}
	visionRoute, err := s.freezeVisionRoute(ctx)
	if err != nil {
		return CreateDocumentResult{}, err
	}
	input.VisionRoute = visionRoute
	operation, created, err := uploads.BeginUploadOperation(ctx, ownerID, corpusID, input)
	if err != nil {
		return CreateDocumentResult{}, err
	}
	if !created && operation.DocumentID == "" && operation.JobID == "" {
		if operation.Terminal {
			return CreateDocumentResult{}, ErrDocumentRetryRequiresReupload
		}
		// The request that inserted the receiving intent is the only request
		// allowed to consume bytes and bind/terminate it. A concurrent replay
		// must fail before reading its body; otherwise its cancellation or read
		// failure could fence the healthy creator's shared operation row.
		return CreateDocumentResult{}, ErrIdempotencyConflict
	}
	input.UploadOperationID = operation.OperationID
	blob, release, err := s.blobStore.Persist(ctx, ownerID, corpusID, input)
	if err != nil {
		return CreateDocumentResult{}, errors.Join(err,
			s.markUploadOperationTerminal(ownerID, corpusID, operation.OperationID, err))
	}
	defer release()
	input.SizeBytes = blob.SizeBytes
	result, createErr := s.ingestRepo.CreateIngestDocument(ctx, ownerID, corpusID, input, blob)
	if createErr == nil {
		result.OperationID = operation.OperationID
		return result, nil
	}
	operationErr := s.markUploadOperationTerminal(
		ownerID, corpusID, operation.OperationID, createErr,
	)
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCleanup()
	referenced, referenceErr := s.ingestRepo.IsIngestBlobPathReferenced(cleanupCtx, blob.StoragePath)
	if referenceErr != nil {
		return CreateDocumentResult{}, errors.Join(createErr, operationErr,
			fmt.Errorf("knowledge: verify rejected ingest object reference: %w", referenceErr))
	}
	if !referenced {
		if removeErr := s.blobStore.RemoveManagedObject(blob.StoragePath); removeErr != nil {
			return CreateDocumentResult{}, errors.Join(createErr, operationErr, removeErr)
		}
	}
	return CreateDocumentResult{}, errors.Join(createErr, operationErr)
}

func (s *SemanticIndexService) markUploadOperationTerminal(
	ownerID, corpusID, operationID string,
	cause error,
) error {
	uploads, ok := s.ingestRepo.(uploadOperationRepository)
	if !ok || operationID == "" {
		return nil
	}
	state := UploadOperationFailed
	errorCode := "upload_failed"
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		state = UploadOperationCancelled
		errorCode = "upload_cancelled"
	}
	markCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := uploads.MarkUploadOperationFailed(
		markCtx, ownerID, corpusID, operationID, state, errorCode,
	); err != nil {
		return fmt.Errorf("knowledge: persist upload terminal projection: %w", err)
	}
	return nil
}

// ListUploadOperationsForCorpus is a side-effect-free renderer recovery read.
// It never queues, retries, or otherwise mutates a physical ingest job.
func (s *SemanticIndexService) ListUploadOperationsForCorpus(
	ctx context.Context,
	ownerID, corpusID string,
) ([]UploadOperationProjection, error) {
	uploads, ok := s.ingestRepo.(uploadOperationRepository)
	if !ok {
		return nil, ErrDocumentIngestUnavailable
	}
	return uploads.ListUploadOperationsForCorpus(ctx, ownerID, corpusID)
}

// MarkUploadResponseDelivered advances only the transport acknowledgement
// boundary. The worker job remains independently durable and authoritative.
func (s *SemanticIndexService) MarkUploadResponseDelivered(
	ctx context.Context,
	ownerID, corpusID, operationID string,
) error {
	uploads, ok := s.ingestRepo.(uploadOperationRepository)
	if !ok {
		return ErrDocumentIngestUnavailable
	}
	return uploads.MarkUploadResponseDelivered(ctx, ownerID, corpusID, operationID)
}

// RetryDocument creates a new durable job for a failed document generation.
// It deliberately does not accept bytes: the repository binds the retry to
// the immutable source already retained for that generation. A failed text
// ingest is retried as a new root job; a failed embedding-only child is retried
// as another child without invoking extraction/OCR again.
func (s *SemanticIndexService) RetryDocument(
	ctx context.Context,
	ownerID, corpusID, documentID, idempotencyKey string,
) (CreateDocumentResult, error) {
	if s.ingestRepo == nil {
		return CreateDocumentResult{}, ErrDocumentIngestUnavailable
	}
	visionRoute, err := s.freezeVisionRoute(ctx)
	if err != nil {
		return CreateDocumentResult{}, err
	}
	if repository, ok := s.ingestRepo.(visionRouteRetryRepository); ok {
		return repository.RetryIngestDocumentWithVisionRoute(
			ctx, ownerID, corpusID, documentID, idempotencyKey, visionRoute,
		)
	}
	return s.ingestRepo.RetryIngestDocument(ctx, ownerID, corpusID, documentID, idempotencyKey)
}

func (s *SemanticIndexService) GetIngestDocument(
	ctx context.Context,
	ownerID, documentID string,
) (PersistedIngestDocument, error) {
	if s.ingestRepo == nil {
		return PersistedIngestDocument{}, ErrDocumentIngestUnavailable
	}
	return s.ingestRepo.GetIngestDocument(ctx, ownerID, documentID)
}

func (s *SemanticIndexService) GetIngestDocumentProjection(
	ctx context.Context,
	ownerID, documentID string,
) (KnowledgeDocumentProjection, error) {
	if s.ingestRepo == nil {
		return KnowledgeDocumentProjection{}, ErrDocumentIngestUnavailable
	}
	return s.ingestRepo.GetIngestDocumentProjection(ctx, ownerID, documentID)
}

func (s *SemanticIndexService) GetIngestDocumentProjectionForCorpus(
	ctx context.Context, ownerID, corpusID, documentID string,
) (KnowledgeDocumentProjection, error) {
	repository, ok := s.ingestRepo.(interface {
		GetIngestDocumentProjectionForCorpus(context.Context, string, string, string) (KnowledgeDocumentProjection, error)
	})
	if !ok {
		return KnowledgeDocumentProjection{}, ErrSemanticIndexNotFound
	}
	return repository.GetIngestDocumentProjectionForCorpus(ctx, ownerID, corpusID, documentID)
}

// ListRecoverableIngestJobsForCorpus exposes only renderer-recoverable root
// ingest jobs. Lease/checkpoint internals remain repository-private.
func (s *SemanticIndexService) ListRecoverableIngestJobsForCorpus(
	ctx context.Context, ownerID, corpusID string,
) ([]KnowledgeJob, error) {
	repository, ok := s.ingestRepo.(interface {
		ListRecoverableIngestJobsForCorpus(context.Context, string, string) ([]KnowledgeJob, error)
	})
	if !ok {
		return nil, ErrDocumentIngestUnavailable
	}
	return repository.ListRecoverableIngestJobsForCorpus(ctx, ownerID, corpusID)
}

// GetPolicy is read-only. Corpus creation and initial auto scheduling require
// the explicit EnsureDefaultPolicy bootstrap command.
func (s *SemanticIndexService) GetPolicy(ctx context.Context, ownerID, corpusID string) (EmbeddingPolicyProjection, error) {
	projection, err := s.repository.GetPolicy(ctx, ownerID, corpusID)
	if err != nil {
		return EmbeddingPolicyProjection{}, err
	}
	catalog, err := s.resolver.Catalog(ctx, ownerID, corpusID)
	if err != nil {
		return EmbeddingPolicyProjection{}, err
	}
	projection.AvailableProfiles = catalog.Profiles
	projection.Recommendation = catalog.Recommendation
	projection.CatalogVersion = catalog.Version
	return projection, nil
}

// EnsureDefaultPolicy explicitly bootstraps owner/corpus and applies auto in
// one repository transaction. It is deliberately a one-time bootstrap: once
// a durable active/desired snapshot or explicit policy exists, startup and
// local-model installation must not re-resolve auto and silently switch vector
// spaces. A user-triggered ApplyPolicy(auto) remains the explicit re-resolution
// boundary.
func (s *SemanticIndexService) EnsureDefaultPolicy(ctx context.Context, ownerID, corpusID string) (ApplyPolicyResult, error) {
	existing, err := s.repository.GetPolicy(ctx, ownerID, corpusID)
	if err == nil {
		uninitialized := existing.PolicyVersion == 0 &&
			existing.Selection.Kind == EmbeddingSelectionDisabled &&
			existing.ActiveRevision == nil && existing.DesiredRevision == nil
		if !uninitialized {
			return applyPolicyNoopFromProjection(existing), nil
		}
	} else if !errors.Is(err, ErrSemanticIndexNotFound) {
		return ApplyPolicyResult{}, err
	}

	selection := EmbeddingSelection{Kind: EmbeddingSelectionAuto}
	profile, err := s.resolve(ctx, ownerID, corpusID, selection)
	if err != nil {
		return ApplyPolicyResult{}, err
	}
	return s.repository.EnsureDefaultPolicy(ctx, ownerID, corpusID, profile)
}

func applyPolicyNoopFromProjection(projection EmbeddingPolicyProjection) ApplyPolicyResult {
	result := ApplyPolicyResult{
		PolicyVersion: projection.PolicyVersion,
		Selection:     projection.Selection,
		Branch:        ApplyPolicyNoop,
	}
	if projection.ActiveRevision != nil {
		activeRevisionID := projection.ActiveRevision.RevisionID
		result.ActiveRevisionID = &activeRevisionID
	}
	if projection.DesiredRevision != nil {
		desiredRevisionID := projection.DesiredRevision.RevisionID
		result.DesiredRevisionID = &desiredRevisionID
		if projection.DesiredRevision.JobID != nil {
			jobID := *projection.DesiredRevision.JobID
			result.JobID = &jobID
		}
	}
	return result
}

func (s *SemanticIndexService) ApplyPolicy(
	ctx context.Context,
	ownerID, corpusID string,
	expectedVersion int64,
	selection EmbeddingSelection,
) (ApplyPolicyResult, error) {
	if err := selection.Validate(); err != nil {
		return ApplyPolicyResult{}, err
	}
	if selection.Kind == EmbeddingSelectionDisabled {
		return s.repository.ApplyPolicy(ctx, ownerID, corpusID, expectedVersion, selection, nil)
	}
	profile, err := s.resolve(ctx, ownerID, corpusID, selection)
	if err != nil {
		return ApplyPolicyResult{}, err
	}
	return s.repository.ApplyPolicy(ctx, ownerID, corpusID, expectedVersion, selection, &profile)
}

func (s *SemanticIndexService) GetJob(ctx context.Context, ownerID, jobID string) (KnowledgeJob, error) {
	return s.repository.GetJob(ctx, ownerID, jobID)
}

func (s *SemanticIndexService) CancelJob(ctx context.Context, ownerID, jobID string) (KnowledgeJob, error) {
	return s.repository.CancelJob(ctx, ownerID, jobID)
}

// GetJobForCorpus and CancelJobForCorpus are the HTTP-facing variants. Worker
// internals may address a globally unique job within an owner, but an API read
// or command must also prove the public corpus alias.
func (s *SemanticIndexService) GetJobForCorpus(
	ctx context.Context, ownerID, corpusID, jobID string,
) (KnowledgeJob, error) {
	repository, ok := s.repository.(corpusScopedSemanticJobRepository)
	if !ok {
		return KnowledgeJob{}, ErrSemanticIndexNotFound
	}
	return repository.GetJobForCorpus(ctx, ownerID, corpusID, jobID)
}

func (s *SemanticIndexService) CancelJobForCorpus(
	ctx context.Context, ownerID, corpusID, jobID string,
) (KnowledgeJob, error) {
	repository, ok := s.repository.(corpusScopedSemanticJobRepository)
	if !ok {
		return KnowledgeJob{}, ErrSemanticIndexNotFound
	}
	return repository.CancelJobForCorpus(ctx, ownerID, corpusID, jobID)
}

func (s *SemanticIndexService) resolve(
	ctx context.Context,
	ownerID, corpusID string,
	selection EmbeddingSelection,
) (EmbeddingProfileSnapshot, error) {
	if err := selection.Validate(); err != nil {
		return EmbeddingProfileSnapshot{}, err
	}
	profile, err := s.resolver.Resolve(ctx, ownerID, corpusID, selection)
	if err != nil {
		return EmbeddingProfileSnapshot{}, err
	}
	if err := profile.Validate(); err != nil {
		return EmbeddingProfileSnapshot{}, err
	}
	if selection.Kind == EmbeddingSelectionProfile && profile.Profile.ProfileID != selection.ProfileID {
		return EmbeddingProfileSnapshot{}, ErrInvalidEmbeddingProfile
	}
	// Model installation is an independent Ollama operation. Policy apply is
	// side-effect free and accepts only a profile that can execute now; it never
	// synthesizes a download job or conflates installation with indexing.
	if !profile.executableNow() {
		return EmbeddingProfileSnapshot{}, ErrProfileUnavailable
	}
	return profile, nil
}
