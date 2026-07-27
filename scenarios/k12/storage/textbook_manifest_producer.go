package k12storage

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
)

const (
	textbookManifestSubjectMath            = "math"
	textbookDefaultModelMissingReason      = "默认模型未配置"
	textbookVisionModelRequiredFailureCode = "vision_model_required"
)

type textbookManifestDocumentRef struct {
	knowledgeOwnerID string
	documentID       string
	generation       int64
}

type textbookManifestFacts struct {
	knowledgeOwnerID string
	manifestOwnerID  string
	corpusUID        string
	documentID       string
	generation       int64
	title            string
	source           string
	documentStatus   string
	lifecycleState   string
	textState        string
	extension        string
	mediaType        string
	sourceDigest     string
	jobID            string
	jobState         string
	lastError        string
	failureCode      string
	actionCode       string
	deleted          bool
}

type textbookManifestSegment struct {
	ref          string
	pageStart    int
	pageEnd      int
	sourceDigest string
}

// ReconcileTextbookManifestLifecycle produces only a manifest candidate inside
// the caller-owned transaction. A TextbookBinding remains exclusively owned by
// profile-bundle confirmation.
func (s *Store) ReconcileTextbookManifestLifecycle(
	ctx context.Context,
	tx *sql.Tx,
	event TextbookManifestLifecycleEvent,
) error {
	if s == nil || tx == nil || strings.TrimSpace(event.OwnerID) == "" ||
		strings.TrimSpace(event.DocumentID) == "" || event.DocumentGeneration < 1 {
		return fmt.Errorf("k12storage: invalid textbook manifest lifecycle event")
	}
	at := event.At.UTC().UnixMilli()
	if at <= 0 {
		at = time.Now().UTC().UnixMilli()
	}
	return reconcileTextbookManifestDocumentTx(
		ctx,
		tx,
		event.OwnerID,
		event.DocumentID,
		event.DocumentGeneration,
		at,
	)
}

