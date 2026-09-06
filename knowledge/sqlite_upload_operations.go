package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/internal/sqliteutil"
)

type uploadOperationRepository interface {
	BeginUploadOperation(
		context.Context, string, string, CreateDocumentInput,
	) (UploadOperationProjection, bool, error)
	MarkUploadOperationFailed(
		context.Context, string, string, string, UploadOperationState, string,
	) error
	MarkUploadResponseDelivered(context.Context, string, string, string) error
	ListUploadOperationsForCorpus(
		context.Context, string, string,
	) ([]UploadOperationProjection, error)
}

type uploadOperationStartupReconciler interface {
	CancelOrphanedReceivingUploadOperations(context.Context, time.Time) error
}

// BeginUploadOperation durably records request identity before any request byte
// is consumed. Replays return the same operation ID and never create a second
// physical ingest job.
func (r *SQLiteSemanticIndexRepository) BeginUploadOperation(
	ctx context.Context,
	ownerID, corpusID string,
	input CreateDocumentInput,
) (UploadOperationProjection, bool, error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return UploadOperationProjection{}, false, err
	}
	filename, _, mediaType, err := validateCreateDocumentInput(input)
	if err != nil {
		return UploadOperationProjection{}, false, err
	}
	fingerprint := hashStrings(
		filename,
		mediaType,
		strconv.FormatInt(input.SizeBytes, 10),
		strings.TrimSpace(input.AgentID),
		strings.TrimSpace(input.LearnerID),
		strings.TrimSpace(input.Subject),
		strings.TrimSpace(input.Grade),
	)

	var projection UploadOperationProjection
	var created bool
	err = sqliteutil.RetryOnBusy(ctx, func() error {
		var attemptErr error
		projection, created, attemptErr = r.beginUploadOperationOnce(
			ctx, ownerID, corpusID, strings.TrimSpace(input.IdempotencyKey),
			filename, mediaType, input.SizeBytes, fingerprint,
		)
		return attemptErr
	})
	return projection, created, err
}

