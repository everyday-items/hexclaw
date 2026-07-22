package knowledge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/internal/sqliteutil"
)

// SQLiteSemanticIndexRepository persists the semantic-index control plane in
// additive tables. It never reads or writes kb_chunks.embedding.
type SQLiteSemanticIndexRepository struct {
	db                *sql.DB
	ingestBlobStore   *localIngestBlobStore
	runningJobCancels *runningJobCancelRegistry
}

var _ SemanticIndexRepository = (*SQLiteSemanticIndexRepository)(nil)

func NewSQLiteSemanticIndexRepository(db *sql.DB) *SQLiteSemanticIndexRepository {
	return &SQLiteSemanticIndexRepository{
		db:                db,
		runningJobCancels: newRunningJobCancelRegistry(),
	}
}

func (r *SQLiteSemanticIndexRepository) registerRunningJobCancel(
	jobID string,
	cancel context.CancelFunc,
) func() {
	return r.runningJobCancels.register(jobID, cancel)
}

type semanticPolicyState struct {
	corpusUID       string
	contentVersion  int64
	activeRevision  string
	desiredRevision string
	version         int64
	selection       EmbeddingSelection
}

type semanticDBQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validateSemanticScope(ownerID, corpusID string) error {
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(corpusID) == "" {
		return fmt.Errorf("%w: owner and corpus are required", ErrSemanticIndexNotFound)
	}
	return nil
}

func semanticID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("knowledge: generate %s id: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(raw[:]), nil
}

func semanticNowMillis() int64 { return time.Now().UTC().UnixMilli() }

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func selectionProfileID(selection EmbeddingSelection) any {
	if selection.Kind != EmbeddingSelectionProfile {
		return nil
	}
	return selection.ProfileID
}

func loadSemanticPolicyState(
	ctx context.Context,
	q semanticDBQueryer,
	ownerID, corpusID string,
) (semanticPolicyState, error) {
	var state semanticPolicyState
	var active, desired, selected sql.NullString
	var kind string
	err := q.QueryRowContext(ctx, `SELECT c.corpus_uid,c.content_version,c.active_revision_id,
		p.desired_revision_id,p.version,p.selection_kind,p.selected_profile_id
		FROM kb_semantic_corpora c
		JOIN kb_embedding_policies p ON p.corpus_uid=c.corpus_uid
		WHERE c.owner_id=? AND c.corpus_alias=?`, ownerID, corpusID).Scan(
		&state.corpusUID, &state.contentVersion, &active, &desired,
		&state.version, &kind, &selected,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return semanticPolicyState{}, ErrSemanticIndexNotFound
	}
	if err != nil {
		return semanticPolicyState{}, fmt.Errorf("knowledge: load embedding policy: %w", err)
	}
	state.activeRevision = active.String
	state.desiredRevision = desired.String
	state.selection = EmbeddingSelection{Kind: EmbeddingSelectionKind(kind), ProfileID: selected.String}
	return state, nil
}

func (r *SQLiteSemanticIndexRepository) GetPolicy(
	ctx context.Context,
	ownerID, corpusID string,
) (EmbeddingPolicyProjection, error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return EmbeddingPolicyProjection{}, err
	}
	state, err := loadSemanticPolicyState(ctx, r.db, ownerID, corpusID)
	if err != nil {
		return EmbeddingPolicyProjection{}, err
	}
	projection := EmbeddingPolicyProjection{
		PolicyVersion: state.version,
		Selection:     state.selection,
		IndexingActivity: IndexingActivity{
			State: IndexingActivityIdle,
		},
		AvailableProfiles: []EmbeddingProfile{},
	}
	if state.activeRevision != "" {
		active, loadErr := r.loadRevisionProjection(ctx, state.corpusUID, state.activeRevision, false)
		if loadErr != nil {
			return EmbeddingPolicyProjection{}, loadErr
		}
		projection.ActiveRevision = &active
	}
	if state.desiredRevision != "" {
		desired, loadErr := r.loadRevisionProjection(ctx, state.corpusUID, state.desiredRevision, true)
		if loadErr != nil {
			return EmbeddingPolicyProjection{}, loadErr
		}
		projection.DesiredRevision = &desired
	}
	activity, err := r.loadIndexingActivity(ctx, state.corpusUID, state.activeRevision, state.desiredRevision)
	if err != nil {
		return EmbeddingPolicyProjection{}, err
	}
	projection.IndexingActivity = activity
	return projection, nil
}

func (r *SQLiteSemanticIndexRepository) loadRevisionProjection(
	ctx context.Context,
	corpusUID, revisionID string,
	desired bool,
) (EmbeddingRevisionProjection, error) {
	var projection EmbeddingRevisionProjection
	var location, availability string
	var expected sql.NullInt64
	var embedded int64
	var publishState string
	err := r.db.QueryRowContext(ctx, `SELECT r.revision_id,r.publish_state,r.expected_chunks,r.embedded_chunks,
		s.resolved_profile_id,s.model_name,s.provider_id,s.provider_name,s.provider_location,
		s.dimension,s.availability
		FROM kb_index_revisions r
		JOIN kb_embedding_profile_snapshots s ON s.profile_snapshot_id=r.profile_snapshot_id
		WHERE r.corpus_uid=? AND r.revision_id=?`, corpusUID, revisionID).Scan(
		&projection.RevisionID, &publishState, &expected, &embedded,
		&projection.Profile.ProfileID, &projection.Profile.ModelName,
		&projection.Profile.ProviderID, &projection.Profile.ProviderName,
		&location, &projection.Profile.Dimension, &availability,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return EmbeddingRevisionProjection{}, ErrSemanticIndexNotFound
	}
	if err != nil {
		return EmbeddingRevisionProjection{}, fmt.Errorf("knowledge: load index revision: %w", err)
	}
	projection.Profile.Location = ProviderLocation(location)
	projection.Profile.Availability = ProfileAvailability(availability)
	projection.Profile.Capability = "embedding"
	if expected.Valid {
		projection.ChunksDone = &embedded
		projection.ChunksTotal = &expected.Int64
	}
	if !desired && publishState == "active" {
		projection.State = VectorIndexReady
		return projection, nil
	}
	job, err := r.getRevisionJob(ctx, corpusUID, revisionID)
	if err != nil && !errors.Is(err, ErrSemanticIndexNotFound) {
		return EmbeddingRevisionProjection{}, err
	}
	if err == nil {
		projection.State = vectorStateForJob(job.State)
		if job.ChunksDone != nil && job.ChunksTotal != nil {
			projection.ChunksDone = job.ChunksDone
			projection.ChunksTotal = job.ChunksTotal
		}
		if job.State == KnowledgeJobQueued || job.State == KnowledgeJobRunning ||
			job.State == KnowledgeJobRetryWait || job.State == KnowledgeJobFailed {
			projection.JobID = stringPointer(job.JobID)
		}
	} else if publishState == "abandoned" {
		projection.State = VectorIndexCancelled
	} else {
		projection.State = VectorIndexPending
	}
	return projection, nil
}

func vectorStateForJob(state KnowledgeJobState) VectorIndexState {
	switch state {
	case KnowledgeJobQueued:
		return VectorIndexPending
	case KnowledgeJobRunning:
		return VectorIndexBuilding
	case KnowledgeJobRetryWait:
		return VectorIndexRetryWait
	case KnowledgeJobSucceeded:
		return VectorIndexReady
	case KnowledgeJobFailed:
		return VectorIndexFailed
	case KnowledgeJobCancelled:
		return VectorIndexCancelled
	default:
		return VectorIndexPending
	}
}

