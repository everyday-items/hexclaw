package knowledge

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const documentGCIdempotencyPrefix = "document-gc|"

type sqliteSemanticMutationScope struct {
	ownerID  string
	corpusID string
}

func (s *sqliteSemanticMutationScope) validate() error {
	if strings.TrimSpace(s.ownerID) == "" || strings.TrimSpace(s.corpusID) == "" {
		return ErrSemanticIndexNotFound
	}
	return nil
}

func (s *sqliteSemanticMutationScope) documentAddedTx(
	ctx context.Context,
	tx *sql.Tx,
	document *Document,
	chunks []*Chunk,
) error {
	if err := s.validate(); err != nil {
		return err
	}
	if document == nil || strings.TrimSpace(document.ID) == "" {
		return fmt.Errorf("knowledge: semantic document id is required")
	}
	state, err := loadSemanticPolicyState(ctx, tx, s.ownerID, s.corpusID)
	if err != nil {
		return err
	}
	now := semanticNowMillis()
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_semantic_document_bindings
		(document_id,owner_id,corpus_uid,content_generation,lifecycle_state,text_state,
		 deleted_at,version,created_at,updated_at)
		VALUES(?,?,?,1,'active','ready',NULL,1,?,?)`, document.ID, s.ownerID,
		state.corpusUID, now, now); err != nil {
		return fmt.Errorf("knowledge: bind added document: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_semantic_document_generations
		(owner_id,corpus_uid,document_id,content_generation,created_at)
		VALUES(?,?,?,1,?)`, s.ownerID, state.corpusUID, document.ID, now); err != nil {
		return fmt.Errorf("knowledge: persist added document generation: %w", err)
	}
	state.contentVersion++
	res, err := tx.ExecContext(ctx, `UPDATE kb_semantic_corpora
		SET content_version=?,updated_at=? WHERE corpus_uid=? AND content_version=?`,
		state.contentVersion, now, state.corpusUID, state.contentVersion-1)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return ErrPolicyVersionConflict
	}
	targetRevision, targetIsDesired, err := semanticMutationTargetRevisionTx(ctx, tx, state)
	if err != nil {
		return err
	}
	if targetRevision == "" {
		return nil
	}
	if err := insertRevisionDocumentTx(ctx, tx, state.corpusUID, targetRevision, document.ID, 1, int64(len(chunks)), now); err != nil {
		return err
	}
	if err := refreshActiveRevisionAggregatesTx(ctx, tx, state.corpusUID, state.activeRevision, now); err != nil {
		return err
	}
	if targetIsDesired {
		if err := requeueStagedRevisionTx(ctx, tx, state.corpusUID, targetRevision, now); err != nil {
			return err
		}
		return advanceActiveRevisionWatermarkIfCompleteTx(
			ctx, tx, state.corpusUID, state.activeRevision, state.contentVersion, now,
		)
	}
	if len(chunks) == 0 {
		if err := markEmptyRevisionDocumentReadyTx(ctx, tx, state.corpusUID, targetRevision, document.ID, 1, now); err != nil {
			return err
		}
		return advanceActiveRevisionWatermarkIfCompleteTx(
			ctx, tx, state.corpusUID, state.activeRevision, state.contentVersion, now,
		)
	}
	return createEmbedDocumentJobTx(ctx, tx, s.ownerID, state, targetRevision, document.ID, 1, int64(len(chunks)), now)
}