func (r *SQLiteSemanticIndexRepository) beginUploadOperationOnce(
	ctx context.Context,
	ownerID, corpusID, idempotencyKey, displayName, mediaType string,
	sizeBytes int64,
	fingerprint string,
) (UploadOperationProjection, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return UploadOperationProjection{}, false, err
	}
	defer tx.Rollback()
	state, err := loadSemanticPolicyState(ctx, tx, ownerID, corpusID)
	if err != nil {
		return UploadOperationProjection{}, false, err
	}
	operationID, err := semanticID("upload")
	if err != nil {
		return UploadOperationProjection{}, false, err
	}
	now := semanticNowMillis()
	result, err := tx.ExecContext(ctx, `INSERT INTO kb_upload_operations
		(operation_id,owner_id,corpus_uid,idempotency_key,request_fingerprint,
		 display_name,media_type,size_bytes,content_digest,document_id,job_id,state,
		 last_error,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,NULL,NULL,NULL,'receiving','',?,?)
		ON CONFLICT(owner_id,corpus_uid,idempotency_key) DO NOTHING`,
		operationID, ownerID, state.corpusUID, idempotencyKey, fingerprint,
		displayName, mediaType, sizeBytes, now, now)
	if err != nil {
		return UploadOperationProjection{}, false, fmt.Errorf("knowledge: begin upload operation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return UploadOperationProjection{}, false, err
	}
	projection, storedFingerprint, err := loadUploadOperationByKeyTx(
		ctx, tx, ownerID, state.corpusUID, idempotencyKey,
	)
	if err != nil {
		return UploadOperationProjection{}, false, err
	}
	// Empty fingerprints are reserved for V71's legacy-job backfill. The
	// existing ingest replay check still binds those rows to the exact digest
	// and immutable source metadata before returning an accepted result.
	if storedFingerprint != "" && storedFingerprint != fingerprint {
		return UploadOperationProjection{}, false, ErrIdempotencyConflict
	}
	// 尚未绑定文档或任务的确定上传失败可由同一请求身份重新接收；
	// 只有条件更新成功的请求取得读取资格，正在接收及已绑定任务保持原重放语义。
	if rows == 0 && storedFingerprint == fingerprint &&
		projection.State == UploadOperationFailed && projection.Error == "upload_failed" &&
		projection.DocumentID == "" && projection.JobID == "" {
		result, err = tx.ExecContext(ctx, `UPDATE kb_upload_operations
			SET state='receiving',last_error='',updated_at=?
			WHERE operation_id=? AND owner_id=? AND corpus_uid=? AND idempotency_key=?
			  AND request_fingerprint=? AND state='failed' AND last_error='upload_failed'
			  AND document_id IS NULL AND job_id IS NULL`,
			now, projection.OperationID, ownerID, state.corpusUID, idempotencyKey, fingerprint)
		if err != nil {
			return UploadOperationProjection{}, false, fmt.Errorf("knowledge: resume unbound upload operation: %w", err)
		}
		rows, err = result.RowsAffected()
		if err != nil {
			return UploadOperationProjection{}, false, err
		}
		projection, _, err = loadUploadOperationByKeyTx(ctx, tx, ownerID, state.corpusUID, idempotencyKey)
		if err != nil {
			return UploadOperationProjection{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return UploadOperationProjection{}, false, err
	}
	return projection, rows == 1, nil
}

func loadUploadOperationByKeyTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerID, corpusUID, idempotencyKey string,
) (UploadOperationProjection, string, error) {
	var projection UploadOperationProjection
	var requestFingerprint string
	var documentID, jobID, contentDigest sql.NullString
	var state string
	var createdAt, updatedAt int64
	err := tx.QueryRowContext(ctx, `SELECT operation_id,owner_id,request_fingerprint,
		display_name,media_type,size_bytes,content_digest,document_id,job_id,state,
		last_error,created_at,updated_at
		FROM kb_upload_operations
		WHERE owner_id=? AND corpus_uid=? AND idempotency_key=?`,
		ownerID, corpusUID, idempotencyKey).Scan(
		&projection.OperationID, &projection.OwnerID, &requestFingerprint,
		&projection.DisplayName, &projection.MediaType, &projection.SizeBytes,
		&contentDigest, &documentID, &jobID, &state, &projection.Error,
		&createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return UploadOperationProjection{}, "", ErrIdempotencyConflict
	}
	if err != nil {
		return UploadOperationProjection{}, "", err
	}
	projection.DocumentID = documentID.String
	projection.JobID = jobID.String
	projection.ContentDigest = contentDigest.String
	projection.State = UploadOperationState(state)
	projection.Stage = uploadOperationStage(projection.State, "")
	projection.Terminal = uploadOperationTerminal(projection.State)
	projection.CreatedAt = time.UnixMilli(createdAt).UTC()
	projection.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return projection, requestFingerprint, nil
}

// bindUploadOperationTx is called from the same transaction that creates or
// replays the immutable source, document and root ingest job. There is no
// crash window in which a projection can contain only some of those IDs.
func bindUploadOperationTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerID, corpusUID, operationID, documentID, jobID, contentDigest string,
	sizeBytes int64,
	now int64,
) error {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		// Direct repository callers predate the application-service UploadIntent
		// boundary. They remain source compatible without creating inferred rows.
		return nil
	}
	if sizeBytes <= 0 || sizeBytes > MaxKnowledgeDocumentBytes {
		return ErrInvalidDocumentUpload
	}
	result, err := tx.ExecContext(ctx, `UPDATE kb_upload_operations
		SET content_digest=?,size_bytes=?,document_id=?,job_id=?,state='pending_response',
		    last_error='',updated_at=?
		WHERE operation_id=? AND owner_id=? AND corpus_uid=?
		  AND state='receiving' AND document_id IS NULL AND job_id IS NULL`,
		contentDigest, sizeBytes, documentID, jobID, now, operationID, ownerID, corpusUID)
	if err != nil {
		return fmt.Errorf("knowledge: bind upload operation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 1 {
		return nil
	}
	var existingDocument, existingJob, existingDigest sql.NullString
	var existingSize int64
	err = tx.QueryRowContext(ctx, `SELECT document_id,job_id,content_digest,size_bytes
		FROM kb_upload_operations
		WHERE operation_id=? AND owner_id=? AND corpus_uid=?`,
		operationID, ownerID, corpusUID).Scan(
		&existingDocument, &existingJob, &existingDigest, &existingSize,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSemanticIndexNotFound
	}
	if err != nil {
		return err
	}
	if existingDocument.String != documentID || existingJob.String != jobID ||
		existingDigest.String != contentDigest || existingSize != sizeBytes {
		return ErrIdempotencyConflict
	}
	return nil
}