func (r *SQLiteSemanticIndexRepository) loadIndexingActivity(
	ctx context.Context,
	corpusUID, activeRevision, desiredRevision string,
) (IndexingActivity, error) {
	target := desiredRevision
	if target == "" {
		target = activeRevision
	}
	activity := IndexingActivity{State: IndexingActivityIdle}
	if target == "" {
		return activity, nil
	}
	query := `SELECT j.state,j.document_id,j.chunks_done,j.chunks_total
		FROM kb_knowledge_jobs j
		JOIN kb_semantic_document_bindings b
		  ON b.corpus_uid=j.corpus_uid AND b.document_id=j.document_id
		 AND b.content_generation=j.document_generation AND b.lifecycle_state='active'
		WHERE j.corpus_uid=? AND j.target_revision_id=? AND j.kind='embed_document'
		  AND j.state IN ('queued','running','retry_wait','failed')`
	if desiredRevision != "" {
		query = `SELECT state,'',chunks_done,chunks_total FROM kb_knowledge_jobs
			WHERE corpus_uid=? AND target_revision_id=? AND kind='rebuild_revision'
			  AND parent_job_id IS NULL AND state IN ('queued','running','retry_wait','failed')`
	}
	rows, err := r.db.QueryContext(ctx, query, corpusUID, target)
	if err != nil {
		return IndexingActivity{}, fmt.Errorf("knowledge: load indexing activity: %w", err)
	}
	defer rows.Close()
	documents := map[string]struct{}{}
	var done, total int64
	hasProgress := true
	hasJobs := false
	hasBuilding, hasRetry, hasFailed := false, false, false
	for rows.Next() {
		hasJobs = true
		var state, documentID string
		var chunksDone, chunksTotal sql.NullInt64
		if err := rows.Scan(&state, &documentID, &chunksDone, &chunksTotal); err != nil {
			return IndexingActivity{}, err
		}
		if documentID != "" {
			documents[documentID] = struct{}{}
		}
		switch KnowledgeJobState(state) {
		case KnowledgeJobQueued, KnowledgeJobRunning:
			hasBuilding = true
		case KnowledgeJobRetryWait:
			hasRetry = true
		case KnowledgeJobFailed:
			hasFailed = true
		}
		if !chunksDone.Valid || !chunksTotal.Valid {
			hasProgress = false
		} else {
			done += chunksDone.Int64
			total += chunksTotal.Int64
		}
	}
	if err := rows.Err(); err != nil {
		return IndexingActivity{}, err
	}
	activity.ProcessingDocuments = int64(len(documents))
	if desiredRevision != "" && hasJobs {
		if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_revision_documents d
			JOIN kb_semantic_document_bindings b
			  ON b.corpus_uid=d.corpus_uid AND b.document_id=d.document_id
			 AND b.content_generation=d.content_generation AND b.lifecycle_state='active'
			WHERE d.corpus_uid=? AND d.revision_id=?`, corpusUID, desiredRevision).Scan(
			&activity.ProcessingDocuments,
		); err != nil {
			return IndexingActivity{}, fmt.Errorf("knowledge: count current desired revision documents: %w", err)
		}
	}
	switch {
	case hasBuilding:
		activity.State = IndexingActivityBuilding
	case hasRetry:
		activity.State = IndexingActivityRetryWait
	case hasFailed:
		activity.State = IndexingActivityFailed
	}
	if hasJobs && hasProgress {
		activity.ChunksDone = &done
		activity.ChunksTotal = &total
	}
	return activity, nil
}

func (r *SQLiteSemanticIndexRepository) EnsureDefaultPolicy(
	ctx context.Context,
	ownerID, corpusID string,
	profile EmbeddingProfileSnapshot,
) (ApplyPolicyResult, error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return ApplyPolicyResult{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ApplyPolicyResult{}, err
	}
	defer tx.Rollback()
	state, err := loadSemanticPolicyState(ctx, tx, ownerID, corpusID)
	if err == nil {
		if state.version == 0 && state.selection.Kind == EmbeddingSelectionDisabled &&
			state.activeRevision == "" && state.desiredRevision == "" {
			if err := profile.Validate(); err != nil {
				return ApplyPolicyResult{}, err
			}
			if !profile.executableNow() {
				return ApplyPolicyResult{}, ErrProfileUnavailable
			}
			result, applyErr := r.applyPolicyTx(ctx, tx, state, 0,
				EmbeddingSelection{Kind: EmbeddingSelectionAuto}, &profile)
			if applyErr != nil {
				return ApplyPolicyResult{}, applyErr
			}
			if err := tx.Commit(); err != nil {
				return ApplyPolicyResult{}, err
			}
			return result, nil
		}
		// Any non-zero policy or frozen active/desired revision is already
		// initialized. Startup must preserve it verbatim; only explicit Apply can
		// re-resolve an auto intent against a changed catalog.
		result, resultErr := r.policyResultTx(ctx, tx, state, ApplyPolicyNoop)
		if resultErr != nil {
			return ApplyPolicyResult{}, resultErr
		}
		if err := tx.Commit(); err != nil {
			return ApplyPolicyResult{}, err
		}
		return result, nil
	}
	if !errors.Is(err, ErrSemanticIndexNotFound) {
		return ApplyPolicyResult{}, err
	}
	if err := profile.Validate(); err != nil {
		return ApplyPolicyResult{}, err
	}
	if !profile.executableNow() {
		return ApplyPolicyResult{}, ErrProfileUnavailable
	}
	corpusUID, err := semanticID("corpus")
	if err != nil {
		return ApplyPolicyResult{}, err
	}
	now := semanticNowMillis()
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_semantic_corpora
		(corpus_uid,owner_id,corpus_alias,content_version,active_revision_id,created_at,updated_at)
		VALUES(?,?,?,0,NULL,?,?)`, corpusUID, ownerID, corpusID, now, now); err != nil {
		return ApplyPolicyResult{}, fmt.Errorf("knowledge: create corpus: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_embedding_policies
		(corpus_uid,selection_kind,selected_profile_id,desired_revision_id,version,updated_at)
		VALUES(?,'disabled',NULL,NULL,0,?)`, corpusUID, now); err != nil {
		return ApplyPolicyResult{}, fmt.Errorf("knowledge: create embedding policy: %w", err)
	}
	state = semanticPolicyState{
		corpusUID: corpusUID,
		selection: EmbeddingSelection{Kind: EmbeddingSelectionDisabled},
	}
	result, err := r.applyPolicyTx(ctx, tx, state, 0,
		EmbeddingSelection{Kind: EmbeddingSelectionAuto}, &profile)
	if err != nil {
		return ApplyPolicyResult{}, err
	}
	if result.ActiveRevisionID == nil && result.DesiredRevisionID == nil {
		return ApplyPolicyResult{}, fmt.Errorf("knowledge: default auto policy has no executable revision")
	}
	if err := tx.Commit(); err != nil {
		return ApplyPolicyResult{}, err
	}
	return result, nil
}

func (r *SQLiteSemanticIndexRepository) ApplyPolicy(
	ctx context.Context,
	ownerID, corpusID string,
	expectedVersion int64,
	selection EmbeddingSelection,
	profile *EmbeddingProfileSnapshot,
) (ApplyPolicyResult, error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return ApplyPolicyResult{}, err
	}
	if err := selection.Validate(); err != nil {
		return ApplyPolicyResult{}, err
	}
	if selection.Kind != EmbeddingSelectionDisabled {
		if profile == nil {
			return ApplyPolicyResult{}, ErrInvalidEmbeddingProfile
		}
		if err := profile.Validate(); err != nil {
			return ApplyPolicyResult{}, err
		}
		if !profile.executableNow() {
			return ApplyPolicyResult{}, ErrProfileUnavailable
		}
	}
	var result ApplyPolicyResult
	err := sqliteutil.RetryOnBusy(ctx, func() error {
		var attemptErr error
		result, attemptErr = r.applyPolicyOnce(ctx, ownerID, corpusID, expectedVersion, selection, profile)
		return attemptErr
	})
	return result, err
}

// applyPolicyOnce executes one complete policy transaction. A BUSY snapshot
// must restart from loadSemanticPolicyState so a losing writer observes the
// winning policy version and returns the domain-level CAS conflict.
func (r *SQLiteSemanticIndexRepository) applyPolicyOnce(
	ctx context.Context,
	ownerID, corpusID string,
	expectedVersion int64,
	selection EmbeddingSelection,
	profile *EmbeddingProfileSnapshot,
) (ApplyPolicyResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ApplyPolicyResult{}, err
	}
	defer tx.Rollback()
	state, err := loadSemanticPolicyState(ctx, tx, ownerID, corpusID)
	if err != nil {
		return ApplyPolicyResult{}, err
	}
	result, err := r.applyPolicyTx(ctx, tx, state, expectedVersion, selection, profile)
	if err != nil {
		return ApplyPolicyResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ApplyPolicyResult{}, err
	}
	return result, nil
}

func (r *SQLiteSemanticIndexRepository) applyPolicyTx(
	ctx context.Context,
	tx *sql.Tx,
	state semanticPolicyState,
	expectedVersion int64,
	selection EmbeddingSelection,
	profile *EmbeddingProfileSnapshot,
) (ApplyPolicyResult, error) {
	if state.version != expectedVersion {
		return ApplyPolicyResult{}, ErrPolicyVersionConflict
	}
	if selection.Kind != EmbeddingSelectionDisabled && (profile == nil || !profile.executableNow()) {
		return ApplyPolicyResult{}, ErrProfileUnavailable
	}
	if selection.Kind == EmbeddingSelectionDisabled {
		if state.version > 0 && state.selection.Kind == EmbeddingSelectionDisabled && state.activeRevision == "" && state.desiredRevision == "" {
			return r.policyResultTx(ctx, tx, state, ApplyPolicyNoop)
		}
		newVersion := state.version + 1
		if err := r.updatePolicyTx(ctx, tx, state, newVersion, selection, ""); err != nil {
			return ApplyPolicyResult{}, err
		}
		if state.desiredRevision != "" {
			if err := r.abandonDesiredTx(ctx, tx, state.corpusUID, state.desiredRevision); err != nil {
				return ApplyPolicyResult{}, err
			}
		}
		if state.activeRevision != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE kb_index_revisions SET publish_state='superseded'
				WHERE corpus_uid=? AND revision_id=? AND publish_state='active'`, state.corpusUID, state.activeRevision); err != nil {
				return ApplyPolicyResult{}, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE kb_semantic_corpora SET active_revision_id=NULL,updated_at=?
			WHERE corpus_uid=?`, semanticNowMillis(), state.corpusUID); err != nil {
			return ApplyPolicyResult{}, err
		}
		state.version, state.selection = newVersion, selection
		state.activeRevision, state.desiredRevision = "", ""
		return r.policyResultTx(ctx, tx, state, ApplyPolicyDisabled)
	}

	targetHash := profile.ProfileConfigHash
	activeHash, err := r.revisionProfileHashTx(ctx, tx, state.corpusUID, state.activeRevision)
	if err != nil {
		return ApplyPolicyResult{}, err
	}
	desiredHash, err := r.revisionProfileHashTx(ctx, tx, state.corpusUID, state.desiredRevision)
	if err != nil {
		return ApplyPolicyResult{}, err
	}
	if state.desiredRevision != "" && desiredHash == targetHash && state.selection.equal(selection) {
		healthy, jobID, err := r.healthyDesiredJobTx(ctx, tx, state.corpusUID, state.desiredRevision)
		if err != nil {
			return ApplyPolicyResult{}, err
		}
		if healthy {
			result, err := r.policyResultTx(ctx, tx, state, ApplyPolicyNoop)
			if err == nil {
				result.JobID = stringPointer(jobID)
			}
			return result, err
		}
	}
	if state.activeRevision != "" && activeHash == targetHash {
		if state.desiredRevision == "" && state.selection.equal(selection) {
			return r.policyResultTx(ctx, tx, state, ApplyPolicyNoop)
		}
		newVersion := state.version + 1
		if err := r.updatePolicyTx(ctx, tx, state, newVersion, selection, ""); err != nil {
			return ApplyPolicyResult{}, err
		}
		if state.desiredRevision != "" {
			if err := r.abandonDesiredTx(ctx, tx, state.corpusUID, state.desiredRevision); err != nil {
				return ApplyPolicyResult{}, err
			}
		}
		state.version, state.selection, state.desiredRevision = newVersion, selection, ""
		if err := reconcileActiveRevisionTx(ctx, tx, "", state, semanticNowMillis()); err != nil {
			return ApplyPolicyResult{}, err
		}
		return r.policyResultTx(ctx, tx, state, ApplyPolicyIntentOnly)
	}

	newVersion := state.version + 1
	if state.desiredRevision != "" {
		if err := r.abandonDesiredTx(ctx, tx, state.corpusUID, state.desiredRevision); err != nil {
			return ApplyPolicyResult{}, err
		}
	}

	if profile.executableNow() && state.contentVersion == 0 {
		// Free the partial UNIQUE(active corpus) slot before inserting the new
		// active revision. Both this state change and the corpus pointer switch
		// remain invisible until the transaction commits, so readers never
		// observe a missing or double active revision.
		if state.activeRevision != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE kb_index_revisions
				SET publish_state='superseded',updated_at=?
				WHERE corpus_uid=? AND revision_id=? AND publish_state='active'`,
				semanticNowMillis(), state.corpusUID, state.activeRevision); err != nil {
				return ApplyPolicyResult{}, err
			}
		}
		revisionID, _, err := r.createRevisionTx(ctx, tx, state, newVersion, *profile, "active")
		if err != nil {
			return ApplyPolicyResult{}, err
		}
		if err := r.updatePolicyTx(ctx, tx, state, newVersion, selection, ""); err != nil {
			return ApplyPolicyResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE kb_semantic_corpora SET active_revision_id=?,updated_at=?
			WHERE corpus_uid=?`, revisionID, semanticNowMillis(), state.corpusUID); err != nil {
			return ApplyPolicyResult{}, err
		}
		state.version, state.selection = newVersion, selection
		state.activeRevision, state.desiredRevision = revisionID, ""
		return r.policyResultTx(ctx, tx, state, ApplyPolicyImmediatePublish)
	}

	revisionID, _, err := r.createRevisionTx(ctx, tx, state, newVersion, *profile, "staged")
	if err != nil {
		return ApplyPolicyResult{}, err
	}
	jobID, err := r.createRevisionJobTx(ctx, tx, state, revisionID, newVersion,
		KnowledgeJobRebuildRevision, profile.ProfileConfigHash)
	if err != nil {
		return ApplyPolicyResult{}, err
	}
	if err := r.updatePolicyTx(ctx, tx, state, newVersion, selection, revisionID); err != nil {
		return ApplyPolicyResult{}, err
	}
	state.version, state.selection, state.desiredRevision = newVersion, selection, revisionID
	result, err := r.policyResultTx(ctx, tx, state, ApplyPolicyStagedRebuild)
	if err == nil {
		result.JobID = stringPointer(jobID)
	}
	return result, err
}

func (r *SQLiteSemanticIndexRepository) updatePolicyTx(
	ctx context.Context,
	tx *sql.Tx,
	state semanticPolicyState,
	newVersion int64,
	selection EmbeddingSelection,
	desiredRevision string,
) error {
	res, err := tx.ExecContext(ctx, `UPDATE kb_embedding_policies
		SET selection_kind=?,selected_profile_id=?,desired_revision_id=?,version=?,updated_at=?
		WHERE corpus_uid=? AND version=?`, selection.Kind, selectionProfileID(selection),
		nullableString(desiredRevision), newVersion, semanticNowMillis(), state.corpusUID, state.version)
	if err != nil {
		return fmt.Errorf("knowledge: update embedding policy: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrPolicyVersionConflict
	}
	return nil
}

func (r *SQLiteSemanticIndexRepository) createRevisionTx(
	ctx context.Context,
	tx *sql.Tx,
	state semanticPolicyState,
	policyVersion int64,
	profile EmbeddingProfileSnapshot,
	publishState string,
) (string, string, error) {
	previousSelection, err := previousPublishedSelectionTx(ctx, tx, state)
	if err != nil {
		return "", "", err
	}
	snapshotID, err := semanticID("profile")
	if err != nil {
		return "", "", err
	}
	revisionID, err := semanticID("revision")
	if err != nil {
		return "", "", err
	}
	now := semanticNowMillis()
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_embedding_profile_snapshots
		(profile_snapshot_id,corpus_uid,resolved_profile_id,provider_id,provider_name,provider_location,
		 model_name,dimension,normalization,chunk_config_hash,profile_config_hash,availability,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, snapshotID, state.corpusUID,
		profile.Profile.ProfileID, profile.Profile.ProviderID, profile.Profile.ProviderName,
		profile.Profile.Location, profile.Profile.ModelName, profile.Profile.Dimension,
		profile.Normalization, profile.ChunkConfigHash, profile.ProfileConfigHash,
		profile.Profile.Availability, now); err != nil {
		return "", "", fmt.Errorf("knowledge: persist embedding profile snapshot: %w", err)
	}
	var expected any
	var published any
	indexedThrough := int64(0)
	if publishState == "active" {
		expected = int64(0)
		published = now
		indexedThrough = state.contentVersion
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_index_revisions
		(revision_id,corpus_uid,profile_snapshot_id,policy_version,previous_selection_kind,
		 previous_selected_profile_id,base_content_version,indexed_through_version,
		 previous_active_revision_id,publish_state,expected_chunks,embedded_chunks,failed_chunks,
		 lease_epoch,chunk_set_digest,created_at,published_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, revisionID, state.corpusUID, snapshotID,
		policyVersion, previousSelection.Kind, selectionProfileID(previousSelection), state.contentVersion,
		indexedThrough, nullableString(state.activeRevision), publishState, expected, 0, 0, 0, "", now, published, now); err != nil {
		return "", "", fmt.Errorf("knowledge: create index revision: %w", err)
	}
	if publishState == "staged" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_revision_documents
			(revision_id,corpus_uid,document_id,content_generation,vector_state,expected_chunks,
			 embedded_chunks,failed_chunks,visible_at,last_error,updated_at)
			SELECT ?,b.corpus_uid,b.document_id,b.content_generation,'pending',COUNT(c.id),
			       0,0,NULL,'',?
			FROM kb_semantic_document_bindings b
			LEFT JOIN kb_chunks c ON c.doc_id=b.document_id
			WHERE b.corpus_uid=? AND b.lifecycle_state='active' AND b.text_state='ready'
			GROUP BY b.corpus_uid,b.document_id,b.content_generation`, revisionID, now, state.corpusUID); err != nil {
			return "", "", fmt.Errorf("knowledge: create revision document manifests: %w", err)
		}
	}
	return revisionID, snapshotID, nil
}

// previousPublishedSelectionTx collapses a chain of uncommitted profile
// switches onto the policy intent that accompanied the revision still serving
// reads. A replacement desired revision must not snapshot the superseded
// desired intent: cancelling the replacement would otherwise restore a
// selection with neither an active compatible revision nor a rebuild job.
func previousPublishedSelectionTx(
	ctx context.Context,
	tx *sql.Tx,
	state semanticPolicyState,
) (EmbeddingSelection, error) {
	if state.desiredRevision == "" {
		return state.selection, nil
	}
	var kind string
	var profileID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT previous_selection_kind,previous_selected_profile_id
		FROM kb_index_revisions WHERE corpus_uid=? AND revision_id=?`,
		state.corpusUID, state.desiredRevision).Scan(&kind, &profileID); err != nil {
		return EmbeddingSelection{}, fmt.Errorf("knowledge: load desired rollback selection: %w", err)
	}
	selection := EmbeddingSelection{Kind: EmbeddingSelectionKind(kind), ProfileID: profileID.String}
	if err := selection.Validate(); err != nil {
		return EmbeddingSelection{}, fmt.Errorf("knowledge: invalid desired rollback selection: %w", err)
	}
	return selection, nil
}

func (r *SQLiteSemanticIndexRepository) createRevisionJobTx(
	ctx context.Context,
	tx *sql.Tx,
	state semanticPolicyState,
	revisionID string,
	policyVersion int64,
	kind KnowledgeJobKind,
	profileHash string,
) (string, error) {
	jobID, err := semanticID("job")
	if err != nil {
		return "", err
	}
	now := semanticNowMillis()
	idempotencyKey := fmt.Sprintf("%s|%s|%d|%s|%d", kind, profileHash, state.contentVersion, revisionID, policyVersion)
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_knowledge_jobs
		(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,target_revision_id,idempotency_key,
		 state,stage,attempt,cancel_requested,lease_owner,lease_epoch,last_error,created_at,updated_at)
		SELECT ?,NULL,?,c.owner_id,?,NULL,?,?, 'queued','embedding',0,0,'',0,'',?,?
		FROM kb_semantic_corpora c WHERE c.corpus_uid=?`, jobID, kind, state.corpusUID,
		revisionID, idempotencyKey, now, now, state.corpusUID); err != nil {
		return "", fmt.Errorf("knowledge: create semantic index job: %w", err)
	}
	return jobID, nil
}

func (r *SQLiteSemanticIndexRepository) revisionProfileHashTx(
	ctx context.Context,
	tx *sql.Tx,
	corpusUID, revisionID string,
) (string, error) {
	if revisionID == "" {
		return "", nil
	}
	var hash string
	err := tx.QueryRowContext(ctx, `SELECT s.profile_config_hash
		FROM kb_index_revisions r JOIN kb_embedding_profile_snapshots s
		ON s.profile_snapshot_id=r.profile_snapshot_id
		WHERE r.corpus_uid=? AND r.revision_id=?`, corpusUID, revisionID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSemanticIndexNotFound
	}
	return hash, err
}

func (r *SQLiteSemanticIndexRepository) healthyDesiredJobTx(
	ctx context.Context,
	tx *sql.Tx,
	corpusUID, revisionID string,
) (bool, string, error) {
	var jobID, state, publishState string
	err := tx.QueryRowContext(ctx, `SELECT j.job_id,j.state,r.publish_state
		FROM kb_index_revisions r JOIN kb_knowledge_jobs j ON j.target_revision_id=r.revision_id
		WHERE r.corpus_uid=? AND r.revision_id=? AND j.parent_job_id IS NULL
		ORDER BY j.created_at DESC LIMIT 1`, corpusUID, revisionID).Scan(&jobID, &state, &publishState)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	healthyState := state == string(KnowledgeJobQueued) || state == string(KnowledgeJobRunning) || state == string(KnowledgeJobRetryWait)
	return publishState == "staged" && healthyState, jobID, nil
}

