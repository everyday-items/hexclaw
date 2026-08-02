package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/internal/sqliteutil"
)

const documentRetryIdempotencyPrefix = "document-retry|"

func (r *SQLiteSemanticIndexRepository) CreateIngestDocument(
	ctx context.Context,
	ownerID, corpusID string,
	input CreateDocumentInput,
	blob IngestBlob,
) (CreateDocumentResult, error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return CreateDocumentResult{}, err
	}
	filename, extension, mediaType, err := validateCreateDocumentInput(input)
	if err != nil {
		return CreateDocumentResult{}, err
	}
	if len(blob.SHA256) != 64 || strings.TrimSpace(blob.StoragePath) == "" || blob.SizeBytes <= 0 ||
		blob.SizeBytes != input.SizeBytes || blob.MediaType != mediaType {
		return CreateDocumentResult{}, fmt.Errorf("%w: invalid persisted blob", ErrInvalidDocumentUpload)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return CreateDocumentResult{}, err
	}
	defer tx.Rollback()
	projectionWasCurrent, err := cjkFTSProjectionCurrentTx(ctx, tx)
	if err != nil {
		return CreateDocumentResult{}, err
	}
	state, err := loadSemanticPolicyState(ctx, tx, ownerID, corpusID)
	if err != nil {
		return CreateDocumentResult{}, err
	}
	if existing, found, findErr := lookupIngestReplayTx(
		ctx, tx, ownerID, state, input, filename, mediaType, blob,
	); findErr != nil {
		return CreateDocumentResult{}, findErr
	} else if found {
		if err := bindUploadOperationTx(
			ctx, tx, ownerID, state.corpusUID, input.UploadOperationID,
			existing.DocumentID, existing.JobID, blob.SHA256, blob.SizeBytes, semanticNowMillis(),
		); err != nil {
			return CreateDocumentResult{}, err
		}
		existing.OperationID = strings.TrimSpace(input.UploadOperationID)
		if err := tx.Commit(); err != nil {
			return CreateDocumentResult{}, err
		}
		return existing, nil
	}

	documentID, previousGeneration, revive, err := findReviveableIngestDocumentTx(
		ctx, tx, ownerID, state.corpusUID, filename,
	)
	if err != nil {
		return CreateDocumentResult{}, err
	}
	if !revive {
		documentID, err = semanticID("doc")
		if err != nil {
			return CreateDocumentResult{}, err
		}
	}
	generation := previousGeneration + 1
	jobID, err := semanticID("job")
	if err != nil {
		return CreateDocumentResult{}, err
	}
	nowMillis := semanticNowMillis()
	now := documentTimeNow()
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_ingest_blobs
		(owner_id,corpus_uid,sha256,storage_path,size_bytes,media_type,created_at)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(owner_id,corpus_uid,sha256) DO NOTHING`,
		ownerID, state.corpusUID, blob.SHA256, blob.StoragePath, blob.SizeBytes,
		blob.MediaType, nowMillis); err != nil {
		return CreateDocumentResult{}, fmt.Errorf("knowledge: persist ingest blob: %w", err)
	}
	var persistedPath, persistedMedia string
	var persistedSize int64
	if err := tx.QueryRowContext(ctx, `SELECT storage_path,size_bytes,media_type
		FROM kb_ingest_blobs WHERE owner_id=? AND corpus_uid=? AND sha256=?`,
		ownerID, state.corpusUID, blob.SHA256).Scan(
		&persistedPath, &persistedSize, &persistedMedia,
	); err != nil {
		return CreateDocumentResult{}, err
	}
	if persistedPath != blob.StoragePath || persistedSize != blob.SizeBytes || persistedMedia != blob.MediaType {
		return CreateDocumentResult{}, fmt.Errorf("%w: content-addressed blob metadata mismatch", ErrIdempotencyConflict)
	}
	if revive {
		if err := retireDocumentGCTx(ctx, tx, ownerID, state.corpusUID, documentID); err != nil {
			return CreateDocumentResult{}, err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM kb_chunks_fts WHERE chunk_id IN (SELECT id FROM kb_chunks WHERE doc_id=?)`,
			documentID); err != nil {
			return CreateDocumentResult{}, fmt.Errorf("knowledge: clear revived ingest FTS: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM kb_chunks_fts_v2 WHERE chunk_id IN (SELECT id FROM kb_chunks WHERE doc_id=?)`,
			documentID); err != nil {
			return CreateDocumentResult{}, fmt.Errorf("knowledge: clear revived ingest CJK FTS v2: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM kb_chunks WHERE doc_id=?`, documentID); err != nil {
			return CreateDocumentResult{}, fmt.Errorf("knowledge: clear revived ingest chunks: %w", err)
		}
		res, err := tx.ExecContext(ctx, `UPDATE kb_documents
			SET title=?,content='',source=?,chunk_count=0,updated_at=?,status='processing',corpus_uid=?,
			    error_message='',source_type='upload',deleted=0
			WHERE id=? AND deleted=1`, filename, "upload:"+filename, now, state.corpusUID, documentID)
		if err != nil {
			return CreateDocumentResult{}, fmt.Errorf("knowledge: revive ingest document: %w", err)
		}
		if rows, _ := res.RowsAffected(); rows != 1 {
			return CreateDocumentResult{}, ErrIdempotencyConflict
		}
		res, err = tx.ExecContext(ctx, `UPDATE kb_semantic_document_bindings
			SET content_generation=?,lifecycle_state='active',text_state='pending',deleted_at=NULL,
			    version=version+1,updated_at=?
			WHERE owner_id=? AND corpus_uid=? AND document_id=? AND content_generation=?
			  AND lifecycle_state='tombstoned'`, generation, nowMillis, ownerID,
			state.corpusUID, documentID, previousGeneration)
		if err != nil {
			return CreateDocumentResult{}, fmt.Errorf("knowledge: revive ingest binding: %w", err)
		}
		if rows, _ := res.RowsAffected(); rows != 1 {
			return CreateDocumentResult{}, ErrIdempotencyConflict
		}
	} else {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_documents
			(id,title,content,source,chunk_count,created_at,updated_at,status,error_message,source_type,deleted,corpus_uid)
			VALUES(?,?, '',?,0,?,?,'processing','','upload',0,?)`, documentID, filename,
			"upload:"+filename, now, now, state.corpusUID); err != nil {
			if isUniqueConstraintErr(err) {
				return CreateDocumentResult{}, ErrIdempotencyConflict
			}
			return CreateDocumentResult{}, fmt.Errorf("knowledge: create ingest document: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_semantic_document_bindings
			(document_id,owner_id,corpus_uid,content_generation,lifecycle_state,text_state,
			 deleted_at,version,created_at,updated_at)
			VALUES(?,?,?,1,'active','pending',NULL,1,?,?)`, documentID, ownerID,
			state.corpusUID, nowMillis, nowMillis); err != nil {
			return CreateDocumentResult{}, fmt.Errorf("knowledge: bind ingest document: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_semantic_document_generations
		(owner_id,corpus_uid,document_id,content_generation,created_at)
		VALUES(?,?,?,?,?)`, ownerID, state.corpusUID, documentID, generation, nowMillis); err != nil {
		return CreateDocumentResult{}, fmt.Errorf("knowledge: persist ingest generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_ingest_document_sources
		(document_id,owner_id,corpus_uid,content_generation,blob_sha256,original_name,
		 extension,media_type,size_bytes,agent_id,learner_id,subject,grade,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, documentID, ownerID, state.corpusUID, generation,
		blob.SHA256, filename, extension, mediaType, blob.SizeBytes,
		strings.TrimSpace(input.AgentID), strings.TrimSpace(input.LearnerID),
		strings.TrimSpace(input.Subject), strings.TrimSpace(input.Grade), nowMillis, nowMillis); err != nil {
		return CreateDocumentResult{}, fmt.Errorf("knowledge: persist ingest source: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_knowledge_jobs
		(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,document_generation,
		 target_revision_id,idempotency_key,state,stage,attempt,cancel_requested,lease_owner,
		 lease_epoch,last_error,created_at,updated_at)
		VALUES(?,NULL,'ingest',?,?,?,?,NULL,?,'queued','extracting',0,0,'',0,'',?,?)`,
		jobID, ownerID, state.corpusUID, documentID, generation, strings.TrimSpace(input.IdempotencyKey),
		nowMillis, nowMillis); err != nil {
		return CreateDocumentResult{}, fmt.Errorf("knowledge: queue ingest job: %w", err)
	}
	if err := persistVisionRouteSnapshotTx(ctx, tx, jobID, input.VisionRoute, nowMillis); err != nil {
		return CreateDocumentResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_job_stage_checkpoints
		(job_id,stage,input_fingerprint,artifact_ref,artifact_digest,state,lease_epoch,created_at,updated_at)
		VALUES(?,'extracting',?,? ,?,'prepared',0,?,?)`, jobID,
		hashStrings(blob.SHA256, extension, mediaType), "blob://"+blob.SHA256,
		blob.SHA256, nowMillis, nowMillis); err != nil {
		return CreateDocumentResult{}, fmt.Errorf("knowledge: prepare ingest checkpoint: %w", err)
	}
	vectorState, err := vectorStateForPolicyTx(ctx, tx, state)
	if err != nil {
		return CreateDocumentResult{}, err
	}
	if err := r.reconcileDocumentIngestLifecycleTx(ctx, tx, KnowledgeJob{
		JobID: jobID, Kind: KnowledgeJobIngest, OwnerID: ownerID,
		CorpusUID: state.corpusUID, DocumentID: documentID,
		DocumentGeneration: generation,
	}, time.UnixMilli(nowMillis)); err != nil {
		return CreateDocumentResult{}, err
	}
	if err := restoreCJKFTSCurrentTx(ctx, tx, projectionWasCurrent); err != nil {
		return CreateDocumentResult{}, fmt.Errorf("knowledge: publish CJK FTS v2 version: %w", err)
	}
	if err := bindUploadOperationTx(
		ctx, tx, ownerID, state.corpusUID, input.UploadOperationID,
		documentID, jobID, blob.SHA256, blob.SizeBytes, nowMillis,
	); err != nil {
		return CreateDocumentResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CreateDocumentResult{}, err
	}
	return CreateDocumentResult{
		OperationID: strings.TrimSpace(input.UploadOperationID),
		DocumentID:  documentID, JobID: jobID, TextIndexState: TextIndexPending,
		VectorIndexState: vectorState,
	}, nil
}

// RetryIngestDocument atomically restores a failed current generation to a
// runnable state and appends a fresh auditable job. The failed job is never
// mutated or reused. Text failures create a new ingest root bound to the
// original source/blob; embedding-only failures create only a new child job.
func (r *SQLiteSemanticIndexRepository) RetryIngestDocument(
	ctx context.Context,
	ownerID, corpusID, documentID, idempotencyKey string,
) (CreateDocumentResult, error) {
	return r.retryIngestDocument(ctx, ownerID, corpusID, documentID, idempotencyKey, nil)
}

func (r *SQLiteSemanticIndexRepository) RetryIngestDocumentWithVisionRoute(
	ctx context.Context,
	ownerID, corpusID, documentID, idempotencyKey string,
	visionRoute *VisionRouteSnapshot,
) (CreateDocumentResult, error) {
	return r.retryIngestDocument(
		ctx, ownerID, corpusID, documentID, idempotencyKey, visionRoute,
	)
}

func (r *SQLiteSemanticIndexRepository) retryIngestDocument(
	ctx context.Context,
	ownerID, corpusID, documentID, idempotencyKey string,
	visionRoute *VisionRouteSnapshot,
) (CreateDocumentResult, error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return CreateDocumentResult{}, err
	}
	documentID = strings.TrimSpace(documentID)
	storageKey, err := documentRetryStorageKey(idempotencyKey)
	if err != nil {
		return CreateDocumentResult{}, err
	}
	if documentID == "" {
		return CreateDocumentResult{}, fmt.Errorf("%w: document_id is required", ErrInvalidDocumentRetry)
	}
	var result CreateDocumentResult
	err = sqliteutil.RetryOnBusy(ctx, func() error {
		var attemptErr error
		result, attemptErr = r.retryIngestDocumentOnce(
			ctx, ownerID, corpusID, documentID, storageKey, visionRoute,
		)
		return attemptErr
	})
	return result, err
}

func (r *SQLiteSemanticIndexRepository) retryIngestDocumentOnce(
	ctx context.Context,
	ownerID, corpusID, documentID, storageKey string,
	visionRoute *VisionRouteSnapshot,
) (CreateDocumentResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return CreateDocumentResult{}, err
	}
	defer tx.Rollback()
	policy, err := loadSemanticPolicyState(ctx, tx, ownerID, corpusID)
	if err != nil {
		return CreateDocumentResult{}, err
	}
	var generation int64
	var lifecycle, textState, documentStatus string
	var deleted int
	err = tx.QueryRowContext(ctx, `SELECT b.content_generation,b.lifecycle_state,b.text_state,d.status,d.deleted
		FROM kb_semantic_document_bindings b
		JOIN kb_documents d ON d.id=b.document_id AND d.corpus_uid=b.corpus_uid
		WHERE b.owner_id=? AND b.corpus_uid=? AND b.document_id=?`,
		ownerID, policy.corpusUID, documentID).Scan(
		&generation, &lifecycle, &textState, &documentStatus, &deleted,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return CreateDocumentResult{}, ErrSemanticIndexNotFound
	}
	if err != nil {
		return CreateDocumentResult{}, err
	}
	if lifecycle != "active" || deleted != 0 || documentStatus == "cancelled" {
		return CreateDocumentResult{}, ErrDocumentRetryRequiresReupload
	}

	// A response-unknown retry replays the already-created job even after that
	// job has completed. The endpoint namespace prevents a caller from
	// accidentally replaying the original upload command with the same header.
	if existing, found, replayErr := lookupDocumentRetryReplayTx(
		ctx, tx, ownerID, policy, documentID, generation, storageKey, TextIndexState(textState),
	); replayErr != nil {
		return CreateDocumentResult{}, replayErr
	} else if found {
		if err := tx.Commit(); err != nil {
			return CreateDocumentResult{}, err
		}
		return existing, nil
	}

	var activeJobs int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE owner_id=? AND corpus_uid=? AND document_id=? AND document_generation=?
		  AND kind IN ('ingest','embed_document') AND cancel_requested=0
		  AND state IN ('queued','running','retry_wait')`, ownerID, policy.corpusUID,
		documentID, generation).Scan(&activeJobs); err != nil {
		return CreateDocumentResult{}, err
	}
	if activeJobs != 0 {
		return CreateDocumentResult{}, ErrIdempotencyConflict
	}

	nowMillis := semanticNowMillis()
	jobID, err := semanticID("job")
	if err != nil {
		return CreateDocumentResult{}, err
	}
	result := CreateDocumentResult{DocumentID: documentID, JobID: jobID}
	switch {
	case TextIndexState(textState) == TextIndexFailed && documentStatus == "failed":
		if err := queueFailedTextRetryTx(ctx, tx, ownerID, policy.corpusUID, documentID,
			generation, storageKey, jobID, nowMillis, visionRoute); err != nil {
			return CreateDocumentResult{}, err
		}
		vectorState, err := vectorStateForPolicyTx(ctx, tx, policy)
		if err != nil {
			return CreateDocumentResult{}, err
		}
		result.TextIndexState = TextIndexPending
		result.VectorIndexState = vectorState
	case TextIndexState(textState) == TextIndexReady && documentStatus == "indexed":
		if err := queueFailedEmbeddingRetryTx(ctx, tx, ownerID, policy.corpusUID, documentID,
			generation, storageKey, jobID, nowMillis); err != nil {
			return CreateDocumentResult{}, err
		}
		result.TextIndexState = TextIndexReady
		result.VectorIndexState = VectorIndexPending
	default:
		return CreateDocumentResult{}, ErrDocumentRetryNotAllowed
	}
	if err := r.reconcileDocumentIngestLifecycleTx(ctx, tx, KnowledgeJob{
		JobID: jobID, Kind: KnowledgeJobIngest, OwnerID: ownerID,
		CorpusUID: policy.corpusUID, DocumentID: documentID,
		DocumentGeneration: generation,
	}, time.UnixMilli(nowMillis)); err != nil {
		return CreateDocumentResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CreateDocumentResult{}, err
	}
	return result, nil
}

func documentRetryStorageKey(idempotencyKey string) (string, error) {
	key := strings.TrimSpace(idempotencyKey)
	if key == "" || len(key) > 256-len(documentRetryIdempotencyPrefix) {
		return "", fmt.Errorf("%w: Idempotency-Key is required", ErrInvalidDocumentRetry)
	}
	return documentRetryIdempotencyPrefix + key, nil
}

func lookupDocumentRetryReplayTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerID string,
	policy semanticPolicyState,
	documentID string,
	generation int64,
	storageKey string,
	textState TextIndexState,
) (CreateDocumentResult, bool, error) {
	job, err := scanSemanticJob(tx.QueryRowContext(ctx, semanticJobSelect+`
		WHERE owner_id=? AND corpus_uid=? AND idempotency_key=?
		  AND kind IN ('ingest','embed_document')
		ORDER BY created_at,job_id LIMIT 1`, ownerID, policy.corpusUID, storageKey))
	if errors.Is(err, ErrSemanticIndexNotFound) {
		return CreateDocumentResult{}, false, nil
	}
	if err != nil {
		return CreateDocumentResult{}, false, err
	}
	if job.DocumentID != documentID || job.DocumentGeneration != generation {
		return CreateDocumentResult{}, false, ErrIdempotencyConflict
	}
	vectorState, err := vectorStateForPolicyTx(ctx, tx, policy)
	if err != nil {
		return CreateDocumentResult{}, false, err
	}
	if job.Kind == KnowledgeJobEmbedDocument {
		var persisted string
		err := tx.QueryRowContext(ctx, `SELECT vector_state FROM kb_revision_documents
			WHERE corpus_uid=? AND revision_id=? AND document_id=? AND content_generation=?`,
			job.CorpusUID, job.TargetRevisionID, documentID, generation).Scan(&persisted)
		if errors.Is(err, sql.ErrNoRows) {
			return CreateDocumentResult{}, false, ErrSemanticIndexNotFound
		}
		if err != nil {
			return CreateDocumentResult{}, false, err
		}
		vectorState = VectorIndexState(persisted)
	}
	return CreateDocumentResult{
		DocumentID: documentID, JobID: job.JobID,
		TextIndexState: textState, VectorIndexState: vectorState,
	}, true, nil
}

func queueFailedTextRetryTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerID, corpusUID, documentID string,
	generation int64,
	storageKey, jobID string,
	nowMillis int64,
	visionRoute *VisionRouteSnapshot,
) error {
	var failedRoots int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
		WHERE owner_id=? AND corpus_uid=? AND document_id=? AND document_generation=?
		  AND kind='ingest' AND state='failed' AND cancel_requested=0`,
		ownerID, corpusUID, documentID, generation).Scan(&failedRoots); err != nil {
		return err
	}
	if failedRoots == 0 {
		return ErrDocumentRetryNotAllowed
	}
	var digest, extension, mediaType string
	err := tx.QueryRowContext(ctx, `SELECT blob_sha256,extension,media_type
		FROM kb_ingest_document_sources
		WHERE owner_id=? AND corpus_uid=? AND document_id=? AND content_generation=?`,
		ownerID, corpusUID, documentID, generation).Scan(&digest, &extension, &mediaType)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDocumentRetryRequiresReupload
	}
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE kb_documents
		SET status='processing',error_message='',updated_at=?
		WHERE id=? AND corpus_uid=? AND deleted=0 AND status='failed'`,
		time.UnixMilli(nowMillis).UTC(), documentID, corpusUID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrJobFenced
	}
	res, err = tx.ExecContext(ctx, `UPDATE kb_semantic_document_bindings
		SET text_state='pending',version=version+1,updated_at=?
		WHERE owner_id=? AND corpus_uid=? AND document_id=? AND content_generation=?
		  AND lifecycle_state='active' AND text_state='failed'`, nowMillis,
		ownerID, corpusUID, documentID, generation)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrJobFenced
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_knowledge_jobs
		(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,document_generation,
		 target_revision_id,idempotency_key,state,stage,attempt,cancel_requested,lease_owner,
		 lease_epoch,last_error,created_at,updated_at)
		VALUES(?,NULL,'ingest',?,?,?,?,NULL,?,'queued','extracting',0,0,'',0,'',?,?)`,
		jobID, ownerID, corpusUID, documentID, generation, storageKey, nowMillis, nowMillis); err != nil {
		if isUniqueConstraintErr(err) {
			return ErrIdempotencyConflict
		}
		return fmt.Errorf("knowledge: queue ingest retry: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_job_stage_checkpoints
		(job_id,stage,input_fingerprint,artifact_ref,artifact_digest,state,lease_epoch,created_at,updated_at)
		VALUES(?,'extracting',?,?,?,'prepared',0,?,?)`, jobID,
		hashStrings(digest, extension, mediaType), "blob://"+digest, digest, nowMillis, nowMillis); err != nil {
		return fmt.Errorf("knowledge: prepare ingest retry checkpoint: %w", err)
	}
	if err := persistVisionRouteSnapshotTx(ctx, tx, jobID, visionRoute, nowMillis); err != nil {
		return err
	}
	return nil
}

func queueFailedEmbeddingRetryTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerID, corpusUID, documentID string,
	generation int64,
	storageKey, jobID string,
	nowMillis int64,
) error {
	var parentJobID, revisionID string
	var expectedChunks, embeddedChunks int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(j.parent_job_id,''),j.target_revision_id,
		COALESCE(rd.expected_chunks,0),rd.embedded_chunks
		FROM kb_knowledge_jobs j
		JOIN kb_semantic_corpora c
		  ON c.owner_id=j.owner_id AND c.corpus_uid=j.corpus_uid
		 AND c.active_revision_id=j.target_revision_id
		JOIN kb_index_revisions r
		  ON r.corpus_uid=j.corpus_uid AND r.revision_id=j.target_revision_id
		 AND r.publish_state='active'
		JOIN kb_revision_documents rd
		  ON rd.corpus_uid=j.corpus_uid AND rd.revision_id=j.target_revision_id
		 AND rd.document_id=j.document_id AND rd.content_generation=j.document_generation
		WHERE j.owner_id=? AND j.corpus_uid=? AND j.document_id=? AND j.document_generation=?
		  AND j.kind='embed_document' AND j.state='failed' AND j.cancel_requested=0
		  AND NOT EXISTS (
			SELECT 1
			FROM kb_embedding_batch_manifests bm
			JOIN kb_knowledge_jobs uncertain_job ON uncertain_job.job_id=bm.job_id
			WHERE uncertain_job.owner_id=j.owner_id
			  AND uncertain_job.corpus_uid=j.corpus_uid
			  AND uncertain_job.document_id=j.document_id
			  AND uncertain_job.document_generation=j.document_generation
			  AND uncertain_job.target_revision_id=j.target_revision_id
			  AND uncertain_job.kind='embed_document'
			  AND bm.state='outcome_unknown'
		  )
		ORDER BY j.finished_at DESC,j.job_id DESC LIMIT 1`, ownerID, corpusUID,
		documentID, generation).Scan(&parentJobID, &revisionID, &expectedChunks, &embeddedChunks)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDocumentRetryNotAllowed
	}
	if err != nil {
		return err
	}
	if parentJobID == "" || expectedChunks <= 0 || embeddedChunks > expectedChunks {
		return ErrDocumentRetryNotAllowed
	}
	res, err := tx.ExecContext(ctx, `UPDATE kb_revision_documents
		SET vector_state='pending',failed_chunks=0,last_error='',visible_at=NULL,updated_at=?
		WHERE corpus_uid=? AND revision_id=? AND document_id=? AND content_generation=?
		  AND vector_state='failed'`, nowMillis, corpusUID, revisionID, documentID, generation)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrDocumentRetryNotAllowed
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_knowledge_jobs
		(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,document_generation,
		 target_revision_id,idempotency_key,state,stage,chunks_done,chunks_total,attempt,
		 cancel_requested,lease_owner,lease_epoch,last_error,created_at,updated_at)
		VALUES(?,?,'embed_document',?,?,?,?,?,?,'queued','embedding',?,?,0,0,'',0,'',?,?)`,
		jobID, parentJobID, ownerID, corpusUID, documentID, generation, revisionID, storageKey,
		embeddedChunks, expectedChunks, nowMillis, nowMillis); err != nil {
		if isUniqueConstraintErr(err) {
			return ErrIdempotencyConflict
		}
		return fmt.Errorf("knowledge: queue embedding retry: %w", err)
	}
	return refreshActiveRevisionAggregatesTx(ctx, tx, corpusUID, revisionID, nowMillis)
}

func lookupIngestReplayTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerID string,
	state semanticPolicyState,
	input CreateDocumentInput,
	filename, mediaType string,
	blob IngestBlob,
) (CreateDocumentResult, bool, error) {
	var jobID, documentID, corpusUID, existingDigest, existingName, existingMedia, textState string
	var agentID, learnerID, subject, grade string
	err := tx.QueryRowContext(ctx, `SELECT j.job_id,j.document_id,j.corpus_uid,s.blob_sha256,
		s.original_name,s.media_type,s.agent_id,s.learner_id,s.subject,s.grade,b.text_state
		FROM kb_knowledge_jobs j
		JOIN kb_ingest_document_sources s
		  ON s.document_id=j.document_id AND s.content_generation=j.document_generation
		JOIN kb_semantic_document_bindings b ON b.document_id=j.document_id
		WHERE j.owner_id=? AND j.corpus_uid=?
		  AND j.kind='ingest' AND j.idempotency_key=?
		ORDER BY j.created_at,j.job_id LIMIT 1`,
		ownerID, state.corpusUID, strings.TrimSpace(input.IdempotencyKey)).Scan(
		&jobID, &documentID, &corpusUID, &existingDigest, &existingName, &existingMedia,
		&agentID, &learnerID, &subject, &grade, &textState,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return CreateDocumentResult{}, false, nil
	}
	if err != nil {
		return CreateDocumentResult{}, false, err
	}
	if corpusUID != state.corpusUID || existingDigest != blob.SHA256 || existingName != filename ||
		existingMedia != mediaType || agentID != strings.TrimSpace(input.AgentID) ||
		learnerID != strings.TrimSpace(input.LearnerID) || subject != strings.TrimSpace(input.Subject) ||
		grade != strings.TrimSpace(input.Grade) {
		return CreateDocumentResult{}, false, ErrIdempotencyConflict
	}
	vectorState, err := vectorStateForPolicyTx(ctx, tx, state)
	if err != nil {
		return CreateDocumentResult{}, false, err
	}
	return CreateDocumentResult{
		DocumentID: documentID, JobID: jobID, TextIndexState: TextIndexState(textState),
		VectorIndexState: vectorState,
	}, true, nil
}

func findReviveableIngestDocumentTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerID, corpusUID, filename string,
) (documentID string, generation int64, revive bool, err error) {
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM kb_semantic_document_bindings b
		JOIN kb_documents d ON d.id=b.document_id
		WHERE b.owner_id=? AND b.corpus_uid=? AND b.lifecycle_state='active' AND d.deleted=0
		  AND d.title=? AND d.source IN (?,?)
	)`, ownerID, corpusUID, filename, "upload:"+filename, "image:upload:"+filename).Scan(&active); err != nil {
		return "", 0, false, err
	}
	if active != 0 {
		return "", 0, false, ErrIdempotencyConflict
	}
	err = tx.QueryRowContext(ctx, `SELECT b.document_id,b.content_generation
		FROM kb_semantic_document_bindings b
		JOIN kb_documents d ON d.id=b.document_id
		WHERE b.owner_id=? AND b.corpus_uid=? AND b.lifecycle_state='tombstoned' AND d.deleted=1
		  AND d.title=? AND d.source IN (?,?)
		ORDER BY b.updated_at DESC,b.document_id LIMIT 1`, ownerID, corpusUID, filename,
		"upload:"+filename, "image:upload:"+filename).Scan(&documentID, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return documentID, generation, true, nil
}

func vectorStateForPolicyTx(
	ctx context.Context,
	tx *sql.Tx,
	state semanticPolicyState,
) (VectorIndexState, error) {
	targetRevision, _, err := semanticMutationTargetRevisionTx(ctx, tx, state)
	if err != nil {
		return "", err
	}
	if targetRevision != "" {
		return VectorIndexPending, nil
	}
	if state.selection.Kind == EmbeddingSelectionDisabled {
		return VectorIndexDisabled, nil
	}
	return VectorIndexFailed, nil
}

func (r *SQLiteSemanticIndexRepository) GetIngestDocument(
	ctx context.Context,
	ownerID, documentID string,
) (PersistedIngestDocument, error) {
	return r.getIngestDocument(ctx, ownerID, "", documentID)
}

func (r *SQLiteSemanticIndexRepository) GetIngestDocumentForCorpusUID(
	ctx context.Context,
	ownerID, corpusUID, documentID string,
) (PersistedIngestDocument, error) {
	if strings.TrimSpace(corpusUID) == "" {
		return PersistedIngestDocument{}, ErrSemanticIndexNotFound
	}
	return r.getIngestDocument(ctx, ownerID, corpusUID, documentID)
}

func (r *SQLiteSemanticIndexRepository) getIngestDocument(
	ctx context.Context,
	ownerID, corpusUID, documentID string,
) (PersistedIngestDocument, error) {
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(documentID) == "" {
		return PersistedIngestDocument{}, ErrSemanticIndexNotFound
	}
	var result PersistedIngestDocument
	query := `SELECT s.document_id,s.owner_id,s.corpus_uid,c.corpus_alias,
		s.content_generation,s.original_name,s.extension,s.media_type,s.size_bytes,s.blob_sha256,
		bl.storage_path,s.agent_id,s.learner_id,s.subject,s.grade
		FROM kb_ingest_document_sources s
		JOIN kb_ingest_blobs bl ON bl.owner_id=s.owner_id AND bl.corpus_uid=s.corpus_uid
		 AND bl.sha256=s.blob_sha256
		JOIN kb_semantic_corpora c ON c.corpus_uid=s.corpus_uid
		JOIN kb_semantic_document_bindings b
		  ON b.document_id=s.document_id AND b.content_generation=s.content_generation
		WHERE s.owner_id=? AND s.document_id=? AND b.lifecycle_state='active'`
	args := []any{ownerID, documentID}
	if corpusUID != "" {
		query += ` AND s.corpus_uid=? AND b.corpus_uid=?`
		args = append(args, corpusUID, corpusUID)
	}
	err := r.db.QueryRowContext(ctx, query, args...).Scan(
		&result.DocumentID, &result.OwnerID, &result.CorpusUID, &result.CorpusAlias,
		&result.ContentGeneration, &result.Filename, &result.Extension, &result.MediaType,
		&result.SizeBytes, &result.SHA256, &result.StoragePath, &result.AgentID,
		&result.LearnerID, &result.Subject, &result.Grade,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PersistedIngestDocument{}, ErrSemanticIndexNotFound
	}
	if err != nil {
		return PersistedIngestDocument{}, err
	}
	return result, nil
}

func (r *SQLiteSemanticIndexRepository) GetIngestDocumentProjection(
	ctx context.Context,
	ownerID, documentID string,
) (KnowledgeDocumentProjection, error) {
	return r.getIngestDocumentProjection(ctx, ownerID, "", documentID)
}

func (r *SQLiteSemanticIndexRepository) GetIngestDocumentProjectionForCorpus(
	ctx context.Context,
	ownerID, corpusID, documentID string,
) (KnowledgeDocumentProjection, error) {
	if strings.TrimSpace(corpusID) == "" {
		return KnowledgeDocumentProjection{}, ErrSemanticIndexNotFound
	}
	return r.getIngestDocumentProjection(ctx, ownerID, corpusID, documentID)
}

func (r *SQLiteSemanticIndexRepository) getIngestDocumentProjection(
	ctx context.Context,
	ownerID, corpusID, documentID string,
) (KnowledgeDocumentProjection, error) {
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(documentID) == "" {
		return KnowledgeDocumentProjection{}, ErrSemanticIndexNotFound
	}
	var result KnowledgeDocumentProjection
	var pageCount sql.NullInt64
	var warningsJSON, textState string
	query := `SELECT s.document_id,s.content_generation,s.owner_id,c.corpus_alias,s.original_name,
		s.media_type,s.size_bytes,s.blob_sha256,s.agent_id,s.learner_id,s.subject,s.grade,
		s.page_count,s.warnings_json,b.text_state
		FROM kb_ingest_document_sources s
		JOIN kb_semantic_corpora c ON c.corpus_uid=s.corpus_uid
		JOIN kb_semantic_document_bindings b
		  ON b.document_id=s.document_id AND b.content_generation=s.content_generation
		WHERE s.owner_id=? AND s.document_id=? AND b.lifecycle_state='active'`
	args := []any{ownerID, documentID}
	if corpusID != "" {
		query += ` AND c.owner_id=? AND c.corpus_alias=?`
		args = append(args, ownerID, corpusID)
	}
	err := r.db.QueryRowContext(ctx, query, args...).Scan(
		&result.DocumentID, &result.DocumentGeneration, &result.OwnerID, &result.CorpusID, &result.Filename,
		&result.MediaType, &result.SizeBytes, &result.SHA256, &result.AgentID,
		&result.LearnerID, &result.Subject, &result.Grade, &pageCount, &warningsJSON, &textState,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return KnowledgeDocumentProjection{}, ErrSemanticIndexNotFound
	}
	if err != nil {
		return KnowledgeDocumentProjection{}, err
	}
	result.TextIndexState = TextIndexState(textState)
	if pageCount.Valid {
		result.PageCount = int64Pointer(pageCount)
	}
	if err := json.Unmarshal([]byte(warningsJSON), &result.Warnings); err != nil {
		return KnowledgeDocumentProjection{}, fmt.Errorf("knowledge: decode ingest warnings: %w", err)
	}
	if result.Warnings == nil {
		result.Warnings = []string{}
	}
	spans, err := r.loadDocumentSourceSpans(ctx, result.DocumentID)
	if err != nil {
		return KnowledgeDocumentProjection{}, err
	}
	result.SourceSpans = spans
	return result, nil
}

func (r *SQLiteSemanticIndexRepository) loadDocumentSourceSpans(
	ctx context.Context,
	documentID string,
) ([]SourceSpan, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT page_start,page_end,source_digest,
		source_offset_start,source_offset_end FROM kb_chunks
		WHERE doc_id=? ORDER BY chunk_index`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	spans := []SourceSpan{}
	seen := map[SourceSpan]struct{}{}
	for rows.Next() {
		var pageStart, pageEnd, offsetStart, offsetEnd sql.NullInt64
		var sourceDigest string
		if err := rows.Scan(&pageStart, &pageEnd, &sourceDigest, &offsetStart, &offsetEnd); err != nil {
			return nil, err
		}
		span := SourceSpan{SourceDigest: sourceDigest}
		if pageStart.Valid {
			span.PageStart = int(pageStart.Int64)
		}
		if pageEnd.Valid {
			span.PageEnd = int(pageEnd.Int64)
		}
		if offsetStart.Valid {
			span.SourceOffsetStart = offsetStart.Int64
		}
		if offsetEnd.Valid {
			span.SourceOffsetEnd = offsetEnd.Int64
		}
		if span.SourceDigest == "" && span.PageStart == 0 && span.SourceOffsetEnd == 0 {
			continue
		}
		if _, duplicate := seen[span]; duplicate {
			continue
		}
		seen[span] = struct{}{}
		spans = append(spans, span)
	}
	return spans, rows.Err()
}

func (r *SQLiteSemanticIndexRepository) ListIngestBlobPaths(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT storage_path FROM kb_ingest_blobs
		ORDER BY owner_id,corpus_uid,sha256`)
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
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

func (r *SQLiteSemanticIndexRepository) IsIngestBlobPathReferenced(ctx context.Context, path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, nil
	}
	var referenced int
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM kb_ingest_blobs b
		JOIN kb_ingest_document_sources s
		  ON s.owner_id=b.owner_id AND s.corpus_uid=b.corpus_uid AND s.blob_sha256=b.sha256
		WHERE b.storage_path=?
	)`, path).Scan(&referenced); err != nil {
		return false, err
	}
	return referenced != 0, nil
}

// SetIngestPageTotal publishes the page manifest under the exact live lease.
// Existing page artifacts are counted rather than trusting caller-maintained
// counters, so pages_done remains a truthful projection after restart.
func (r *SQLiteSemanticIndexRepository) SetIngestPageTotal(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	sourceDigest string,
	pagesTotal int64,
) error {
	if len(sourceDigest) != 64 || pagesTotal <= 0 {
		return fmt.Errorf("%w: invalid page manifest", ErrInvalidDocumentUpload)
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
	if job.Kind != KnowledgeJobIngest || job.DocumentID == "" {
		return ErrJobFenced
	}
	if job.PagesTotal != nil && *job.PagesTotal != pagesTotal {
		return fmt.Errorf("%w: page total changed from %d to %d",
			ErrInvalidDocumentUpload, *job.PagesTotal, pagesTotal)
	}
	if err := validateIngestPageSource(ctx, tx, job, sourceDigest); err != nil {
		return err
	}
	var pagesDone int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_ingest_page_checkpoints
		WHERE job_id=? AND source_digest=? AND pages_total=?`,
		job.JobID, sourceDigest, pagesTotal).Scan(&pagesDone); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs
		SET stage='ocr',pages_done=?,pages_total=?,heartbeat_at=?,updated_at=?
		WHERE job_id=? AND owner_id=? AND corpus_uid=? AND kind='ingest'
		  AND state='running' AND cancel_requested=0 AND lease_owner=?
		  AND lease_epoch=? AND lease_expires_at>?`,
		pagesDone, pagesTotal, now.UTC().UnixMilli(), now.UTC().UnixMilli(),
		lease.JobID, lease.OwnerID, lease.CorpusUID, lease.WorkerID, lease.Epoch,
		now.UTC().UnixMilli())
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrJobFenced
	}
	if err := r.reconcileDocumentIngestLifecycleTx(ctx, tx, job, now); err != nil {
		return err
	}
	return tx.Commit()
}