func (s *sqliteSemanticMutationScope) documentReplacedTx(
	ctx context.Context,
	tx *sql.Tx,
	document *Document,
	chunks []*Chunk,
) error {
	if err := s.validate(); err != nil {
		return err
	}
	state, err := loadSemanticPolicyState(ctx, tx, s.ownerID, s.corpusID)
	if err != nil {
		return err
	}
	var previousGeneration int64
	err = tx.QueryRowContext(ctx, `SELECT content_generation
		FROM kb_semantic_document_bindings
		WHERE owner_id=? AND corpus_uid=? AND document_id=?
		  AND lifecycle_state IN ('active','tombstoned')`,
		s.ownerID, state.corpusUID, document.ID).Scan(&previousGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSemanticIndexNotFound
	}
	if err != nil {
		return err
	}
	now := semanticNowMillis()
	if err := retireDocumentGCTx(ctx, tx, s.ownerID, state.corpusUID, document.ID); err != nil {
		return err
	}
	if err := fenceDocumentJobsTx(ctx, tx, state.corpusUID, document.ID, now); err != nil {
		return err
	}
	generation := previousGeneration + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_semantic_document_generations
		(owner_id,corpus_uid,document_id,content_generation,created_at)
		VALUES(?,?,?,?,?)`, s.ownerID, state.corpusUID, document.ID, generation, now); err != nil {
		return fmt.Errorf("knowledge: persist replacement generation: %w", err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE kb_semantic_document_bindings
		SET content_generation=?,lifecycle_state='active',text_state='ready',deleted_at=NULL,
		 version=version+1,updated_at=?
		WHERE owner_id=? AND corpus_uid=? AND document_id=? AND content_generation=?`,
		generation, now, s.ownerID, state.corpusUID, document.ID, previousGeneration)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return ErrJobFenced
	}
	if err := bumpSemanticContentVersionTx(ctx, tx, &state, now); err != nil {
		return err
	}
	targetRevision, targetIsDesired, err := semanticMutationTargetRevisionTx(ctx, tx, state)
	if err != nil {
		return err
	}
	if targetRevision == "" {
		return nil
	}
	if err := insertRevisionDocumentTx(ctx, tx, state.corpusUID, targetRevision,
		document.ID, generation, int64(len(chunks)), now); err != nil {
		return err
	}
	if err := refreshActiveRevisionAggregatesTx(ctx, tx, state.corpusUID, state.activeRevision, now); err != nil {
		return err
	}
	if targetIsDesired {
		if err := requeueStagedRevisionTx(ctx, tx, state.corpusUID, targetRevision, now); err != nil {
			return err
		}
		return advanceActiveRevisionWatermarkIfCompleteTx(
			ctx, tx, state.corpusUID, state.activeRevision, state.contentVersion, now,
		)
	}
	if len(chunks) == 0 {
		if err := markEmptyRevisionDocumentReadyTx(ctx, tx, state.corpusUID, targetRevision,
			document.ID, generation, now); err != nil {
			return err
		}
		return advanceActiveRevisionWatermarkIfCompleteTx(
			ctx, tx, state.corpusUID, state.activeRevision, state.contentVersion, now,
		)
	}
	return createEmbedDocumentJobTx(ctx, tx, s.ownerID, state, targetRevision,
		document.ID, generation, int64(len(chunks)), now)
}