func (r *SQLiteSemanticIndexRepository) abandonDesiredTx(
	ctx context.Context,
	tx *sql.Tx,
	corpusUID, revisionID string,
) error {
	now := semanticNowMillis()
	if _, err := tx.ExecContext(ctx, `UPDATE kb_index_revisions
		SET publish_state='abandoned',lease_epoch=lease_epoch+1
		WHERE corpus_uid=? AND revision_id=? AND publish_state='staged'`, corpusUID, revisionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs
		SET state='cancelled',cancel_requested=1,next_attempt_at=NULL,lease_epoch=lease_epoch+1,
		 lease_owner='',lease_expires_at=NULL,heartbeat_at=NULL,updated_at=?,finished_at=?
		WHERE corpus_uid=? AND target_revision_id=?
		  AND state IN ('queued','running','retry_wait')`, now, now, corpusUID, revisionID); err != nil {
		return err
	}
	return nil
}

func (r *SQLiteSemanticIndexRepository) policyResultTx(
	ctx context.Context,
	tx *sql.Tx,
	state semanticPolicyState,
	branch ApplyPolicyBranch,
) (ApplyPolicyResult, error) {
	result := ApplyPolicyResult{
		PolicyVersion: state.version, Selection: state.selection,
		ActiveRevisionID:  stringPointer(state.activeRevision),
		DesiredRevisionID: stringPointer(state.desiredRevision), Branch: branch,
	}
	if state.desiredRevision != "" {
		var jobID string
		err := tx.QueryRowContext(ctx, `SELECT job_id FROM kb_knowledge_jobs
			WHERE corpus_uid=? AND target_revision_id=? AND parent_job_id IS NULL
			ORDER BY created_at DESC LIMIT 1`, state.corpusUID, state.desiredRevision).Scan(&jobID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ApplyPolicyResult{}, err
		}
		result.JobID = stringPointer(jobID)
	}
	return result, nil
}

// BindLegacyDefaultCorpus explicitly assigns the pre-v23 global document rows
// to one owner/corpus. Callers must only invoke this for the single desktop
// legacy corpus they have already authenticated; the repository never guesses
// ownership. The command is idempotent for the same scope and fails closed if a
// document was already bound elsewhere.
func (r *SQLiteSemanticIndexRepository) BindLegacyDefaultCorpus(
	ctx context.Context,
	ownerID, corpusID string,
) (LegacyCorpusBinding, error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return LegacyCorpusBinding{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return LegacyCorpusBinding{}, err
	}
	defer tx.Rollback()
	state, err := loadSemanticPolicyState(ctx, tx, ownerID, corpusID)
	if errors.Is(err, ErrSemanticIndexNotFound) {
		corpusUID, idErr := semanticID("corpus")
		if idErr != nil {
			return LegacyCorpusBinding{}, idErr
		}
		now := semanticNowMillis()
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_semantic_corpora
			(corpus_uid,owner_id,corpus_alias,content_version,active_revision_id,created_at,updated_at)
			VALUES(?,?,?,0,NULL,?,?)`, corpusUID, ownerID, corpusID, now, now); err != nil {
			return LegacyCorpusBinding{}, fmt.Errorf("knowledge: create legacy corpus binding: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_embedding_policies
			(corpus_uid,selection_kind,selected_profile_id,desired_revision_id,version,updated_at)
			VALUES(?,'disabled',NULL,NULL,0,?)`, corpusUID, now); err != nil {
			return LegacyCorpusBinding{}, fmt.Errorf("knowledge: create legacy corpus policy: %w", err)
		}
		state = semanticPolicyState{
			corpusUID: corpusUID,
			selection: EmbeddingSelection{Kind: EmbeddingSelectionDisabled},
		}
	} else if err != nil {
		return LegacyCorpusBinding{}, err
	}
	var hasDocumentCorpusUID bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM pragma_table_info('kb_documents') WHERE name='corpus_uid'
	)`).Scan(&hasDocumentCorpusUID); err != nil {
		return LegacyCorpusBinding{}, fmt.Errorf("knowledge: inspect document scope column: %w", err)
	}
	legacyDocumentsSQL := `SELECT d.id FROM kb_documents d WHERE d.deleted=0`
	if hasDocumentCorpusUID {
		legacyDocumentsSQL += ` AND (d.corpus_uid IS NULL OR d.corpus_uid='')`
	}
	legacyDocumentsSQL += ` ORDER BY d.id`
	rows, err := tx.QueryContext(ctx, legacyDocumentsSQL)
	if err != nil {
		return LegacyCorpusBinding{}, fmt.Errorf("knowledge: list legacy documents: %w", err)
	}
	var documentIDs []string
	for rows.Next() {
		var documentID string
		if err := rows.Scan(&documentID); err != nil {
			rows.Close()
			return LegacyCorpusBinding{}, err
		}
		documentIDs = append(documentIDs, documentID)
	}
	if err := rows.Close(); err != nil {
		return LegacyCorpusBinding{}, err
	}
	now := semanticNowMillis()
	var inserted int64
	type newlyBoundDocument struct {
		documentID string
		generation int64
		chunks     int64
	}
	newlyBound := make([]newlyBoundDocument, 0, len(documentIDs))
	for _, documentID := range documentIDs {
		res, err := tx.ExecContext(ctx, `INSERT INTO kb_semantic_document_bindings
			(document_id,owner_id,corpus_uid,content_generation,lifecycle_state,text_state,
			 deleted_at,version,created_at,updated_at)
			VALUES(?,?,?,1,'active','ready',NULL,0,?,?)
			ON CONFLICT(document_id) DO NOTHING`, documentID, ownerID, state.corpusUID, now, now)
		if err != nil {
			return LegacyCorpusBinding{}, fmt.Errorf("knowledge: bind legacy document: %w", err)
		}
		n, _ := res.RowsAffected()
		generation := int64(1)
		if n == 0 {
			var existingOwner, existingCorpus string
			if err := tx.QueryRowContext(ctx, `SELECT owner_id,corpus_uid,content_generation
				FROM kb_semantic_document_bindings WHERE document_id=?`, documentID).Scan(
				&existingOwner, &existingCorpus, &generation,
			); err != nil {
				return LegacyCorpusBinding{}, err
			}
			if existingOwner != ownerID || existingCorpus != state.corpusUID {
				return LegacyCorpusBinding{}, ErrSemanticIndexNotFound
			}
		} else {
			inserted += n
			var chunks int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_chunks WHERE doc_id=?`, documentID).Scan(&chunks); err != nil {
				return LegacyCorpusBinding{}, err
			}
			newlyBound = append(newlyBound, newlyBoundDocument{
				documentID: documentID, generation: generation, chunks: chunks,
			})
		}
		// Backfill the immutable generation parent even for an already-bound
		// document. This closes the crash window between upgrading the schema
		// and a later startup reconcile without mutating historical rows.
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_semantic_document_generations
			(owner_id,corpus_uid,document_id,content_generation,created_at)
			VALUES(?,?,?,?,?) ON CONFLICT(corpus_uid,document_id,content_generation) DO NOTHING`,
			ownerID, state.corpusUID, documentID, generation, now); err != nil {
			return LegacyCorpusBinding{}, fmt.Errorf("knowledge: persist legacy document generation: %w", err)
		}
		if hasDocumentCorpusUID {
			res, err = tx.ExecContext(ctx, `UPDATE kb_documents SET corpus_uid=?
				WHERE id=? AND (corpus_uid IS NULL OR corpus_uid='')`, state.corpusUID, documentID)
			if err != nil {
				return LegacyCorpusBinding{}, fmt.Errorf("knowledge: assign legacy document corpus: %w", err)
			}
			if affected, _ := res.RowsAffected(); affected != 1 {
				return LegacyCorpusBinding{}, ErrSemanticIndexNotFound
			}
		}
	}
	if inserted > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE kb_semantic_corpora
			SET content_version=content_version+?,updated_at=? WHERE corpus_uid=?`,
			inserted, now, state.corpusUID); err != nil {
			return LegacyCorpusBinding{}, err
		}
	}
	// Once a policy has an executable revision, startup reconciliation extends
	// that immutable target instead of requiring an empty/uninitialized policy.
	// A desired rebuild takes precedence; otherwise each newly discovered
	// document is queued against the active revision. All rows are committed in
	// the same transaction as the binding/content-version bump.
	targetRevision, targetIsDesired, err := semanticMutationTargetRevisionTx(ctx, tx, state)
	if err != nil {
		return LegacyCorpusBinding{}, err
	}
	activeTarget := targetRevision != "" && !targetIsDesired
	if targetRevision != "" {
		for _, document := range newlyBound {
			if _, err := tx.ExecContext(ctx, `INSERT INTO kb_revision_documents
				(revision_id,corpus_uid,document_id,content_generation,vector_state,expected_chunks,
				 embedded_chunks,failed_chunks,visible_at,last_error,updated_at)
				VALUES(?,?,?,?,'pending',?,0,0,NULL,'',?)
				ON CONFLICT(revision_id,document_id,content_generation) DO NOTHING`,
				targetRevision, state.corpusUID, document.documentID, document.generation,
				document.chunks, now); err != nil {
				return LegacyCorpusBinding{}, fmt.Errorf("knowledge: reconcile revision document: %w", err)
			}
			if !activeTarget {
				continue
			}
			res, err := tx.ExecContext(ctx, `UPDATE kb_index_revisions
				SET expected_chunks=COALESCE(expected_chunks,0)+?,updated_at=?
				WHERE corpus_uid=? AND revision_id=? AND publish_state='active'`,
				document.chunks, now, state.corpusUID, targetRevision)
			if err != nil {
				return LegacyCorpusBinding{}, err
			}
			if affected, _ := res.RowsAffected(); affected != 1 {
				return LegacyCorpusBinding{}, ErrSemanticIndexNotFound
			}
			jobID, err := semanticID("job")
			if err != nil {
				return LegacyCorpusBinding{}, err
			}
			idempotencyKey := fmt.Sprintf("active-document|%s|%s|%d", targetRevision,
				document.documentID, document.generation)
			if _, err := tx.ExecContext(ctx, `INSERT INTO kb_knowledge_jobs
				(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,document_generation,
				 target_revision_id,idempotency_key,state,stage,chunks_done,chunks_total,attempt,
				 cancel_requested,lease_owner,lease_epoch,last_error,created_at,updated_at)
				VALUES(?,NULL,'embed_document',?,?,?,?,?,?,'queued','embedding',0,?,0,0,'',0,'',?,?)`,
				jobID, ownerID, state.corpusUID, document.documentID, document.generation,
				targetRevision, idempotencyKey, document.chunks, now, now); err != nil {
				return LegacyCorpusBinding{}, fmt.Errorf("knowledge: queue reconciled document: %w", err)
			}
		}
	}
	var binding LegacyCorpusBinding
	binding.CorpusUID = state.corpusUID
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT b.document_id),COUNT(c.id),c0.content_version
		FROM kb_semantic_corpora c0
		LEFT JOIN kb_semantic_document_bindings b ON b.corpus_uid=c0.corpus_uid AND b.lifecycle_state='active'
		LEFT JOIN kb_chunks c ON c.doc_id=b.document_id
		WHERE c0.corpus_uid=? GROUP BY c0.content_version`, state.corpusUID).Scan(
		&binding.Documents, &binding.Chunks, &binding.ContentVersion,
	); err != nil {
		return LegacyCorpusBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return LegacyCorpusBinding{}, err
	}
	return binding, nil
}

// RecordCorpusContentChange is the future Manager integration seam. It only
// advances the corpus CAS version; document/chunk binding is a separate worker
// input operation.
func (r *SQLiteSemanticIndexRepository) RecordCorpusContentChange(
	ctx context.Context,
	ownerID, corpusID string,
) (int64, error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return 0, err
	}
	res, err := r.db.ExecContext(ctx, `UPDATE kb_semantic_corpora
		SET content_version=content_version+1,updated_at=? WHERE owner_id=? AND corpus_alias=?`,
		semanticNowMillis(), ownerID, corpusID)
	if err != nil {
		return 0, err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return 0, ErrSemanticIndexNotFound
	}
	var version int64
	if err := r.db.QueryRowContext(ctx, `SELECT content_version FROM kb_semantic_corpora
		WHERE owner_id=? AND corpus_alias=?`, ownerID, corpusID).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

func (r *SQLiteSemanticIndexRepository) GetJob(ctx context.Context, ownerID, jobID string) (KnowledgeJob, error) {
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(jobID) == "" {
		return KnowledgeJob{}, ErrSemanticIndexNotFound
	}
	return scanSemanticJob(r.db.QueryRowContext(ctx, semanticJobSelect+` WHERE owner_id=? AND job_id=?`, ownerID, jobID))
}

func (r *SQLiteSemanticIndexRepository) GetJobForCorpus(
	ctx context.Context, ownerID, corpusID, jobID string,
) (KnowledgeJob, error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil || strings.TrimSpace(jobID) == "" {
		return KnowledgeJob{}, ErrSemanticIndexNotFound
	}
	return scanSemanticJob(r.db.QueryRowContext(ctx, semanticJobSelect+`
		WHERE owner_id=? AND corpus_uid=(
			SELECT c.corpus_uid FROM kb_semantic_corpora c
			WHERE c.owner_id=? AND c.corpus_alias=?
		) AND job_id=?`, ownerID, ownerID, corpusID, jobID))
}

func (r *SQLiteSemanticIndexRepository) getRevisionJob(
	ctx context.Context,
	corpusUID, revisionID string,
) (KnowledgeJob, error) {
	return scanSemanticJob(r.db.QueryRowContext(ctx, semanticJobSelect+`
		WHERE corpus_uid=? AND target_revision_id=? AND parent_job_id IS NULL
		ORDER BY created_at DESC LIMIT 1`, corpusUID, revisionID))
}

const semanticJobSelect = `SELECT job_id,COALESCE(parent_job_id,''),kind,owner_id,corpus_uid,
	COALESCE(document_id,''),COALESCE(document_generation,0),COALESCE(target_revision_id,''),state,stage,
	pages_done,pages_total,chunks_done,chunks_total,attempt,next_attempt_at,cancel_requested,
	lease_owner,lease_epoch,lease_expires_at,heartbeat_at,last_error,created_at,updated_at
	FROM kb_knowledge_jobs`

func scanSemanticJob(row *sql.Row) (KnowledgeJob, error) {
	var job KnowledgeJob
	var pagesDone, pagesTotal, chunksDone, chunksTotal sql.NullInt64
	var nextAttempt, leaseExpires, heartbeat sql.NullInt64
	var cancelRequested int
	var createdAt, updatedAt int64
	err := row.Scan(
		&job.JobID, &job.ParentJobID, &job.Kind, &job.OwnerID, &job.CorpusUID,
		&job.DocumentID, &job.DocumentGeneration, &job.TargetRevisionID, &job.State, &job.Stage,
		&pagesDone, &pagesTotal, &chunksDone, &chunksTotal, &job.Attempt,
		&nextAttempt, &cancelRequested, &job.LeaseOwner, &job.LeaseEpoch,
		&leaseExpires, &heartbeat, &job.LastError, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return KnowledgeJob{}, ErrSemanticIndexNotFound
	}
	if err != nil {
		return KnowledgeJob{}, fmt.Errorf("knowledge: scan semantic index job: %w", err)
	}
	job.PagesDone, job.PagesTotal = int64Pointer(pagesDone), int64Pointer(pagesTotal)
	job.ChunksDone, job.ChunksTotal = int64Pointer(chunksDone), int64Pointer(chunksTotal)
	job.NextAttemptAt, job.LeaseExpiresAt, job.HeartbeatAt = timePointer(nextAttempt), timePointer(leaseExpires), timePointer(heartbeat)
	job.CancelRequested = cancelRequested != 0
	job.CreatedAt = time.UnixMilli(createdAt).UTC()
	job.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return job, nil
}

func int64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	v := value.Int64
	return &v
}

func timePointer(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	v := time.UnixMilli(value.Int64).UTC()
	return &v
}

func (r *SQLiteSemanticIndexRepository) CancelJob(ctx context.Context, ownerID, jobID string) (KnowledgeJob, error) {
	var job KnowledgeJob
	err := sqliteutil.RetryOnBusy(ctx, func() error {
		var attemptErr error
		job, attemptErr = r.cancelJobOnce(ctx, ownerID, jobID)
		return attemptErr
	})
	if err == nil {
		r.runningJobCancels.cancel(jobID)
	}
	return job, err
}

