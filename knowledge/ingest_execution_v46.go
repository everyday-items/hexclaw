package knowledge

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

var (
	ErrInvalidVisionRouteSnapshot = errors.New("knowledge: invalid vision route snapshot")
	ErrInvalidIngestSegmentPlan   = errors.New("knowledge: invalid ingest segment plan")
	ErrVisionModelRequired        = errors.New("knowledge: vision model required")
)

const VisionModelRequiredFailureCode = "vision_model_required"

// VisionRouteSnapshot is the secret-free executable identity frozen when an
// ingest root is accepted. Provider/model settings may change afterwards; a
// worker must still execute this exact pair or fail closed.
type VisionRouteSnapshot struct {
	ProviderInstanceID  string   `json:"provider_instance_id"`
	ProviderName        string   `json:"provider_name"`
	ProviderDisplayName string   `json:"provider_display_name"`
	Model               string   `json:"model"`
	Capabilities        []string `json:"capabilities"`
	Fingerprint         string   `json:"fingerprint"`
}

func (s VisionRouteSnapshot) Canonical() VisionRouteSnapshot {
	s.ProviderInstanceID = strings.TrimSpace(s.ProviderInstanceID)
	s.ProviderName = strings.TrimSpace(s.ProviderName)
	s.ProviderDisplayName = strings.TrimSpace(s.ProviderDisplayName)
	s.Model = strings.TrimSpace(s.Model)
	seen := map[string]struct{}{}
	capabilities := make([]string, 0, len(s.Capabilities))
	for _, capability := range s.Capabilities {
		capability = strings.ToLower(strings.TrimSpace(capability))
		if capability == "" {
			continue
		}
		if _, exists := seen[capability]; exists {
			continue
		}
		seen[capability] = struct{}{}
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)
	s.Capabilities = capabilities
	digest := sha256.Sum256([]byte(strings.Join([]string{
		s.ProviderInstanceID,
		s.ProviderName,
		s.ProviderDisplayName,
		s.Model,
		strings.Join(s.Capabilities, ","),
	}, "\x00")))
	s.Fingerprint = hex.EncodeToString(digest[:])
	return s
}

func (s VisionRouteSnapshot) Validate() error {
	s = s.Canonical()
	if s.ProviderInstanceID == "" || s.ProviderName == "" ||
		s.ProviderDisplayName == "" || s.Model == "" {
		return ErrInvalidVisionRouteSnapshot
	}
	return nil
}

func (s VisionRouteSnapshot) HasCapability(required string) bool {
	required = strings.ToLower(strings.TrimSpace(required))
	for _, capability := range s.Canonical().Capabilities {
		if capability == required {
			return true
		}
	}
	return false
}

func (s VisionRouteSnapshot) MarshalCapabilitiesJSON() (string, error) {
	payload, err := json.Marshal(s.Canonical().Capabilities)
	return string(payload), err
}

func (s *VisionRouteSnapshot) UnmarshalCapabilitiesJSON(payload string) error {
	if s == nil {
		return ErrInvalidVisionRouteSnapshot
	}
	if err := json.Unmarshal([]byte(payload), &s.Capabilities); err != nil {
		return err
	}
	*s = s.Canonical()
	return nil
}

type visionRouteSnapshotContextKey struct{}

func WithVisionRouteSnapshot(ctx context.Context, snapshot VisionRouteSnapshot) context.Context {
	return context.WithValue(ctx, visionRouteSnapshotContextKey{}, snapshot.Canonical())
}

func VisionRouteSnapshotFromContext(ctx context.Context) (VisionRouteSnapshot, bool) {
	if ctx == nil {
		return VisionRouteSnapshot{}, false
	}
	snapshot, ok := ctx.Value(visionRouteSnapshotContextKey{}).(VisionRouteSnapshot)
	return snapshot, ok
}

type IngestSegmentMode string

const (
	IngestSegmentText   IngestSegmentMode = "text"
	IngestSegmentVisual IngestSegmentMode = "ocr_vlm"
)

type IngestPagePlan struct {
	PageNumber int
	Mode       IngestSegmentMode
}

type IngestSegmentPlan struct {
	Ordinal   int
	PageStart int
	PageEnd   int
	Mode      IngestSegmentMode
}

