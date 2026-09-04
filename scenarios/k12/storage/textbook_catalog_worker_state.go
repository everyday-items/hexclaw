package k12storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const TextbookCatalogExtractorContract = "checkpoint-toc-footer-v2"

var ErrTextbookCatalogSourceIncomplete = errors.New("textbook catalog source evidence incomplete")

type TextbookCatalogSourcePage struct {
	PDFPage          int
	Content          string
	ContentDigest    string
	SourceOffsetFrom int64
	SourceOffsetTo   int64
	SegmentRefs      []string
}

type TextbookCatalogSource struct {
	IngestJobID      string
	SourcePlanDigest string
	DocumentTitle    string
	SourceDigest     string
	Pages            []TextbookCatalogSourcePage
}

type TextbookCatalogFailure struct {
	Code     string
	Message  string
	Terminal bool
	RetryAt  time.Time
}

type textbookCatalogSnapshotPage struct {
	PDFPage          int    `json:"pdf_page"`
	ContentDigest    string `json:"content_digest"`
	SourceOffsetFrom int64  `json:"source_offset_start"`
	SourceOffsetTo   int64  `json:"source_offset_end"`
}

type textbookCatalogSnapshotChunk struct {
	ID               string `json:"id"`
	PageFrom         int    `json:"page_from"`
	PageTo           int    `json:"page_to"`
	SourceOffsetFrom int64  `json:"source_offset_start"`
	SourceOffsetTo   int64  `json:"source_offset_end"`
}

type textbookCatalogSourceSnapshot struct {
	IngestJobID  string                         `json:"ingest_job_id"`
	SourceDigest string                         `json:"source_digest"`
	PagesTotal   int                            `json:"pages_total"`
	SegmentPlan  string                         `json:"segment_plan_digest"`
	Pages        []textbookCatalogSnapshotPage  `json:"pages"`
	Chunks       []textbookCatalogSnapshotChunk `json:"chunks"`
}