// cancelJobOnce executes one complete cancellation transaction. The terminal
// job is read before commit, keeping all fallible work inside the retry attempt;
// a successful commit is therefore never followed by a retryable operation.
func (r *SQLiteSemanticIndexRepository) cancelJobOnce(
	ctx context.Context,
	ownerID, jobID string,
) (KnowledgeJob, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return KnowledgeJob{}, err
	}
	defer tx.Rollback()
	job, err := scanSemanticJob(tx.QueryRowContext(ctx, semanticJobSelect+` WHERE owner_id=? AND job_id=?`, ownerID, jobID))
	if err != nil {
		return KnowledgeJob{}, err
	}
	if (job.State == KnowledgeJobSucceeded && job.Kind != KnowledgeJobIngest) || job.State == KnowledgeJobCancelled ||
		(job.State == KnowledgeJobFailed && job.CancelRequested) {
		if err := tx.Commit(); err != nil {
			return KnowledgeJob{}, err
		}
		return job, nil
	}
	now := semanticNowMillis()
	if job.State == KnowledgeJobFailed {
		if _, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs
			SET cancel_requested=1,lease_epoch=lease_epoch+1,updated_at=?
			WHERE job_id=? AND owner_id=? AND corpus_uid=? AND state='failed'`,
			now, jobID, ownerID, job.CorpusUID); err != nil {
			return KnowledgeJob{}, err
		}
	}
	if job.State == KnowledgeJobSucceeded && job.Kind == KnowledgeJobIngest {
		if _, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs
			SET cancel_requested=1,lease_epoch=lease_epoch+1,updated_at=?
			WHERE job_id=? AND owner_id=? AND corpus_uid=? AND state='succeeded'`,
			now, jobID, ownerID, job.CorpusUID); err != nil {
			return KnowledgeJob{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs SET state='cancelled',cancel_requested=1,next_attempt_at=NULL,
		lease_epoch=lease_epoch+1,lease_owner='',lease_expires_at=NULL,heartbeat_at=NULL,updated_at=?,finished_at=?
		WHERE owner_id=? AND corpus_uid=? AND (job_id=? OR parent_job_id=?)
		  AND state IN ('queued','running','retry_wait')`, now, now, ownerID, job.CorpusUID, jobID, jobID); err != nil {
		return KnowledgeJob{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kb_embedding_batch_manifests
		SET state='cancelled',next_attempt_at=NULL,last_error='',updated_at=?
		WHERE job_id IN (
		  SELECT child.job_id FROM kb_knowledge_jobs child
		  WHERE child.owner_id=? AND child.corpus_uid=?
		    AND (child.job_id=? OR child.parent_job_id=?)
		) AND state IN ('prepared','in_flight','retry_wait','outcome_unknown')`,
		now, ownerID, job.CorpusUID, jobID, jobID); err != nil {
		return KnowledgeJob{}, err
	}
	if job.Kind == KnowledgeJobIngest && job.DocumentID != "" && job.State != KnowledgeJobSucceeded {
		if _, err := tx.ExecContext(ctx, `UPDATE kb_documents
			SET deleted=1,status='cancelled',error_message='',updated_at=?
			WHERE id=? AND corpus_uid=? AND deleted=0 AND status IN ('processing','failed')`,
			time.UnixMilli(now).UTC(), job.DocumentID, job.CorpusUID); err != nil {
			return KnowledgeJob{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE kb_semantic_document_bindings
			SET lifecycle_state='tombstoned',text_state='failed',deleted_at=?,version=version+1,updated_at=?
			WHERE owner_id=? AND corpus_uid=? AND document_id=? AND content_generation=?
			  AND lifecycle_state='active'`, now, now, ownerID, job.CorpusUID,
			job.DocumentID, job.DocumentGeneration); err != nil {
			return KnowledgeJob{}, err
		}
	}
	if job.Kind == KnowledgeJobEmbedDocument && job.TargetRevisionID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE kb_revision_documents
			SET vector_state='cancelled',visible_at=NULL,last_error='',updated_at=?
			WHERE corpus_uid=? AND revision_id=? AND document_id=? AND content_generation=?
			  AND vector_state IN ('pending','building','retry_wait','ready','failed')`, now, job.CorpusUID,
			job.TargetRevisionID, job.DocumentID, job.DocumentGeneration); err != nil {
			return KnowledgeJob{}, err
		}
	}
	if job.Kind == KnowledgeJobIngest {
		if _, err := tx.ExecContext(ctx, `UPDATE kb_revision_documents AS rd
			SET vector_state='cancelled',visible_at=NULL,last_error='',updated_at=?
			WHERE rd.corpus_uid=? AND rd.vector_state IN ('pending','building','retry_wait','ready')
			  AND EXISTS (
			    SELECT 1 FROM kb_knowledge_jobs child
			    WHERE child.parent_job_id=? AND child.owner_id=? AND child.corpus_uid=rd.corpus_uid
			      AND child.target_revision_id=rd.revision_id AND child.document_id=rd.document_id
			      AND child.document_generation=rd.content_generation AND child.state='cancelled'
			  )`, now, job.CorpusUID, job.JobID, ownerID); err != nil {
			return KnowledgeJob{}, err
		}
	}
	if job.TargetRevisionID != "" {
		var previousKind string
		var previousProfile sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT previous_selection_kind,previous_selected_profile_id
			FROM kb_index_revisions WHERE corpus_uid=? AND revision_id=?`,
			job.CorpusUID, job.TargetRevisionID).Scan(&previousKind, &previousProfile)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return KnowledgeJob{}, err
		}
		if err == nil {
			state, loadErr := loadSemanticPolicyStateByUID(ctx, tx, ownerID, job.CorpusUID)
			if loadErr != nil {
				return KnowledgeJob{}, loadErr
			}
			if state.desiredRevision == job.TargetRevisionID {
				restore := EmbeddingSelection{Kind: EmbeddingSelectionKind(previousKind), ProfileID: previousProfile.String}
				if state.activeRevision == "" {
					restore = EmbeddingSelection{Kind: EmbeddingSelectionDisabled}
				}
				if err := r.updatePolicyTx(ctx, tx, state, state.version+1, restore, ""); err != nil {
					return KnowledgeJob{}, err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE kb_index_revisions
					SET publish_state='abandoned',lease_epoch=lease_epoch+1
					WHERE corpus_uid=? AND revision_id=? AND publish_state='staged'`,
					job.CorpusUID, job.TargetRevisionID); err != nil {
					return KnowledgeJob{}, err
				}
				// Mutations made while the desired revision was building were
				// intentionally routed there. Once it is cancelled, reconcile any
				// missing current generations into the restored active revision in
				// this same transaction so search cannot remain permanently text-only.
				if err := reconcileActiveRevisionTx(ctx, tx, ownerID, state, now); err != nil {
					return KnowledgeJob{}, err
				}
			}
		}
	}
	job, err = scanSemanticJob(tx.QueryRowContext(ctx, semanticJobSelect+` WHERE owner_id=? AND job_id=?`, ownerID, jobID))
	if err != nil {
		return KnowledgeJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return KnowledgeJob{}, err
	}
	return job, nil
}

func (r *SQLiteSemanticIndexRepository) CancelJobForCorpus(
	ctx context.Context, ownerID, corpusID, jobID string,
) (KnowledgeJob, error) {
	if _, err := r.GetJobForCorpus(ctx, ownerID, corpusID, jobID); err != nil {
		return KnowledgeJob{}, err
	}
	// corpus_uid and job_id are immutable; after the scoped read succeeds the
	// existing transactional cancellation command cannot cross corpora.
	return r.CancelJob(ctx, ownerID, jobID)
}

func loadSemanticPolicyStateByUID(
	ctx context.Context,
	q semanticDBQueryer,
	ownerID, corpusUID string,
) (semanticPolicyState, error) {
	var state semanticPolicyState
	var active, desired, selected sql.NullString
	var kind string
	err := q.QueryRowContext(ctx, `SELECT c.corpus_uid,c.content_version,c.active_revision_id,
		p.desired_revision_id,p.version,p.selection_kind,p.selected_profile_id
		FROM kb_semantic_corpora c JOIN kb_embedding_policies p ON p.corpus_uid=c.corpus_uid
		WHERE c.owner_id=? AND c.corpus_uid=?`, ownerID, corpusUID).Scan(
		&state.corpusUID, &state.contentVersion, &active, &desired, &state.version, &kind, &selected,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return semanticPolicyState{}, ErrSemanticIndexNotFound
	}
	if err != nil {
		return semanticPolicyState{}, err
	}
	state.activeRevision, state.desiredRevision = active.String, desired.String
	state.selection = EmbeddingSelection{Kind: EmbeddingSelectionKind(kind), ProfileID: selected.String}
	return state, nil
}

// ClaimNextJobForCorpus atomically claims queued/retryable work or steals an
// expired lease within one authenticated owner/corpus scope. Explicit now makes
// expiry behavior deterministic in tests and workers.
func (r *SQLiteSemanticIndexRepository) ClaimNextJobForCorpus(
	ctx context.Context,
	ownerID, corpusID string,
	workerID string,
	now time.Time,
	leaseDuration time.Duration,
) (KnowledgeJob, bool, error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return KnowledgeJob{}, false, err
	}
	if strings.TrimSpace(workerID) == "" || leaseDuration <= 0 {
		return KnowledgeJob{}, false, fmt.Errorf("knowledge: invalid worker lease")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return KnowledgeJob{}, false, err
	}
	defer tx.Rollback()
	state, err := loadSemanticPolicyState(ctx, tx, ownerID, corpusID)
	if err != nil {
		return KnowledgeJob{}, false, err
	}
	nowMillis := now.UTC().UnixMilli()
	var jobID string
	err = tx.QueryRowContext(ctx, `SELECT job_id FROM kb_knowledge_jobs
		WHERE owner_id=? AND corpus_uid=? AND cancel_requested=0 AND (
		 state='queued' OR
		 (state='retry_wait' AND next_attempt_at IS NOT NULL AND next_attempt_at<=?) OR
		 (state='running' AND lease_expires_at IS NOT NULL AND lease_expires_at<=?)
		) ORDER BY created_at,job_id LIMIT 1`, ownerID, state.corpusUID, nowMillis, nowMillis).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return KnowledgeJob{}, false, nil
	}
	if err != nil {
		return KnowledgeJob{}, false, err
	}
	expires := now.Add(leaseDuration).UTC().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs SET state='running',attempt=attempt+1,
		next_attempt_at=NULL,lease_owner=?,lease_epoch=lease_epoch+1,
		lease_expires_at=?,heartbeat_at=?,updated_at=?
		WHERE job_id=? AND owner_id=? AND corpus_uid=? AND cancel_requested=0 AND (
		 state='queued' OR
		 (state='retry_wait' AND next_attempt_at IS NOT NULL AND next_attempt_at<=?) OR
		 (state='running' AND lease_expires_at IS NOT NULL AND lease_expires_at<=?)
		)`, workerID, expires, nowMillis, nowMillis, jobID, ownerID, state.corpusUID, nowMillis, nowMillis)
	if err != nil {
		return KnowledgeJob{}, false, err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return KnowledgeJob{}, false, nil
	}
	job, err := scanSemanticJob(tx.QueryRowContext(ctx, semanticJobSelect+` WHERE job_id=?`, jobID))
	if err != nil {
		return KnowledgeJob{}, false, err
	}
	if job.TargetRevisionID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE kb_index_revisions SET lease_epoch=?
			WHERE revision_id=? AND corpus_uid=? AND publish_state='staged'`,
			job.LeaseEpoch, job.TargetRevisionID, job.CorpusUID); err != nil {
			return KnowledgeJob{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return KnowledgeJob{}, false, err
	}
	return job, true, nil
}

func validateJobProgress(update JobProgressUpdate) error {
	for _, pair := range []struct{ done, total *int64 }{
		{update.PagesDone, update.PagesTotal}, {update.ChunksDone, update.ChunksTotal},
	} {
		if (pair.done == nil) != (pair.total == nil) {
			return fmt.Errorf("knowledge: progress done/total must be set together")
		}
		if pair.done != nil && (*pair.done < 0 || *pair.total < 0 || *pair.done > *pair.total) {
			return fmt.Errorf("knowledge: invalid progress range")
		}
	}
	return nil
}

func (r *SQLiteSemanticIndexRepository) SaveJobProgress(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	update JobProgressUpdate,
) error {
	if err := validateJobProgress(update); err != nil {
		return err
	}
	pagesSet, chunksSet := update.PagesDone != nil, update.ChunksDone != nil
	var pagesDone, pagesTotal, chunksDone, chunksTotal any
	if pagesSet {
		pagesDone, pagesTotal = *update.PagesDone, *update.PagesTotal
	}
	if chunksSet {
		chunksDone, chunksTotal = *update.ChunksDone, *update.ChunksTotal
	}
	res, err := r.db.ExecContext(ctx, `UPDATE kb_knowledge_jobs SET stage=?,
		pages_done=CASE WHEN ? THEN ? ELSE pages_done END,
		pages_total=CASE WHEN ? THEN ? ELSE pages_total END,
		chunks_done=CASE WHEN ? THEN ? ELSE chunks_done END,
		chunks_total=CASE WHEN ? THEN ? ELSE chunks_total END,
		heartbeat_at=?,updated_at=?
		WHERE job_id=? AND owner_id=? AND corpus_uid=? AND state='running'
		 AND cancel_requested=0 AND lease_owner=? AND lease_epoch=? AND lease_expires_at>?`,
		update.Stage, pagesSet, pagesDone, pagesSet, pagesTotal,
		chunksSet, chunksDone, chunksSet, chunksTotal,
		now.UTC().UnixMilli(), now.UTC().UnixMilli(), lease.JobID, lease.OwnerID,
		lease.CorpusUID, lease.WorkerID, lease.Epoch, now.UTC().UnixMilli())
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrJobFenced
	}
	return nil
}

// RenewJobLease extends one live lease without changing its fencing epoch.
func (r *SQLiteSemanticIndexRepository) RenewJobLease(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	leaseDuration time.Duration,
) (JobLease, error) {
	if leaseDuration <= 0 {
		return JobLease{}, fmt.Errorf("knowledge: lease duration must be positive")
	}
	nowMillis := now.UTC().UnixMilli()
	expiresAt := now.Add(leaseDuration).UTC()
	res, err := r.db.ExecContext(ctx, `UPDATE kb_knowledge_jobs
		SET lease_expires_at=?,heartbeat_at=?,updated_at=?
		WHERE job_id=? AND owner_id=? AND corpus_uid=? AND state='running' AND cancel_requested=0
		 AND lease_owner=? AND lease_epoch=? AND lease_expires_at>?`, expiresAt.UnixMilli(),
		nowMillis, nowMillis, lease.JobID, lease.OwnerID, lease.CorpusUID,
		lease.WorkerID, lease.Epoch, nowMillis)
	if err != nil {
		return JobLease{}, err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return JobLease{}, ErrJobFenced
	}
	lease.ExpiresAt = expiresAt
	return lease, nil
}