// PlanAdaptiveIngestSegments keeps the public document whole. Text runs are
// bounded at 20 pages; visual runs use the renderer's existing batch cap.
func PlanAdaptiveIngestSegments(pages []IngestPagePlan, visualPageCap int) ([]IngestSegmentPlan, error) {
	if len(pages) == 0 || visualPageCap <= 0 {
		return nil, ErrInvalidIngestSegmentPlan
	}
	segments := make([]IngestSegmentPlan, 0, len(pages))
	for index := 0; index < len(pages); {
		page := pages[index]
		if page.PageNumber != index+1 ||
			(page.Mode != IngestSegmentText && page.Mode != IngestSegmentVisual) {
			return nil, ErrInvalidIngestSegmentPlan
		}
		cap := 20
		if page.Mode == IngestSegmentVisual {
			cap = visualPageCap
		}
		end := index
		for end+1 < len(pages) && end-index+1 < cap &&
			pages[end+1].Mode == page.Mode &&
			pages[end+1].PageNumber == pages[end].PageNumber+1 {
			end++
		}
		segments = append(segments, IngestSegmentPlan{
			Ordinal: len(segments) + 1,
			PageStart: page.PageNumber,
			PageEnd: pages[end].PageNumber,
			Mode: page.Mode,
		})
		index = end + 1
	}
	return segments, nil
}

// IngestSegmentProgress is optional so small/non-PDF processors remain source
// compatible while resumable PDF ingestion persists its invisible plan.
type IngestSegmentProgress interface {
	SaveSegmentPlan(context.Context, string, []IngestSegmentPlan) error
}

type VisionModelRequiredError struct {
	Route         VisionRouteSnapshot
	AffectedPages []int
}

func NewVisionModelRequiredError(route VisionRouteSnapshot, affectedPages []int) *VisionModelRequiredError {
	pages := append([]int(nil), affectedPages...)
	sort.Ints(pages)
	return &VisionModelRequiredError{Route: route.Canonical(), AffectedPages: pages}
}

func (e *VisionModelRequiredError) Error() string {
	if e == nil {
		return ErrVisionModelRequired.Error()
	}
	return fmt.Sprintf(
		"默认模型 %s · %s 不支持图片识别；第 %v 页需要 OCR/VLM，请在 LLM 设置中选择支持视觉的默认模型后重试",
		e.Route.ProviderDisplayName, e.Route.Model, e.AffectedPages,
	)
}

func (e *VisionModelRequiredError) Unwrap() error { return ErrVisionModelRequired }

type KnowledgeJobFailure struct {
	Code                string `json:"code"`
	Message             string `json:"message"`
	AffectedPages       []int  `json:"affected_pages,omitempty"`
	ProviderDisplayName string `json:"provider_display_name,omitempty"`
	Model               string `json:"model,omitempty"`
	ActionCode          string `json:"action_code,omitempty"`
}

func KnowledgeJobFailureFromError(err error) KnowledgeJobFailure {
	var visionFailure *VisionModelRequiredError
	if errors.As(err, &visionFailure) {
		return KnowledgeJobFailure{
			Code: VisionModelRequiredFailureCode,
			Message: visionFailure.Error(),
			AffectedPages: append([]int(nil), visionFailure.AffectedPages...),
			ProviderDisplayName: visionFailure.Route.ProviderDisplayName,
			Model: visionFailure.Route.Model,
			ActionCode: "configure_default_vision_model",
		}
	}
	return KnowledgeJobFailure{Code: "job_failed", Message: err.Error()}
}

func (f KnowledgeJobFailure) validate() error {
	if strings.TrimSpace(f.Code) == "" || strings.TrimSpace(f.Message) == "" {
		return fmt.Errorf("knowledge: invalid structured job failure")
	}
	return nil
}

func persistVisionRouteSnapshotTx(
	ctx context.Context,
	tx *sql.Tx,
	jobID string,
	snapshot *VisionRouteSnapshot,
	nowMillis int64,
) error {
	if snapshot == nil {
		return nil
	}
	canonical := snapshot.Canonical()
	if err := canonical.Validate(); err != nil {
		return err
	}
	capabilities, err := canonical.MarshalCapabilitiesJSON()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_ingest_execution_snapshots
		(job_id,provider_instance_id,provider_name,provider_display_name,model,
		 capabilities_json,selection_fingerprint,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, jobID, canonical.ProviderInstanceID,
		canonical.ProviderName, canonical.ProviderDisplayName, canonical.Model,
		capabilities, canonical.Fingerprint, nowMillis); err != nil {
		return fmt.Errorf("knowledge: persist ingest vision route: %w", err)
	}
	return nil
}

