package k12storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
)

var ErrTextbookCatalogJobFenced = errors.New("textbook catalog job fenced")

type TextbookCatalogJobClaim struct {
	JobID               string
	ManifestID          string
	OwnerID             string
	DocumentID          string
	DocumentGeneration  int64
	SourceDigest        string
	RequestDigest       string
	LeaseOwner          string
	LeaseEpoch          int64
	LeaseExpiresAtMilli int64
	Attempt             int
	IngestJobID         string
	SourcePlanDigest    string
	ExtractorContract   string
}

type TextbookCatalogPageProof struct {
	LogicalPage        int      `json:"logical_page"`
	PDFPage            int      `json:"pdf_page"`
	EvidencePage       int      `json:"evidence_page"`
	EvidenceOffsetFrom int      `json:"evidence_offset_start"`
	EvidenceOffsetTo   int      `json:"evidence_offset_end"`
	EvidenceDigest     string   `json:"evidence_digest"`
	Method             string   `json:"method"`
	SegmentRefs        []string `json:"segment_refs"`
}

type TextbookCatalogPublication struct {
	CatalogJSON json.RawMessage
	PageProofs  []TextbookCatalogPageProof
}

func (s *Store) ClaimTextbookCatalogJob(
	ctx context.Context,
	workerID string,
	now time.Time,
	lease time.Duration,
) (TextbookCatalogJobClaim, bool, error) {
	workerID = strings.TrimSpace(workerID)
	nowMilli := now.UTC().UnixMilli()
	leaseMilli := lease.Milliseconds()
	if s == nil || s.db == nil || workerID == "" || nowMilli <= 0 ||
		leaseMilli <= 0 || nowMilli > int64(^uint64(0)>>1)-leaseMilli {
		return TextbookCatalogJobClaim{}, false,
			fmt.Errorf("k12storage: invalid textbook catalog claim")
	}
	leaseExpiresAt := nowMilli + leaseMilli
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TextbookCatalogJobClaim{}, false,
			fmt.Errorf("k12storage: begin textbook catalog claim: %w", err)
	}
	defer tx.Rollback()

	// Jobs whose immutable Knowledge identity is no longer current must never
	// be handed to a provider after a document replacement or deletion.
	if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_catalog_jobs AS j
		SET state='cancelled',lease_owner='',lease_expires_at=0,next_attempt_at=0,
		    last_error='source identity is no longer active',updated_at=?
		WHERE j.state IN ('queued','retry_wait','failed_retryable','running')
		  AND NOT EXISTS (
		    SELECT 1
		    FROM k12_textbook_manifests m
		    JOIN kb_semantic_document_bindings b
		      ON b.owner_id=m.owner_id
		     AND b.document_id=m.document_id
		     AND b.content_generation=m.document_generation
		    JOIN kb_documents d ON d.id=m.document_id
		    WHERE m.manifest_id=j.manifest_id
		      AND m.owner_id=j.owner_id
		      AND m.document_id=j.document_id
		      AND m.document_generation=j.document_generation
		      AND m.source_digest=j.source_digest
		      AND m.state='extracting'
		      AND b.lifecycle_state='active'
		      AND b.text_state='ready'
		      AND d.deleted=0
		  )`, nowMilli); err != nil {
		return TextbookCatalogJobClaim{}, false,
			fmt.Errorf("k12storage: cancel stale textbook catalog jobs: %w", err)
	}

	var claim TextbookCatalogJobClaim
	err = tx.QueryRowContext(ctx, `UPDATE k12_textbook_catalog_jobs
		SET state='running',attempt=attempt+1,lease_owner=?,
		    lease_epoch=lease_epoch+1,lease_expires_at=?,heartbeat_at=?,
		    next_attempt_at=0,last_error='',failure_code='',updated_at=?
		WHERE job_id=(
		  SELECT j.job_id
		  FROM k12_textbook_catalog_jobs j
		  JOIN k12_textbook_manifests m ON m.manifest_id=j.manifest_id
		  JOIN kb_semantic_document_bindings b
		    ON b.owner_id=m.owner_id
		   AND b.document_id=m.document_id
		   AND b.content_generation=m.document_generation
		  JOIN kb_documents d ON d.id=m.document_id
		  WHERE (
		      j.state='queued'
		      OR (j.state IN ('retry_wait','failed_retryable') AND j.next_attempt_at<=?)
		      OR (j.state='running' AND j.lease_expires_at<=?)
		    )
		    AND m.owner_id=j.owner_id
		    AND m.document_id=j.document_id
		    AND m.document_generation=j.document_generation
		    AND m.source_digest=j.source_digest
		    AND m.state='extracting'
		    AND b.lifecycle_state='active'
		    AND b.text_state='ready'
		    AND d.deleted=0
		  ORDER BY j.created_at,j.job_id
		  LIMIT 1
		)
		AND (
		  state='queued'
		  OR (state IN ('retry_wait','failed_retryable') AND next_attempt_at<=?)
		  OR (state='running' AND lease_expires_at<=?)
		)
		RETURNING job_id,manifest_id,owner_id,document_id,document_generation,
		          source_digest,request_digest,lease_owner,lease_epoch,lease_expires_at,
		          attempt,ingest_job_id,source_plan_digest,extractor_contract`,
		workerID, leaseExpiresAt, nowMilli, nowMilli, nowMilli, nowMilli, nowMilli, nowMilli,
	).Scan(
		&claim.JobID,
		&claim.ManifestID,
		&claim.OwnerID,
		&claim.DocumentID,
		&claim.DocumentGeneration,
		&claim.SourceDigest,
		&claim.RequestDigest,
		&claim.LeaseOwner,
		&claim.LeaseEpoch,
		&claim.LeaseExpiresAtMilli,
		&claim.Attempt,
		&claim.IngestJobID,
		&claim.SourcePlanDigest,
		&claim.ExtractorContract,
	)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return TextbookCatalogJobClaim{}, false,
				fmt.Errorf("k12storage: commit empty textbook catalog claim: %w", err)
		}
		return TextbookCatalogJobClaim{}, false, nil
	}
	if err != nil {
		return TextbookCatalogJobClaim{}, false,
			fmt.Errorf("k12storage: claim textbook catalog job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return TextbookCatalogJobClaim{}, false,
			fmt.Errorf("k12storage: commit textbook catalog claim: %w", err)
	}
	return claim, true, nil
}

func (s *Store) PublishTextbookCatalog(
	ctx context.Context,
	claim TextbookCatalogJobClaim,
	publication TextbookCatalogPublication,
	now time.Time,
) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("k12storage: textbook catalog store unavailable")
	}
	catalog, canonicalCatalog, catalogDigest, resultDigest, err :=
		validateTextbookCatalogPublication(publication)
	if err != nil {
		return err
	}
	if claim.IngestJobID != "" || claim.SourcePlanDigest != "" {
		if strings.TrimSpace(claim.IngestJobID) == "" ||
			!validSHA256Digest(claim.SourcePlanDigest) {
			return ErrTextbookCatalogJobFenced
		}
		resultDigest = textbookStableDigest(
			resultDigest, claim.IngestJobID, claim.SourcePlanDigest,
		)
	}
	nowMilli := now.UTC().UnixMilli()
	if nowMilli <= 0 {
		return fmt.Errorf("k12storage: invalid textbook catalog publish time")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("k12storage: begin textbook catalog publication: %w", err)
	}
	defer tx.Rollback()

	var current textbookCatalogJobState
	err = tx.QueryRowContext(ctx, `SELECT manifest_id,owner_id,document_id,
		document_generation,source_digest,request_digest,state,lease_owner,
		lease_epoch,lease_expires_at,result_digest,ingest_job_id,source_plan_digest
		FROM k12_textbook_catalog_jobs WHERE job_id=?`, claim.JobID).Scan(
		&current.manifestID,
		&current.ownerID,
		&current.documentID,
		&current.documentGeneration,
		&current.sourceDigest,
		&current.requestDigest,
		&current.state,
		&current.leaseOwner,
		&current.leaseEpoch,
		&current.leaseExpiresAt,
		&current.resultDigest,
		&current.ingestJobID,
		&current.sourcePlanDigest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTextbookCatalogJobFenced
	}
	if err != nil {
		return fmt.Errorf("k12storage: load textbook catalog job: %w", err)
	}
	if !current.matchesClaim(claim) {
		return ErrTextbookCatalogJobFenced
	}
	if current.state == "succeeded" {
		if current.leaseEpoch == claim.LeaseEpoch &&
			current.resultDigest == resultDigest {
			return nil
		}
		return ErrTextbookCatalogJobFenced
	}
	if current.state != "running" || current.leaseOwner != claim.LeaseOwner ||
		current.leaseEpoch != claim.LeaseEpoch || current.leaseExpiresAt <= nowMilli {
		return ErrTextbookCatalogJobFenced
	}

	var manifestState string
	err = tx.QueryRowContext(ctx, `SELECT m.state
		FROM k12_textbook_manifests m
		JOIN kb_semantic_document_bindings b
		  ON b.owner_id=m.owner_id
		 AND b.document_id=m.document_id
		 AND b.content_generation=m.document_generation
		JOIN kb_documents d ON d.id=m.document_id
		WHERE m.manifest_id=? AND m.owner_id=? AND m.document_id=?
		  AND m.document_generation=? AND m.source_digest=? AND m.subject='math'
		  AND b.lifecycle_state='active' AND b.text_state='ready' AND d.deleted=0
		LIMIT 1`,
		claim.ManifestID,
		claim.OwnerID,
		claim.DocumentID,
		claim.DocumentGeneration,
		claim.SourceDigest,
	).Scan(&manifestState)
	if errors.Is(err, sql.ErrNoRows) || manifestState != "extracting" {
		return ErrTextbookCatalogJobFenced
	}
	if err != nil {
		return fmt.Errorf("k12storage: validate textbook catalog identity: %w", err)
	}

	var existingMappings, existingSegments int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM k12_textbook_page_mappings WHERE manifest_id=?),
		(SELECT COUNT(*) FROM k12_textbook_manifest_segments WHERE manifest_id=?)`,
		claim.ManifestID, claim.ManifestID,
	).Scan(&existingMappings, &existingSegments); err != nil {
		return fmt.Errorf("k12storage: inspect textbook catalog publication: %w", err)
	}
	if existingMappings != 0 || existingSegments != 0 {
		verified, err := hasVerifiedTextbookManifestProof(ctx, tx, claim.ManifestID)
		if err != nil {
			return err
		}
		if verified {
			return fmt.Errorf("%w: textbook catalog already has verified proof",
				records.ErrIllegalTransition)
		}
	}

	for index, proof := range publication.PageProofs {
		if err := validateTextbookCatalogPageProofTx(
			ctx, tx, claim, catalog.PageRefs[index], proof,
		); err != nil {
			return err
		}
	}
	// Legacy versions could persist catalog/segment guesses before a page-map
	// proof existed. Supersede only those unverified remnants, and only after
	// the complete replacement proposal has passed every deterministic check.
	if existingMappings != 0 || existingSegments != 0 {
		if err := clearUnverifiedTextbookCatalogArtifactsTx(
			ctx, tx, claim.ManifestID, nowMilli,
		); err != nil {
			return err
		}
	}

	for index, proof := range publication.PageProofs {
		mappingID := "tbpm-" + textbookStableDigest(
			claim.ManifestID,
			strconv.Itoa(proof.LogicalPage),
			strconv.Itoa(proof.PDFPage),
			proof.EvidenceDigest,
		)[:32]
		if _, err := tx.ExecContext(ctx, `INSERT INTO k12_textbook_page_mappings
			(mapping_id,manifest_id,logical_page,pdf_page,evidence_page,
			 evidence_offset_start,evidence_offset_end,evidence_digest,method,
			 verification_state,document_id,document_generation,source_digest,
			 created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,'verified',?,?,?,?,?)`,
			mappingID,
			claim.ManifestID,
			proof.LogicalPage,
			proof.PDFPage,
			proof.EvidencePage,
			proof.EvidenceOffsetFrom,
			proof.EvidenceOffsetTo,
			proof.EvidenceDigest,
			proof.Method,
			claim.DocumentID,
			claim.DocumentGeneration,
			claim.SourceDigest,
			nowMilli,
			nowMilli,
		); err != nil {
			return fmt.Errorf("k12storage: insert textbook page proof %d: %w", index, err)
		}
		for _, segmentRef := range proof.SegmentRefs {
			segmentID := textbookManifestSegmentID(
				claim.ManifestID, proof.LogicalPage, segmentRef,
			)
			if _, err := tx.ExecContext(ctx, `INSERT INTO k12_textbook_manifest_segments
				(segment_id,manifest_id,logical_page,segment_ref,pdf_page,document_id,
				 document_generation,source_digest,created_at,updated_at)
				VALUES(?,?,?,?,?,?,?,?,?,?)`,
				segmentID,
				claim.ManifestID,
				proof.LogicalPage,
				segmentRef,
				proof.PDFPage,
				claim.DocumentID,
				claim.DocumentGeneration,
				claim.SourceDigest,
				nowMilli,
				nowMilli,
			); err != nil {
				return fmt.Errorf("k12storage: insert textbook catalog segment: %w", err)
			}
		}
	}

	result, err := tx.ExecContext(ctx, `UPDATE k12_textbook_manifests
		SET catalog_json=?,catalog_digest=?,state='ready_for_confirmation',
		    retryable=0,failure_message='',updated_at=?
		WHERE manifest_id=? AND owner_id=? AND document_id=?
		  AND document_generation=? AND source_digest=? AND state='extracting'`,
		string(canonicalCatalog),
		catalogDigest,
		nowMilli,
		claim.ManifestID,
		claim.OwnerID,
		claim.DocumentID,
		claim.DocumentGeneration,
		claim.SourceDigest,
	)
	if err != nil {
		return fmt.Errorf("k12storage: publish textbook manifest catalog: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrTextbookCatalogJobFenced
	}
	result, err = tx.ExecContext(ctx, `UPDATE k12_textbook_catalog_jobs
		SET state='succeeded',result_digest=?,last_error='',lease_owner='',
		    lease_expires_at=0,updated_at=?
		WHERE job_id=? AND state='running' AND lease_owner=? AND lease_epoch=?
		  AND lease_expires_at>?`,
		resultDigest,
		nowMilli,
		claim.JobID,
		claim.LeaseOwner,
		claim.LeaseEpoch,
		nowMilli,
	)
	if err != nil {
		return fmt.Errorf("k12storage: finish textbook catalog job: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrTextbookCatalogJobFenced
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("k12storage: commit textbook catalog publication: %w", err)
	}
	return nil
}

type textbookCatalogJobState struct {
	manifestID         string
	ownerID            string
	documentID         string
	documentGeneration int64
	sourceDigest       string
	requestDigest      string
	state              string
	leaseOwner         string
	leaseEpoch         int64
	leaseExpiresAt     int64
	resultDigest       string
	ingestJobID        string
	sourcePlanDigest   string
}

func (state textbookCatalogJobState) matchesClaim(claim TextbookCatalogJobClaim) bool {
	return strings.TrimSpace(claim.JobID) != "" &&
		state.manifestID == claim.ManifestID &&
		state.ownerID == claim.OwnerID &&
		state.documentID == claim.DocumentID &&
		state.documentGeneration == claim.DocumentGeneration &&
		state.sourceDigest == claim.SourceDigest &&
		state.requestDigest == claim.RequestDigest &&
		state.ingestJobID == claim.IngestJobID &&
		state.sourcePlanDigest == claim.SourcePlanDigest
}

type textbookCatalogDocument struct {
	Subject         string                   `json:"subject"`
	TextbookEdition string                   `json:"textbook_edition"`
	TextbookVersion string                   `json:"textbook_version"`
	Title           string                   `json:"title"`
	Volume          string                   `json:"volume"`
	PageMin         int                      `json:"page_min"`
	PageMax         int                      `json:"page_max"`
	Units           []textbookCatalogUnit    `json:"units"`
	PageRefs        []textbookCatalogPageRef `json:"page_refs"`
}

type textbookCatalogUnit struct {
	UnitID   string                  `json:"unit_id"`
	Title    string                  `json:"title"`
	PageFrom int                     `json:"page_from"`
	PageTo   int                     `json:"page_to"`
	Lessons  []textbookCatalogLesson `json:"lessons"`
}

type textbookCatalogLesson struct {
	LessonID string `json:"lesson_id"`
	Title    string `json:"title"`
	PageFrom int    `json:"page_from"`
	PageTo   int    `json:"page_to"`
}

type textbookCatalogPageRef struct {
	LogicalPage int      `json:"logical_page"`
	PDFPage     int      `json:"pdf_page"`
	SegmentRefs []string `json:"segment_refs"`
}

func validateTextbookCatalogPublication(
	publication TextbookCatalogPublication,
) (textbookCatalogDocument, []byte, string, string, error) {
	var catalog textbookCatalogDocument
	decoder := json.NewDecoder(bytes.NewReader(publication.CatalogJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return textbookCatalogDocument{}, nil, "", "",
			fmt.Errorf("%w: decode textbook catalog: %v", records.ErrIllegalTransition, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return textbookCatalogDocument{}, nil, "", "",
			fmt.Errorf("%w: decode textbook catalog: %v", records.ErrIllegalTransition, err)
	}
	catalog.Subject = strings.TrimSpace(catalog.Subject)
	catalog.TextbookEdition = strings.TrimSpace(catalog.TextbookEdition)
	catalog.TextbookVersion = strings.TrimSpace(catalog.TextbookVersion)
	catalog.Title = strings.TrimSpace(catalog.Title)
	catalog.Volume = strings.TrimSpace(catalog.Volume)
	if catalog.Subject != textbookManifestSubjectMath ||
		catalog.TextbookEdition == "" || catalog.TextbookVersion == "" ||
		catalog.Title == "" || catalog.Volume == "" || catalog.PageMin < 1 ||
		catalog.PageMax < catalog.PageMin || len(catalog.Units) == 0 {
		return textbookCatalogDocument{}, nil, "", "",
			fmt.Errorf("%w: invalid textbook catalog metadata", records.ErrIllegalTransition)
	}
	unitIDs := make(map[string]struct{}, len(catalog.Units))
	previousUnitFrom := 0
	for unitIndex := range catalog.Units {
		unit := &catalog.Units[unitIndex]
		unit.UnitID = strings.TrimSpace(unit.UnitID)
		unit.Title = strings.TrimSpace(unit.Title)
		if unit.UnitID == "" || unit.Title == "" || unit.PageFrom < catalog.PageMin ||
			unit.PageTo < unit.PageFrom || unit.PageTo > catalog.PageMax ||
			unit.PageFrom < previousUnitFrom {
			return textbookCatalogDocument{}, nil, "", "",
				fmt.Errorf("%w: invalid textbook catalog unit", records.ErrIllegalTransition)
		}
		if _, duplicate := unitIDs[unit.UnitID]; duplicate {
			return textbookCatalogDocument{}, nil, "", "",
				fmt.Errorf("%w: duplicate textbook catalog unit", records.ErrIllegalTransition)
		}
		unitIDs[unit.UnitID] = struct{}{}
		previousUnitFrom = unit.PageFrom
		lessonIDs := make(map[string]struct{}, len(unit.Lessons))
		previousLessonFrom := 0
		for lessonIndex := range unit.Lessons {
			lesson := &unit.Lessons[lessonIndex]
			lesson.LessonID = strings.TrimSpace(lesson.LessonID)
			lesson.Title = strings.TrimSpace(lesson.Title)
			if lesson.LessonID == "" || lesson.Title == "" ||
				lesson.PageFrom < unit.PageFrom || lesson.PageTo < lesson.PageFrom ||
				lesson.PageTo > unit.PageTo || lesson.PageFrom < previousLessonFrom {
				return textbookCatalogDocument{}, nil, "", "",
					fmt.Errorf("%w: invalid textbook catalog lesson", records.ErrIllegalTransition)
			}
			if _, duplicate := lessonIDs[lesson.LessonID]; duplicate {
				return textbookCatalogDocument{}, nil, "", "",
					fmt.Errorf("%w: duplicate textbook catalog lesson", records.ErrIllegalTransition)
			}
			lessonIDs[lesson.LessonID] = struct{}{}
			previousLessonFrom = lesson.PageFrom
		}
	}
	wantPages := catalog.PageMax - catalog.PageMin + 1
	if len(catalog.PageRefs) != wantPages || len(publication.PageProofs) != wantPages {
		return textbookCatalogDocument{}, nil, "", "",
			fmt.Errorf("%w: incomplete textbook page map", records.ErrIllegalTransition)
	}
	previousPDFPage := 0
	for index := range catalog.PageRefs {
		pageRef := &catalog.PageRefs[index]
		proof := &publication.PageProofs[index]
		if pageRef.LogicalPage != catalog.PageMin+index || pageRef.PDFPage <= previousPDFPage ||
			pageRef.LogicalPage != proof.LogicalPage || pageRef.PDFPage != proof.PDFPage ||
			proof.EvidencePage != proof.PDFPage || proof.Method != "printed_anchor" ||
			proof.EvidenceOffsetFrom < 0 || proof.EvidenceOffsetTo <= proof.EvidenceOffsetFrom ||
			!validSHA256Digest(proof.EvidenceDigest) || len(pageRef.SegmentRefs) == 0 ||
			!sameExactSegmentRefs(pageRef.SegmentRefs, proof.SegmentRefs) {
			return textbookCatalogDocument{}, nil, "", "",
				fmt.Errorf("%w: invalid textbook page proof", records.ErrIllegalTransition)
		}
		seenSegments := make(map[string]struct{}, len(pageRef.SegmentRefs))
		for segmentIndex, segmentRef := range pageRef.SegmentRefs {
			trimmed := strings.TrimSpace(segmentRef)
			if trimmed == "" || trimmed != segmentRef {
				return textbookCatalogDocument{}, nil, "", "",
					fmt.Errorf("%w: invalid textbook segment reference", records.ErrIllegalTransition)
			}
			if _, duplicate := seenSegments[trimmed]; duplicate {
				return textbookCatalogDocument{}, nil, "", "",
					fmt.Errorf("%w: duplicate textbook segment reference", records.ErrIllegalTransition)
			}
			seenSegments[trimmed] = struct{}{}
			pageRef.SegmentRefs[segmentIndex] = trimmed
		}
		previousPDFPage = pageRef.PDFPage
	}
	canonicalCatalog, err := json.Marshal(catalog)
	if err != nil {
		return textbookCatalogDocument{}, nil, "", "", err
	}
	canonicalProofs, err := json.Marshal(publication.PageProofs)
	if err != nil {
		return textbookCatalogDocument{}, nil, "", "", err
	}
	catalogDigest := sha256Hex(canonicalCatalog)
	resultDigest := textbookStableDigest(string(canonicalCatalog), string(canonicalProofs))
	return catalog, canonicalCatalog, catalogDigest, resultDigest, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values")
	}
	return err
}

func validSHA256Digest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func sameExactSegmentRefs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func validateTextbookCatalogPageProofTx(
	ctx context.Context,
	tx *sql.Tx,
	claim TextbookCatalogJobClaim,
	pageRef textbookCatalogPageRef,
	proof TextbookCatalogPageProof,
) error {
	var content, contentDigest string
	err := tx.QueryRowContext(ctx, `SELECT p.content,p.content_digest
		FROM kb_ingest_page_checkpoints p
		JOIN kb_knowledge_jobs j ON j.job_id=p.job_id
		WHERE j.kind='ingest' AND j.owner_id=? AND j.document_id=?
		  AND j.document_generation=? AND j.state='succeeded'
		  AND j.pages_total IS NOT NULL AND j.pages_done=j.pages_total
		  AND p.pages_total=j.pages_total AND p.lease_epoch>0
		  AND p.lease_epoch<=j.lease_epoch
		  AND p.page_number=? AND p.source_digest=?
		  AND (?='' OR j.job_id=?)
		ORDER BY COALESCE(j.finished_at,j.updated_at) DESC,j.created_at DESC,j.job_id DESC
		LIMIT 1`,
		claim.OwnerID,
		claim.DocumentID,
		claim.DocumentGeneration,
		proof.EvidencePage,
		claim.SourceDigest,
		claim.IngestJobID,
		claim.IngestJobID,
	).Scan(&content, &contentDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: textbook page checkpoint is missing", records.ErrIllegalTransition)
	}
	if err != nil {
		return fmt.Errorf("k12storage: load textbook page checkpoint: %w", err)
	}
	if contentDigest != proof.EvidenceDigest || sha256Hex([]byte(content)) != contentDigest ||
		proof.EvidenceOffsetTo > len(content) ||
		strings.TrimSpace(content[proof.EvidenceOffsetFrom:proof.EvidenceOffsetTo]) !=
			strconv.Itoa(pageRef.LogicalPage) {
		return fmt.Errorf("%w: textbook printed-page evidence mismatch", records.ErrIllegalTransition)
	}
	for _, segmentRef := range proof.SegmentRefs {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM kb_chunks
			WHERE id=? AND doc_id=? AND source_digest=?
			  AND page_start IS NOT NULL AND page_end IS NOT NULL
			  AND page_start<=? AND page_end>=?
		)`,
			segmentRef,
			claim.DocumentID,
			claim.SourceDigest,
			proof.PDFPage,
			proof.PDFPage,
		).Scan(&exists); err != nil {
			return fmt.Errorf("k12storage: validate textbook segment: %w", err)
		}
		if exists != 1 {
			return fmt.Errorf("%w: textbook segment proof mismatch", records.ErrIllegalTransition)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM kb_chunks
		WHERE doc_id=? AND source_digest=?
		  AND page_start IS NOT NULL AND page_end IS NOT NULL
		  AND source_offset_start IS NOT NULL AND source_offset_end IS NOT NULL
		  AND source_offset_end>source_offset_start
		  AND page_start<=? AND page_end>=?
		ORDER BY id`,
		claim.DocumentID, claim.SourceDigest, proof.PDFPage, proof.PDFPage,
	)
	if err != nil {
		return fmt.Errorf("k12storage: list exact textbook segments: %w", err)
	}
	wantRefs := make([]string, 0, len(proof.SegmentRefs))
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			rows.Close()
			return err
		}
		wantRefs = append(wantRefs, ref)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	gotRefs := append([]string(nil), proof.SegmentRefs...)
	sort.Strings(gotRefs)
	if len(wantRefs) == 0 || !sameExactSegmentRefs(wantRefs, gotRefs) {
		return fmt.Errorf("%w: incomplete textbook segment proof", records.ErrIllegalTransition)
	}
	return nil
}