func (s *sqliteSemanticMutationScope) documentDeletedTx(
	ctx context.Context,
	tx *sql.Tx,
	documentID string,
) error {
	if err := s.validate(); err != nil {
		return err
	}
	state, err := loadSemanticPolicyState(ctx, tx, s.ownerID, s.corpusID)
	if err != nil {
		return err
	}
	now := semanticNowMillis()
	var previousGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT content_generation
		FROM kb_semantic_document_bindings
		WHERE owner_id=? AND corpus_uid=? AND document_id=? AND lifecycle_state='active'`,
		s.ownerID, state.corpusUID, documentID).Scan(&previousGeneration); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			var deleted int
			var lifecycle sql.NullString
			rootErr := tx.QueryRowContext(ctx, `SELECT d.deleted,b.lifecycle_state
				FROM kb_documents d LEFT JOIN kb_semantic_document_bindings b
				  ON b.document_id=d.id AND b.owner_id=? AND b.corpus_uid=d.corpus_uid
				WHERE d.id=? AND d.corpus_uid=?`,
				s.ownerID, documentID, state.corpusUID).Scan(&deleted, &lifecycle)
			if errors.Is(rootErr, sql.ErrNoRows) {
				var exists int
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
					SELECT 1 FROM kb_documents WHERE id=?
				)`, documentID).Scan(&exists); err != nil {
					return err
				}
				if exists != 0 {
					return ErrSemanticIndexNotFound
				}
				return nil
			}
			if rootErr != nil {
				return rootErr
			}
			if lifecycle.Valid {
				if deleted != 1 || lifecycle.String != "tombstoned" {
					return ErrSemanticIndexNotFound
				}
				return queueDocumentGCTx(ctx, tx, s.ownerID, state.corpusUID, documentID, now)
			}
			if deleted != 0 && deleted != 1 {
				return ErrSemanticIndexNotFound
			}
			if deleted == 0 {
				res, updateErr := tx.ExecContext(ctx, `UPDATE kb_documents SET deleted=1,updated_at=?
					WHERE id=? AND corpus_uid=? AND deleted=0`,
					time.UnixMilli(now).UTC(), documentID, state.corpusUID)
				if updateErr != nil {
					return updateErr
				}
				if affected, _ := res.RowsAffected(); affected != 1 {
					return ErrSemanticIndexNotFound
				}
			}
			if err := fenceDocumentJobsTx(ctx, tx, state.corpusUID, documentID, now); err != nil {
				return err
			}
			return queueDocumentGCTx(ctx, tx, s.ownerID, state.corpusUID, documentID, now)
		}
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE kb_documents SET deleted=1,updated_at=?
		WHERE id=? AND corpus_uid=? AND deleted=0`, time.UnixMilli(now).UTC(), documentID, state.corpusUID)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return ErrSemanticIndexNotFound
	}
	tombstoneGeneration := previousGeneration + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_semantic_document_generations
		(owner_id,corpus_uid,document_id,content_generation,created_at)
		VALUES(?,?,?,?,?)`, s.ownerID, state.corpusUID, documentID, tombstoneGeneration, now); err != nil {
		return fmt.Errorf("knowledge: persist deletion generation: %w", err)
	}
	res, err = tx.ExecContext(ctx, `UPDATE kb_semantic_document_bindings
		SET content_generation=?,lifecycle_state='tombstoned',deleted_at=?,version=version+1,updated_at=?
		WHERE owner_id=? AND corpus_uid=? AND document_id=? AND content_generation=?
		  AND lifecycle_state='active'`, tombstoneGeneration,
		now, now, s.ownerID, state.corpusUID, documentID, previousGeneration)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return ErrSemanticIndexNotFound
	}
	if err := fenceDocumentJobsTx(ctx, tx, state.corpusUID, documentID, now); err != nil {
		return err
	}
	if err := bumpSemanticContentVersionTx(ctx, tx, &state, now); err != nil {
		return err
	}
	if err := refreshActiveRevisionAggregatesTx(ctx, tx, state.corpusUID, state.activeRevision, now); err != nil {
		return err
	}
	_, desiredIsHealthy, err := semanticMutationTargetRevisionTx(ctx, tx, state)
	if err != nil {
		return err
	}
	if desiredIsHealthy {
		if err := requeueStagedRevisionTx(ctx, tx, state.corpusUID, state.desiredRevision, now); err != nil {
			return err
		}
	}
	if err := advanceActiveRevisionWatermarkIfCompleteTx(
		ctx, tx, state.corpusUID, state.activeRevision, state.contentVersion, now,
	); err != nil {
		return err
	}
	return queueDocumentGCTx(ctx, tx, s.ownerID, state.corpusUID, documentID, now)
}

func retireDocumentGCTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerID, corpusUID, documentID string,
) error {
	idempotencyKey := documentGCIdempotencyPrefix + hex.EncodeToString([]byte(documentID))
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_knowledge_jobs
		WHERE owner_id=? AND corpus_uid=? AND kind='gc' AND idempotency_key=?`,
		ownerID, corpusUID, idempotencyKey); err != nil {
		return fmt.Errorf("knowledge: retire obsolete document GC: %w", err)
	}
	return nil
}

func queueDocumentGCTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerID, corpusUID, documentID string,
	now int64,
) error {
	jobID, err := semanticID("job")
	if err != nil {
		return err
	}
	idempotencyKey := documentGCIdempotencyPrefix + hex.EncodeToString([]byte(documentID))
	_, err = tx.ExecContext(ctx, `INSERT INTO kb_knowledge_jobs
		(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,document_generation,
		 target_revision_id,idempotency_key,state,stage,attempt,cancel_requested,lease_owner,
		 lease_epoch,last_error,created_at,updated_at)
		VALUES(?,NULL,'gc',?,?,NULL,NULL,NULL,?,'queued','gc',0,0,'',0,'',?,?)
		ON CONFLICT(owner_id,corpus_uid,kind,idempotency_key) DO NOTHING`,
		jobID, ownerID, corpusUID, idempotencyKey, now, now)
	if err != nil {
		return fmt.Errorf("knowledge: queue document GC: %w", err)
	}
	return nil
}

// semanticMutationTargetRevisionTx keeps document CRUD text-first when a
// desired rebuild is terminal. A desired pointer is intentionally retained for
// failed-job audit and explicit user disposition, but only a live staged root
// job is a writable revision target. Otherwise mutations incrementally extend
// the old active revision, or remain text-only when there is no active one.
func semanticMutationTargetRevisionTx(
	ctx context.Context,
	tx *sql.Tx,
	state semanticPolicyState,
) (revisionID string, desired bool, err error) {
	if state.desiredRevision != "" {
		var healthy int
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM kb_index_revisions r
			JOIN kb_knowledge_jobs j
			  ON j.corpus_uid=r.corpus_uid AND j.target_revision_id=r.revision_id
			WHERE r.corpus_uid=? AND r.revision_id=? AND r.publish_state='staged'
			  AND j.kind='rebuild_revision' AND j.parent_job_id IS NULL
			  AND j.cancel_requested=0 AND j.state IN ('queued','running','retry_wait')
		)`, state.corpusUID, state.desiredRevision).Scan(&healthy)
		if err != nil {
			return "", false, err
		}
		if healthy != 0 {
			return state.desiredRevision, true, nil
		}
	}
	return state.activeRevision, false, nil
}

func fenceDocumentJobsTx(
	ctx context.Context,
	tx *sql.Tx,
	corpusUID, documentID string,
	now int64,
) error {
	_, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs
		SET state='cancelled',cancel_requested=1,next_attempt_at=NULL,
		 lease_owner='',lease_epoch=lease_epoch+1,lease_expires_at=NULL,
		 heartbeat_at=NULL,updated_at=?,finished_at=?
		WHERE corpus_uid=? AND document_id=? AND state IN ('queued','running','retry_wait')`,
		now, now, corpusUID, documentID)
	return err
}

func bumpSemanticContentVersionTx(
	ctx context.Context,
	tx *sql.Tx,
	state *semanticPolicyState,
	now int64,
) error {
	previous := state.contentVersion
	state.contentVersion++
	res, err := tx.ExecContext(ctx, `UPDATE kb_semantic_corpora
		SET content_version=?,updated_at=? WHERE corpus_uid=? AND content_version=?`,
		state.contentVersion, now, state.corpusUID, previous)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return ErrJobFenced
	}
	return nil
}

func insertRevisionDocumentTx(
	ctx context.Context,
	tx *sql.Tx,
	corpusUID, revisionID, documentID string,
	generation, expectedChunks int64,
	now int64,
) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO kb_revision_documents
		(revision_id,corpus_uid,document_id,content_generation,vector_state,
		 expected_chunks,embedded_chunks,failed_chunks,visible_at,last_error,updated_at)
		VALUES(?,?,?,?, 'pending',?,0,0,NULL,'',?)`, revisionID, corpusUID,
		documentID, generation, expectedChunks, now)
	if err != nil {
		return fmt.Errorf("knowledge: create revision document: %w", err)
	}
	return nil
}