func (r *SQLiteSemanticIndexRepository) RetryJob(
	ctx context.Context,
	lease JobLease,
	now, nextAttempt time.Time,
	lastError string,
) (KnowledgeJob, error) {
	if !nextAttempt.After(now) {
		return KnowledgeJob{}, fmt.Errorf("knowledge: retry next_attempt_at must be in the future")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return KnowledgeJob{}, err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return KnowledgeJob{}, err
	}
	nowMillis := now.UTC().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs SET state='retry_wait',
		next_attempt_at=?,last_error=?,lease_owner='',lease_epoch=lease_epoch+1,
		lease_expires_at=NULL,heartbeat_at=NULL,updated_at=?
		WHERE job_id=? AND owner_id=? AND corpus_uid=? AND state='running'
		 AND lease_owner=? AND lease_epoch=? AND cancel_requested=0`, nextAttempt.UTC().UnixMilli(),
		lastError, nowMillis, job.JobID, job.OwnerID, job.CorpusUID, lease.WorkerID, lease.Epoch)
	if err != nil {
		return KnowledgeJob{}, err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return KnowledgeJob{}, ErrJobFenced
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kb_embedding_batch_manifests
		SET state='retry_wait',next_attempt_at=?,last_error=?,updated_at=?
		WHERE job_id=? AND state='in_flight' AND lease_epoch=?`,
		nextAttempt.UTC().UnixMilli(), lastError, nowMillis, job.JobID, lease.Epoch); err != nil {
		return KnowledgeJob{}, err
	}
	if job.TargetRevisionID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE kb_index_revisions SET lease_epoch=lease_epoch+1,updated_at=?
			WHERE revision_id=? AND corpus_uid=? AND lease_epoch=?`, nowMillis,
			job.TargetRevisionID, job.CorpusUID, lease.Epoch); err != nil {
			return KnowledgeJob{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return KnowledgeJob{}, err
	}
	return r.GetJob(ctx, job.OwnerID, job.JobID)
}

func (r *SQLiteSemanticIndexRepository) FailJob(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	lastError string,
) (KnowledgeJob, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return KnowledgeJob{}, err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return KnowledgeJob{}, err
	}
	nowMillis := now.UTC().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs SET state='failed',next_attempt_at=NULL,
		last_error=?,lease_owner='',lease_epoch=lease_epoch+1,lease_expires_at=NULL,
		heartbeat_at=NULL,updated_at=?,finished_at=?
		WHERE job_id=? AND owner_id=? AND corpus_uid=? AND state='running'
		 AND lease_owner=? AND lease_epoch=? AND cancel_requested=0`, lastError, nowMillis, nowMillis,
		job.JobID, job.OwnerID, job.CorpusUID, lease.WorkerID, lease.Epoch)
	if err != nil {
		return KnowledgeJob{}, err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return KnowledgeJob{}, ErrJobFenced
	}
	// outcome_unknown is deliberately excluded: a timeout/transport break is
	// not converted into a retryable or ordinary failed batch by the enclosing
	// Job transition. It remains queryable until explicit reconciliation/cancel.
	if _, err := tx.ExecContext(ctx, `UPDATE kb_embedding_batch_manifests
		SET state='failed',next_attempt_at=NULL,last_error=?,updated_at=?
		WHERE job_id=? AND state IN ('prepared','in_flight','retry_wait')`,
		lastError, nowMillis, job.JobID); err != nil {
		return KnowledgeJob{}, err
	}
	if job.Kind == KnowledgeJobIngest && job.DocumentID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE kb_documents
			SET status='failed',error_message=?,updated_at=?
			WHERE id=? AND corpus_uid=? AND deleted=0 AND status='processing'`, lastError,
			time.UnixMilli(nowMillis).UTC(), job.DocumentID, job.CorpusUID); err != nil {
			return KnowledgeJob{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE kb_semantic_document_bindings
			SET text_state='failed',version=version+1,updated_at=?
			WHERE owner_id=? AND corpus_uid=? AND document_id=? AND content_generation=?
			  AND lifecycle_state='active'`, nowMillis, job.OwnerID, job.CorpusUID,
			job.DocumentID, job.DocumentGeneration); err != nil {
			return KnowledgeJob{}, err
		}
	}
	if job.Kind == KnowledgeJobEmbedDocument && job.DocumentID != "" && job.TargetRevisionID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE kb_revision_documents
			SET vector_state='failed',failed_chunks=COALESCE(expected_chunks,0)-embedded_chunks,
			    last_error=?,visible_at=NULL,updated_at=?
			WHERE corpus_uid=? AND revision_id=? AND document_id=? AND content_generation=?
			  AND vector_state IN ('pending','building','retry_wait')
			  AND expected_chunks IS NOT NULL`, lastError, nowMillis, job.CorpusUID,
			job.TargetRevisionID, job.DocumentID, job.DocumentGeneration); err != nil {
			return KnowledgeJob{}, err
		}
		if err := refreshActiveRevisionAggregatesTx(ctx, tx, job.CorpusUID,
			job.TargetRevisionID, nowMillis); err != nil {
			return KnowledgeJob{}, err
		}
	}
	if job.TargetRevisionID != "" {
		state, loadErr := loadSemanticPolicyStateByUID(ctx, tx, job.OwnerID, job.CorpusUID)
		if loadErr != nil {
			return KnowledgeJob{}, loadErr
		}
		if state.desiredRevision == job.TargetRevisionID {
			revisionResult, updateErr := tx.ExecContext(ctx, `UPDATE kb_index_revisions
				SET publish_state='abandoned',lease_epoch=lease_epoch+1,updated_at=?
				WHERE revision_id=? AND corpus_uid=? AND publish_state='staged' AND lease_epoch=?`,
				nowMillis, job.TargetRevisionID, job.CorpusUID, lease.Epoch)
			if updateErr != nil {
				return KnowledgeJob{}, updateErr
			}
			if affected, _ := revisionResult.RowsAffected(); affected != 1 {
				return KnowledgeJob{}, ErrJobFenced
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE kb_index_revisions
			SET lease_epoch=lease_epoch+1,updated_at=?
			WHERE revision_id=? AND corpus_uid=? AND lease_epoch=?`,
			nowMillis, job.TargetRevisionID, job.CorpusUID, lease.Epoch); err != nil {
			return KnowledgeJob{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return KnowledgeJob{}, err
	}
	return r.GetJob(ctx, job.OwnerID, job.JobID)
}

func validateStageCheckpoint(checkpoint StageCheckpoint) error {
	if checkpoint.Stage == "" || strings.TrimSpace(checkpoint.InputFingerprint) == "" {
		return fmt.Errorf("knowledge: checkpoint stage and input fingerprint are required")
	}
	if checkpoint.State != StageCheckpointPrepared && checkpoint.State != StageCheckpointSucceeded {
		return fmt.Errorf("knowledge: invalid checkpoint state %q", checkpoint.State)
	}
	return nil
}

// SaveStageCheckpoint commits a resumable stage artifact only while the exact
// lease remains live. A succeeded checkpoint is immutable and identical replay
// is idempotent.
func (r *SQLiteSemanticIndexRepository) SaveStageCheckpoint(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	checkpoint StageCheckpoint,
) error {
	if err := validateStageCheckpoint(checkpoint); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nowMillis := now.UTC().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs SET heartbeat_at=?,updated_at=?
		WHERE job_id=? AND owner_id=? AND corpus_uid=? AND state='running' AND cancel_requested=0
		 AND lease_owner=? AND lease_epoch=? AND lease_expires_at>?`, nowMillis, nowMillis,
		lease.JobID, lease.OwnerID, lease.CorpusUID, lease.WorkerID, lease.Epoch, nowMillis)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrJobFenced
	}
	var existing StageCheckpoint
	var state string
	var createdAt, updatedAt int64
	err = tx.QueryRowContext(ctx, `SELECT input_fingerprint,artifact_ref,artifact_digest,state,
		lease_epoch,created_at,updated_at FROM kb_job_stage_checkpoints WHERE job_id=? AND stage=?`,
		lease.JobID, checkpoint.Stage).Scan(&existing.InputFingerprint, &existing.ArtifactRef,
		&existing.ArtifactDigest, &state, &existing.LeaseEpoch, &createdAt, &updatedAt)
	if err == nil && StageCheckpointState(state) == StageCheckpointSucceeded {
		if existing.InputFingerprint != checkpoint.InputFingerprint || existing.ArtifactRef != checkpoint.ArtifactRef ||
			existing.ArtifactDigest != checkpoint.ArtifactDigest || checkpoint.State != StageCheckpointSucceeded {
			return fmt.Errorf("knowledge: succeeded checkpoint is immutable")
		}
		return tx.Commit()
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_job_stage_checkpoints
		(job_id,stage,input_fingerprint,artifact_ref,artifact_digest,state,lease_epoch,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(job_id,stage) DO UPDATE SET
		 input_fingerprint=excluded.input_fingerprint,artifact_ref=excluded.artifact_ref,
		 artifact_digest=excluded.artifact_digest,state=excluded.state,
		 lease_epoch=excluded.lease_epoch,updated_at=excluded.updated_at`,
		lease.JobID, checkpoint.Stage, checkpoint.InputFingerprint, checkpoint.ArtifactRef,
		checkpoint.ArtifactDigest, checkpoint.State, lease.Epoch, nowMillis, nowMillis); err != nil {
		return fmt.Errorf("knowledge: save stage checkpoint: %w", err)
	}
	return tx.Commit()
}

func (r *SQLiteSemanticIndexRepository) GetStageCheckpoint(
	ctx context.Context,
	ownerID, jobID string,
	stage JobStage,
) (StageCheckpoint, error) {
	var checkpoint StageCheckpoint
	var state string
	var createdAt, updatedAt int64
	err := r.db.QueryRowContext(ctx, `SELECT cp.job_id,cp.stage,cp.input_fingerprint,cp.artifact_ref,
		cp.artifact_digest,cp.state,cp.lease_epoch,cp.created_at,cp.updated_at
		FROM kb_job_stage_checkpoints cp JOIN kb_knowledge_jobs j ON j.job_id=cp.job_id
		WHERE j.owner_id=? AND cp.job_id=? AND cp.stage=?`, ownerID, jobID, stage).Scan(
		&checkpoint.JobID, &checkpoint.Stage, &checkpoint.InputFingerprint, &checkpoint.ArtifactRef,
		&checkpoint.ArtifactDigest, &state, &checkpoint.LeaseEpoch, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return StageCheckpoint{}, ErrSemanticIndexNotFound
	}
	if err != nil {
		return StageCheckpoint{}, err
	}
	checkpoint.State = StageCheckpointState(state)
	checkpoint.CreatedAt = time.UnixMilli(createdAt).UTC()
	checkpoint.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return checkpoint, nil
}

func validateLiveLease(job KnowledgeJob, lease JobLease, now time.Time) error {
	if job.JobID != lease.JobID || job.OwnerID != lease.OwnerID || job.CorpusUID != lease.CorpusUID ||
		job.State != KnowledgeJobRunning || job.CancelRequested || job.LeaseOwner != lease.WorkerID ||
		job.LeaseEpoch != lease.Epoch || job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.After(now.UTC()) {
		return ErrJobFenced
	}
	return nil
}

func loadLiveJob(
	ctx context.Context,
	q semanticDBQueryer,
	lease JobLease,
	now time.Time,
) (KnowledgeJob, error) {
	job, err := scanSemanticJob(q.QueryRowContext(ctx, semanticJobSelect+`
		WHERE job_id=? AND owner_id=? AND corpus_uid=?`, lease.JobID, lease.OwnerID, lease.CorpusUID))
	if err != nil {
		return KnowledgeJob{}, ErrJobFenced
	}
	if err := validateLiveLease(job, lease, now); err != nil {
		return KnowledgeJob{}, err
	}
	return job, nil
}

func loadExecutionPlanVia(
	ctx context.Context,
	q semanticDBQueryer,
	job KnowledgeJob,
) (JobExecutionPlan, error) {
	var plan JobExecutionPlan
	var previousActive sql.NullString
	var location, availability string
	err := q.QueryRowContext(ctx, `SELECT c.corpus_uid,c.corpus_alias,r.revision_id,r.policy_version,
		r.base_content_version,c.content_version,r.previous_active_revision_id,
		s.profile_snapshot_id,s.resolved_profile_id,s.provider_id,s.provider_name,
		s.provider_location,s.model_name,s.dimension,s.normalization,s.chunk_config_hash,
		s.profile_config_hash,s.availability
		FROM kb_knowledge_jobs j
		JOIN kb_semantic_corpora c ON c.corpus_uid=j.corpus_uid
		JOIN kb_index_revisions r ON r.corpus_uid=j.corpus_uid AND r.revision_id=j.target_revision_id
		JOIN kb_embedding_profile_snapshots s ON s.profile_snapshot_id=r.profile_snapshot_id
		WHERE j.job_id=? AND j.owner_id=? AND j.corpus_uid=? AND r.revision_id=?`,
		job.JobID, job.OwnerID, job.CorpusUID, job.TargetRevisionID).Scan(
		&plan.CorpusUID, &plan.CorpusAlias, &plan.RevisionID, &plan.PolicyVersion,
		&plan.BaseContentVersion, &plan.ContentVersion, &previousActive,
		&plan.Snapshot.SnapshotID, &plan.Snapshot.Profile.ProfileID,
		&plan.Snapshot.Profile.ProviderID, &plan.Snapshot.Profile.ProviderName,
		&location, &plan.Snapshot.Profile.ModelName, &plan.Snapshot.Profile.Dimension,
		&plan.Snapshot.Normalization, &plan.Snapshot.ChunkConfigHash,
		&plan.Snapshot.ProfileConfigHash, &availability,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return JobExecutionPlan{}, ErrSemanticIndexNotFound
	}
	if err != nil {
		return JobExecutionPlan{}, fmt.Errorf("knowledge: load semantic job execution plan: %w", err)
	}
	plan.PreviousActiveRevisionID = stringPointer(previousActive.String)
	plan.Snapshot.Profile.Location = ProviderLocation(location)
	plan.Snapshot.Profile.Availability = ProfileAvailability(availability)
	plan.Snapshot.Profile.Capability = "embedding"
	return plan, nil
}

func (r *SQLiteSemanticIndexRepository) LoadJobExecutionPlan(
	ctx context.Context,
	lease JobLease,
	now time.Time,
) (JobExecutionPlan, error) {
	job, err := loadLiveJob(ctx, r.db, lease, now)
	if err != nil {
		return JobExecutionPlan{}, err
	}
	return loadExecutionPlanVia(ctx, r.db, job)
}

func (r *SQLiteSemanticIndexRepository) GetActiveRevisionPlan(
	ctx context.Context,
	ownerID, corpusID string,
) (ActiveRevisionPlan, error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return ActiveRevisionPlan{}, err
	}
	var plan ActiveRevisionPlan
	var location, availability string
	err := r.db.QueryRowContext(ctx, `SELECT c.corpus_uid,r.revision_id,
		s.profile_snapshot_id,s.resolved_profile_id,s.provider_id,s.provider_name,
		s.provider_location,s.model_name,s.dimension,s.normalization,s.chunk_config_hash,
		s.profile_config_hash,s.availability
		FROM kb_semantic_corpora c
		JOIN kb_index_revisions r ON r.corpus_uid=c.corpus_uid AND r.revision_id=c.active_revision_id
		JOIN kb_embedding_profile_snapshots s ON s.profile_snapshot_id=r.profile_snapshot_id
		WHERE c.owner_id=? AND c.corpus_alias=? AND r.publish_state='active'`, ownerID, corpusID).Scan(
		&plan.CorpusUID, &plan.RevisionID, &plan.Snapshot.SnapshotID,
		&plan.Snapshot.Profile.ProfileID, &plan.Snapshot.Profile.ProviderID,
		&plan.Snapshot.Profile.ProviderName, &location, &plan.Snapshot.Profile.ModelName,
		&plan.Snapshot.Profile.Dimension, &plan.Snapshot.Normalization,
		&plan.Snapshot.ChunkConfigHash, &plan.Snapshot.ProfileConfigHash, &availability,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ActiveRevisionPlan{}, ErrSemanticIndexNotFound
	}
	if err != nil {
		return ActiveRevisionPlan{}, err
	}
	plan.Snapshot.Profile.Location = ProviderLocation(location)
	plan.Snapshot.Profile.Availability = ProfileAvailability(availability)
	plan.Snapshot.Profile.Capability = "embedding"
	return plan, nil
}