func loadTextbookCatalogSourceSnapshot(
	ctx context.Context,
	q dbQueryer,
	jobID, ownerID, documentID string,
	generation int64,
	sourceDigest string,
) (textbookCatalogSourceSnapshot, string, error) {
	var jobState string
	var pagesTotal, pagesDone int
	var jobLeaseEpoch int64
	err := q.QueryRowContext(ctx, `SELECT j.state,COALESCE(j.pages_total,0),
		COALESCE(j.pages_done,0),j.lease_epoch
		FROM kb_knowledge_jobs j
		JOIN kb_ingest_document_sources s
		  ON s.owner_id=j.owner_id AND s.corpus_uid=j.corpus_uid
		 AND s.document_id=j.document_id
		 AND s.content_generation=j.document_generation
		WHERE j.job_id=? AND j.kind='ingest' AND j.owner_id=? AND j.document_id=?
		  AND j.document_generation=? AND s.blob_sha256=?`,
		jobID, ownerID, documentID, generation, sourceDigest,
	).Scan(&jobState, &pagesTotal, &pagesDone, &jobLeaseEpoch)
	if errors.Is(err, sql.ErrNoRows) || jobState != "succeeded" ||
		pagesTotal < 1 || pagesDone != pagesTotal {
		return textbookCatalogSourceSnapshot{}, "", ErrTextbookCatalogSourceIncomplete
	}
	if err != nil {
		return textbookCatalogSourceSnapshot{}, "", err
	}

	snapshot := textbookCatalogSourceSnapshot{
		IngestJobID: jobID, SourceDigest: sourceDigest, PagesTotal: pagesTotal,
		Pages: make([]textbookCatalogSnapshotPage, 0, pagesTotal),
	}
	rows, err := q.QueryContext(ctx, `SELECT page_number,content,content_digest,
		source_offset_start,source_offset_end,lease_epoch
		FROM kb_ingest_page_checkpoints
		WHERE job_id=? AND source_digest=? AND pages_total=? ORDER BY page_number`,
		jobID, sourceDigest, pagesTotal)
	if err != nil {
		return textbookCatalogSourceSnapshot{}, "", err
	}
	previousOffsetTo := int64(0)
	for rows.Next() {
		var page textbookCatalogSnapshotPage
		var content string
		var checkpointLeaseEpoch int64
		if err := rows.Scan(&page.PDFPage, &content, &page.ContentDigest,
			&page.SourceOffsetFrom, &page.SourceOffsetTo, &checkpointLeaseEpoch); err != nil {
			rows.Close()
			return textbookCatalogSourceSnapshot{}, "", err
		}
		if page.PDFPage != len(snapshot.Pages)+1 ||
			page.SourceOffsetFrom < 0 || page.SourceOffsetTo <= page.SourceOffsetFrom ||
			page.SourceOffsetFrom < previousOffsetTo || checkpointLeaseEpoch <= 0 ||
			checkpointLeaseEpoch > jobLeaseEpoch ||
			sha256Hex([]byte(content)) != page.ContentDigest {
			rows.Close()
			return textbookCatalogSourceSnapshot{}, "", ErrTextbookCatalogSourceIncomplete
		}
		snapshot.Pages = append(snapshot.Pages, page)
		previousOffsetTo = page.SourceOffsetTo
	}
	if err := rows.Close(); err != nil {
		return textbookCatalogSourceSnapshot{}, "", err
	}
	if len(snapshot.Pages) != pagesTotal {
		return textbookCatalogSourceSnapshot{}, "", ErrTextbookCatalogSourceIncomplete
	}

	rows, err = q.QueryContext(ctx, `SELECT ordinal,page_start,page_end,state,source_digest,plan_digest
		FROM kb_ingest_segments WHERE job_id=? ORDER BY ordinal`, jobID)
	if err != nil {
		return textbookCatalogSourceSnapshot{}, "", err
	}
	expectedOrdinal, expectedPage := 1, 1
	for rows.Next() {
		var ordinal, pageFrom, pageTo int
		var state, segmentSource, planDigest string
		if err := rows.Scan(&ordinal, &pageFrom, &pageTo, &state, &segmentSource, &planDigest); err != nil {
			rows.Close()
			return textbookCatalogSourceSnapshot{}, "", err
		}
		if ordinal != expectedOrdinal || pageFrom != expectedPage || pageTo < pageFrom ||
			state != "ready" || segmentSource != sourceDigest || !validSHA256Digest(planDigest) ||
			(snapshot.SegmentPlan != "" && snapshot.SegmentPlan != planDigest) {
			rows.Close()
			return textbookCatalogSourceSnapshot{}, "", ErrTextbookCatalogSourceIncomplete
		}
		snapshot.SegmentPlan = planDigest
		expectedOrdinal++
		expectedPage = pageTo + 1
	}
	if err := rows.Close(); err != nil {
		return textbookCatalogSourceSnapshot{}, "", err
	}
	if snapshot.SegmentPlan != "" && expectedPage != pagesTotal+1 {
		return textbookCatalogSourceSnapshot{}, "", ErrTextbookCatalogSourceIncomplete
	}

	rows, err = q.QueryContext(ctx, `SELECT id,page_start,page_end,
		source_offset_start,source_offset_end
		FROM kb_chunks WHERE doc_id=? AND source_digest=?
		ORDER BY chunk_index,id`, documentID, sourceDigest)
	if err != nil {
		return textbookCatalogSourceSnapshot{}, "", err
	}
	for rows.Next() {
		var chunk textbookCatalogSnapshotChunk
		var pageFrom, pageTo, offsetFrom, offsetTo sql.NullInt64
		if err := rows.Scan(&chunk.ID, &pageFrom, &pageTo, &offsetFrom, &offsetTo); err != nil {
			rows.Close()
			return textbookCatalogSourceSnapshot{}, "", err
		}
		if !pageFrom.Valid || !pageTo.Valid || !offsetFrom.Valid || !offsetTo.Valid ||
			pageFrom.Int64 < 1 || pageTo.Int64 < pageFrom.Int64 ||
			pageTo.Int64 > int64(pagesTotal) ||
			offsetFrom.Int64 < 0 || offsetTo.Int64 <= offsetFrom.Int64 {
			rows.Close()
			return textbookCatalogSourceSnapshot{}, "", ErrTextbookCatalogSourceIncomplete
		}
		chunk.PageFrom, chunk.PageTo = int(pageFrom.Int64), int(pageTo.Int64)
		chunk.SourceOffsetFrom, chunk.SourceOffsetTo = offsetFrom.Int64, offsetTo.Int64
		if chunk.SourceOffsetFrom < snapshot.Pages[chunk.PageFrom-1].SourceOffsetFrom ||
			chunk.SourceOffsetTo > snapshot.Pages[chunk.PageTo-1].SourceOffsetTo {
			rows.Close()
			return textbookCatalogSourceSnapshot{}, "", ErrTextbookCatalogSourceIncomplete
		}
		snapshot.Chunks = append(snapshot.Chunks, chunk)
	}
	if err := rows.Close(); err != nil {
		return textbookCatalogSourceSnapshot{}, "", err
	}
	if len(snapshot.Chunks) == 0 {
		return textbookCatalogSourceSnapshot{}, "", ErrTextbookCatalogSourceIncomplete
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return textbookCatalogSourceSnapshot{}, "", err
	}
	digest := sha256.Sum256(encoded)
	return snapshot, hex.EncodeToString(digest[:]), nil
}