func markEmptyRevisionDocumentReadyTx(
	ctx context.Context,
	tx *sql.Tx,
	corpusUID, revisionID, documentID string,
	generation, now int64,
) error {
	_, err := tx.ExecContext(ctx, `UPDATE kb_revision_documents
		SET vector_state='ready',visible_at=?,updated_at=?
		WHERE corpus_uid=? AND revision_id=? AND document_id=? AND content_generation=?
		  AND expected_chunks=0`, now, now, corpusUID, revisionID, documentID, generation)
	return err
}

// refreshActiveRevisionAggregatesTx derives active revision counters from the
// current binding set instead of accumulating historical generations. Vector
// history remains immutable, while API progress always describes what readers
// can actually see now.
func refreshActiveRevisionAggregatesTx(
	ctx context.Context,
	tx *sql.Tx,
	corpusUID, revisionID string,
	now int64,
) error {
	if revisionID == "" {
		return nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE kb_index_revisions AS r SET
		expected_chunks=COALESCE((
		  SELECT SUM(rd.expected_chunks) FROM kb_revision_documents rd
		  JOIN kb_semantic_document_bindings b
		    ON b.corpus_uid=rd.corpus_uid AND b.document_id=rd.document_id
		   AND b.content_generation=rd.content_generation
		  WHERE rd.corpus_uid=r.corpus_uid AND rd.revision_id=r.revision_id
		    AND b.lifecycle_state='active' AND b.text_state='ready'
		),0),
		embedded_chunks=COALESCE((
		  SELECT SUM(rd.embedded_chunks) FROM kb_revision_documents rd
		  JOIN kb_semantic_document_bindings b
		    ON b.corpus_uid=rd.corpus_uid AND b.document_id=rd.document_id
		   AND b.content_generation=rd.content_generation
		  WHERE rd.corpus_uid=r.corpus_uid AND rd.revision_id=r.revision_id
		    AND b.lifecycle_state='active' AND b.text_state='ready'
		),0),
		failed_chunks=COALESCE((
		  SELECT SUM(rd.failed_chunks) FROM kb_revision_documents rd
		  JOIN kb_semantic_document_bindings b
		    ON b.corpus_uid=rd.corpus_uid AND b.document_id=rd.document_id
		   AND b.content_generation=rd.content_generation
		  WHERE rd.corpus_uid=r.corpus_uid AND rd.revision_id=r.revision_id
		    AND b.lifecycle_state='active' AND b.text_state='ready'
		),0),updated_at=?
		WHERE r.corpus_uid=? AND r.revision_id=? AND r.publish_state='active'`,
		now, corpusUID, revisionID)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return ErrJobFenced
	}
	return nil
}

func createEmbedDocumentJobTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerID string,
	state semanticPolicyState,
	revisionID, documentID string,
	generation, chunksTotal, now int64,
) error {
	return createEmbedDocumentJobWithParentTx(ctx, tx, ownerID, state, revisionID,
		documentID, generation, chunksTotal, "", now)
}

func createEmbedDocumentJobWithParentTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerID string,
	state semanticPolicyState,
	revisionID, documentID string,
	generation, chunksTotal int64,
	parentJobID string,
	now int64,
) error {
	jobID, err := semanticID("job")
	if err != nil {
		return err
	}
	idempotencyKey := fmt.Sprintf("embed_document|%s|%d|%s|%d",
		documentID, generation, revisionID, state.contentVersion)
	var parent any
	if strings.TrimSpace(parentJobID) != "" {
		parent = parentJobID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO kb_knowledge_jobs
		(job_id,parent_job_id,kind,owner_id,corpus_uid,document_id,document_generation,
		 target_revision_id,idempotency_key,state,stage,chunks_done,chunks_total,attempt,
		 cancel_requested,lease_owner,lease_epoch,last_error,created_at,updated_at)
		VALUES(?,?,'embed_document',?,?,?,?,?,?,'queued','embedding',0,?,0,0,'',0,'',?,?)`,
		jobID, parent, ownerID, state.corpusUID, documentID, generation, revisionID, idempotencyKey,
		chunksTotal, now, now)
	if err != nil {
		return fmt.Errorf("knowledge: queue document embedding: %w", err)
	}
	return nil
}