func (r *SQLiteSemanticIndexRepository) ListRevisionChunkInputs(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	after *RevisionChunkCursor,
	limit int,
) ([]RevisionChunkInput, error) {
	if limit <= 0 || limit > 1000 {
		return nil, fmt.Errorf("knowledge: chunk input limit must be between 1 and 1000")
	}
	job, err := loadLiveJob(ctx, r.db, lease, now)
	if err != nil {
		return nil, err
	}
	if job.TargetRevisionID == "" {
		return nil, ErrJobFenced
	}
	documentID, chunkIndex, chunkID := "", -1, ""
	if after != nil {
		documentID, chunkIndex, chunkID = after.DocumentID, after.ChunkIndex, after.ChunkID
	}
	rows, err := r.db.QueryContext(ctx, `SELECT b.document_id,b.content_generation,c.id,c.chunk_index,c.content
		FROM kb_revision_documents rd
		JOIN kb_semantic_document_bindings b
		  ON b.corpus_uid=rd.corpus_uid AND b.document_id=rd.document_id
		 AND b.content_generation=rd.content_generation
		JOIN kb_chunks c ON c.doc_id=b.document_id
		WHERE rd.revision_id=? AND rd.corpus_uid=? AND b.lifecycle_state='active'
		  AND (? <> 'embed_document' OR
		       (rd.document_id=? AND rd.content_generation=?))
		  AND NOT EXISTS (
		    SELECT 1 FROM kb_revision_vectors v
		    WHERE v.revision_id=rd.revision_id AND v.corpus_uid=rd.corpus_uid
		      AND v.document_id=b.document_id AND v.content_generation=b.content_generation
		      AND v.chunk_id=c.id
		  )
		  AND (b.document_id>? OR
		       (b.document_id=? AND c.chunk_index>?) OR
		       (b.document_id=? AND c.chunk_index=? AND c.id>?))
		ORDER BY b.document_id,c.chunk_index,c.id LIMIT ?`, job.TargetRevisionID, job.CorpusUID,
		job.Kind, job.DocumentID, job.DocumentGeneration,
		documentID, documentID, chunkIndex, documentID, chunkIndex, chunkID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	inputs := make([]RevisionChunkInput, 0, limit)
	for rows.Next() {
		var input RevisionChunkInput
		if err := rows.Scan(&input.DocumentID, &input.ContentGeneration, &input.ChunkID,
			&input.ChunkIndex, &input.Content); err != nil {
			return nil, err
		}
		hash := sha256.Sum256([]byte(input.Content))
		input.ContentHash = hex.EncodeToString(hash[:])
		input.Cursor = RevisionChunkCursor{
			DocumentID: input.DocumentID, ChunkIndex: input.ChunkIndex, ChunkID: input.ChunkID,
		}
		inputs = append(inputs, input)
	}
	return inputs, rows.Err()
}

func (r *SQLiteSemanticIndexRepository) GetRevisionBuildSummary(
	ctx context.Context,
	lease JobLease,
	now time.Time,
) (RevisionBuildSummary, error) {
	job, err := loadLiveJob(ctx, r.db, lease, now)
	if err != nil {
		return RevisionBuildSummary{}, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT b.document_id,b.content_generation,c.id,c.chunk_index,c.content
		FROM kb_revision_documents rd
		JOIN kb_semantic_document_bindings b
		  ON b.corpus_uid=rd.corpus_uid AND b.document_id=rd.document_id
		 AND b.content_generation=rd.content_generation
		JOIN kb_chunks c ON c.doc_id=b.document_id
		WHERE rd.revision_id=? AND rd.corpus_uid=? AND b.lifecycle_state='active'
		  AND (? <> 'embed_document' OR
		       (rd.document_id=? AND rd.content_generation=?))
		ORDER BY b.document_id,c.chunk_index,c.id`, job.TargetRevisionID, job.CorpusUID,
		job.Kind, job.DocumentID, job.DocumentGeneration)
	if err != nil {
		return RevisionBuildSummary{}, err
	}
	digest := sha256.New()
	var expected int64
	for rows.Next() {
		var documentID, chunkID, content string
		var generation int64
		var chunkIndex int
		if err := rows.Scan(&documentID, &generation, &chunkID, &chunkIndex, &content); err != nil {
			rows.Close()
			return RevisionBuildSummary{}, err
		}
		contentHash := sha256.Sum256([]byte(content))
		_, _ = fmt.Fprintf(digest, "%s\x00%d\x00%s\x00%d\x00%x\n",
			documentID, generation, chunkID, chunkIndex, contentHash)
		expected++
	}
	if err := rows.Close(); err != nil {
		return RevisionBuildSummary{}, err
	}
	var summary RevisionBuildSummary
	summary.RevisionID = job.TargetRevisionID
	summary.ExpectedChunks = expected
	summary.ChunkSetDigest = hex.EncodeToString(digest.Sum(nil))
	if job.Kind == KnowledgeJobEmbedDocument {
		err = r.db.QueryRowContext(ctx, `SELECT rd.embedded_chunks,rd.failed_chunks,r.indexed_through_version
			FROM kb_revision_documents rd
			JOIN kb_index_revisions r
			  ON r.corpus_uid=rd.corpus_uid AND r.revision_id=rd.revision_id
			WHERE rd.revision_id=? AND rd.corpus_uid=?
			  AND rd.document_id=? AND rd.content_generation=?`, job.TargetRevisionID,
			job.CorpusUID, job.DocumentID, job.DocumentGeneration).Scan(
			&summary.EmbeddedChunks, &summary.FailedChunks, &summary.IndexedThroughVersion,
		)
	} else {
		err = r.db.QueryRowContext(ctx, `SELECT
			COALESCE((SELECT SUM(rd.embedded_chunks)
			  FROM kb_revision_documents rd
			  JOIN kb_semantic_document_bindings b
			    ON b.corpus_uid=rd.corpus_uid AND b.document_id=rd.document_id
			   AND b.content_generation=rd.content_generation
			  WHERE rd.revision_id=r.revision_id AND rd.corpus_uid=r.corpus_uid
			    AND b.lifecycle_state='active' AND b.text_state='ready'),0),
			COALESCE((SELECT SUM(rd.failed_chunks)
			  FROM kb_revision_documents rd
			  JOIN kb_semantic_document_bindings b
			    ON b.corpus_uid=rd.corpus_uid AND b.document_id=rd.document_id
			   AND b.content_generation=rd.content_generation
			  WHERE rd.revision_id=r.revision_id AND rd.corpus_uid=r.corpus_uid
			    AND b.lifecycle_state='active' AND b.text_state='ready'),0),
			r.indexed_through_version
			FROM kb_index_revisions r WHERE r.revision_id=? AND r.corpus_uid=?`,
			job.TargetRevisionID, job.CorpusUID).Scan(
			&summary.EmbeddedChunks, &summary.FailedChunks, &summary.IndexedThroughVersion,
		)
	}
	if err != nil {
		return RevisionBuildSummary{}, err
	}
	return summary, nil
}

func validateEmbeddingBatchManifest(manifest EmbeddingBatchManifest) error {
	if strings.TrimSpace(manifest.ChunkIDsDigest) == "" || strings.TrimSpace(manifest.PayloadDigest) == "" ||
		strings.TrimSpace(manifest.ClientRequestKey) == "" || len(manifest.Chunks) == 0 {
		return fmt.Errorf("knowledge: incomplete embedding batch manifest")
	}
	seen := make(map[string]struct{}, len(manifest.Chunks))
	for i, chunk := range manifest.Chunks {
		if chunk.Ordinal != i || strings.TrimSpace(chunk.ChunkID) == "" || strings.TrimSpace(chunk.ContentHash) == "" {
			return fmt.Errorf("knowledge: invalid embedding batch chunk at ordinal %d", i)
		}
		if _, exists := seen[chunk.ChunkID]; exists {
			return fmt.Errorf("knowledge: duplicate embedding batch chunk %q", chunk.ChunkID)
		}
		seen[chunk.ChunkID] = struct{}{}
	}
	return nil
}

func (r *SQLiteSemanticIndexRepository) CreateEmbeddingBatchManifest(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	manifest EmbeddingBatchManifest,
) (EmbeddingBatchManifest, error) {
	if err := validateEmbeddingBatchManifest(manifest); err != nil {
		return EmbeddingBatchManifest{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return EmbeddingBatchManifest{}, err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return EmbeddingBatchManifest{}, err
	}
	plan, err := loadExecutionPlanVia(ctx, tx, job)
	if err != nil {
		return EmbeddingBatchManifest{}, err
	}
	for _, chunk := range manifest.Chunks {
		var content string
		err := tx.QueryRowContext(ctx, `SELECT c.content
			FROM kb_revision_documents rd
			JOIN kb_semantic_document_bindings b
			 ON b.corpus_uid=rd.corpus_uid AND b.document_id=rd.document_id
			 AND b.content_generation=rd.content_generation
			JOIN kb_chunks c ON c.doc_id=b.document_id
			WHERE rd.revision_id=? AND rd.corpus_uid=? AND c.id=? AND b.lifecycle_state='active'
			  AND (? <> 'embed_document' OR
			       (b.document_id=? AND b.content_generation=?))`,
			plan.RevisionID, plan.CorpusUID, chunk.ChunkID,
			job.Kind, job.DocumentID, job.DocumentGeneration).Scan(&content)
		if errors.Is(err, sql.ErrNoRows) {
			return EmbeddingBatchManifest{}, ErrInvalidRevisionVector
		}
		if err != nil {
			return EmbeddingBatchManifest{}, err
		}
		hash := sha256.Sum256([]byte(content))
		if hex.EncodeToString(hash[:]) != chunk.ContentHash {
			return EmbeddingBatchManifest{}, ErrInvalidRevisionVector
		}
	}
	// A provider failure may leave a prepared manifest behind. On restart the
	// deterministic batch identity must resume that manifest under the newly
	// claimed lease instead of colliding with its client_request_key.
	var existing EmbeddingBatchManifest
	var existingState string
	err = tx.QueryRowContext(ctx, `SELECT batch_id,job_id,revision_id,profile_config_hash,
		chunk_ids_digest,payload_digest,client_request_key,state,attempts,provider_request_id,lease_epoch
		FROM kb_embedding_batch_manifests
		WHERE job_id=? AND revision_id=? AND chunk_ids_digest=? AND payload_digest=?`,
		job.JobID, plan.RevisionID, manifest.ChunkIDsDigest, manifest.PayloadDigest).Scan(
		&existing.BatchID, &existing.JobID, &existing.RevisionID, &existing.ProfileConfigHash,
		&existing.ChunkIDsDigest, &existing.PayloadDigest, &existing.ClientRequestKey,
		&existingState, &existing.Attempts, &existing.ProviderRequestID, &existing.LeaseEpoch,
	)
	if err == nil {
		if existing.ClientRequestKey != manifest.ClientRequestKey ||
			existing.ProfileConfigHash != plan.Snapshot.ProfileConfigHash {
			return EmbeddingBatchManifest{}, fmt.Errorf("knowledge: embedding batch identity conflict")
		}
		existing.State = EmbeddingBatchState(existingState)
		existing.Chunks = append([]EmbeddingBatchChunk(nil), manifest.Chunks...)
		if existing.State == EmbeddingBatchSucceeded {
			if err := tx.Commit(); err != nil {
				return EmbeddingBatchManifest{}, err
			}
			return existing, nil
		}
		if existing.State != EmbeddingBatchPrepared && existing.State != EmbeddingBatchRetryWait {
			return EmbeddingBatchManifest{}, fmt.Errorf("knowledge: embedding batch %s is not resumable", existing.BatchID)
		}
		nowMillis := now.UTC().UnixMilli()
		res, updateErr := tx.ExecContext(ctx, `UPDATE kb_embedding_batch_manifests
			SET state='prepared',next_attempt_at=NULL,lease_epoch=?,updated_at=?
			WHERE batch_id=? AND state IN ('prepared','retry_wait')`,
			lease.Epoch, nowMillis, existing.BatchID)
		if updateErr != nil {
			return EmbeddingBatchManifest{}, updateErr
		}
		if affected, _ := res.RowsAffected(); affected != 1 {
			return EmbeddingBatchManifest{}, ErrJobFenced
		}
		existing.State = EmbeddingBatchPrepared
		existing.LeaseEpoch = lease.Epoch
		if err := tx.Commit(); err != nil {
			return EmbeddingBatchManifest{}, err
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return EmbeddingBatchManifest{}, err
	}
	if manifest.BatchID == "" {
		manifest.BatchID, err = semanticID("batch")
		if err != nil {
			return EmbeddingBatchManifest{}, err
		}
	}
	manifest.JobID = job.JobID
	manifest.RevisionID = plan.RevisionID
	manifest.ProfileConfigHash = plan.Snapshot.ProfileConfigHash
	manifest.State = EmbeddingBatchPrepared
	manifest.LeaseEpoch = lease.Epoch
	nowMillis := now.UTC().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_embedding_batch_manifests
		(batch_id,job_id,revision_id,profile_config_hash,chunk_ids_digest,payload_digest,
		 client_request_key,state,attempts,next_attempt_at,provider_request_id,lease_epoch,last_error,
		 created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,'prepared',0,NULL,'',?,'',?,?)`, manifest.BatchID, manifest.JobID,
		manifest.RevisionID, manifest.ProfileConfigHash, manifest.ChunkIDsDigest,
		manifest.PayloadDigest, manifest.ClientRequestKey, manifest.LeaseEpoch, nowMillis, nowMillis); err != nil {
		return EmbeddingBatchManifest{}, fmt.Errorf("knowledge: create embedding batch manifest: %w", err)
	}
	for _, chunk := range manifest.Chunks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_embedding_batch_chunks
			(batch_id,ordinal,chunk_id,content_hash) VALUES(?,?,?,?)`,
			manifest.BatchID, chunk.Ordinal, chunk.ChunkID, chunk.ContentHash); err != nil {
			return EmbeddingBatchManifest{}, fmt.Errorf("knowledge: persist embedding batch chunk: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return EmbeddingBatchManifest{}, err
	}
	return manifest, nil
}

// BeginEmbeddingBatch durably records that the provider request is about to
// leave the process. From this point a timeout or transport break cannot be
// treated as proof that the provider did not execute the request.
func (r *SQLiteSemanticIndexRepository) BeginEmbeddingBatch(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	batchID string,
) error {
	if strings.TrimSpace(batchID) == "" {
		return ErrJobFenced
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return err
	}
	nowMillis := now.UTC().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE kb_embedding_batch_manifests
		SET state='in_flight',attempts=attempts+1,last_error='',updated_at=?
		WHERE batch_id=? AND job_id=? AND state='prepared' AND lease_epoch=?`,
		nowMillis, batchID, job.JobID, lease.Epoch)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return ErrJobFenced
	}
	return tx.Commit()
}