func (s *Store) RenewTextbookCatalogJob(
	ctx context.Context,
	claim TextbookCatalogJobClaim,
	now time.Time,
	lease time.Duration,
) error {
	if s == nil || s.db == nil || lease <= 0 {
		return fmt.Errorf("k12storage: invalid textbook catalog lease renewal")
	}
	nowMilli := now.UTC().UnixMilli()
	expires := nowMilli + lease.Milliseconds()
	result, err := s.db.ExecContext(ctx, `UPDATE k12_textbook_catalog_jobs
		SET lease_expires_at=?,heartbeat_at=?,updated_at=?
		WHERE job_id=? AND manifest_id=? AND owner_id=? AND document_id=?
		  AND document_generation=? AND source_digest=? AND request_digest=?
		  AND state='running' AND lease_owner=? AND lease_epoch=?
		  AND lease_expires_at>?`,
		expires, nowMilli, nowMilli,
		claim.JobID, claim.ManifestID, claim.OwnerID, claim.DocumentID,
		claim.DocumentGeneration, claim.SourceDigest, claim.RequestDigest,
		claim.LeaseOwner, claim.LeaseEpoch, nowMilli,
	)
	if err != nil {
		return fmt.Errorf("k12storage: renew textbook catalog lease: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrTextbookCatalogJobFenced
	}
	return nil
}

func (s *Store) LoadTextbookCatalogSource(
	ctx context.Context,
	claim TextbookCatalogJobClaim,
	now time.Time,
) (TextbookCatalogSource, error) {
	if s == nil || s.db == nil {
		return TextbookCatalogSource{}, fmt.Errorf("k12storage: textbook catalog store unavailable")
	}
	nowMilli := now.UTC().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TextbookCatalogSource{}, err
	}
	defer tx.Rollback()
	var current textbookCatalogJobState
	var documentTitle, extractorContract string
	err = tx.QueryRowContext(ctx, `SELECT j.manifest_id,j.owner_id,j.document_id,
		j.document_generation,j.source_digest,j.request_digest,j.state,j.lease_owner,
		j.lease_epoch,j.lease_expires_at,j.result_digest,j.ingest_job_id,
		j.source_plan_digest,m.document_title,j.extractor_contract
		FROM k12_textbook_catalog_jobs j
		JOIN k12_textbook_manifests m ON m.manifest_id=j.manifest_id
		WHERE j.job_id=?`, claim.JobID).Scan(
		&current.manifestID, &current.ownerID, &current.documentID,
		&current.documentGeneration, &current.sourceDigest, &current.requestDigest,
		&current.state, &current.leaseOwner, &current.leaseEpoch,
		&current.leaseExpiresAt, &current.resultDigest, &current.ingestJobID,
		&current.sourcePlanDigest, &documentTitle, &extractorContract,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return TextbookCatalogSource{}, ErrTextbookCatalogJobFenced
	}
	if err != nil {
		return TextbookCatalogSource{}, err
	}
	if !current.matchesClaim(claim) || current.state != "running" ||
		current.leaseOwner != claim.LeaseOwner || current.leaseEpoch != claim.LeaseEpoch ||
		current.leaseExpiresAt <= nowMilli || current.ingestJobID == "" ||
		!validSHA256Digest(current.sourcePlanDigest) ||
		extractorContract != TextbookCatalogExtractorContract {
		return TextbookCatalogSource{}, ErrTextbookCatalogJobFenced
	}
	snapshot, digest, err := loadTextbookCatalogSourceSnapshot(
		ctx, tx, current.ingestJobID, current.ownerID, current.documentID,
		current.documentGeneration, current.sourceDigest,
	)
	if err != nil {
		return TextbookCatalogSource{}, err
	}
	if digest != current.sourcePlanDigest {
		return TextbookCatalogSource{}, ErrTextbookCatalogJobFenced
	}

	source := TextbookCatalogSource{
		IngestJobID: current.ingestJobID, SourcePlanDigest: digest,
		DocumentTitle: documentTitle, SourceDigest: current.sourceDigest,
		Pages: make([]TextbookCatalogSourcePage, 0, snapshot.PagesTotal),
	}
	rows, err := tx.QueryContext(ctx, `SELECT page_number,content,content_digest,
		source_offset_start,source_offset_end
		FROM kb_ingest_page_checkpoints WHERE job_id=? AND source_digest=?
		ORDER BY page_number`, current.ingestJobID, current.sourceDigest)
	if err != nil {
		return TextbookCatalogSource{}, err
	}
	for rows.Next() {
		var page TextbookCatalogSourcePage
		if err := rows.Scan(&page.PDFPage, &page.Content, &page.ContentDigest,
			&page.SourceOffsetFrom, &page.SourceOffsetTo); err != nil {
			rows.Close()
			return TextbookCatalogSource{}, err
		}
		for _, chunk := range snapshot.Chunks {
			if chunk.PageFrom <= page.PDFPage && chunk.PageTo >= page.PDFPage {
				page.SegmentRefs = append(page.SegmentRefs, chunk.ID)
			}
		}
		sort.Strings(page.SegmentRefs)
		source.Pages = append(source.Pages, page)
	}
	if err := rows.Close(); err != nil {
		return TextbookCatalogSource{}, err
	}
	if len(source.Pages) != snapshot.PagesTotal {
		return TextbookCatalogSource{}, ErrTextbookCatalogSourceIncomplete
	}
	if err := tx.Commit(); err != nil {
		return TextbookCatalogSource{}, err
	}
	return source, nil
}