func (r *SQLiteSemanticIndexRepository) MarkUploadOperationFailed(
	ctx context.Context,
	ownerID, corpusID, operationID string,
	state UploadOperationState,
	errorCode string,
) error {
	if state != UploadOperationFailed && state != UploadOperationCancelled {
		return fmt.Errorf("knowledge: invalid pre-accept upload terminal state %q", state)
	}
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return err
	}
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return ErrSemanticIndexNotFound
	}
	errorCode = strings.TrimSpace(errorCode)
	if len(errorCode) > 128 {
		errorCode = errorCode[:128]
	}
	return sqliteutil.RetryOnBusy(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		semanticState, err := loadSemanticPolicyState(ctx, tx, ownerID, corpusID)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE kb_upload_operations
			SET state=?,last_error=?,updated_at=?
			WHERE operation_id=? AND owner_id=? AND corpus_uid=?
			  AND state='receiving' AND document_id IS NULL AND job_id IS NULL`,
			state, errorCode, semanticNowMillis(), operationID, ownerID, semanticState.corpusUID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			var found int
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM kb_upload_operations
				WHERE operation_id=? AND owner_id=? AND corpus_uid=?)`,
				operationID, ownerID, semanticState.corpusUID).Scan(&found); err != nil {
				return err
			}
			if found == 0 {
				return ErrSemanticIndexNotFound
			}
		}
		return tx.Commit()
	})
}

// CancelOrphanedReceivingUploadOperations fences pre-accept operations left by
// an earlier Sidecar process. ConfigureDocumentIngest invokes it before the
// HTTP server starts, so no upload owned by the current process can be fenced.
// The cutoff makes the command deterministic and safe to repeat.
func (r *SQLiteSemanticIndexRepository) CancelOrphanedReceivingUploadOperations(
	ctx context.Context,
	startupCutoff time.Time,
) error {
	cutoffMillis := startupCutoff.UTC().UnixMilli()
	if cutoffMillis <= 0 {
		return fmt.Errorf("knowledge: invalid upload recovery startup cutoff")
	}
	return sqliteutil.RetryOnBusy(ctx, func() error {
		_, err := r.db.ExecContext(ctx, `UPDATE kb_upload_operations
			SET state='cancelled',last_error='sidecar_restarted_before_acceptance',updated_at=?
			WHERE state='receiving' AND document_id IS NULL AND job_id IS NULL
			  AND created_at<=?`, cutoffMillis, cutoffMillis)
		if err != nil {
			return fmt.Errorf("knowledge: fence orphaned receiving uploads: %w", err)
		}
		return nil
	})
}