// LoadIngestPageCheckpoints returns only artifacts belonging to this job and
// immutable source. A read is also lease-fenced so a stale parser cannot use a
// new worker's progress as authority and continue producing side effects.
func (r *SQLiteSemanticIndexRepository) LoadIngestPageCheckpoints(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	sourceDigest string,
	pagesTotal int64,
) ([]IngestPageCheckpoint, error) {
	if len(sourceDigest) != 64 || pagesTotal <= 0 {
		return nil, fmt.Errorf("%w: invalid page manifest", ErrInvalidDocumentUpload)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return nil, err
	}
	if job.Kind != KnowledgeJobIngest || job.DocumentID == "" {
		return nil, ErrJobFenced
	}
	if job.PagesTotal != nil && *job.PagesTotal != pagesTotal {
		return nil, fmt.Errorf("%w: page total changed", ErrInvalidDocumentUpload)
	}
	if err := validateIngestPageSource(ctx, tx, job, sourceDigest); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT page_number,pages_total,source_digest,
		extraction_mode,content,content_digest,source_offset_start,source_offset_end
		FROM kb_ingest_page_checkpoints
		WHERE job_id=? AND source_digest=? AND pages_total=? ORDER BY page_number`,
		job.JobID, sourceDigest, pagesTotal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	checkpoints := []IngestPageCheckpoint{}
	for rows.Next() {
		var checkpoint IngestPageCheckpoint
		if err := rows.Scan(
			&checkpoint.PageNumber, &checkpoint.PagesTotal, &checkpoint.SourceDigest,
			&checkpoint.ExtractionMode, &checkpoint.Content, &checkpoint.ContentDigest,
			&checkpoint.SourceOffsetStart, &checkpoint.SourceOffsetEnd,
		); err != nil {
			return nil, err
		}
		if ingestPageContentDigest(checkpoint.Content) != checkpoint.ContentDigest {
			return nil, fmt.Errorf("%w: corrupt page checkpoint %d",
				ErrInvalidDocumentUpload, checkpoint.PageNumber)
		}
		checkpoints = append(checkpoints, checkpoint)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return checkpoints, nil
}

// SaveIngestPageCheckpoint atomically publishes one immutable page artifact
// and recomputes the job progress projection under the exact lease epoch.
func (r *SQLiteSemanticIndexRepository) SaveIngestPageCheckpoint(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	checkpoint IngestPageCheckpoint,
) error {
	if err := validateIngestPageCheckpoint(checkpoint); err != nil {
		return err
	}
	digest := ingestPageContentDigest(checkpoint.Content)
	if checkpoint.ContentDigest != "" && checkpoint.ContentDigest != digest {
		return fmt.Errorf("%w: page content digest mismatch", ErrInvalidDocumentUpload)
	}
	checkpoint.ContentDigest = digest
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return err
	}
	if job.Kind != KnowledgeJobIngest || job.DocumentID == "" {
		return ErrJobFenced
	}
	if job.PagesTotal != nil && *job.PagesTotal != checkpoint.PagesTotal {
		return fmt.Errorf("%w: page total changed", ErrInvalidDocumentUpload)
	}
	if err := validateIngestPageSource(ctx, tx, job, checkpoint.SourceDigest); err != nil {
		return err
	}
	nowMillis := now.UTC().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_ingest_page_checkpoints
		(job_id,page_number,pages_total,source_digest,extraction_mode,content,content_digest,
		 source_offset_start,source_offset_end,lease_epoch,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(job_id,page_number) DO NOTHING`,
		job.JobID, checkpoint.PageNumber, checkpoint.PagesTotal, checkpoint.SourceDigest,
		checkpoint.ExtractionMode, checkpoint.Content, checkpoint.ContentDigest,
		checkpoint.SourceOffsetStart, checkpoint.SourceOffsetEnd, lease.Epoch,
		nowMillis, nowMillis); err != nil {
		return fmt.Errorf("knowledge: save page checkpoint: %w", err)
	}
	var existing IngestPageCheckpoint
	if err := tx.QueryRowContext(ctx, `SELECT page_number,pages_total,source_digest,
		extraction_mode,content,content_digest,source_offset_start,source_offset_end
		FROM kb_ingest_page_checkpoints WHERE job_id=? AND page_number=?`,
		job.JobID, checkpoint.PageNumber).Scan(
		&existing.PageNumber, &existing.PagesTotal, &existing.SourceDigest,
		&existing.ExtractionMode, &existing.Content, &existing.ContentDigest,
		&existing.SourceOffsetStart, &existing.SourceOffsetEnd,
	); err != nil {
		return err
	}
	if existing != checkpoint {
		return fmt.Errorf("%w: conflicting immutable page checkpoint %d",
			ErrInvalidDocumentUpload, checkpoint.PageNumber)
	}
	var pagesDone int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_ingest_page_checkpoints
		WHERE job_id=? AND source_digest=? AND pages_total=?`,
		job.JobID, checkpoint.SourceDigest, checkpoint.PagesTotal).Scan(&pagesDone); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs
		SET stage='ocr',pages_done=?,pages_total=?,heartbeat_at=?,updated_at=?
		WHERE job_id=? AND owner_id=? AND corpus_uid=? AND kind='ingest'
		  AND state='running' AND cancel_requested=0 AND lease_owner=?
		  AND lease_epoch=? AND lease_expires_at>?`,
		pagesDone, checkpoint.PagesTotal, nowMillis, nowMillis, lease.JobID,
		lease.OwnerID, lease.CorpusUID, lease.WorkerID, lease.Epoch, nowMillis)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrJobFenced
	}
	if err := refreshIngestSegmentStatesTx(ctx, tx, job.JobID, nowMillis); err != nil {
		return err
	}
	return tx.Commit()
}

func validateIngestPageCheckpoint(checkpoint IngestPageCheckpoint) error {
	if checkpoint.PageNumber <= 0 || checkpoint.PagesTotal <= 0 ||
		int64(checkpoint.PageNumber) > checkpoint.PagesTotal || len(checkpoint.SourceDigest) != 64 ||
		strings.TrimSpace(checkpoint.Content) == "" || checkpoint.SourceOffsetStart < 0 ||
		checkpoint.SourceOffsetEnd < checkpoint.SourceOffsetStart {
		return fmt.Errorf("%w: invalid page checkpoint", ErrInvalidDocumentUpload)
	}
	switch checkpoint.ExtractionMode {
	case "text", "ocr_vlm", "image", "document":
		return nil
	default:
		return fmt.Errorf("%w: invalid page extraction mode", ErrInvalidDocumentUpload)
	}
}