func (s *Store) FailTextbookCatalogJob(
	ctx context.Context,
	claim TextbookCatalogJobClaim,
	failure TextbookCatalogFailure,
	now time.Time,
) error {
	code := strings.TrimSpace(failure.Code)
	message := strings.TrimSpace(failure.Message)
	if s == nil || s.db == nil || code == "" || len(code) > 128 ||
		message == "" || len(message) > 1024 {
		return fmt.Errorf("k12storage: invalid textbook catalog failure")
	}
	nowMilli := now.UTC().UnixMilli()
	nextAttempt := int64(0)
	state := "failed_terminal"
	if !failure.Terminal {
		nextAttempt = failure.RetryAt.UTC().UnixMilli()
		if nextAttempt <= nowMilli {
			return fmt.Errorf("k12storage: retry deadline must be in the future")
		}
		state = "retry_wait"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE k12_textbook_catalog_jobs
		SET state=?,next_attempt_at=?,failure_code=?,last_error=?,lease_owner='',
		    lease_expires_at=0,updated_at=?
		WHERE job_id=? AND manifest_id=? AND owner_id=? AND document_id=?
		  AND document_generation=? AND source_digest=? AND request_digest=?
		  AND state='running' AND lease_owner=? AND lease_epoch=?
		  AND lease_expires_at>?`,
		state, nextAttempt, code, message, nowMilli,
		claim.JobID, claim.ManifestID, claim.OwnerID, claim.DocumentID,
		claim.DocumentGeneration, claim.SourceDigest, claim.RequestDigest,
		claim.LeaseOwner, claim.LeaseEpoch, nowMilli,
	)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrTextbookCatalogJobFenced
	}
	manifestState, manifestMessage := "extracting", ""
	if failure.Terminal {
		manifestState, manifestMessage = "failed_terminal", message
	}
	result, err = tx.ExecContext(ctx, `UPDATE k12_textbook_manifests
		SET state=?,retryable=0,failure_message=?,updated_at=?
		WHERE manifest_id=? AND owner_id=? AND document_id=?
		  AND document_generation=? AND source_digest=? AND state='extracting'`,
		manifestState, manifestMessage, nowMilli,
		claim.ManifestID, claim.OwnerID, claim.DocumentID,
		claim.DocumentGeneration, claim.SourceDigest,
	)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrTextbookCatalogJobFenced
	}
	return tx.Commit()
}

// RecoverTextbookCatalogJobs closes the missed-event/legacy gap before each
// worker claim. It is deliberately bounded. A ready document without a full
// immutable page/chunk snapshot becomes an explicit terminal catalog failure;
// it is never converted into guessed page mappings.
func (s *Store) RecoverTextbookCatalogJobs(
	ctx context.Context,
	now time.Time,
	limit int,
) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("k12storage: textbook catalog store unavailable")
	}
	if limit <= 0 || limit > 128 {
		limit = 32
	}
	nowMilli := now.UTC().UnixMilli()
	if nowMilli <= 0 {
		return fmt.Errorf("k12storage: invalid textbook catalog recovery time")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Acquire SQLite's write reservation before the recovery reads. Otherwise
	// two processes can both start deferred read transactions and fail together
	// while upgrading to writers.
	if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_catalog_jobs
		SET updated_at=updated_at WHERE 0`); err != nil {
		return err
	}
	type pendingSnapshot struct {
		jobID, manifestID, ownerID, documentID, sourceDigest string
		generation                                           int64
		staleExtractor                                       int
	}
	legacyRows, err := tx.QueryContext(ctx, `SELECT j.job_id,j.manifest_id,j.owner_id,
		j.document_id,j.document_generation,j.source_digest,
		CASE WHEN j.state='failed_terminal' AND j.failure_code='catalog_evidence_incomplete'
		          AND j.extractor_contract<>? THEN 1 ELSE 0 END
		FROM k12_textbook_catalog_jobs j
		JOIN k12_textbook_manifests m ON m.manifest_id=j.manifest_id
		WHERE (((j.state IN ('queued','retry_wait','failed_retryable')
		          OR (j.state='failed_terminal' AND j.failure_code='source_evidence_incomplete'))
		         AND (j.ingest_job_id='' OR length(j.source_plan_digest)<>64)
		         AND (m.state IN ('extracting','failed_retryable')
		          OR (j.state='failed_terminal' AND j.failure_code='source_evidence_incomplete'
		              AND m.state='failed_terminal')))
		    OR (j.state='failed_terminal' AND j.failure_code='catalog_evidence_incomplete'
		        AND j.extractor_contract<>? AND m.state='failed_terminal'))
		ORDER BY j.created_at,j.job_id LIMIT ?`,
		TextbookCatalogExtractorContract, TextbookCatalogExtractorContract, limit)
	if err != nil {
		return err
	}
	legacy := make([]pendingSnapshot, 0, limit)
	for legacyRows.Next() {
		var item pendingSnapshot
		if err := legacyRows.Scan(&item.jobID, &item.manifestID, &item.ownerID,
			&item.documentID, &item.generation, &item.sourceDigest,
			&item.staleExtractor); err != nil {
			legacyRows.Close()
			return err
		}
		legacy = append(legacy, item)
	}
	if err := legacyRows.Close(); err != nil {
		return err
	}
	for _, item := range legacy {
		facts, found, err := loadTextbookManifestFactsTx(
			ctx, tx, item.ownerID, item.documentID, item.generation,
		)
		if err != nil {
			return err
		}
		if facts.sourceDigest == "" {
			facts.sourceDigest = item.sourceDigest
		}
		ingestJobID, sourcePlanDigest := strings.TrimSpace(facts.jobID), ""
		if found && ingestJobID != "" {
			_, sourcePlanDigest, err = loadTextbookCatalogSourceSnapshot(
				ctx, tx, ingestJobID, item.ownerID, item.documentID,
				item.generation, item.sourceDigest,
			)
			if err != nil && !errors.Is(err, ErrTextbookCatalogSourceIncomplete) {
				return err
			}
		}
		state, failureCode, lastError := "queued", "", ""
		if !validSHA256Digest(sourcePlanDigest) {
			state = "failed_terminal"
			failureCode = "source_evidence_incomplete"
			lastError = "识别失败"
			ingestJobID, sourcePlanDigest = "", ""
		}
		requestDigest := textbookStableDigest(
			item.manifestID, item.ownerID, item.documentID,
			fmt.Sprintf("%d", item.generation), item.sourceDigest,
			ingestJobID, sourcePlanDigest, TextbookCatalogExtractorContract,
		)
		if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_catalog_jobs
			SET ingest_job_id=?,source_plan_digest=?,extractor_contract=?,
			    request_digest=?,state=?,failure_code=?,last_error=?,
			    attempt=CASE WHEN ?=1 THEN 0 ELSE attempt END,
			    result_digest=CASE WHEN ?=1 THEN '' ELSE result_digest END,
			    next_attempt_at=0,lease_owner='',lease_expires_at=0,updated_at=?
			WHERE job_id=? AND (((state IN ('queued','retry_wait','failed_retryable')
			  OR (state='failed_terminal' AND failure_code='source_evidence_incomplete'))
			  AND (ingest_job_id='' OR length(source_plan_digest)<>64))
			  OR (?=1 AND state='failed_terminal'
			      AND failure_code='catalog_evidence_incomplete' AND extractor_contract<>?))`,
			ingestJobID, sourcePlanDigest, TextbookCatalogExtractorContract,
			requestDigest, state, failureCode, lastError,
			item.staleExtractor, item.staleExtractor, nowMilli, item.jobID,
			item.staleExtractor, TextbookCatalogExtractorContract,
		); err != nil {
			return err
		}
		if state == "failed_terminal" {
			if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_manifests
				SET state='failed_terminal',retryable=0,failure_message=?,updated_at=?
				WHERE manifest_id=? AND state IN ('extracting','failed_retryable')`,
				lastError, nowMilli, item.manifestID,
			); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_manifests
				SET state='extracting',retryable=0,failure_message='',updated_at=?
				WHERE manifest_id=? AND state IN ('failed_retryable','failed_terminal')`,
				nowMilli, item.manifestID,
			); err != nil {
				return err
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.manifest_id,m.owner_id,m.document_id,
		m.document_generation,m.source_digest
		FROM k12_textbook_manifests m
		JOIN kb_semantic_document_bindings b
		  ON b.owner_id=m.owner_id AND b.document_id=m.document_id
		 AND b.content_generation=m.document_generation
		JOIN kb_documents d ON d.id=m.document_id
		WHERE m.subject='math' AND m.state='extracting'
		  AND (m.catalog_json IS NULL OR length(trim(m.catalog_json))=0)
		  AND b.lifecycle_state='active' AND b.text_state='ready' AND d.deleted=0
		  AND NOT EXISTS (SELECT 1 FROM k12_textbook_catalog_jobs j
		                  WHERE j.manifest_id=m.manifest_id)
		ORDER BY m.created_at,m.manifest_id LIMIT ?`, limit)
	if err != nil {
		return err
	}
	type candidate struct {
		manifestID, ownerID, documentID, sourceDigest string
		generation                                    int64
	}
	candidates := make([]candidate, 0, limit)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.manifestID, &item.ownerID, &item.documentID,
			&item.generation, &item.sourceDigest); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range candidates {
		facts, found, err := loadTextbookManifestFactsTx(
			ctx, tx, item.ownerID, item.documentID, item.generation,
		)
		if err != nil {
			return err
		}
		if facts.sourceDigest == "" {
			facts.sourceDigest = item.sourceDigest
		}
		if !found || facts.lifecycleState != "active" || facts.textState != "ready" ||
			facts.deleted || facts.sourceDigest == "" {
			continue
		}
		if err := enqueueTextbookCatalogJobTx(ctx, tx, item.manifestID, facts, nowMilli); err != nil {
			return err
		}
		var state, lastError string
		if err := tx.QueryRowContext(ctx, `SELECT state,last_error
			FROM k12_textbook_catalog_jobs WHERE manifest_id=?`, item.manifestID).Scan(
			&state, &lastError,
		); err != nil {
			return err
		}
		if state == "failed_terminal" {
			if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_manifests
				SET state='failed_terminal',retryable=0,failure_message=?,updated_at=?
				WHERE manifest_id=? AND state='extracting'`,
				lastError, nowMilli, item.manifestID,
			); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
