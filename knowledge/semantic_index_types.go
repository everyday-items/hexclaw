package knowledge

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrInvalidSelection marks a malformed embedding selection tagged union.
	ErrInvalidSelection = errors.New("knowledge: invalid embedding selection")
	// ErrInvalidEmbeddingProfile marks an execution profile that cannot form a
	// safe, immutable vector space.
	ErrInvalidEmbeddingProfile = errors.New("knowledge: invalid embedding profile")
	// ErrProfileUnavailable means a selection cannot currently resolve to an
	// installed or connected embedding profile. Download is a separate action.
	ErrProfileUnavailable = errors.New("knowledge: embedding profile unavailable")
	// ErrSemanticIndexNotFound deliberately hides cross-owner existence.
	ErrSemanticIndexNotFound = errors.New("knowledge: semantic index resource not found")
	// ErrPolicyVersionConflict is returned before any policy side effects.
	ErrPolicyVersionConflict = errors.New("knowledge: embedding policy version conflict")
	// ErrJobFenced rejects work submitted with an expired or cancelled lease.
	ErrJobFenced = errors.New("knowledge: semantic index job lease fenced")
	// ErrPublishConflict rejects a stale staged-revision publication.
	ErrPublishConflict = errors.New("knowledge: semantic index revision publish conflict")
	// ErrInvalidRevisionVector rejects provider output that does not match the
	// immutable target snapshot or manifest.
	ErrInvalidRevisionVector = errors.New("knowledge: invalid semantic index revision vector")
)

// EmbeddingSelectionKind is the only user-controlled dimension of embedding
// policy. Provider, model and location are resolved profile facts.
type EmbeddingSelectionKind string

const (
	EmbeddingSelectionAuto     EmbeddingSelectionKind = "auto"
	EmbeddingSelectionProfile  EmbeddingSelectionKind = "profile"
	EmbeddingSelectionDisabled EmbeddingSelectionKind = "disabled"
)

// EmbeddingSelection is a strict tagged union:
//
//	{"kind":"auto"}
//	{"kind":"profile","profile_id":"..."}
//	{"kind":"disabled"}
type EmbeddingSelection struct {
	Kind      EmbeddingSelectionKind `json:"kind"`
	ProfileID string                 `json:"profile_id,omitempty"`
}

// Validate rejects contradictory or incomplete tagged-union shapes.
func (s EmbeddingSelection) Validate() error {
	profileID := strings.TrimSpace(s.ProfileID)
	switch s.Kind {
	case EmbeddingSelectionAuto, EmbeddingSelectionDisabled:
		if profileID != "" {
			return fmt.Errorf("%w: %s cannot carry profile_id", ErrInvalidSelection, s.Kind)
		}
	case EmbeddingSelectionProfile:
		if profileID == "" {
			return fmt.Errorf("%w: profile requires profile_id", ErrInvalidSelection)
		}
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidSelection, s.Kind)
	}
	return nil
}

func (s EmbeddingSelection) equal(other EmbeddingSelection) bool {
	return s.Kind == other.Kind && strings.TrimSpace(s.ProfileID) == strings.TrimSpace(other.ProfileID)
}

// ProviderLocation is an immutable resolved profile fact, never a policy input.
type ProviderLocation string

const (
	ProviderLocationLocal ProviderLocation = "local"
	ProviderLocationCloud ProviderLocation = "cloud"
)

// ProfileAvailability describes whether a catalog profile can execute now or
// first needs a durable model-download job.
type ProfileAvailability string

const (
	ProfileAvailabilityInstalled    ProfileAvailability = "installed"
	ProfileAvailabilityDownloadable ProfileAvailability = "downloadable"
	ProfileAvailabilityDownloading  ProfileAvailability = "downloading"
	ProfileAvailabilityConnected    ProfileAvailability = "connected"
	ProfileAvailabilityUnavailable  ProfileAvailability = "unavailable"
)

// EmbeddingProfile is the approved API catalog item shape.
type EmbeddingProfile struct {
	ProfileID    string              `json:"profile_id"`
	ModelName    string              `json:"model_name"`
	ProviderID   string              `json:"provider_id"`
	ProviderName string              `json:"provider_name"`
	Location     ProviderLocation    `json:"location"`
	Capability   string              `json:"capability"`
	Dimension    int                 `json:"dimension"`
	Availability ProfileAvailability `json:"availability"`
	DisplayOrder int                 `json:"display_order"`
}