func (r *SQLiteSemanticIndexRepository) MarkUploadResponseDelivered(
	ctx context.Context,
	ownerID, corpusID, operationID string,
) error {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return err
	}
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return ErrSemanticIndexNotFound
	}
	return sqliteutil.RetryOnBusy(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		semanticState, err := loadSemanticPolicyState(ctx, tx, ownerID, corpusID)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE kb_upload_operations
			SET state='queued',updated_at=?
			WHERE operation_id=? AND owner_id=? AND corpus_uid=?
			  AND state='pending_response' AND document_id IS NOT NULL AND job_id IS NOT NULL`,
			semanticNowMillis(), operationID, ownerID, semanticState.corpusUID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			var found int
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM kb_upload_operations
				WHERE operation_id=? AND owner_id=? AND corpus_uid=?)`,
				operationID, ownerID, semanticState.corpusUID).Scan(&found); err != nil {
				return err
			}
			if found == 0 {
				return ErrSemanticIndexNotFound
			}
		}
		return tx.Commit()
	})
}

func (r *SQLiteSemanticIndexRepository) ListUploadOperationsForCorpus(
	ctx context.Context,
	ownerID, corpusID string,
) ([]UploadOperationProjection, error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return nil, err
	}
	semanticState, err := loadSemanticPolicyState(ctx, r.db, ownerID, corpusID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT
		o.operation_id,o.owner_id,c.corpus_alias,
		COALESCE(o.document_id,''),COALESCE(o.job_id,''),o.display_name,o.media_type,
		o.size_bytes,COALESCE(o.content_digest,''),o.state,o.last_error,
		o.created_at,o.updated_at,COALESCE(j.state,''),COALESCE(j.stage,''),
		COALESCE(j.last_error,''),COALESCE(j.updated_at,0)
		FROM kb_upload_operations o
		JOIN kb_semantic_corpora c
		  ON c.owner_id=o.owner_id AND c.corpus_uid=o.corpus_uid
		LEFT JOIN kb_knowledge_jobs j
		  ON j.job_id=o.job_id AND j.owner_id=o.owner_id AND j.corpus_uid=o.corpus_uid
		WHERE o.owner_id=? AND o.corpus_uid=?
		ORDER BY o.updated_at DESC,o.operation_id`, ownerID, semanticState.corpusUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := make([]UploadOperationProjection, 0)
	for rows.Next() {
		var operation UploadOperationProjection
		var persistedState, jobState, jobStage, jobError string
		var createdAt, operationUpdatedAt, jobUpdatedAt int64
		if err := rows.Scan(
			&operation.OperationID, &operation.OwnerID, &operation.CorpusID,
			&operation.DocumentID, &operation.JobID, &operation.DisplayName,
			&operation.MediaType, &operation.SizeBytes, &operation.ContentDigest,
			&persistedState, &operation.Error, &createdAt, &operationUpdatedAt,
			&jobState, &jobStage, &jobError, &jobUpdatedAt,
		); err != nil {
			return nil, err
		}
		operation.State = UploadOperationState(persistedState)
		if operation.State != UploadOperationPendingResponse && jobState != "" {
			operation.State = UploadOperationState(jobState)
		}
		if jobError != "" {
			operation.Error = jobError
		}
		operation.Stage = uploadOperationStage(operation.State, jobStage)
		operation.Terminal = uploadOperationTerminal(operation.State)
		operation.CreatedAt = time.UnixMilli(createdAt).UTC()
		if jobUpdatedAt > operationUpdatedAt && operation.State != UploadOperationPendingResponse {
			operationUpdatedAt = jobUpdatedAt
		}
		operation.UpdatedAt = time.UnixMilli(operationUpdatedAt).UTC()
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return operations, nil
}

func uploadOperationStage(state UploadOperationState, jobStage string) string {
	switch state {
	case UploadOperationReceiving, UploadOperationPendingResponse:
		return string(state)
	case UploadOperationQueued, UploadOperationRunning, UploadOperationRetryWait,
		UploadOperationSucceeded, UploadOperationFailed, UploadOperationCancelled:
		if strings.TrimSpace(jobStage) != "" {
			return jobStage
		}
		return string(state)
	default:
		return string(state)
	}
}

func uploadOperationTerminal(state UploadOperationState) bool {
	return state == UploadOperationSucceeded || state == UploadOperationFailed ||
		state == UploadOperationCancelled
}