func (r *SQLiteSemanticIndexRepository) GetIngestDocumentForJob(
	ctx context.Context,
	ownerID, corpusUID, documentID, jobID string,
) (PersistedIngestDocument, error) {
	source, err := r.GetIngestDocumentForCorpusUID(ctx, ownerID, corpusUID, documentID)
	if err != nil {
		return PersistedIngestDocument{}, err
	}
	source.JobID = jobID
	var snapshot VisionRouteSnapshot
	var capabilities string
	err = r.db.QueryRowContext(ctx, `SELECT provider_instance_id,provider_name,
		provider_display_name,model,capabilities_json,selection_fingerprint
		FROM kb_ingest_execution_snapshots WHERE job_id=?`, jobID).Scan(
		&snapshot.ProviderInstanceID, &snapshot.ProviderName, &snapshot.ProviderDisplayName,
		&snapshot.Model, &capabilities, &snapshot.Fingerprint,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return source, nil
	}
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return source, nil
		}
		return PersistedIngestDocument{}, err
	}
	if err := snapshot.UnmarshalCapabilitiesJSON(capabilities); err != nil {
		return PersistedIngestDocument{}, err
	}
	source.VisionRoute = &snapshot
	return source, nil
}

func (r *SQLiteSemanticIndexRepository) SaveIngestSegmentPlan(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	sourceDigest string,
	segments []IngestSegmentPlan,
) error {
	if len(sourceDigest) != 64 || len(segments) == 0 {
		return ErrInvalidIngestSegmentPlan
	}
	expectedPage := 1
	for index, segment := range segments {
		if segment.Ordinal != index+1 || segment.PageStart != expectedPage ||
			segment.PageEnd < segment.PageStart ||
			(segment.Mode != IngestSegmentText && segment.Mode != IngestSegmentVisual) {
			return ErrInvalidIngestSegmentPlan
		}
		expectedPage = segment.PageEnd + 1
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
	encoded, err := json.Marshal(segments)
	if err != nil {
		return err
	}
	planDigestBytes := sha256.Sum256(encoded)
	planDigest := hex.EncodeToString(planDigestBytes[:])
	nowMillis := now.UTC().UnixMilli()
	for _, segment := range segments {
		segmentID := "segment_" + hashStrings(job.JobID, fmt.Sprint(segment.Ordinal), planDigest)[:32]
		if _, err := tx.ExecContext(ctx, `INSERT INTO kb_ingest_segments
			(segment_id,job_id,document_id,document_generation,ordinal,page_start,page_end,
			 extraction_mode,state,source_digest,plan_digest,last_error,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,'planned',?,?, '',?,?)
			ON CONFLICT(job_id,ordinal) DO NOTHING`,
			segmentID, job.JobID, job.DocumentID, job.DocumentGeneration, segment.Ordinal,
			segment.PageStart, segment.PageEnd, segment.Mode, sourceDigest, planDigest,
			nowMillis, nowMillis); err != nil {
			return fmt.Errorf("knowledge: persist ingest segment plan: %w", err)
		}
		var stored IngestSegmentPlan
		var storedDigest, storedSource string
		if err := tx.QueryRowContext(ctx, `SELECT ordinal,page_start,page_end,extraction_mode,
			plan_digest,source_digest FROM kb_ingest_segments WHERE job_id=? AND ordinal=?`,
			job.JobID, segment.Ordinal).Scan(&stored.Ordinal, &stored.PageStart, &stored.PageEnd,
			&stored.Mode, &storedDigest, &storedSource); err != nil {
			return err
		}
		if stored != segment || storedDigest != planDigest || storedSource != sourceDigest {
			return ErrJobFenced
		}
	}
	if err := refreshIngestSegmentStatesTx(ctx, tx, job.JobID, nowMillis); err != nil {
		return err
	}
	return tx.Commit()
}

func refreshIngestSegmentStatesTx(
	ctx context.Context,
	tx *sql.Tx,
	jobID string,
	nowMillis int64,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT ordinal,page_start,page_end,extraction_mode,
		source_digest,state FROM kb_ingest_segments
		WHERE job_id=? AND state IN ('planned','processing') ORDER BY ordinal`, jobID)
	if err != nil {
		return err
	}
	type segmentState struct {
		ordinal            int
		pageStart, pageEnd int
		mode, digest, state string
	}
	segments := []segmentState{}
	for rows.Next() {
		var segment segmentState
		if err := rows.Scan(&segment.ordinal, &segment.pageStart, &segment.pageEnd,
			&segment.mode, &segment.digest, &segment.state); err != nil {
			rows.Close()
			return err
		}
		segments = append(segments, segment)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, segment := range segments {
		var completed int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_ingest_page_checkpoints
			WHERE job_id=? AND page_number BETWEEN ? AND ?
			  AND source_digest=? AND extraction_mode=?`,
			jobID, segment.pageStart, segment.pageEnd, segment.digest, segment.mode,
		).Scan(&completed); err != nil {
			return err
		}
		nextState := "planned"
		switch pageCount := segment.pageEnd - segment.pageStart + 1; {
		case completed == pageCount:
			nextState = "ready"
		case completed > 0:
			nextState = "processing"
		}
		if nextState == segment.state {
			continue
		}
		res, err := tx.ExecContext(ctx, `UPDATE kb_ingest_segments
			SET state=?,last_error='',updated_at=?
			WHERE job_id=? AND ordinal=? AND state=?`,
			nextState, nowMillis, jobID, segment.ordinal, segment.state)
		if err != nil {
			return err
		}
		if affected, _ := res.RowsAffected(); affected != 1 {
			return ErrJobFenced
		}
	}
	return nil
}