// EmbeddingProfileSnapshot freezes every vector-space compatibility fact for
// one revision. SnapshotID is assigned by the repository.
type EmbeddingProfileSnapshot struct {
	SnapshotID        string           `json:"-"`
	Profile           EmbeddingProfile `json:"profile"`
	Normalization     string           `json:"-"`
	ChunkConfigHash   string           `json:"-"`
	ProfileConfigHash string           `json:"-"`
}

// Validate prevents an incomplete or contradictory profile from becoming a
// persisted revision snapshot.
func (s EmbeddingProfileSnapshot) Validate() error {
	p := s.Profile
	if strings.TrimSpace(p.ProfileID) == "" || strings.TrimSpace(p.ProviderID) == "" ||
		strings.TrimSpace(p.ModelName) == "" || p.Dimension <= 0 || p.Capability != "embedding" ||
		strings.TrimSpace(s.ChunkConfigHash) == "" || strings.TrimSpace(s.ProfileConfigHash) == "" {
		return ErrInvalidEmbeddingProfile
	}
	if p.Location != ProviderLocationLocal && p.Location != ProviderLocationCloud {
		return ErrInvalidEmbeddingProfile
	}
	if s.Normalization != "l2" && s.Normalization != "none" {
		return ErrInvalidEmbeddingProfile
	}
	switch p.Availability {
	case ProfileAvailabilityInstalled, ProfileAvailabilityDownloadable,
		ProfileAvailabilityDownloading, ProfileAvailabilityConnected,
		ProfileAvailabilityUnavailable:
	default:
		return ErrInvalidEmbeddingProfile
	}
	return nil
}

func (s EmbeddingProfileSnapshot) executableNow() bool {
	return s.Profile.Availability == ProfileAvailabilityInstalled ||
		s.Profile.Availability == ProfileAvailabilityConnected
}

// EmbeddingProfileCatalog is a corpus-scoped, read-only provider catalog.
type EmbeddingProfileCatalog struct {
	Profiles       []EmbeddingProfile
	Recommendation *EmbeddingRecommendation
	Version        int64
}

type EmbeddingRecommendation struct {
	ProfileID  *string `json:"profile_id"`
	ReasonCode string  `json:"reason_code"`
	ReasonText string  `json:"reason_text"`
}

type VectorIndexState string

const (
	VectorIndexDisabled  VectorIndexState = "disabled"
	VectorIndexPending   VectorIndexState = "pending"
	VectorIndexBuilding  VectorIndexState = "building"
	VectorIndexRetryWait VectorIndexState = "retry_wait"
	VectorIndexReady     VectorIndexState = "ready"
	VectorIndexFailed    VectorIndexState = "failed"
	VectorIndexCancelled VectorIndexState = "cancelled"
)

// EmbeddingRevisionProjection matches the approved desktop contract. JobID is
// the cancellable root job identifier and is never synthesized from RevisionID.
type EmbeddingRevisionProjection struct {
	RevisionID        string           `json:"revision_id"`
	State             VectorIndexState `json:"state"`
	ProfileConfigHash string           `json:"profile_config_hash"`
	Profile           EmbeddingProfile `json:"profile"`
	ChunksDone        *int64           `json:"chunks_done,omitempty"`
	ChunksTotal       *int64           `json:"chunks_total,omitempty"`
	JobID             *string          `json:"job_id,omitempty"`
}

type IndexingActivityState string

const (
	IndexingActivityIdle      IndexingActivityState = "idle"
	IndexingActivityBuilding  IndexingActivityState = "building"
	IndexingActivityRetryWait IndexingActivityState = "retry_wait"
	IndexingActivityFailed    IndexingActivityState = "failed"
)

// IndexingActivity is derived solely from durable jobs and committed progress.
type IndexingActivity struct {
	State               IndexingActivityState `json:"state"`
	ProcessingDocuments int64                 `json:"processing_documents"`
	ChunksDone          *int64                `json:"chunks_done"`
	ChunksTotal         *int64                `json:"chunks_total"`
}

// EmbeddingPolicyProjection is the API-facing policy read model.
type EmbeddingPolicyProjection struct {
	PolicyVersion     int64                        `json:"policy_version"`
	Selection         EmbeddingSelection           `json:"selection"`
	ActiveRevision    *EmbeddingRevisionProjection `json:"active_revision"`
	DesiredRevision   *EmbeddingRevisionProjection `json:"desired_revision"`
	IndexingActivity  IndexingActivity             `json:"indexing_activity"`
	AvailableProfiles []EmbeddingProfile           `json:"available_profiles"`
	Recommendation    *EmbeddingRecommendation     `json:"recommendation"`
	CatalogVersion    int64                        `json:"catalog_version"`
}