func requeueStagedRevisionTx(
	ctx context.Context,
	tx *sql.Tx,
	corpusUID, revisionID string,
	now int64,
) error {
	// A staged revision may already contain committed batches for a document
	// generation that was replaced or deleted while its worker was in flight.
	// Those rows are not part of the new corpus snapshot and must be removed
	// before progress is resumed, otherwise aggregate progress can exceed the
	// current chunk set and a succeeded manifest can shadow new work.
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_revision_vectors
		WHERE corpus_uid=? AND revision_id=? AND NOT EXISTS (
		  SELECT 1 FROM kb_semantic_document_bindings b
		  WHERE b.corpus_uid=kb_revision_vectors.corpus_uid
		    AND b.document_id=kb_revision_vectors.document_id
		    AND b.content_generation=kb_revision_vectors.content_generation
		    AND b.lifecycle_state='active' AND b.text_state='ready'
		)`, corpusUID, revisionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_revision_documents
		WHERE corpus_uid=? AND revision_id=? AND NOT EXISTS (
		  SELECT 1 FROM kb_semantic_document_bindings b
		  WHERE b.corpus_uid=kb_revision_documents.corpus_uid
		    AND b.document_id=kb_revision_documents.document_id
		    AND b.content_generation=kb_revision_documents.content_generation
		    AND b.lifecycle_state='active' AND b.text_state='ready'
		)`, corpusUID, revisionID); err != nil {
		return err
	}
	var embedded, failed, expected int64
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(rd.embedded_chunks),0),COALESCE(SUM(rd.failed_chunks),0),
		COALESCE(SUM(rd.expected_chunks),0)
		FROM kb_revision_documents rd
		JOIN kb_semantic_document_bindings b
		  ON b.corpus_uid=rd.corpus_uid AND b.document_id=rd.document_id
		 AND b.content_generation=rd.content_generation
		WHERE rd.corpus_uid=? AND rd.revision_id=?
		  AND b.lifecycle_state='active' AND b.text_state='ready'`, corpusUID, revisionID).Scan(
		&embedded, &failed, &expected,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM kb_job_stage_checkpoints
		WHERE job_id IN (
		  SELECT job_id FROM kb_knowledge_jobs
		  WHERE corpus_uid=? AND target_revision_id=? AND kind='rebuild_revision'
		)`, corpusUID, revisionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs
		SET state='queued',next_attempt_at=NULL,cancel_requested=0,lease_owner='',
		 lease_epoch=lease_epoch+1,lease_expires_at=NULL,heartbeat_at=NULL,
		 chunks_done=?,chunks_total=?,last_error='',updated_at=?,finished_at=NULL
		WHERE corpus_uid=? AND target_revision_id=? AND kind='rebuild_revision'
		  AND state IN ('queued','running','retry_wait')`, embedded, expected, now, corpusUID, revisionID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE kb_index_revisions
		SET lease_epoch=lease_epoch+1,expected_chunks=NULL,embedded_chunks=?,failed_chunks=?,
		 indexed_through_version=0,chunk_set_digest='',updated_at=?
		WHERE corpus_uid=? AND revision_id=? AND publish_state='staged'`,
		embedded, failed, now, corpusUID, revisionID)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return ErrJobFenced
	}
	return nil
}