func ingestPageContentDigest(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func validateIngestPageSource(
	ctx context.Context,
	q semanticDBQueryer,
	job KnowledgeJob,
	sourceDigest string,
) error {
	var valid int
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM kb_ingest_document_sources s
		WHERE s.owner_id=? AND s.corpus_uid=? AND s.document_id=?
		  AND s.content_generation=? AND s.blob_sha256=?
	)`, job.OwnerID, job.CorpusUID, job.DocumentID, job.DocumentGeneration,
		sourceDigest).Scan(&valid); err != nil {
		return err
	}
	if valid != 1 {
		return ErrJobFenced
	}
	return nil
}

// CompleteIngestDocument atomically publishes parsed text/FTS, advances the
// corpus content version and succeeds the leased root job. Vector work is
// never executed here; when an active revision exists it is queued as a child
// embed_document job in this same transaction.
func (r *SQLiteSemanticIndexRepository) CompleteIngestDocument(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	prepared PreparedIngestDocument,
) error {
	if err := validatePreparedIngestDocument(prepared); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	projectionWasCurrent, err := cjkFTSProjectionCurrentTx(ctx, tx)
	if err != nil {
		return err
	}
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return err
	}
	if job.Kind != KnowledgeJobIngest || job.DocumentID == "" ||
		prepared.Document.ID != job.DocumentID {
		return ErrJobFenced
	}

	var source PersistedIngestDocument
	err = tx.QueryRowContext(ctx, `SELECT s.document_id,s.owner_id,s.corpus_uid,c.corpus_alias,
		s.content_generation,s.original_name,s.extension,s.media_type,s.size_bytes,s.blob_sha256,
		bl.storage_path,s.agent_id,s.learner_id,s.subject,s.grade
		FROM kb_ingest_document_sources s
		JOIN kb_ingest_blobs bl ON bl.owner_id=s.owner_id AND bl.corpus_uid=s.corpus_uid
		 AND bl.sha256=s.blob_sha256
		JOIN kb_semantic_corpora c ON c.corpus_uid=s.corpus_uid
		JOIN kb_semantic_document_bindings b ON b.document_id=s.document_id
		WHERE s.owner_id=? AND s.corpus_uid=? AND s.document_id=?
		  AND s.content_generation=? AND b.lifecycle_state='active'
		  AND b.content_generation=s.content_generation AND b.text_state IN ('pending','building')`,
		job.OwnerID, job.CorpusUID, job.DocumentID, job.DocumentGeneration).Scan(
		&source.DocumentID, &source.OwnerID, &source.CorpusUID, &source.CorpusAlias,
		&source.ContentGeneration, &source.Filename, &source.Extension, &source.MediaType,
		&source.SizeBytes, &source.SHA256, &source.StoragePath, &source.AgentID,
		&source.LearnerID, &source.Subject, &source.Grade,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrJobFenced
	}
	if err != nil {
		return err
	}
	if source.Extension == ".pdf" || strings.EqualFold(source.MediaType, "application/pdf") {
		if err := validateCompletePDFPageCheckpointsTx(
			ctx, tx, job, source.SHA256, prepared.PageCount,
		); err != nil {
			return err
		}
	} else {
		var checkpointCount int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_ingest_page_checkpoints
			WHERE job_id=? AND source_digest=? AND pages_total=?`, job.JobID, source.SHA256,
			prepared.PageCount).Scan(&checkpointCount); err != nil {
			return err
		}
		if checkpointCount > 0 && checkpointCount != prepared.PageCount {
			return fmt.Errorf("%w: incomplete durable page checkpoint set %d/%d",
				ErrInvalidDocumentUpload, checkpointCount, prepared.PageCount)
		}
	}
	if err := validateReadyIngestSegmentsTx(
		ctx, tx, job.JobID, prepared.PageCount,
	); err != nil {
		return err
	}
	now = now.UTC()
	nowMillis := now.UnixMilli()
	sourceType := "upload"
	sourceLabel := "upload:" + source.Filename
	if strings.HasPrefix(strings.ToLower(source.MediaType), "image/") {
		sourceType = "image"
		sourceLabel = "image:upload:" + source.Filename
	}
	prepared.Document.Title = source.Filename
	prepared.Document.Source = sourceLabel
	prepared.Document.SourceType = sourceType
	prepared.Document.Status = "indexed"
	prepared.Document.ErrorMessage = ""
	prepared.Document.ChunkCount = len(prepared.Chunks)
	prepared.Document.UpdatedAt = now
	for i, chunk := range prepared.Chunks {
		chunk.DocID = job.DocumentID
		chunk.DocTitle = source.Filename
		chunk.Source = prepared.Document.Source
		chunk.SourceType = sourceType
		chunk.ChunkCount = len(prepared.Chunks)
		chunk.Index = i
		chunk.Embedding = nil
		chunk.CreatedAt = now
		if chunk.SourceDigest == "" {
			chunk.SourceDigest = source.SHA256
		} else if chunk.SourceDigest != source.SHA256 {
			return fmt.Errorf("%w: chunk source digest mismatch", ErrInvalidDocumentUpload)
		}
	}

	res, err := tx.ExecContext(ctx, `UPDATE kb_documents SET title=?,content=?,source=?,chunk_count=?,
		updated_at=?,status='indexed',error_message='',source_type=?
		WHERE id=? AND corpus_uid=? AND deleted=0 AND status='processing'`, prepared.Document.Title,
		prepared.Document.Content, prepared.Document.Source, len(prepared.Chunks), now, sourceType,
		job.DocumentID, job.CorpusUID)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrJobFenced
	}
	for _, chunk := range prepared.Chunks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_chunks
			(id,doc_id,content,chunk_index,embedding,created_at,page_start,page_end,
			 source_digest,source_offset_start,source_offset_end)
			VALUES(?,?,?,?,NULL,?,?,?,?,?,?)`,
			chunk.ID, job.DocumentID, chunk.Content, chunk.Index, now,
			nullablePositiveInt(chunk.PageStart), nullablePositiveInt(chunk.PageEnd),
			chunk.SourceDigest, nullableOffset(chunk.SourceOffsetStart, chunk.SourceOffsetEnd, false),
			nullableOffset(chunk.SourceOffsetStart, chunk.SourceOffsetEnd, true)); err != nil {
			return fmt.Errorf("knowledge: publish ingest chunk: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_chunks_fts(content,chunk_id) VALUES(?,?)`,
			chunk.Content, chunk.ID); err != nil {
			return fmt.Errorf("knowledge: publish ingest FTS: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_chunks_fts_v2(tokens,chunk_id) VALUES(?,?)`,
			cjkFTSIndexText(chunk.Content), chunk.ID); err != nil {
			return fmt.Errorf("knowledge: publish ingest CJK FTS v2: %w", err)
		}
	}
	res, err = tx.ExecContext(ctx, `UPDATE kb_semantic_document_bindings
		SET text_state='ready',version=version+1,updated_at=?
		WHERE owner_id=? AND corpus_uid=? AND document_id=? AND content_generation=?
		  AND lifecycle_state='active' AND text_state IN ('pending','building')`, nowMillis,
		job.OwnerID, job.CorpusUID, job.DocumentID, job.DocumentGeneration)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrJobFenced
	}
	warnings := prepared.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	warningsJSON, err := json.Marshal(warnings)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kb_ingest_document_sources
		SET page_count=?,warnings_json=?,updated_at=?
		WHERE owner_id=? AND corpus_uid=? AND document_id=? AND content_generation=?`,
		prepared.PageCount, string(warningsJSON), nowMillis, job.OwnerID, job.CorpusUID,
		job.DocumentID, job.DocumentGeneration); err != nil {
		return err
	}

	state, err := loadSemanticPolicyStateByUID(ctx, tx, job.OwnerID, job.CorpusUID)
	if err != nil {
		return err
	}
	if err := bumpSemanticContentVersionTx(ctx, tx, &state, nowMillis); err != nil {
		return err
	}
	targetRevision, desiredIsHealthy, err := semanticMutationTargetRevisionTx(ctx, tx, state)
	if err != nil {
		return err
	}
	if targetRevision != "" {
		if err := insertRevisionDocumentTx(ctx, tx, state.corpusUID, targetRevision,
			job.DocumentID, job.DocumentGeneration, int64(len(prepared.Chunks)), nowMillis); err != nil {
			return err
		}
		if err := refreshActiveRevisionAggregatesTx(ctx, tx, state.corpusUID, state.activeRevision, nowMillis); err != nil {
			return err
		}
		if desiredIsHealthy {
			if err := requeueStagedRevisionTx(ctx, tx, state.corpusUID, targetRevision, nowMillis); err != nil {
				return err
			}
		} else if len(prepared.Chunks) == 0 {
			if err := markEmptyRevisionDocumentReadyTx(ctx, tx, state.corpusUID, targetRevision,
				job.DocumentID, job.DocumentGeneration, nowMillis); err != nil {
				return err
			}
		} else if err := createEmbedDocumentJobWithParentTx(ctx, tx, job.OwnerID, state,
			targetRevision, job.DocumentID, job.DocumentGeneration,
			int64(len(prepared.Chunks)), job.JobID, nowMillis); err != nil {
			return err
		}
	}

	extractFingerprint := hashStrings(source.SHA256, source.Extension, source.MediaType)
	chunkDigestParts := []string{job.DocumentID, prepared.Document.Content}
	for _, chunk := range prepared.Chunks {
		chunkDigestParts = append(chunkDigestParts, chunk.ID, chunk.Content)
	}
	chunkDigest := hashStrings(chunkDigestParts...)
	for _, checkpoint := range []StageCheckpoint{
		{Stage: JobStageExtracting, InputFingerprint: extractFingerprint,
			ArtifactRef: "blob://" + source.SHA256, ArtifactDigest: source.SHA256, State: StageCheckpointSucceeded},
		{Stage: JobStageChunking, InputFingerprint: hashStrings(source.SHA256, prepared.Document.Content),
			ArtifactRef: "document://" + job.DocumentID + "/chunks", ArtifactDigest: chunkDigest, State: StageCheckpointSucceeded},
		{Stage: JobStageTextIndexing, InputFingerprint: chunkDigest,
			ArtifactRef: "document://" + job.DocumentID + "/text-index", ArtifactDigest: chunkDigest, State: StageCheckpointSucceeded},
	} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_job_stage_checkpoints
			(job_id,stage,input_fingerprint,artifact_ref,artifact_digest,state,lease_epoch,created_at,updated_at)
			VALUES(?,?,?,?,?,'succeeded',?,?,?)
			ON CONFLICT(job_id,stage) DO UPDATE SET
			 input_fingerprint=excluded.input_fingerprint,artifact_ref=excluded.artifact_ref,
			 artifact_digest=excluded.artifact_digest,state='succeeded',lease_epoch=excluded.lease_epoch,
			 updated_at=excluded.updated_at`, job.JobID, checkpoint.Stage,
			checkpoint.InputFingerprint, checkpoint.ArtifactRef, checkpoint.ArtifactDigest,
			lease.Epoch, nowMillis, nowMillis); err != nil {
			return fmt.Errorf("knowledge: publish ingest checkpoint: %w", err)
		}
	}
	chunksTotal := int64(len(prepared.Chunks))
	res, err = tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs
		SET state='succeeded',stage='text_indexing',pages_done=?,pages_total=?,
		 chunks_done=?,chunks_total=?,last_error='',lease_owner='',lease_epoch=lease_epoch+1,
		 lease_expires_at=NULL,heartbeat_at=NULL,updated_at=?,finished_at=?
		WHERE job_id=? AND owner_id=? AND corpus_uid=? AND kind='ingest'
		 AND document_id=? AND document_generation=? AND state='running' AND cancel_requested=0
		 AND lease_owner=? AND lease_epoch=? AND lease_expires_at>?`, prepared.PageCount,
		prepared.PageCount, chunksTotal, chunksTotal, nowMillis, nowMillis, job.JobID,
		job.OwnerID, job.CorpusUID, job.DocumentID, job.DocumentGeneration,
		lease.WorkerID, lease.Epoch, nowMillis)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		return ErrJobFenced
	}
	if err := restoreCJKFTSCurrentTx(ctx, tx, projectionWasCurrent); err != nil {
		return fmt.Errorf("knowledge: publish CJK FTS v2 version: %w", err)
	}
	return tx.Commit()
}

func validateCompletePDFPageCheckpointsTx(
	ctx context.Context,
	tx *sql.Tx,
	job KnowledgeJob,
	sourceDigest string,
	pagesTotal int64,
) error {
	if job.PagesDone == nil || job.PagesTotal == nil || *job.PagesDone != pagesTotal ||
		*job.PagesTotal != pagesTotal {
		return fmt.Errorf("%w: PDF page progress does not match completion %s/%d",
			ErrInvalidDocumentUpload, formatJobPageProgress(job), pagesTotal)
	}
	rows, err := tx.QueryContext(ctx, `SELECT page_number,pages_total,source_digest,content,content_digest
		FROM kb_ingest_page_checkpoints WHERE job_id=? ORDER BY page_number`, job.JobID)
	if err != nil {
		return err
	}
	defer rows.Close()
	expectedPage := 1
	for rows.Next() {
		var pageNumber int
		var checkpointTotal int64
		var checkpointDigest, content, contentDigest string
		if err := rows.Scan(
			&pageNumber, &checkpointTotal, &checkpointDigest, &content, &contentDigest,
		); err != nil {
			return err
		}
		if pageNumber != expectedPage || checkpointTotal != pagesTotal ||
			checkpointDigest != sourceDigest || ingestPageContentDigest(content) != contentDigest {
			return fmt.Errorf("%w: invalid durable PDF page checkpoint %d",
				ErrInvalidDocumentUpload, pageNumber)
		}
		expectedPage++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	completed := int64(expectedPage - 1)
	if completed != pagesTotal {
		return fmt.Errorf("%w: incomplete durable PDF page checkpoint set %d/%d",
			ErrInvalidDocumentUpload, completed, pagesTotal)
	}
	return nil
}

func formatJobPageProgress(job KnowledgeJob) string {
	if job.PagesDone == nil || job.PagesTotal == nil {
		return "unset"
	}
	return fmt.Sprintf("%d/%d", *job.PagesDone, *job.PagesTotal)
}

func validatePreparedIngestDocument(prepared PreparedIngestDocument) error {
	if prepared.Document == nil || strings.TrimSpace(prepared.Document.ID) == "" ||
		strings.TrimSpace(prepared.Document.Content) == "" || prepared.PageCount <= 0 ||
		len(prepared.Chunks) == 0 {
		return fmt.Errorf("%w: empty parsed document", ErrInvalidDocumentUpload)
	}
	seen := make(map[string]struct{}, len(prepared.Chunks))
	for i, chunk := range prepared.Chunks {
		if chunk == nil || strings.TrimSpace(chunk.ID) == "" ||
			strings.TrimSpace(chunk.Content) == "" || chunk.DocID != prepared.Document.ID {
			return fmt.Errorf("%w: invalid chunk %d", ErrInvalidDocumentUpload, i)
		}
		if _, duplicate := seen[chunk.ID]; duplicate {
			return fmt.Errorf("%w: duplicate chunk id", ErrInvalidDocumentUpload)
		}
		if chunk.PageStart < 0 || chunk.PageEnd < chunk.PageStart ||
			chunk.SourceOffsetStart < 0 || chunk.SourceOffsetEnd < chunk.SourceOffsetStart {
			return fmt.Errorf("%w: invalid chunk source span", ErrInvalidDocumentUpload)
		}
		if chunk.PageStart > 0 && (chunk.PageEnd == 0 || len(chunk.SourceDigest) != 64 ||
			chunk.SourceOffsetEnd <= chunk.SourceOffsetStart) {
			return fmt.Errorf("%w: incomplete chunk source span", ErrInvalidDocumentUpload)
		}
		seen[chunk.ID] = struct{}{}
	}
	return nil
}