type ApplyPolicyBranch string

const (
	ApplyPolicyNoop             ApplyPolicyBranch = "noop"
	ApplyPolicyDisabled         ApplyPolicyBranch = "disabled"
	ApplyPolicyImmediatePublish ApplyPolicyBranch = "immediate_publish"
	ApplyPolicyIntentOnly       ApplyPolicyBranch = "intent_only"
	ApplyPolicyStagedRebuild    ApplyPolicyBranch = "staged_rebuild"
)

type ApplyPolicyResult struct {
	PolicyVersion     int64              `json:"policy_version"`
	Selection         EmbeddingSelection `json:"selection"`
	ActiveRevisionID  *string            `json:"active_revision_id"`
	DesiredRevisionID *string            `json:"desired_revision_id"`
	JobID             *string            `json:"job_id,omitempty"`
	Branch            ApplyPolicyBranch  `json:"-"`
}

type KnowledgeJobKind string

const (
	KnowledgeJobIngest          KnowledgeJobKind = "ingest"
	KnowledgeJobDownloadModel   KnowledgeJobKind = "download_model"
	KnowledgeJobRebuildRevision KnowledgeJobKind = "rebuild_revision"
	KnowledgeJobEmbedDocument   KnowledgeJobKind = "embed_document"
	KnowledgeJobGC              KnowledgeJobKind = "gc"
)

type KnowledgeJobState string

const (
	KnowledgeJobQueued    KnowledgeJobState = "queued"
	KnowledgeJobRunning   KnowledgeJobState = "running"
	KnowledgeJobRetryWait KnowledgeJobState = "retry_wait"
	KnowledgeJobSucceeded KnowledgeJobState = "succeeded"
	KnowledgeJobFailed    KnowledgeJobState = "failed"
	KnowledgeJobCancelled KnowledgeJobState = "cancelled"
)

type JobStage string

const (
	JobStageExtracting   JobStage = "extracting"
	JobStageOCR          JobStage = "ocr"
	JobStageChunking     JobStage = "chunking"
	JobStageTextIndexing JobStage = "text_indexing"
	JobStageEmbedding    JobStage = "embedding"
	JobStagePublishing   JobStage = "publishing"
	JobStageGC           JobStage = "gc"
)

// KnowledgeJob is durable worker state. Ownership and lease details are kept
// out of JSON while remaining available to trusted in-process workers.
type KnowledgeJob struct {
	JobID              string                `json:"job_id"`
	ParentJobID        string                `json:"parent_job_id,omitempty"`
	Kind               KnowledgeJobKind      `json:"kind"`
	OwnerID            string                `json:"-"`
	CorpusUID          string                `json:"-"`
	DocumentID         string                `json:"document_id,omitempty"`
	DocumentGeneration int64                 `json:"-"`
	TargetRevisionID   string                `json:"target_revision_id,omitempty"`
	State              KnowledgeJobState     `json:"state"`
	Stage              JobStage              `json:"stage"`
	PagesDone          *int64                `json:"pages_done"`
	PagesTotal         *int64                `json:"pages_total"`
	ChunksDone         *int64                `json:"chunks_done"`
	ChunksTotal        *int64                `json:"chunks_total"`
	Attempt            int                   `json:"attempt"`
	NextAttemptAt      *time.Time            `json:"next_attempt_at,omitempty"`
	CancelRequested    bool                  `json:"cancel_requested"`
	LeaseOwner         string                `json:"-"`
	LeaseEpoch         int64                 `json:"-"`
	LeaseExpiresAt     *time.Time            `json:"-"`
	HeartbeatAt        *time.Time            `json:"-"`
	LastError          string                `json:"last_error,omitempty"`
	Failure            *KnowledgeJobFailure  `json:"failure,omitempty"`
	OCRPageReceipts    []OCRPageRouteReceipt `json:"ocr_page_route_receipts"`
	CreatedAt          time.Time             `json:"created_at"`
	UpdatedAt          time.Time             `json:"updated_at"`
}