// MarkEmbeddingBatchOutcomeUnknown is intentionally one-way in v0.5.0.
// Automatic workers cannot move this batch back to prepared/retry_wait: only
// a future provider-specific reconciliation command may prove it safe.
func (r *SQLiteSemanticIndexRepository) MarkEmbeddingBatchOutcomeUnknown(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	batchID, lastError string,
) error {
	if strings.TrimSpace(batchID) == "" || strings.TrimSpace(lastError) == "" {
		return ErrJobFenced
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return err
	}
	nowMillis := now.UTC().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE kb_embedding_batch_manifests
		SET state='outcome_unknown',next_attempt_at=NULL,last_error=?,updated_at=?
		WHERE batch_id=? AND job_id=? AND state='in_flight' AND lease_epoch=?`,
		lastError, nowMillis, batchID, job.JobID, lease.Epoch)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return ErrJobFenced
	}
	return tx.Commit()
}

type documentGCPlan struct {
	DocumentID         string   `json:"document_id"`
	ManagedObjectPaths []string `json:"managed_object_paths"`
}

// GarbageCollectDocument physically removes one tombstoned document and all
// of its rebuildable runtime state. Relational cleanup and its managed-object
// paths are committed first while the GC job remains live. Physical deletion
// then happens outside the SQLite transaction; only after it succeeds are the
// zero-reference blob rows and GC job finalized. A crash or filesystem error
// therefore reuses the prepared checkpoint instead of losing cleanup work.
func (r *SQLiteSemanticIndexRepository) GarbageCollectDocument(
	ctx context.Context,
	lease JobLease,
	now time.Time,
) error {
	plan, err := r.prepareDocumentGC(ctx, lease, now)
	if err != nil {
		return err
	}
	if len(plan.ManagedObjectPaths) != 0 && r.ingestBlobStore == nil {
		return ErrDocumentIngestUnavailable
	}
	for _, path := range plan.ManagedObjectPaths {
		if err := ctx.Err(); err != nil {
			return err
		}
		release := r.ingestBlobStore.acquireObjectPath(path)
		referenced, referenceErr := r.IsIngestBlobPathReferenced(ctx, path)
		if referenceErr == nil && !referenced {
			referenceErr = r.ingestBlobStore.RemoveManagedObject(path)
		}
		release()
		if referenceErr != nil {
			return referenceErr
		}
	}
	return r.finalizeDocumentGC(ctx, lease, now, plan)
}

func (r *SQLiteSemanticIndexRepository) prepareDocumentGC(
	ctx context.Context,
	lease JobLease,
	now time.Time,
) (documentGCPlan, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return documentGCPlan{}, err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return documentGCPlan{}, err
	}
	if job.Kind != KnowledgeJobGC {
		return documentGCPlan{}, ErrJobFenced
	}
	var idempotencyKey string
	if err := tx.QueryRowContext(ctx, `SELECT idempotency_key FROM kb_knowledge_jobs
		WHERE job_id=? AND owner_id=? AND corpus_uid=?`,
		job.JobID, job.OwnerID, job.CorpusUID).Scan(&idempotencyKey); err != nil {
		return documentGCPlan{}, err
	}
	if !strings.HasPrefix(idempotencyKey, documentGCIdempotencyPrefix) {
		return documentGCPlan{}, ErrJobFenced
	}
	documentBytes, err := hex.DecodeString(strings.TrimPrefix(idempotencyKey, documentGCIdempotencyPrefix))
	if err != nil || len(documentBytes) == 0 {
		return documentGCPlan{}, ErrJobFenced
	}
	documentID := string(documentBytes)
	fingerprint := hashStrings(job.OwnerID, job.CorpusUID, documentID)
	var checkpointFingerprint, artifactRef, artifactDigest, checkpointState string
	err = tx.QueryRowContext(ctx, `SELECT input_fingerprint,artifact_ref,artifact_digest,state
		FROM kb_job_stage_checkpoints WHERE job_id=? AND stage='gc'`, job.JobID).
		Scan(&checkpointFingerprint, &artifactRef, &artifactDigest, &checkpointState)
	if err == nil {
		plan, decodeErr := decodeDocumentGCPlan(
			documentID, fingerprint, checkpointFingerprint, artifactRef, artifactDigest, checkpointState,
		)
		if decodeErr != nil {
			return documentGCPlan{}, decodeErr
		}
		if err := tx.Commit(); err != nil {
			return documentGCPlan{}, err
		}
		return plan, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return documentGCPlan{}, err
	}
	var tombstoned int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM kb_documents d
		JOIN kb_semantic_document_bindings b ON b.document_id=d.id
		WHERE d.id=? AND d.corpus_uid=? AND d.deleted=1
		  AND b.owner_id=? AND b.corpus_uid=? AND b.lifecycle_state='tombstoned'
	)`, documentID, job.CorpusUID, job.OwnerID, job.CorpusUID).Scan(&tombstoned); err != nil {
		return documentGCPlan{}, err
	}
	if tombstoned == 0 {
		return documentGCPlan{}, ErrJobFenced
	}
	paths, err := loadDocumentGCManagedPaths(ctx, tx, job.OwnerID, job.CorpusUID, documentID)
	if err != nil {
		return documentGCPlan{}, err
	}
	if len(paths) != 0 {
		if r.ingestBlobStore == nil {
			return documentGCPlan{}, ErrDocumentIngestUnavailable
		}
		for _, path := range paths {
			if !isManagedIngestObjectPath(r.ingestBlobStore.root, path) {
				return documentGCPlan{}, fmt.Errorf("knowledge: refusing to collect unmanaged ingest object")
			}
		}
	}
	plan := documentGCPlan{DocumentID: documentID, ManagedObjectPaths: paths}
	encodedPlan, err := json.Marshal(plan)
	if err != nil {
		return documentGCPlan{}, err
	}
	artifactRef = string(encodedPlan)
	artifactDigest = hashStrings(artifactRef)
	nowMillis := now.UTC().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_job_stage_checkpoints
		(job_id,stage,input_fingerprint,artifact_ref,artifact_digest,state,lease_epoch,created_at,updated_at)
		VALUES(?,'gc',?,?,?,'prepared',?,?,?)`, job.JobID, fingerprint, artifactRef,
		artifactDigest, lease.Epoch, nowMillis, nowMillis); err != nil {
		return documentGCPlan{}, fmt.Errorf("knowledge: prepare document GC checkpoint: %w", err)
	}

	// A staged rebuild batch may mix this document with others and its root job
	// is not document-scoped. Drop any such manifest before deleting chunk IDs;
	// current-generation inputs will deterministically create a new manifest.
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_embedding_batch_manifests
		WHERE batch_id IN (
		  SELECT DISTINCT bc.batch_id FROM kb_embedding_batch_chunks bc
		  JOIN kb_chunks c ON c.id=bc.chunk_id WHERE c.doc_id=?
		)`, documentID); err != nil {
		return documentGCPlan{}, err
	}
	// Deleting document-scoped jobs cascades their stage/page checkpoints and
	// remaining batch manifests. The active GC row is deliberately unscoped.
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_knowledge_jobs
		WHERE corpus_uid=? AND document_id=?`, job.CorpusUID, documentID); err != nil {
		return documentGCPlan{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_revision_documents
		WHERE corpus_uid=? AND document_id=?`, job.CorpusUID, documentID); err != nil {
		return documentGCPlan{}, err
	}
	var activeRevision sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT active_revision_id FROM kb_semantic_corpora
		WHERE owner_id=? AND corpus_uid=?`, job.OwnerID, job.CorpusUID).Scan(&activeRevision); err != nil {
		return documentGCPlan{}, err
	}
	if activeRevision.Valid {
		if err := refreshActiveRevisionAggregatesTx(
			ctx, tx, job.CorpusUID, activeRevision.String, nowMillis,
		); err != nil {
			return documentGCPlan{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_ingest_document_sources
		WHERE owner_id=? AND corpus_uid=? AND document_id=?`,
		job.OwnerID, job.CorpusUID, documentID); err != nil {
		return documentGCPlan{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_semantic_document_bindings
		WHERE owner_id=? AND corpus_uid=? AND document_id=? AND lifecycle_state='tombstoned'`,
		job.OwnerID, job.CorpusUID, documentID); err != nil {
		return documentGCPlan{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_chunks_fts
		WHERE chunk_id IN (SELECT id FROM kb_chunks WHERE doc_id=?)`, documentID); err != nil {
		return documentGCPlan{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_chunks WHERE doc_id=?`, documentID); err != nil {
		return documentGCPlan{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_semantic_document_generations
		WHERE owner_id=? AND corpus_uid=? AND document_id=?`,
		job.OwnerID, job.CorpusUID, documentID); err != nil {
		return documentGCPlan{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_documents
		WHERE id=? AND corpus_uid=? AND deleted=1`, documentID, job.CorpusUID); err != nil {
		return documentGCPlan{}, err
	}
	if err := tx.Commit(); err != nil {
		return documentGCPlan{}, err
	}
	return plan, nil
}

func loadDocumentGCManagedPaths(
	ctx context.Context,
	tx *sql.Tx,
	ownerID, corpusUID, documentID string,
) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT blob.storage_path
		FROM kb_ingest_document_sources source JOIN kb_ingest_blobs blob
		  ON blob.owner_id=source.owner_id AND blob.corpus_uid=source.corpus_uid
		 AND blob.sha256=source.blob_sha256
		WHERE source.owner_id=? AND source.corpus_uid=? AND source.document_id=?
		ORDER BY blob.storage_path`, ownerID, corpusUID, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := []string{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		if strings.TrimSpace(path) == "" {
			return nil, ErrJobFenced
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func decodeDocumentGCPlan(
	documentID, expectedFingerprint, checkpointFingerprint, artifactRef, artifactDigest, state string,
) (documentGCPlan, error) {
	if state != string(StageCheckpointPrepared) || checkpointFingerprint != expectedFingerprint ||
		artifactDigest != hashStrings(artifactRef) {
		return documentGCPlan{}, ErrJobFenced
	}
	var plan documentGCPlan
	if err := json.Unmarshal([]byte(artifactRef), &plan); err != nil || plan.DocumentID != documentID {
		return documentGCPlan{}, ErrJobFenced
	}
	if expectedFingerprint == "" {
		return documentGCPlan{}, ErrJobFenced
	}
	seen := make(map[string]struct{}, len(plan.ManagedObjectPaths))
	for _, path := range plan.ManagedObjectPaths {
		if strings.TrimSpace(path) == "" {
			return documentGCPlan{}, ErrJobFenced
		}
		if _, duplicate := seen[path]; duplicate {
			return documentGCPlan{}, ErrJobFenced
		}
		seen[path] = struct{}{}
	}
	return plan, nil
}

func (r *SQLiteSemanticIndexRepository) finalizeDocumentGC(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	plan documentGCPlan,
) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return err
	}
	if job.Kind != KnowledgeJobGC {
		return ErrJobFenced
	}
	for _, path := range plan.ManagedObjectPaths {
		if _, err := tx.ExecContext(ctx, `DELETE FROM kb_ingest_blobs AS blob
			WHERE blob.owner_id=? AND blob.corpus_uid=? AND blob.storage_path=?
			  AND NOT EXISTS (
			    SELECT 1 FROM kb_ingest_document_sources source
			    WHERE source.owner_id=blob.owner_id AND source.corpus_uid=blob.corpus_uid
			      AND source.blob_sha256=blob.sha256
			  )`, job.OwnerID, job.CorpusUID, path); err != nil {
			return err
		}
	}
	nowMillis := now.UTC().UnixMilli()
	res, err := tx.ExecContext(ctx, `DELETE FROM kb_knowledge_jobs
		WHERE job_id=? AND owner_id=? AND corpus_uid=? AND kind='gc'
		  AND state='running' AND cancel_requested=0 AND lease_owner=? AND lease_epoch=?
		  AND lease_expires_at>?`, job.JobID, job.OwnerID, job.CorpusUID,
		lease.WorkerID, lease.Epoch, nowMillis)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return ErrJobFenced
	}
	return tx.Commit()
}

type revisionVectorTarget struct {
	documentID        string
	contentGeneration int64
	chunkID           string
	chunkIndex        int
	contentHash       string
}

func encodeRevisionVector(values []float32) []byte {
	encoded := make([]byte, len(values)*4)
	for i, value := range values {
		binary.LittleEndian.PutUint32(encoded[i*4:], math.Float32bits(value))
	}
	return encoded
}

func validateRevisionVector(input RevisionVector, target revisionVectorTarget, dimension int) error {
	if input.DocumentID != target.documentID || input.ContentGeneration != target.contentGeneration ||
		input.ChunkID != target.chunkID || input.ChunkIndex != target.chunkIndex ||
		input.ContentHash != target.contentHash || len(input.Values) != dimension {
		return ErrInvalidRevisionVector
	}
	for _, value := range input.Values {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return ErrInvalidRevisionVector
		}
	}
	return nil
}

func (r *SQLiteSemanticIndexRepository) CommitEmbeddingBatch(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	commit EmbeddingBatchCommit,
) error {
	if strings.TrimSpace(commit.BatchID) == "" || commit.ChunksDone < 0 || commit.ChunksTotal < 0 ||
		commit.ChunksDone > commit.ChunksTotal || len(commit.Vectors) == 0 {
		return ErrInvalidRevisionVector
	}
	if commit.Checkpoint != nil {
		if err := validateStageCheckpoint(*commit.Checkpoint); err != nil {
			return err
		}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return err
	}
	plan, err := loadExecutionPlanVia(ctx, tx, job)
	if err != nil {
		return err
	}
	var manifestState, revisionID, profileHash string
	var manifestEpoch int64
	err = tx.QueryRowContext(ctx, `SELECT state,revision_id,profile_config_hash,lease_epoch
		FROM kb_embedding_batch_manifests WHERE batch_id=? AND job_id=?`,
		commit.BatchID, job.JobID).Scan(&manifestState, &revisionID, &profileHash, &manifestEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidRevisionVector
	}
	if err != nil {
		return err
	}
	if manifestState == string(EmbeddingBatchSucceeded) {
		return tx.Commit()
	}
	if manifestState != string(EmbeddingBatchInFlight) || revisionID != plan.RevisionID ||
		profileHash != plan.Snapshot.ProfileConfigHash || manifestEpoch != lease.Epoch {
		return ErrJobFenced
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.document_id,b.content_generation,c.id,c.chunk_index,bc.content_hash,c.content
		FROM kb_embedding_batch_chunks bc
		JOIN kb_embedding_batch_manifests bm ON bm.batch_id=bc.batch_id
		JOIN kb_chunks c ON c.id=bc.chunk_id
		JOIN kb_semantic_document_bindings b ON b.document_id=c.doc_id AND b.corpus_uid=?
		JOIN kb_revision_documents rd
		 ON rd.revision_id=bm.revision_id AND rd.corpus_uid=b.corpus_uid
		 AND rd.document_id=b.document_id AND rd.content_generation=b.content_generation
		WHERE bc.batch_id=? AND b.lifecycle_state='active'
		  AND (? <> 'embed_document' OR
		       (b.document_id=? AND b.content_generation=?))
		ORDER BY bc.ordinal`, job.CorpusUID, commit.BatchID,
		job.Kind, job.DocumentID, job.DocumentGeneration)
	if err != nil {
		return err
	}
	targets := make(map[string]revisionVectorTarget, len(commit.Vectors))
	for rows.Next() {
		var target revisionVectorTarget
		var content string
		if err := rows.Scan(&target.documentID, &target.contentGeneration, &target.chunkID,
			&target.chunkIndex, &target.contentHash, &content); err != nil {
			rows.Close()
			return err
		}
		hash := sha256.Sum256([]byte(content))
		if hex.EncodeToString(hash[:]) != target.contentHash {
			rows.Close()
			return ErrInvalidRevisionVector
		}
		targets[target.chunkID] = target
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(targets) != len(commit.Vectors) {
		return ErrInvalidRevisionVector
	}
	for _, vector := range commit.Vectors {
		target, ok := targets[vector.ChunkID]
		if !ok || validateRevisionVector(vector, target, plan.Snapshot.Profile.Dimension) != nil {
			return ErrInvalidRevisionVector
		}
	}

	nowMillis := now.UTC().UnixMilli()
	documentCounts := make(map[string]int64)
	for _, vector := range commit.Vectors {
		_, err := tx.ExecContext(ctx, `INSERT INTO kb_revision_vectors
			(revision_id,corpus_uid,document_id,content_generation,chunk_id,chunk_index,
			 chunk_content_hash,profile_snapshot_id,profile_config_hash,provider_id,
			 provider_location,model_name,dimension,embedding,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, plan.RevisionID, plan.CorpusUID,
			vector.DocumentID, vector.ContentGeneration, vector.ChunkID, vector.ChunkIndex,
			vector.ContentHash, plan.Snapshot.SnapshotID, plan.Snapshot.ProfileConfigHash,
			plan.Snapshot.Profile.ProviderID, plan.Snapshot.Profile.Location,
			plan.Snapshot.Profile.ModelName, plan.Snapshot.Profile.Dimension,
			encodeRevisionVector(vector.Values), nowMillis)
		if err != nil {
			return fmt.Errorf("knowledge: persist revision vector: %w", err)
		}
		documentCounts[fmt.Sprintf("%s\x00%d", vector.DocumentID, vector.ContentGeneration)]++
	}
	for key, count := range documentCounts {
		parts := strings.SplitN(key, "\x00", 2)
		var generation int64
		if _, err := fmt.Sscan(parts[1], &generation); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE kb_revision_documents
			SET vector_state='building',embedded_chunks=embedded_chunks+?,updated_at=?
			WHERE revision_id=? AND corpus_uid=? AND document_id=? AND content_generation=?
			  AND expected_chunks IS NOT NULL AND embedded_chunks+?<=expected_chunks`, count, nowMillis,
			plan.RevisionID, plan.CorpusUID, parts[0], generation, count)
		if err != nil {
			return err
		}
		if affected, _ := res.RowsAffected(); affected != 1 {
			return ErrInvalidRevisionVector
		}
	}
	// Rebuild rows live under a staged revision and can be marked ready as each
	// batch lands because readers cannot select that revision before publish.
	// Incremental rows target the already-active revision, so their final batch
	// must remain invisible until CompleteActiveRevisionJob atomically publishes
	// the row and succeeds the exact fenced job lease.
	if _, err := tx.ExecContext(ctx, `UPDATE kb_revision_documents
		SET vector_state='ready',visible_at=?,updated_at=?
		WHERE revision_id=? AND corpus_uid=? AND expected_chunks IS NOT NULL
		  AND embedded_chunks=expected_chunks AND failed_chunks=0
		  AND ?<>'embed_document'`,
		nowMillis, nowMillis, plan.RevisionID, plan.CorpusUID, job.Kind); err != nil {
		return err
	}
	revisionUpdate, err := tx.ExecContext(ctx, `UPDATE kb_index_revisions
		SET embedded_chunks=embedded_chunks+?,updated_at=? WHERE revision_id=? AND corpus_uid=?
		 AND (expected_chunks IS NULL OR embedded_chunks+?<=expected_chunks)`,
		len(commit.Vectors), nowMillis, plan.RevisionID, plan.CorpusUID, len(commit.Vectors))
	if err != nil {
		return err
	}
	if affected, _ := revisionUpdate.RowsAffected(); affected != 1 {
		return ErrInvalidRevisionVector
	}
	if commit.Checkpoint != nil {
		cp := commit.Checkpoint
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_job_stage_checkpoints
			(job_id,stage,input_fingerprint,artifact_ref,artifact_digest,state,lease_epoch,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(job_id,stage) DO UPDATE SET
			 input_fingerprint=excluded.input_fingerprint,artifact_ref=excluded.artifact_ref,
			 artifact_digest=excluded.artifact_digest,state=excluded.state,
			 lease_epoch=excluded.lease_epoch,updated_at=excluded.updated_at`, job.JobID,
			cp.Stage, cp.InputFingerprint, cp.ArtifactRef, cp.ArtifactDigest, cp.State,
			lease.Epoch, nowMillis, nowMillis); err != nil {
			return fmt.Errorf("knowledge: commit embedding checkpoint: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs SET stage='embedding',chunks_done=?,chunks_total=?,
		heartbeat_at=?,updated_at=? WHERE job_id=? AND owner_id=? AND corpus_uid=? AND state='running'
		AND cancel_requested=0 AND lease_owner=? AND lease_epoch=? AND lease_expires_at>?`,
		commit.ChunksDone, commit.ChunksTotal, nowMillis, nowMillis, job.JobID, job.OwnerID,
		job.CorpusUID, lease.WorkerID, lease.Epoch, nowMillis)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return ErrJobFenced
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kb_embedding_batch_manifests
		SET state='succeeded',provider_request_id=?,updated_at=?
		WHERE batch_id=? AND job_id=? AND state='in_flight' AND lease_epoch=?`,
		commit.ProviderRequestID, nowMillis, commit.BatchID, job.JobID, lease.Epoch); err != nil {
		return err
	}
	return tx.Commit()
}

// CompleteActiveRevisionJob commits an incremental document build into the
// already-published revision. It deliberately does not run the staged publish
// protocol or touch policy pointers: readers start seeing the document only
// after its revision-document row is ready and this exact live lease succeeds.
func (r *SQLiteSemanticIndexRepository) CompleteActiveRevisionJob(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	expectedContentVersion int64,
) error {
	if expectedContentVersion < 0 {
		return ErrJobFenced
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return err
	}
	if job.Kind != KnowledgeJobEmbedDocument || job.DocumentID == "" || job.TargetRevisionID == "" {
		return ErrJobFenced
	}
	// Join through the current binding as well as the immutable generation
	// fact. A superseded generation can therefore never become query-visible
	// after a slow/stale provider response returns.
	var generation, currentContentVersion int64
	err = tx.QueryRowContext(ctx, `SELECT j.document_generation,c.content_version
		FROM kb_knowledge_jobs j
		JOIN kb_semantic_corpora c
		  ON c.corpus_uid=j.corpus_uid AND c.owner_id=j.owner_id
		 AND c.active_revision_id=j.target_revision_id AND c.content_version>=?
		JOIN kb_semantic_document_bindings b
		  ON b.corpus_uid=j.corpus_uid AND b.document_id=j.document_id
		 AND b.content_generation=j.document_generation AND b.lifecycle_state='active'
		JOIN kb_revision_documents rd
		  ON rd.corpus_uid=j.corpus_uid AND rd.revision_id=j.target_revision_id
		 AND rd.document_id=j.document_id AND rd.content_generation=j.document_generation
		WHERE j.job_id=? AND j.owner_id=? AND j.corpus_uid=?
		  AND j.kind='embed_document' AND rd.vector_state='building'
		  AND rd.visible_at IS NULL AND rd.expected_chunks IS NOT NULL
		  AND rd.embedded_chunks=rd.expected_chunks AND rd.failed_chunks=0`,
		expectedContentVersion, job.JobID, job.OwnerID, job.CorpusUID).Scan(
		&generation, &currentContentVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrJobFenced
	}
	if err != nil {
		return err
	}
	nowMillis := now.UTC().UnixMilli()
	projectionUpdate, err := tx.ExecContext(ctx, `UPDATE kb_revision_documents
		SET vector_state='ready',visible_at=?,last_error='',updated_at=?
		WHERE corpus_uid=? AND revision_id=? AND document_id=? AND content_generation=?
		  AND vector_state='building' AND visible_at IS NULL
		  AND expected_chunks IS NOT NULL AND embedded_chunks=expected_chunks AND failed_chunks=0`,
		nowMillis, nowMillis, job.CorpusUID, job.TargetRevisionID, job.DocumentID, generation)
	if err != nil {
		return err
	}
	if affected, _ := projectionUpdate.RowsAffected(); affected != 1 {
		return ErrJobFenced
	}
	if err := advanceActiveRevisionWatermarkIfCompleteTx(ctx, tx, job.CorpusUID,
		job.TargetRevisionID, currentContentVersion, nowMillis); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs
		SET state='succeeded',stage='embedding',lease_epoch=lease_epoch+1,
		 lease_owner='',lease_expires_at=NULL,heartbeat_at=NULL,updated_at=?,finished_at=?
		WHERE job_id=? AND owner_id=? AND corpus_uid=? AND kind='embed_document'
		 AND document_id=? AND document_generation=? AND target_revision_id=?
		 AND state='running' AND cancel_requested=0 AND lease_owner=? AND lease_epoch=?
		 AND lease_expires_at>?`, nowMillis, nowMillis, job.JobID, job.OwnerID, job.CorpusUID,
		job.DocumentID, generation, job.TargetRevisionID, lease.WorkerID, lease.Epoch, nowMillis)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return ErrJobFenced
	}
	return tx.Commit()
}

func (r *SQLiteSemanticIndexRepository) PrepareRevisionForPublish(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	preparation RevisionPublishPreparation,
) error {
	if preparation.IndexedThroughVersion < 0 || preparation.ExpectedChunks < 0 || strings.TrimSpace(preparation.ChunkSetDigest) == "" {
		return fmt.Errorf("knowledge: invalid revision publish preparation")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs SET stage='publishing',heartbeat_at=?,updated_at=?
		WHERE job_id=? AND owner_id=? AND corpus_uid=? AND state='running' AND cancel_requested=0
		 AND lease_owner=? AND lease_epoch=? AND lease_expires_at>?`, now.UTC().UnixMilli(), now.UTC().UnixMilli(),
		lease.JobID, lease.OwnerID, lease.CorpusUID, lease.WorkerID, lease.Epoch, now.UTC().UnixMilli())
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrJobFenced
	}
	res, err = tx.ExecContext(ctx, `UPDATE kb_index_revisions SET indexed_through_version=?,
		chunk_set_digest=?,expected_chunks=? WHERE revision_id=(SELECT target_revision_id FROM kb_knowledge_jobs WHERE job_id=?)
		AND corpus_uid=? AND publish_state='staged' AND lease_epoch=?`, preparation.IndexedThroughVersion,
		preparation.ChunkSetDigest, preparation.ExpectedChunks, lease.JobID, lease.CorpusUID, lease.Epoch)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrJobFenced
	}
	return tx.Commit()
}