// reconcileTextbookManifestCandidates is the restart/missed-event recovery
// path. It consumes the same producer as the transactional Knowledge hook.
func reconcileTextbookManifestCandidates(
	ctx context.Context,
	db *sql.DB,
	ownerID, subject string,
	at int64,
) error {
	if db == nil || strings.TrimSpace(ownerID) == "" ||
		subject != textbookManifestSubjectMath {
		return fmt.Errorf("k12storage: invalid textbook manifest candidate scope")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT b.owner_id,b.document_id,b.content_generation
		FROM kb_semantic_document_bindings b
		JOIN kb_documents d ON d.id=b.document_id
		LEFT JOIN kb_ingest_document_sources s
		  ON s.document_id=b.document_id
		 AND s.content_generation=b.content_generation
		WHERE (b.owner_id=? OR s.agent_id=?)
		  AND (
		    lower(COALESCE(s.extension,''))='.pdf'
		    OR lower(COALESCE(s.media_type,''))='application/pdf'
		    OR lower(d.title) LIKE '%.pdf'
		    OR lower(d.source) LIKE '%.pdf'
		    OR EXISTS(
		      SELECT 1 FROM k12_textbook_manifests m
		      WHERE m.document_id=b.document_id
		        AND m.document_generation=b.content_generation
		        AND m.owner_id=?
		    )
		  )
		ORDER BY b.document_id,b.content_generation`,
		ownerID, ownerID, ownerID)
	if err != nil {
		return fmt.Errorf("k12storage: list textbook manifest candidates: %w", err)
	}
	refs := make([]textbookManifestDocumentRef, 0)
	for rows.Next() {
		var ref textbookManifestDocumentRef
		if err := rows.Scan(
			&ref.knowledgeOwnerID,
			&ref.documentID,
			&ref.generation,
		); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, ref)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, ref := range refs {
		if err := reconcileTextbookManifestDocumentTx(
			ctx,
			tx,
			ref.knowledgeOwnerID,
			ref.documentID,
			ref.generation,
			at,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func reconcileTextbookManifestDocumentTx(
	ctx context.Context,
	tx *sql.Tx,
	knowledgeOwnerID, documentID string,
	generation, at int64,
) error {
	facts, found, err := loadTextbookManifestFactsTx(
		ctx,
		tx,
		knowledgeOwnerID,
		documentID,
		generation,
	)
	if err != nil || !found {
		return err
	}
	if !isTextbookPDF(facts) {
		return nil
	}
	existingID, existingDigest, catalog, found, err :=
		loadExistingTextbookManifestTx(
			ctx,
			tx,
			facts.manifestOwnerID,
			documentID,
			generation,
		)
	if err != nil {
		return err
	}
	hasIngestLifecycleFacts := facts.jobID != "" ||
		facts.extension != "" ||
		facts.mediaType != "" ||
		facts.sourceDigest != ""
	if facts.sourceDigest == "" {
		facts.sourceDigest, err = loadTextbookChunkDigestTx(
			ctx,
			tx,
			documentID,
		)
		if err != nil {
			return err
		}
	}
	if facts.sourceDigest == "" {
		return nil
	}
	if found && existingDigest != facts.sourceDigest {
		return fmt.Errorf(
			"k12storage: textbook manifest source digest drift for %s generation %d",
			documentID,
			generation,
		)
	}
	if found && !hasIngestLifecycleFacts &&
		facts.lifecycleState == "active" && !facts.deleted {
		return nil
	}
	manifestID := existingID
	if manifestID == "" {
		manifestID = textbookManifestID(
			facts.manifestOwnerID,
			documentID,
			generation,
		)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_textbook_manifests
		(manifest_id,owner_id,document_id,document_generation,document_title,subject,
		 source_digest,state,retryable,failure_message,text_index_state,vector_index_state,
		 catalog_json,catalog_digest,created_at,updated_at)
		VALUES(?,?,?,?,?,'math',?,'waiting_ingest',0,'','pending','pending',NULL,NULL,?,?)
		ON CONFLICT(owner_id,document_id,document_generation,subject) DO NOTHING`,
		manifestID,
		facts.manifestOwnerID,
		documentID,
		generation,
		facts.title,
		facts.sourceDigest,
		at,
		at,
	); err != nil {
		return fmt.Errorf("k12storage: create textbook manifest: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_manifests
		SET state='stale',retryable=0,failure_message='',
		    text_index_state='stale',vector_index_state='stale',updated_at=?
		WHERE owner_id=? AND document_id=? AND subject='math'
		  AND document_generation<>? AND state<>'stale'`,
		at,
		facts.manifestOwnerID,
		documentID,
		generation,
	); err != nil {
		return fmt.Errorf("k12storage: stale replaced textbook manifests: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_bindings
		SET status='invalidated',updated_at=?
		WHERE owner_id=? AND document_id=? AND status='active'
		  AND document_generation<>?`,
		at,
		facts.manifestOwnerID,
		documentID,
		generation,
	); err != nil {
		return fmt.Errorf("k12storage: invalidate replaced textbook bindings: %w", err)
	}

	segments := make([]textbookManifestSegment, 0)
	if facts.lifecycleState == "active" && !facts.deleted &&
		facts.textState == "ready" {
		segments, err = loadTextbookManifestSegmentsTx(
			ctx,
			tx,
			documentID,
			facts.sourceDigest,
		)
		if err != nil {
			return err
		}
		for _, segment := range segments {
			for page := segment.pageStart; page <= segment.pageEnd; page++ {
				segmentID := textbookManifestSegmentID(manifestID, page, segment.ref)
				if _, err := tx.ExecContext(ctx, `INSERT INTO k12_textbook_manifest_segments
					(segment_id,manifest_id,logical_page,segment_ref,pdf_page,document_id,
					 document_generation,source_digest,created_at,updated_at)
					VALUES(?,?,?,?,?,?,?,?,?,?)
					ON CONFLICT(manifest_id,logical_page,segment_ref) DO NOTHING`,
					segmentID,
					manifestID,
					page,
					segment.ref,
					page,
					documentID,
					generation,
					facts.sourceDigest,
					at,
					at,
				); err != nil {
					return fmt.Errorf("k12storage: create textbook manifest segment: %w", err)
				}
			}
		}
	}

	state, retryable, failureMessage :=
		projectTextbookManifestState(facts, catalog.Valid, len(segments))
	textState := projectTextbookTextIndexState(facts)
	vectorState, err := projectTextbookVectorIndexStateTx(
		ctx,
		tx,
		facts,
	)
	if err != nil {
		return err
	}
	if state == "stale" {
		textState, vectorState = "stale", "stale"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_manifests
		SET state=?,retryable=?,failure_message=?,text_index_state=?,
		    vector_index_state=?,updated_at=?
		WHERE manifest_id=?`,
		state,
		boolInt(retryable),
		failureMessage,
		textState,
		vectorState,
		at,
		manifestID,
	); err != nil {
		return fmt.Errorf("k12storage: advance textbook manifest: %w", err)
	}
	return nil
}

func loadTextbookManifestFactsTx(
	ctx context.Context,
	tx *sql.Tx,
	knowledgeOwnerID, documentID string,
	generation int64,
) (textbookManifestFacts, bool, error) {
	var facts textbookManifestFacts
	var deleted int
	err := tx.QueryRowContext(ctx, `SELECT b.owner_id,b.corpus_uid,b.lifecycle_state,b.text_state,
		d.title,d.source,d.status,d.deleted,COALESCE(s.agent_id,''),
		COALESCE(s.extension,''),COALESCE(s.media_type,''),COALESCE(s.blob_sha256,'')
		FROM kb_semantic_document_bindings b
		JOIN kb_documents d ON d.id=b.document_id
		LEFT JOIN kb_ingest_document_sources s
		  ON s.document_id=b.document_id
		 AND s.content_generation=b.content_generation
		WHERE b.owner_id=? AND b.document_id=? AND b.content_generation=?`,
		knowledgeOwnerID,
		documentID,
		generation,
	).Scan(
		&facts.knowledgeOwnerID,
		&facts.corpusUID,
		&facts.lifecycleState,
		&facts.textState,
		&facts.title,
		&facts.source,
		&facts.documentStatus,
		&deleted,
		&facts.manifestOwnerID,
		&facts.extension,
		&facts.mediaType,
		&facts.sourceDigest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return textbookManifestFacts{}, false, nil
	}
	if err != nil {
		return textbookManifestFacts{}, false, err
	}
	if strings.TrimSpace(facts.manifestOwnerID) == "" {
		facts.manifestOwnerID = facts.knowledgeOwnerID
	}
	facts.documentID = documentID
	facts.generation = generation
	facts.deleted = deleted != 0

	err = tx.QueryRowContext(ctx, `SELECT job_id,state,last_error
		FROM kb_knowledge_jobs
		WHERE owner_id=? AND corpus_uid=? AND document_id=?
		  AND document_generation=? AND kind='ingest'
		ORDER BY created_at DESC,job_id DESC LIMIT 1`,
		facts.knowledgeOwnerID,
		facts.corpusUID,
		documentID,
		generation,
	).Scan(&facts.jobID, &facts.jobState, &facts.lastError)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return textbookManifestFacts{}, false, err
	}
	if facts.jobID != "" {
		err = tx.QueryRowContext(ctx, `SELECT code,action_code
			FROM kb_job_failures WHERE job_id=?`, facts.jobID).Scan(
			&facts.failureCode,
			&facts.actionCode,
		)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return textbookManifestFacts{}, false, err
		}
	}
	return facts, true, nil
}

func loadExistingTextbookManifestTx(
	ctx context.Context,
	tx *sql.Tx,
	ownerID, documentID string,
	generation int64,
) (
	manifestID string,
	sourceDigest string,
	catalog sql.NullString,
	found bool,
	err error,
) {
	err = tx.QueryRowContext(ctx, `SELECT manifest_id,source_digest,catalog_json
		FROM k12_textbook_manifests
		WHERE owner_id=? AND document_id=? AND document_generation=?
		  AND subject='math'`,
		ownerID,
		documentID,
		generation,
	).Scan(&manifestID, &sourceDigest, &catalog)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", sql.NullString{}, false, nil
	}
	return manifestID, sourceDigest, catalog, err == nil, err
}

func loadTextbookChunkDigestTx(
	ctx context.Context,
	tx *sql.Tx,
	documentID string,
) (string, error) {
	var minDigest, maxDigest sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT MIN(NULLIF(source_digest,'')),
		MAX(NULLIF(source_digest,'')) FROM kb_chunks WHERE doc_id=?`,
		documentID,
	).Scan(&minDigest, &maxDigest); err != nil {
		return "", err
	}
	if !minDigest.Valid || !maxDigest.Valid ||
		minDigest.String != maxDigest.String {
		return "", nil
	}
	return strings.TrimSpace(minDigest.String), nil
}

func loadTextbookManifestSegmentsTx(
	ctx context.Context,
	tx *sql.Tx,
	documentID, sourceDigest string,
) ([]textbookManifestSegment, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,COALESCE(page_start,0),
		COALESCE(page_end,0),source_digest
		FROM kb_chunks WHERE doc_id=? ORDER BY chunk_index,id`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	segments := make([]textbookManifestSegment, 0)
	for rows.Next() {
		var segment textbookManifestSegment
		if err := rows.Scan(
			&segment.ref,
			&segment.pageStart,
			&segment.pageEnd,
			&segment.sourceDigest,
		); err != nil {
			return nil, err
		}
		if segment.pageStart < 1 || segment.pageEnd < segment.pageStart ||
			segment.sourceDigest != sourceDigest {
			continue
		}
		segments = append(segments, segment)
	}
	return segments, rows.Err()
}

func projectTextbookManifestState(
	facts textbookManifestFacts,
	hasCatalog bool,
	segmentCount int,
) (state string, retryable bool, failureMessage string) {
	if facts.lifecycleState != "active" || facts.deleted {
		return "stale", false, ""
	}
	switch facts.textState {
	case "failed":
		if facts.failureCode == textbookVisionModelRequiredFailureCode ||
			facts.actionCode == "configure_default_vision_model" {
			return "failed_retryable", true, textbookDefaultModelMissingReason
		}
		message := strings.TrimSpace(facts.lastError)
		if message == "" {
			message = "教材识别失败"
		}
		return "failed_terminal", false, message
	case "ready":
		if hasCatalog && segmentCount > 0 {
			return "ready_for_confirmation", false, ""
		}
		return "extracting", false, ""
	case "building":
		return "extracting", false, ""
	default:
		if facts.jobState == "running" || facts.jobState == "retry_wait" {
			return "extracting", false, ""
		}
		return "waiting_ingest", false, ""
	}
}

func projectTextbookTextIndexState(
	facts textbookManifestFacts,
) string {
	switch facts.textState {
	case "ready", "failed":
		return facts.textState
	case "building":
		return "building"
	default:
		if facts.jobState == "running" {
			return "building"
		}
		return "pending"
	}
}

func projectTextbookVectorIndexStateTx(
	ctx context.Context,
	tx *sql.Tx,
	facts textbookManifestFacts,
) (string, error) {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT rd.vector_state
		FROM kb_semantic_corpora c
		JOIN kb_revision_documents rd
		  ON rd.corpus_uid=c.corpus_uid
		 AND rd.revision_id=c.active_revision_id
		WHERE c.corpus_uid=? AND rd.document_id=?
		  AND rd.content_generation=?`,
		facts.corpusUID,
		facts.documentID,
		facts.generation,
	).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "pending", nil
	}
	if err != nil {
		return "", err
	}
	switch state {
	case "ready", "failed", "pending", "building":
		return state, nil
	case "retry_wait":
		return "building", nil
	default:
		return "pending", nil
	}
}

func isTextbookPDF(facts textbookManifestFacts) bool {
	return strings.EqualFold(strings.TrimSpace(facts.extension), ".pdf") ||
		strings.EqualFold(strings.TrimSpace(facts.mediaType), "application/pdf") ||
		strings.HasSuffix(strings.ToLower(strings.TrimSpace(facts.title)), ".pdf") ||
		strings.HasSuffix(strings.ToLower(strings.TrimSpace(facts.source)), ".pdf")
}

func textbookManifestID(
	ownerID, documentID string,
	generation int64,
) string {
	return "tbm-" + textbookStableDigest(
		ownerID,
		documentID,
		fmt.Sprintf("%d", generation),
		textbookManifestSubjectMath,
	)[:32]
}

func textbookManifestSegmentID(
	manifestID string,
	page int,
	segmentRef string,
) string {
	return "tbms-" + textbookStableDigest(
		manifestID,
		fmt.Sprintf("%d", page),
		segmentRef,
	)[:32]
}

func textbookStableDigest(values ...string) string {
	payload, _ := json.Marshal(values)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