type JobLease struct {
	JobID     string
	OwnerID   string
	CorpusUID string
	WorkerID  string
	Epoch     int64
	ExpiresAt time.Time
}

func (j KnowledgeJob) Lease() JobLease {
	lease := JobLease{
		JobID: j.JobID, OwnerID: j.OwnerID, CorpusUID: j.CorpusUID,
		WorkerID: j.LeaseOwner, Epoch: j.LeaseEpoch,
	}
	if j.LeaseExpiresAt != nil {
		lease.ExpiresAt = *j.LeaseExpiresAt
	}
	return lease
}

type JobProgressUpdate struct {
	Stage       JobStage
	PagesDone   *int64
	PagesTotal  *int64
	ChunksDone  *int64
	ChunksTotal *int64
}

// LegacyCorpusBinding is the auditable result of explicitly assigning the
// pre-v23 global knowledge tables to one desktop owner/corpus.
type LegacyCorpusBinding struct {
	CorpusUID      string
	Documents      int64
	Chunks         int64
	ContentVersion int64
}

type StageCheckpointState string

const (
	StageCheckpointPrepared  StageCheckpointState = "prepared"
	StageCheckpointSucceeded StageCheckpointState = "succeeded"
)

type StageCheckpoint struct {
	JobID            string
	Stage            JobStage
	InputFingerprint string
	ArtifactRef      string
	ArtifactDigest   string
	State            StageCheckpointState
	LeaseEpoch       int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type JobExecutionPlan struct {
	CorpusUID                string
	CorpusAlias              string
	RevisionID               string
	PolicyVersion            int64
	BaseContentVersion       int64
	ContentVersion           int64
	PreviousActiveRevisionID *string
	Snapshot                 EmbeddingProfileSnapshot
}

type ActiveRevisionPlan struct {
	CorpusUID  string
	RevisionID string
	Snapshot   EmbeddingProfileSnapshot
}

type RevisionChunkCursor struct {
	DocumentID string
	ChunkIndex int
	ChunkID    string
}

type RevisionChunkInput struct {
	Cursor            RevisionChunkCursor
	DocumentID        string
	ContentGeneration int64
	ChunkID           string
	ChunkIndex        int
	Content           string
	ContentHash       string
}

type EmbeddingBatchState string

const (
	EmbeddingBatchPrepared       EmbeddingBatchState = "prepared"
	EmbeddingBatchInFlight       EmbeddingBatchState = "in_flight"
	EmbeddingBatchRetryWait      EmbeddingBatchState = "retry_wait"
	EmbeddingBatchSucceeded      EmbeddingBatchState = "succeeded"
	EmbeddingBatchFailed         EmbeddingBatchState = "failed"
	EmbeddingBatchCancelled      EmbeddingBatchState = "cancelled"
	EmbeddingBatchOutcomeUnknown EmbeddingBatchState = "outcome_unknown"
)

type EmbeddingBatchChunk struct {
	Ordinal     int
	ChunkID     string
	ContentHash string
}

type EmbeddingBatchManifest struct {
	BatchID           string
	JobID             string
	RevisionID        string
	ProfileConfigHash string
	ChunkIDsDigest    string
	PayloadDigest     string
	ClientRequestKey  string
	State             EmbeddingBatchState
	Attempts          int
	ProviderRequestID string
	LeaseEpoch        int64
	Chunks            []EmbeddingBatchChunk
}

type RevisionVector struct {
	DocumentID        string
	ContentGeneration int64
	ChunkID           string
	ChunkIndex        int
	ContentHash       string
	Values            []float32
}

type EmbeddingBatchCommit struct {
	BatchID           string
	Vectors           []RevisionVector
	ChunksDone        int64
	ChunksTotal       int64
	ProviderRequestID string
	Checkpoint        *StageCheckpoint
}

type RevisionBuildSummary struct {
	RevisionID            string
	ChunkSetDigest        string
	ExpectedChunks        int64
	EmbeddedChunks        int64
	FailedChunks          int64
	IndexedThroughVersion int64
}

type RevisionPublishPreparation struct {
	IndexedThroughVersion int64
	ChunkSetDigest        string
	ExpectedChunks        int64
}

type PublishRevisionCommand struct {
	Lease                    JobLease
	Now                      time.Time
	OwnerID                  string
	CorpusID                 string
	RevisionID               string
	ExpectedPolicyVersion    int64
	ExpectedActiveRevisionID *string
	ExpectedContentVersion   int64
}