func validateReadyIngestSegmentsTx(
	ctx context.Context,
	tx *sql.Tx,
	jobID string,
	pagesTotal int64,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT ordinal,page_start,page_end,state
		FROM kb_ingest_segments WHERE job_id=? ORDER BY ordinal`, jobID)
	if err != nil {
		return err
	}
	defer rows.Close()
	expectedOrdinal, expectedPage, segmentCount := 1, 1, 0
	for rows.Next() {
		var ordinal, pageStart, pageEnd int
		var state string
		if err := rows.Scan(&ordinal, &pageStart, &pageEnd, &state); err != nil {
			return err
		}
		segmentCount++
		if ordinal != expectedOrdinal || pageStart != expectedPage || pageEnd < pageStart ||
			state != "ready" {
			return fmt.Errorf("%w: ingest segment %d is not a contiguous ready segment",
				ErrInvalidDocumentUpload, ordinal)
		}
		expectedOrdinal++
		expectedPage = pageEnd + 1
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if segmentCount > 0 && int64(expectedPage-1) != pagesTotal {
		return fmt.Errorf("%w: ingest segment coverage=%d pages_total=%d",
			ErrInvalidDocumentUpload, expectedPage-1, pagesTotal)
	}
	return nil
}

// readyIngestSegmentVisibilitySQL is the single visibility contract shared by
// text and vector search. Legacy documents without a segment plan retain their
// existing visibility. For segmented documents, a chunk must have honest page
// coordinates, overlap at least one ready segment, and overlap no non-ready
// segment from the latest durable ingest plan for its current generation.
func readyIngestSegmentVisibilitySQL(bindingAlias, chunkAlias string) string {
	latestJob := fmt.Sprintf(`(
		SELECT j.job_id FROM kb_knowledge_jobs j
		WHERE j.owner_id=%[1]s.owner_id AND j.corpus_uid=%[1]s.corpus_uid
		  AND j.document_id=%[1]s.document_id
		  AND j.document_generation=%[1]s.content_generation
		  AND j.kind='ingest'
		  AND EXISTS (SELECT 1 FROM kb_ingest_segments planned WHERE planned.job_id=j.job_id)
		ORDER BY j.created_at DESC,j.job_id DESC LIMIT 1
	)`, bindingAlias)
	return fmt.Sprintf(`(
		NOT EXISTS (
			SELECT 1 FROM kb_ingest_segments any_segment
			WHERE any_segment.job_id=%[1]s
		)
		OR (
			%[2]s.page_start IS NOT NULL AND %[2]s.page_end IS NOT NULL
			AND EXISTS (
				SELECT 1 FROM kb_ingest_segments ready_segment
				WHERE ready_segment.job_id=%[1]s AND ready_segment.state='ready'
				  AND %[2]s.page_start<=ready_segment.page_end
				  AND %[2]s.page_end>=ready_segment.page_start
			)
			AND NOT EXISTS (
				SELECT 1 FROM kb_ingest_segments blocked_segment
				WHERE blocked_segment.job_id=%[1]s AND blocked_segment.state<>'ready'
				  AND %[2]s.page_start<=blocked_segment.page_end
				  AND %[2]s.page_end>=blocked_segment.page_start
			)
		)
	)`, latestJob, chunkAlias)
}

func (r *SQLiteSemanticIndexRepository) loadJobFailure(
	ctx context.Context,
	jobID string,
) (*KnowledgeJobFailure, error) {
	var failure KnowledgeJobFailure
	var affectedPages string
	err := r.db.QueryRowContext(ctx, `SELECT code,message,affected_pages_json,
		provider_display_name,model,action_code FROM kb_job_failures WHERE job_id=?`,
		jobID).Scan(&failure.Code, &failure.Message, &affectedPages,
		&failure.ProviderDisplayName, &failure.Model, &failure.ActionCode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil, nil
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(affectedPages), &failure.AffectedPages); err != nil {
		return nil, err
	}
	return &failure, nil
}