func (r *SQLiteSemanticIndexRepository) PublishRevisionCAS(
	ctx context.Context,
	command PublishRevisionCommand,
) error {
	if command.OwnerID != command.Lease.OwnerID || command.RevisionID == "" || command.Now.IsZero() {
		return ErrPublishConflict
	}
	publishNow := command.Now.UTC()
	return sqliteutil.RetryOnBusy(ctx, func() error {
		return r.publishRevisionCASOnce(ctx, command, publishNow)
	})
}

// publishRevisionCASOnce executes one complete publish transaction so a BUSY
// retry reloads policy, job, and revision fences from the committed winner.
func (r *SQLiteSemanticIndexRepository) publishRevisionCASOnce(
	ctx context.Context,
	command PublishRevisionCommand,
	publishNow time.Time,
) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := loadSemanticPolicyState(ctx, tx, command.OwnerID, command.CorpusID)
	if err != nil {
		if errors.Is(err, ErrSemanticIndexNotFound) {
			return ErrPublishConflict
		}
		return err
	}
	expectedActive := ""
	if command.ExpectedActiveRevisionID != nil {
		expectedActive = *command.ExpectedActiveRevisionID
	}
	if state.corpusUID != command.Lease.CorpusUID || state.version != command.ExpectedPolicyVersion ||
		state.contentVersion != command.ExpectedContentVersion || state.activeRevision != expectedActive ||
		state.desiredRevision != command.RevisionID {
		return ErrPublishConflict
	}
	job, err := scanSemanticJob(tx.QueryRowContext(ctx, semanticJobSelect+` WHERE job_id=? AND owner_id=? AND corpus_uid=?`,
		command.Lease.JobID, command.OwnerID, state.corpusUID))
	if err != nil || job.State != KnowledgeJobRunning || job.CancelRequested ||
		job.LeaseOwner != command.Lease.WorkerID || job.LeaseEpoch != command.Lease.Epoch ||
		job.TargetRevisionID != command.RevisionID {
		return ErrPublishConflict
	}
	if job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.After(publishNow) {
		return ErrJobFenced
	}
	var publishState, previousActive string
	var indexedThrough, embedded, failed, leaseEpoch int64
	var expected sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT publish_state,COALESCE(previous_active_revision_id,''),
		indexed_through_version,expected_chunks,embedded_chunks,failed_chunks,lease_epoch
		FROM kb_index_revisions WHERE corpus_uid=? AND revision_id=?`, state.corpusUID, command.RevisionID).Scan(
		&publishState, &previousActive, &indexedThrough, &expected, &embedded, &failed, &leaseEpoch,
	)
	if err != nil || publishState != "staged" || previousActive != expectedActive ||
		indexedThrough != command.ExpectedContentVersion || !expected.Valid || expected.Int64 != embedded ||
		failed != 0 || leaseEpoch != command.Lease.Epoch {
		return ErrPublishConflict
	}
	now := publishNow.UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE kb_embedding_policies SET desired_revision_id=NULL,updated_at=?
		WHERE corpus_uid=? AND version=? AND desired_revision_id=?`, now, state.corpusUID,
		command.ExpectedPolicyVersion, command.RevisionID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrPublishConflict
	}
	res, err = tx.ExecContext(ctx, `UPDATE kb_semantic_corpora SET active_revision_id=?,updated_at=?
		WHERE corpus_uid=? AND content_version=? AND
		((active_revision_id IS NULL AND ?='') OR active_revision_id=?)`, command.RevisionID, now,
		state.corpusUID, command.ExpectedContentVersion, expectedActive, expectedActive)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrPublishConflict
	}
	if expectedActive != "" {
		res, err = tx.ExecContext(ctx, `UPDATE kb_index_revisions SET publish_state='superseded'
			WHERE corpus_uid=? AND revision_id=? AND publish_state='active'`, state.corpusUID, expectedActive)
		if err != nil {
			return err
		}
		if rows, _ := res.RowsAffected(); rows != 1 {
			return ErrPublishConflict
		}
	}
	res, err = tx.ExecContext(ctx, `UPDATE kb_index_revisions SET publish_state='active',published_at=?
		WHERE corpus_uid=? AND revision_id=? AND publish_state='staged' AND lease_epoch=?`, now,
		state.corpusUID, command.RevisionID, command.Lease.Epoch)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrPublishConflict
	}
	res, err = tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs SET state='succeeded',stage='publishing',
		lease_epoch=lease_epoch+1,lease_owner='',lease_expires_at=NULL,heartbeat_at=NULL,updated_at=?,finished_at=?
		WHERE job_id=? AND owner_id=? AND corpus_uid=? AND target_revision_id=?
		  AND state='running' AND cancel_requested=0 AND lease_owner=? AND lease_epoch=?
		  AND lease_expires_at>?`, now, now, command.Lease.JobID, command.OwnerID, state.corpusUID,
		command.RevisionID, command.Lease.WorkerID, command.Lease.Epoch, now)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrJobFenced
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}