// advanceActiveRevisionWatermarkIfCompleteTx advances the corpus-wide
// indexed-through watermark only when every current, active document
// generation is ready in that active revision. Individual document jobs may
// finish out of order; the first one must never claim later corpus mutations.
func advanceActiveRevisionWatermarkIfCompleteTx(
	ctx context.Context,
	tx *sql.Tx,
	corpusUID, revisionID string,
	contentVersion, now int64,
) error {
	if revisionID == "" {
		return nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE kb_index_revisions
		SET indexed_through_version=CASE WHEN NOT EXISTS (
		  SELECT 1 FROM kb_semantic_document_bindings b
		  LEFT JOIN kb_revision_documents rd
		    ON rd.corpus_uid=b.corpus_uid AND rd.revision_id=?
		   AND rd.document_id=b.document_id
		   AND rd.content_generation=b.content_generation
		  WHERE b.corpus_uid=? AND b.lifecycle_state='active' AND (
		    b.text_state<>'ready' OR rd.document_id IS NULL OR
		    rd.vector_state<>'ready' OR rd.visible_at IS NULL OR
		    rd.expected_chunks IS NULL OR rd.embedded_chunks<>rd.expected_chunks OR
		    rd.failed_chunks<>0
		  )
		) THEN ? ELSE indexed_through_version END,
		updated_at=?
		WHERE corpus_uid=? AND revision_id=? AND publish_state='active'
		  AND EXISTS (
		    SELECT 1 FROM kb_semantic_corpora c
		    WHERE c.corpus_uid=? AND c.active_revision_id=? AND c.content_version=?
		  )`, revisionID, corpusUID, contentVersion, now, corpusUID, revisionID,
		corpusUID, revisionID, contentVersion)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return ErrJobFenced
	}
	return nil
}

// reconcileActiveRevisionTx creates incremental jobs for current generations
// that were written only into a desired revision. It is used when the user
// cancels that desired rebuild and the previous active revision resumes being
// the sole semantic read path.
func reconcileActiveRevisionTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerID string,
	state semanticPolicyState,
	now int64,
) error {
	if state.activeRevision == "" {
		return nil
	}
	if strings.TrimSpace(ownerID) == "" {
		if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM kb_semantic_corpora
			WHERE corpus_uid=?`, state.corpusUID).Scan(&ownerID); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.document_id,b.content_generation,COUNT(c.id)
		FROM kb_semantic_document_bindings b
		LEFT JOIN kb_chunks c ON c.doc_id=b.document_id
		LEFT JOIN kb_revision_documents rd
		  ON rd.corpus_uid=b.corpus_uid AND rd.revision_id=?
		 AND rd.document_id=b.document_id AND rd.content_generation=b.content_generation
		WHERE b.corpus_uid=? AND b.lifecycle_state='active' AND b.text_state='ready'
		  AND rd.document_id IS NULL
		GROUP BY b.document_id,b.content_generation
		ORDER BY b.document_id`, state.activeRevision, state.corpusUID)
	if err != nil {
		return err
	}
	type missingDocument struct {
		id         string
		generation int64
		chunks     int64
	}
	missing := make([]missingDocument, 0)
	for rows.Next() {
		var document missingDocument
		if err := rows.Scan(&document.id, &document.generation, &document.chunks); err != nil {
			rows.Close()
			return err
		}
		missing = append(missing, document)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, document := range missing {
		if err := insertRevisionDocumentTx(ctx, tx, state.corpusUID, state.activeRevision,
			document.id, document.generation, document.chunks, now); err != nil {
			return err
		}
		if document.chunks == 0 {
			if err := markEmptyRevisionDocumentReadyTx(ctx, tx, state.corpusUID,
				state.activeRevision, document.id, document.generation, now); err != nil {
				return err
			}
			continue
		}
		if err := createEmbedDocumentJobTx(ctx, tx, ownerID, state, state.activeRevision,
			document.id, document.generation, document.chunks, now); err != nil {
			return err
		}
	}
	if err := refreshActiveRevisionAggregatesTx(ctx, tx, state.corpusUID, state.activeRevision, now); err != nil {
		return err
	}
	return advanceActiveRevisionWatermarkIfCompleteTx(
		ctx, tx, state.corpusUID, state.activeRevision, state.contentVersion, now,
	)
}