func enqueueTextbookCatalogJobTx(
	ctx context.Context,
	tx *sql.Tx,
	manifestID string,
	facts textbookManifestFacts,
	at int64,
) error {
	ingestJobID := strings.TrimSpace(facts.jobID)
	sourcePlanDigest := ""
	jobState := "queued"
	failureCode := ""
	lastError := ""
	if ingestJobID != "" {
		var err error
		_, sourcePlanDigest, err = loadTextbookCatalogSourceSnapshot(
			ctx, tx, ingestJobID, facts.knowledgeOwnerID, facts.documentID,
			facts.generation, facts.sourceDigest,
		)
		if err != nil && !errors.Is(err, ErrTextbookCatalogSourceIncomplete) {
			return fmt.Errorf("k12storage: freeze textbook catalog source: %w", err)
		}
		if err != nil {
			sourcePlanDigest = ""
		}
	}
	if ingestJobID == "" || !validSHA256Digest(sourcePlanDigest) {
		jobState = "failed_terminal"
		failureCode = "source_evidence_incomplete"
		lastError = "识别失败"
	}
	jobID := "tbcj-" + textbookStableDigest(
		manifestID, facts.knowledgeOwnerID, facts.documentID,
		fmt.Sprintf("%d", facts.generation), facts.sourceDigest, ingestJobID,
		sourcePlanDigest, TextbookCatalogExtractorContract,
	)[:32]
	requestDigest := textbookStableDigest(
		manifestID, facts.knowledgeOwnerID, facts.documentID,
		fmt.Sprintf("%d", facts.generation), facts.sourceDigest, ingestJobID,
		sourcePlanDigest, TextbookCatalogExtractorContract,
	)
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_textbook_catalog_jobs
		(job_id,manifest_id,owner_id,document_id,document_generation,source_digest,
		 state,attempt,lease_owner,lease_epoch,lease_expires_at,request_digest,
		 result_digest,last_error,created_at,updated_at,ingest_job_id,
		 source_plan_digest,extractor_contract,next_attempt_at,heartbeat_at,failure_code)
		VALUES(?,?,?,?,?,?,?,0,'',0,0,?,'',?,?,?,?,?,?,0,0,?)
		ON CONFLICT(manifest_id) DO NOTHING`,
		jobID, manifestID, facts.knowledgeOwnerID, facts.documentID,
		facts.generation, facts.sourceDigest, jobState, requestDigest, lastError,
		at, at, ingestJobID, sourcePlanDigest, TextbookCatalogExtractorContract,
		failureCode,
	); err != nil {
		return fmt.Errorf("k12storage: enqueue textbook catalog job: %w", err)
	}
	return nil
}

func loadVerifiedTextbookSegmentCountTx(
	ctx context.Context,
	tx *sql.Tx,
	manifestID string,
) (int, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM k12_textbook_manifest_segments s
		JOIN k12_textbook_page_mappings p
		  ON p.manifest_id=s.manifest_id
		 AND p.logical_page=s.logical_page
		 AND p.pdf_page=s.pdf_page
		 AND p.verification_state='verified'
		WHERE s.manifest_id=?`, manifestID).Scan(&count); err != nil {
		return 0, fmt.Errorf("k12storage: count verified textbook segments: %w", err)
	}
	return count, nil
}

func clearUnverifiedTextbookCatalogArtifactsTx(
	ctx context.Context,
	tx *sql.Tx,
	manifestID string,
	at int64,
) error {
	verified, err := hasVerifiedTextbookManifestProof(ctx, tx, manifestID)
	if err != nil {
		return err
	}
	if verified {
		return fmt.Errorf("%w: verified textbook proof cannot be rebuilt in place",
			records.ErrIllegalTransition)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM k12_textbook_manifest_segments
		WHERE manifest_id=?`, manifestID); err != nil {
		return fmt.Errorf("k12storage: clear unverified textbook segments: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM k12_textbook_page_mappings
		WHERE manifest_id=?`, manifestID); err != nil {
		return fmt.Errorf("k12storage: clear unverified textbook page mappings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_textbook_manifests
		SET catalog_json=NULL,catalog_digest=NULL,state='extracting',retryable=0,
		    failure_message='',updated_at=?
		WHERE manifest_id=?`, at, manifestID); err != nil {
		return fmt.Errorf("k12storage: clear unverified textbook catalog: %w", err)
	}
	return nil
}
