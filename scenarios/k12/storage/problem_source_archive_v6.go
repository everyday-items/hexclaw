package k12storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/viewcontract"
)

// ProblemSourceArchiveV6 is the checksum-covered, provider-envelope-free
// durability closure for V50/V51/V72/V73 source actions. It deliberately
// carries only the dispatch/model control-plane rows referenced by that
// closure. Raw physical result_content never crosses the archive boundary;
// the sole allowed result_json is the strictly decoded typed page-summary
// projection needed to finish a post-provider/pre-artifact crash locally.
type ProblemSourceArchiveV6 struct {
	PageAssets                 []ProblemSourceArchivePageAsset                 `json:"page_assets,omitempty"`
	Dispatches                 []k12.ImageTaskDispatch                         `json:"dispatches,omitempty"`
	DispatchOwners             []ProblemSourceArchiveDispatchOwner             `json:"dispatch_owners,omitempty"`
	HomeworkSubmissions        []k12.HomeworkSubmission                        `json:"homework_submissions,omitempty"`
	StructureSnapshots         []ProblemSourceArchiveStructureSnapshot         `json:"structure_snapshots,omitempty"`
	StructureMembers           []ProblemSourceArchiveStructureMember           `json:"structure_members,omitempty"`
	DependencyGroups           []ProblemSourceArchiveDependencyGroup           `json:"dependency_groups,omitempty"`
	ActionReceipts             []ProblemSourceArchiveActionReceipt             `json:"action_receipts,omitempty"`
	FinalizationGenerations    []ProblemSourceArchiveFinalizationGeneration    `json:"finalization_generations,omitempty"`
	InputRevisions             []ProblemSourceArchiveInputRevision             `json:"input_revisions,omitempty"`
	ReprocessJobs              []ProblemSourceArchiveReprocessJob              `json:"reprocess_jobs,omitempty"`
	ModelInvocations           []k12.ModelInvocation                           `json:"model_invocations,omitempty"`
	ModelPhysicalInvocations   []k12.ModelPhysicalInvocation                   `json:"model_physical_invocations,omitempty"`
	RecognitionResults         []ProblemSourceArchiveRecognitionResult         `json:"recognition_results,omitempty"`
	RecognitionItems           []ProblemSourceArchiveRecognitionItem           `json:"recognition_items,omitempty"`
	RecognitionPhysicalResults []ProblemSourceArchiveRecognitionPhysicalResult `json:"recognition_physical_results,omitempty"`
}

// IsEmpty reports whether the archive carries no source-action durability
// facts. It is intentionally structural: nil and empty slices are equivalent.
func (a ProblemSourceArchiveV6) IsEmpty() bool {
	return len(a.PageAssets) == 0 && len(a.Dispatches) == 0 &&
		len(a.DispatchOwners) == 0 && len(a.HomeworkSubmissions) == 0 &&
		len(a.StructureSnapshots) == 0 && len(a.StructureMembers) == 0 &&
		len(a.DependencyGroups) == 0 && len(a.ActionReceipts) == 0 &&
		len(a.FinalizationGenerations) == 0 &&
		len(a.InputRevisions) == 0 && len(a.ReprocessJobs) == 0 &&
		len(a.ModelInvocations) == 0 && len(a.ModelPhysicalInvocations) == 0 &&
		len(a.RecognitionResults) == 0 && len(a.RecognitionItems) == 0 &&
		len(a.RecognitionPhysicalResults) == 0
}

// ProblemSourceArchiveFinalizationGeneration freezes the aggregate CAS
// generation shared by source actions and the optional immutable final
// artifact. Artifact is typed and provider-payload-free; when it names a
// summary invocation that invocation must also be present in ModelInvocations.
type ProblemSourceArchiveFinalizationGeneration struct {
	AgentName  string                    `json:"agent_name"`
	JobID      string                    `json:"job_id"`
	Generation int64                     `json:"generation"`
	Artifact   *k12.GradingFinalArtifact `json:"artifact,omitempty"`
}

type ProblemSourceArchivePageAsset struct {
	OwnerScope               string `json:"owner_scope"`
	AgentName                string `json:"agent_name"`
	PageAssetID              string `json:"page_asset_id"`
	ContentDigest            string `json:"content_digest"`
	MediaType                string `json:"media_type"`
	SizeBytes                int64  `json:"size_bytes"`
	PixelWidth               int    `json:"pixel_width"`
	PixelHeight              int    `json:"pixel_height"`
	OrientationPolicy        string `json:"orientation_policy"`
	OrientationPolicyVersion string `json:"orientation_policy_version"`
	TransformChainJSON       string `json:"transform_chain_json"`
	StorageState             string `json:"storage_state"`
	ReadyAt                  int64  `json:"ready_at"`
	LastError                string `json:"last_error"`
	CreatedAt                int64  `json:"created_at"`
	UpdatedAt                int64  `json:"updated_at"`
}

type ProblemSourceArchiveDispatchOwner struct {
	DispatchID string `json:"dispatch_id"`
	OwnerScope string `json:"owner_scope"`
	AgentName  string `json:"agent_name"`
	CreatedAt  int64  `json:"created_at"`
}

type ProblemSourceArchiveStructureSnapshot struct {
	AgentName          string `json:"agent_name"`
	SubmissionID       string `json:"submission_id"`
	StructureVersion   int    `json:"structure_version"`
	StructureDigest    string `json:"structure_digest"`
	MappingState       string `json:"mapping_state"`
	CurrentDisposition string `json:"current_disposition"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}

type ProblemSourceArchiveStructureMember struct {
	AgentName             string `json:"agent_name"`
	SubmissionID          string `json:"submission_id"`
	StructureVersion      int    `json:"structure_version"`
	ProblemID             string `json:"problem_id"`
	Ordinal               int    `json:"ordinal"`
	ProblemKind           string `json:"problem_kind"`
	ParentProblemID       string `json:"parent_problem_id"`
	SubproblemNo          string `json:"subproblem_no"`
	SourceNumberPathJSON  string `json:"source_number_path_json"`
	DisplayLabel          string `json:"display_label"`
	SourceSectionPathJSON string `json:"source_section_path_json"`
	SourceSectionLabel    string `json:"source_section_label"`
	SystemSectionOrdinal  int    `json:"system_section_ordinal"`
	SystemDisplayLabel    string `json:"system_display_label"`
	DependencyGroupID     string `json:"dependency_group_id"`
	InputRevision         int    `json:"input_revision"`
}

type ProblemSourceArchiveDependencyGroup struct {
	AgentName         string `json:"agent_name"`
	SubmissionID      string `json:"submission_id"`
	StructureVersion  int    `json:"structure_version"`
	DependencyGroupID string `json:"dependency_group_id"`
	State             string `json:"state"`
	StateRevision     int    `json:"state_revision"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

type ProblemSourceArchiveActionReceipt struct {
	CommandReceiptID       string          `json:"command_receipt_id"`
	OwnerScope             string          `json:"owner_scope"`
	AgentName              string          `json:"agent_name"`
	DispatchID             string          `json:"dispatch_id"`
	JobID                  string          `json:"job_id"`
	ProblemID              string          `json:"problem_id"`
	IdempotencyKey         string          `json:"idempotency_key"`
	RequestDigest          string          `json:"request_digest"`
	Action                 string          `json:"action"`
	StructureVersion       int             `json:"structure_version"`
	ExpectedInputRevision  int             `json:"expected_input_revision"`
	ResultInputRevision    int             `json:"result_input_revision"`
	RequestJSON            json.RawMessage `json:"request_json"`
	AffectedProblemIDsJSON json.RawMessage `json:"affected_problem_ids_json"`
	ResponseJSON           json.RawMessage `json:"response_json"`
	CreatedAt              int64           `json:"created_at"`
	UpdatedAt              int64           `json:"updated_at"`
}

type ProblemSourceArchiveInputRevision struct {
	AgentName                 string          `json:"agent_name"`
	SubmissionID              string          `json:"submission_id"`
	StructureVersion          int             `json:"structure_version"`
	ProblemID                 string          `json:"problem_id"`
	InputRevision             int             `json:"input_revision"`
	PageAssetID               string          `json:"page_asset_id"`
	SourceRegionJSON          json.RawMessage `json:"source_region_json,omitempty"`
	StemRaw                   string          `json:"stem_raw"`
	AnswerRaw                 string          `json:"answer_raw"`
	AnswerBBoxJSON            string          `json:"answer_bbox_json"`
	QuestionCanonicalMarkdown string          `json:"question_canonical_markdown"`
	AnswerCanonicalMarkdown   string          `json:"answer_canonical_markdown"`
	InputDigest               string          `json:"input_digest"`
	CurrentDisposition        string          `json:"current_disposition"`
	OriginCommandReceiptID    string          `json:"origin_command_receipt_id,omitempty"`
	OriginKind                string          `json:"origin_kind"`
	CreatedAt                 int64           `json:"created_at"`
	UpdatedAt                 int64           `json:"updated_at"`
}

type ProblemSourceArchiveReprocessJob struct {
	WorkID                       string                       `json:"work_id"`
	CommandReceiptID             string                       `json:"command_receipt_id"`
	OwnerScope                   string                       `json:"owner_scope"`
	AgentName                    string                       `json:"agent_name"`
	DispatchID                   string                       `json:"dispatch_id"`
	JobID                        string                       `json:"job_id"`
	ProblemID                    string                       `json:"problem_id"`
	Action                       string                       `json:"action"`
	StructureVersion             int                          `json:"structure_version"`
	InputRevision                int                          `json:"input_revision"`
	InputDigest                  string                       `json:"input_digest"`
	AffectedProblemIDs           []string                     `json:"affected_problem_ids"`
	RequestJSON                  json.RawMessage              `json:"request_json"`
	Status                       ProblemSourceReprocessStatus `json:"status"`
	LeaseOwner                   string                       `json:"lease_owner"`
	LeaseEpoch                   int64                        `json:"lease_epoch"`
	LeaseExpiresAtMilli          int64                        `json:"lease_expires_at_milli"`
	AttemptCount                 int                          `json:"attempt_count"`
	NextAttemptAtMilli           int64                        `json:"next_attempt_at_milli"`
	ReconciliationOwner          string                       `json:"reconciliation_owner"`
	ReconciliationEpoch          int64                        `json:"reconciliation_epoch"`
	ReconciliationExpiresAtMilli int64                        `json:"reconciliation_expires_at_milli"`
	ReconciliationAttemptCount   int                          `json:"reconciliation_attempt_count"`
	NextReconcileAtMilli         int64                        `json:"next_reconcile_at_milli"`
	FailureCode                  string                       `json:"failure_code"`
	FailureDetail                string                       `json:"failure_detail"`
	CreatedAt                    int64                        `json:"created_at"`
	UpdatedAt                    int64                        `json:"updated_at"`
}

type ProblemSourceArchiveRecognitionResult struct {
	WorkID                  string          `json:"work_id"`
	CommandReceiptID        string          `json:"command_receipt_id"`
	OwnerScope              string          `json:"owner_scope"`
	AgentName               string          `json:"agent_name"`
	SubmissionID            string          `json:"submission_id"`
	DispatchID              string          `json:"dispatch_id"`
	JobID                   string          `json:"job_id"`
	PathProblemID           string          `json:"path_problem_id"`
	ParentInvocationID      string          `json:"parent_invocation_id"`
	ParentRequestDigest     string          `json:"parent_request_digest"`
	ParentInvocationAttempt int             `json:"parent_invocation_attempt"`
	Action                  string          `json:"action"`
	StructureVersion        int             `json:"structure_version"`
	SourceInputRevision     int             `json:"source_input_revision"`
	ResultInputRevision     int             `json:"result_input_revision"`
	ResultDigest            string          `json:"result_digest"`
	MappingState            string          `json:"mapping_state"`
	StructureDigest         string          `json:"structure_digest"`
	AffectedProblemIDsJSON  json.RawMessage `json:"affected_problem_ids_json"`
	CreatedAt               int64           `json:"created_at"`
}

type ProblemSourceArchiveRecognitionItem struct {
	WorkID                           string          `json:"work_id"`
	Ordinal                          int             `json:"ordinal"`
	OwnerScope                       string          `json:"owner_scope"`
	AgentName                        string          `json:"agent_name"`
	SubmissionID                     string          `json:"submission_id"`
	StructureVersion                 int             `json:"structure_version"`
	ProblemID                        string          `json:"problem_id"`
	SourceInputRevision              int             `json:"source_input_revision"`
	ResultInputRevision              int             `json:"result_input_revision"`
	InputDigest                      string          `json:"input_digest"`
	PageAssetID                      string          `json:"page_asset_id"`
	SourceRegionJSON                 json.RawMessage `json:"source_region_json,omitempty"`
	SourceContentDigest              string          `json:"source_content_digest"`
	SourceMediaType                  string          `json:"source_media_type"`
	SourceSizeBytes                  int64           `json:"source_size_bytes"`
	SourcePixelWidth                 int             `json:"source_pixel_width"`
	SourcePixelHeight                int             `json:"source_pixel_height"`
	SourceOrientationPolicy          string          `json:"source_orientation_policy"`
	SourceOrientationPolicyVersion   string          `json:"source_orientation_policy_version"`
	SourceTransformChainJSON         json.RawMessage `json:"source_transform_chain_json"`
	StemRaw                          string          `json:"stem_raw"`
	QuestionCanonicalMarkdown        string          `json:"question_canonical_markdown"`
	AnswerState                      string          `json:"answer_state"`
	AnswerRaw                        string          `json:"answer_raw"`
	AnswerCanonicalMarkdown          string          `json:"answer_canonical_markdown"`
	AnswerBBoxJSON                   string          `json:"answer_bbox_json"`
	Subject                          string          `json:"subject"`
	KnowledgePointsJSON              json.RawMessage `json:"knowledge_points_json"`
	RecognitionConfidence            *float64        `json:"recognition_confidence,omitempty"`
	OCRSignalsJSON                   json.RawMessage `json:"ocr_signals_json"`
	EvidenceTranscriptionsJSON       json.RawMessage `json:"evidence_transcriptions_json"`
	AnswerEvidenceTranscriptionsJSON json.RawMessage `json:"answer_evidence_transcriptions_json"`
	ConfirmationRequired             bool            `json:"confirmation_required"`
	ConfirmationReasonsJSON          json.RawMessage `json:"confirmation_reasons_json"`
	CreatedAt                        int64           `json:"created_at"`
}

type ProblemSourceArchiveRecognitionPhysicalResult struct {
	WorkID               string `json:"work_id"`
	Ordinal              int    `json:"ordinal"`
	ParentInvocationID   string `json:"parent_invocation_id"`
	PhysicalInvocationID string `json:"physical_invocation_id"`
	PhysicalUnit         string `json:"physical_unit"`
	ResultDigest         string `json:"result_digest"`
	CreatedAt            int64  `json:"created_at"`
}

func archivePageAsset(asset PageAssetMetadata) ProblemSourceArchivePageAsset {
	return ProblemSourceArchivePageAsset{
		OwnerScope: asset.OwnerScope, AgentName: asset.AgentName, PageAssetID: asset.PageAssetID,
		ContentDigest: asset.ContentDigest, MediaType: asset.MediaType, SizeBytes: asset.SizeBytes,
		PixelWidth: asset.PixelWidth, PixelHeight: asset.PixelHeight,
		OrientationPolicy: string(asset.OrientationPolicy), OrientationPolicyVersion: asset.OrientationPolicyVersion,
		TransformChainJSON: asset.TransformChainJSON, StorageState: string(asset.StorageState),
		ReadyAt: asset.ReadyAt, LastError: asset.LastError, CreatedAt: asset.CreatedAt, UpdatedAt: asset.UpdatedAt,
	}
}

func archiveReprocessJob(job ProblemSourceReprocessJob) ProblemSourceArchiveReprocessJob {
	return ProblemSourceArchiveReprocessJob{
		WorkID: job.WorkID, CommandReceiptID: job.CommandReceiptID, OwnerScope: job.OwnerScope,
		AgentName: job.AgentName, DispatchID: job.DispatchID, JobID: job.JobID, ProblemID: job.ProblemID,
		Action: job.Action, StructureVersion: job.StructureVersion, InputRevision: job.InputRevision,
		InputDigest: job.InputDigest, AffectedProblemIDs: append([]string(nil), job.AffectedProblemIDs...),
		RequestJSON: append(json.RawMessage(nil), job.RequestJSON...), Status: job.Status,
		LeaseOwner: job.LeaseOwner, LeaseEpoch: job.LeaseEpoch, LeaseExpiresAtMilli: job.LeaseExpiresAtMilli,
		AttemptCount: job.AttemptCount, NextAttemptAtMilli: job.NextAttemptAtMilli,
		ReconciliationOwner: job.ReconciliationOwner, ReconciliationEpoch: job.ReconciliationEpoch,
		ReconciliationExpiresAtMilli: job.ReconciliationExpiresAtMilli,
		ReconciliationAttemptCount:   job.ReconciliationAttemptCount,
		NextReconcileAtMilli:         job.NextReconcileAtMilli,
		FailureCode:                  job.FailureCode, FailureDetail: job.FailureDetail,
		CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
	}
}

// ExportProblemSourceArchiveV6 exports one Agent's exact source-action closure.
func (s *Store) ExportProblemSourceArchiveV6(ctx context.Context, agentName string) (ProblemSourceArchiveV6, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ProblemSourceArchiveV6{}, fmt.Errorf("begin problem-source archive snapshot: %w", err)
	}
	defer tx.Rollback()
	archive, err := s.exportProblemSourceArchiveV6Via(ctx, tx, agentName)
	if err != nil {
		return ProblemSourceArchiveV6{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProblemSourceArchiveV6{}, fmt.Errorf("commit problem-source archive snapshot: %w", err)
	}
	return archive, nil
}

func (s *Store) ExportProblemSourceArchiveV6Tx(ctx context.Context, tx *sql.Tx, agentName string) (ProblemSourceArchiveV6, error) {
	if tx == nil {
		return ProblemSourceArchiveV6{}, fmt.Errorf("k12storage: nil problem-source archive transaction")
	}
	return s.exportProblemSourceArchiveV6Via(ctx, tx, agentName)
}

func (s *Store) exportProblemSourceArchiveV6Via(ctx context.Context, q dbQueryer, agentName string) (ProblemSourceArchiveV6, error) {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return ProblemSourceArchiveV6{}, fmt.Errorf("k12storage: problem-source archive agent is empty")
	}
	var out ProblemSourceArchiveV6
	assetIDs := map[string]struct{}{}
	jobIDs := map[string]struct{}{}

	dispatchRows, err := q.QueryContext(ctx, imageTaskDispatchSelect+`
		WHERE agent_name=? AND EXISTS (
			SELECT 1 FROM k12_problem_source_action_receipts r
			WHERE r.dispatch_id=k12_image_task_dispatches.dispatch_id
		) ORDER BY created_at,dispatch_id`, agentName)
	if err != nil {
		return out, fmt.Errorf("export source dispatches: %w", err)
	}
	for dispatchRows.Next() {
		dispatch, scanErr := scanImageTaskDispatch(dispatchRows)
		if scanErr != nil {
			dispatchRows.Close()
			return out, scanErr
		}
		collectProblemSourcePageAssetRefs(assetIDs, dispatch.SourceAssetRefs)
		out.Dispatches = append(out.Dispatches, dispatch)
	}
	if err := rowsDone(dispatchRows); err != nil {
		return out, err
	}

	rows, err := q.QueryContext(ctx, `SELECT o.dispatch_id,o.owner_scope,o.agent_name,o.created_at
		FROM k12_image_task_owner_scopes o
		JOIN k12_problem_source_action_receipts r ON r.dispatch_id=o.dispatch_id
		WHERE o.agent_name=? GROUP BY o.dispatch_id ORDER BY o.created_at,o.dispatch_id`, agentName)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var item ProblemSourceArchiveDispatchOwner
		if err := rows.Scan(&item.DispatchID, &item.OwnerScope, &item.AgentName, &item.CreatedAt); err != nil {
			rows.Close()
			return out, err
		}
		out.DispatchOwners = append(out.DispatchOwners, item)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}

	rows, err = q.QueryContext(ctx, `SELECT submission_id,dispatch_id,agent_name,learner_id,
		source_kind,source_ref,source_asset_refs_json,task_intent,status,grading_job_id,
		idempotency_key,version,created_at,updated_at FROM k12_homework_submissions
		WHERE agent_name=? AND dispatch_id IN (SELECT dispatch_id FROM k12_problem_source_action_receipts WHERE agent_name=?)
		ORDER BY created_at,submission_id`, agentName, agentName)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var item k12.HomeworkSubmission
		var assets string
		if err := rows.Scan(&item.SubmissionID, &item.DispatchID, &item.AgentName, &item.LearnerID, &item.SourceKind,
			&item.SourceRef, &assets, &item.TaskIntent, &item.Status, &item.GradingJobID, &item.IdempotencyKey,
			&item.Version, &item.CreatedAt, &item.UpdatedAt); err != nil {
			rows.Close()
			return out, err
		}
		if err := json.Unmarshal([]byte(assets), &item.SourceAssetRefs); err != nil {
			rows.Close()
			return out, err
		}
		collectProblemSourcePageAssetRefs(assetIDs, item.SourceAssetRefs)
		out.HomeworkSubmissions = append(out.HomeworkSubmissions, item)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}

	rows, err = q.QueryContext(ctx, `SELECT agent_name,submission_id,structure_version,structure_digest,mapping_state,current_disposition,created_at,updated_at
		FROM k12_problem_structure_snapshots WHERE agent_name=? AND submission_id IN
		(SELECT p.submission_id FROM k12_problem_source_action_receipts r
		 JOIN k12_problems p ON p.agent_name=r.agent_name AND p.problem_id=r.problem_id
		 WHERE r.agent_name=?)
		ORDER BY submission_id,structure_version`, agentName, agentName)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v ProblemSourceArchiveStructureSnapshot
		if err := rows.Scan(&v.AgentName, &v.SubmissionID, &v.StructureVersion, &v.StructureDigest, &v.MappingState, &v.CurrentDisposition, &v.CreatedAt, &v.UpdatedAt); err != nil {
			rows.Close()
			return out, err
		}
		out.StructureSnapshots = append(out.StructureSnapshots, v)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}

	rows, err = q.QueryContext(ctx, `SELECT agent_name,submission_id,structure_version,problem_id,ordinal,problem_kind,parent_problem_id,subproblem_no,
		source_number_path_json,display_label,source_section_path_json,source_section_label,system_section_ordinal,system_display_label,dependency_group_id,input_revision
		FROM k12_problem_structure_members WHERE agent_name=? AND submission_id IN
		(SELECT p.submission_id FROM k12_problem_source_action_receipts r
		 JOIN k12_problems p ON p.agent_name=r.agent_name AND p.problem_id=r.problem_id
		 WHERE r.agent_name=?)
		ORDER BY submission_id,structure_version,ordinal,problem_id`, agentName, agentName)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v ProblemSourceArchiveStructureMember
		if err := rows.Scan(&v.AgentName, &v.SubmissionID, &v.StructureVersion, &v.ProblemID, &v.Ordinal, &v.ProblemKind, &v.ParentProblemID, &v.SubproblemNo, &v.SourceNumberPathJSON, &v.DisplayLabel, &v.SourceSectionPathJSON, &v.SourceSectionLabel, &v.SystemSectionOrdinal, &v.SystemDisplayLabel, &v.DependencyGroupID, &v.InputRevision); err != nil {
			rows.Close()
			return out, err
		}
		out.StructureMembers = append(out.StructureMembers, v)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}

	rows, err = q.QueryContext(ctx, `SELECT agent_name,submission_id,structure_version,dependency_group_id,state,state_revision,created_at,updated_at
		FROM k12_problem_dependency_groups WHERE agent_name=? AND submission_id IN
		(SELECT p.submission_id FROM k12_problem_source_action_receipts r
		 JOIN k12_problems p ON p.agent_name=r.agent_name AND p.problem_id=r.problem_id
		 WHERE r.agent_name=?)
		ORDER BY submission_id,structure_version,dependency_group_id`, agentName, agentName)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v ProblemSourceArchiveDependencyGroup
		if err := rows.Scan(&v.AgentName, &v.SubmissionID, &v.StructureVersion, &v.DependencyGroupID, &v.State, &v.StateRevision, &v.CreatedAt, &v.UpdatedAt); err != nil {
			rows.Close()
			return out, err
		}
		out.DependencyGroups = append(out.DependencyGroups, v)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}

	rows, err = q.QueryContext(ctx, `SELECT command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,problem_id,idempotency_key,request_digest,action,
		structure_version,expected_input_revision,result_input_revision,request_json,affected_problem_ids_json,response_json,created_at,updated_at
		FROM k12_problem_source_action_receipts WHERE agent_name=? ORDER BY created_at,command_receipt_id`, agentName)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v ProblemSourceArchiveActionReceipt
		var request, affected, response string
		if err := rows.Scan(&v.CommandReceiptID, &v.OwnerScope, &v.AgentName, &v.DispatchID, &v.JobID, &v.ProblemID, &v.IdempotencyKey, &v.RequestDigest, &v.Action, &v.StructureVersion, &v.ExpectedInputRevision, &v.ResultInputRevision, &request, &affected, &response, &v.CreatedAt, &v.UpdatedAt); err != nil {
			rows.Close()
			return out, err
		}
		v.RequestJSON = json.RawMessage(request)
		v.AffectedProblemIDsJSON = json.RawMessage(affected)
		v.ResponseJSON = json.RawMessage(response)
		jobIDs[v.JobID] = struct{}{}
		out.ActionReceipts = append(out.ActionReceipts, v)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}

	rows, err = q.QueryContext(ctx, `SELECT agent_name,submission_id,structure_version,problem_id,input_revision,page_asset_id,source_region_json,
		stem_raw,answer_raw,answer_bbox_json,question_canonical_markdown,answer_canonical_markdown,input_digest,current_disposition,
		origin_command_receipt_id,origin_kind,created_at,updated_at FROM k12_problem_input_revisions WHERE agent_name=?
		AND submission_id IN (
		 SELECT p.submission_id FROM k12_problem_source_action_receipts r
		 JOIN k12_problems p ON p.agent_name=r.agent_name AND p.problem_id=r.problem_id
		 WHERE r.agent_name=?
		)
		ORDER BY submission_id,structure_version,problem_id,input_revision`, agentName, agentName)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v ProblemSourceArchiveInputRevision
		var region, origin sql.NullString
		if err := rows.Scan(&v.AgentName, &v.SubmissionID, &v.StructureVersion, &v.ProblemID, &v.InputRevision, &v.PageAssetID, &region, &v.StemRaw, &v.AnswerRaw, &v.AnswerBBoxJSON, &v.QuestionCanonicalMarkdown, &v.AnswerCanonicalMarkdown, &v.InputDigest, &v.CurrentDisposition, &origin, &v.OriginKind, &v.CreatedAt, &v.UpdatedAt); err != nil {
			rows.Close()
			return out, err
		}
		if region.Valid {
			v.SourceRegionJSON = json.RawMessage(region.String)
		}
		if origin.Valid {
			v.OriginCommandReceiptID = origin.String
		}
		assetIDs[v.PageAssetID] = struct{}{}
		out.InputRevisions = append(out.InputRevisions, v)
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}

	rows, err = q.QueryContext(ctx, `SELECT `+problemSourceReprocessColumns+` FROM k12_problem_source_reprocess_jobs WHERE agent_name=? ORDER BY created_at,work_id`, agentName)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		v, scanErr := scanProblemSourceReprocessJob(rows)
		if scanErr != nil {
			rows.Close()
			return out, scanErr
		}
		out.ReprocessJobs = append(out.ReprocessJobs, archiveReprocessJob(v))
	}
	if err := rowsDone(rows); err != nil {
		return out, err
	}

	if err := exportProblemSourceRecognitionArchive(ctx, q, agentName, &out, assetIDs); err != nil {
		return out, err
	}
	if err := exportProblemSourceFinalizations(ctx, q, agentName, &out, jobIDs); err != nil {
		return out, err
	}
	assetList := make([]string, 0, len(assetIDs))
	for id := range assetIDs {
		assetList = append(assetList, id)
	}
	sort.Strings(assetList)
	for _, id := range assetList {
		asset, scanErr := scanPageAsset(q.QueryRowContext(ctx, `SELECT `+pageAssetColumns+` FROM k12_page_assets WHERE agent_name=? AND page_asset_id=?`, agentName, id))
		if scanErr != nil {
			return out, fmt.Errorf("export PageAsset %q: %w", id, scanErr)
		}
		out.PageAssets = append(out.PageAssets, archivePageAsset(asset))
	}
	if err := ValidateProblemSourceArchiveV6(agentName, out); err != nil {
		return ProblemSourceArchiveV6{}, err
	}
	return out, nil
}

func exportProblemSourceFinalizations(
	ctx context.Context,
	q dbQueryer,
	agentName string,
	out *ProblemSourceArchiveV6,
	jobIDs map[string]struct{},
) error {
	jobs := make([]string, 0, len(jobIDs))
	for jobID := range jobIDs {
		jobs = append(jobs, jobID)
	}
	sort.Strings(jobs)
	modelIDs := make(map[string]struct{}, len(out.ModelInvocations))
	for _, invocation := range out.ModelInvocations {
		modelIDs[invocation.InvocationID] = struct{}{}
	}
	for _, jobID := range jobs {
		var generation int64
		if err := q.QueryRowContext(ctx, `SELECT finalization_generation
			FROM k12_grading_jobs WHERE agent_name=? AND record_id=?`,
			agentName, jobID,
		).Scan(&generation); err != nil {
			return fmt.Errorf("export source finalization generation %q: %w", jobID, err)
		}
		state := ProblemSourceArchiveFinalizationGeneration{
			AgentName: agentName, JobID: jobID, Generation: generation,
		}
		artifact, err := getGradingFinalArtifactByJobVia(ctx, q, agentName, jobID)
		if err != nil && !errors.Is(err, records.ErrNotFound) {
			return err
		}
		if err == nil {
			var artifactGeneration int64
			if err := q.QueryRowContext(ctx, `SELECT finalization_generation
				FROM k12_grading_final_artifacts
				WHERE agent_name=? AND job_id=?`, agentName, jobID,
			).Scan(&artifactGeneration); err != nil {
				return fmt.Errorf("export source final artifact generation %q: %w", jobID, err)
			}
			if artifactGeneration != generation {
				return fmt.Errorf("source finalization generation mismatch for job %q: job=%d artifact=%d", jobID, generation, artifactGeneration)
			}
			artifactCopy := artifact
			state.Artifact = &artifactCopy
			if artifact.SummaryInvocationID != "" {
				if _, exists := modelIDs[artifact.SummaryInvocationID]; !exists {
					invocation, err := getModelInvocationByIDVia(ctx, q, artifact.SummaryInvocationID)
					if err != nil {
						return fmt.Errorf("export final artifact summary invocation %q: %w", artifact.SummaryInvocationID, err)
					}
					if invocation.AgentName != agentName || invocation.JobID != jobID {
						return fmt.Errorf("final artifact summary invocation scope mismatch")
					}
					invocation.ProviderIdempotencyKey = ""
					invocation.ExternalRequestID = ""
					out.ModelInvocations = append(out.ModelInvocations, invocation)
					modelIDs[invocation.InvocationID] = struct{}{}
				}
			}
		}
		rows, err := q.QueryContext(ctx, `SELECT `+modelInvocationColumns+`
			FROM k12_model_invocations
			WHERE agent_name=? AND job_id=? AND stage=?
			ORDER BY attempt,invocation_id`,
			agentName, jobID, k12.GradingStageProjecting,
		)
		if err != nil {
			return fmt.Errorf("export source projecting invocations for job %q: %w", jobID, err)
		}
		for rows.Next() {
			invocation, scanErr := scanModelInvocation(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			if _, exists := modelIDs[invocation.InvocationID]; exists {
				continue
			}
			invocation.ProviderIdempotencyKey = ""
			invocation.ExternalRequestID = ""
			out.ModelInvocations = append(out.ModelInvocations, invocation)
			modelIDs[invocation.InvocationID] = struct{}{}
		}
		if err := rowsDone(rows); err != nil {
			return err
		}
		out.FinalizationGenerations = append(out.FinalizationGenerations, state)
	}
	sort.Slice(out.ModelInvocations, func(i, j int) bool {
		return out.ModelInvocations[i].InvocationID < out.ModelInvocations[j].InvocationID
	})
	return nil
}

func collectProblemSourcePageAssetRefs(ids map[string]struct{}, refs []string) {
	for _, ref := range refs {
		if strings.HasPrefix(ref, "asset://") {
			ids[ref] = struct{}{}
		}
	}
}

func rowsDone(rows *sql.Rows) error {
	err := rows.Err()
	closeErr := rows.Close()
	return errors.Join(err, closeErr)
}

func exportProblemSourceRecognitionArchive(ctx context.Context, q dbQueryer, agentName string, out *ProblemSourceArchiveV6, assetIDs map[string]struct{}) error {
	rows, err := q.QueryContext(ctx, `SELECT work_id,command_receipt_id,owner_scope,agent_name,submission_id,dispatch_id,job_id,path_problem_id,
		parent_invocation_id,parent_request_digest,parent_invocation_attempt,action,structure_version,source_input_revision,result_input_revision,
		result_digest,mapping_state,structure_digest,affected_problem_ids_json,created_at FROM k12_problem_source_recognition_results
		WHERE agent_name=? ORDER BY created_at,work_id`, agentName)
	if err != nil {
		return err
	}
	parentIDs := map[string]struct{}{}
	for rows.Next() {
		var v ProblemSourceArchiveRecognitionResult
		var affected string
		if err := rows.Scan(&v.WorkID, &v.CommandReceiptID, &v.OwnerScope, &v.AgentName, &v.SubmissionID, &v.DispatchID, &v.JobID, &v.PathProblemID, &v.ParentInvocationID, &v.ParentRequestDigest, &v.ParentInvocationAttempt, &v.Action, &v.StructureVersion, &v.SourceInputRevision, &v.ResultInputRevision, &v.ResultDigest, &v.MappingState, &v.StructureDigest, &affected, &v.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		v.AffectedProblemIDsJSON = json.RawMessage(affected)
		parentIDs[v.ParentInvocationID] = struct{}{}
		out.RecognitionResults = append(out.RecognitionResults, v)
	}
	if err := rowsDone(rows); err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT work_id,ordinal,owner_scope,agent_name,submission_id,structure_version,problem_id,source_input_revision,
		result_input_revision,input_digest,page_asset_id,source_region_json,source_content_digest,source_media_type,source_size_bytes,
		source_pixel_width,source_pixel_height,source_orientation_policy,source_orientation_policy_version,source_transform_chain_json,
		stem_raw,question_canonical_markdown,answer_state,answer_raw,answer_canonical_markdown,answer_bbox_json,subject,
		knowledge_points_json,recognition_confidence,ocr_signals_json,evidence_transcriptions_json,answer_evidence_transcriptions_json,
		confirmation_required,confirmation_reasons_json,created_at FROM k12_problem_source_recognition_items WHERE agent_name=? ORDER BY work_id,ordinal`, agentName)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v ProblemSourceArchiveRecognitionItem
		var region sql.NullString
		var transform, kp, signals, evidence, answerEvidence, reasons string
		var confidence sql.NullFloat64
		var confirm int
		if err := rows.Scan(&v.WorkID, &v.Ordinal, &v.OwnerScope, &v.AgentName, &v.SubmissionID, &v.StructureVersion, &v.ProblemID, &v.SourceInputRevision, &v.ResultInputRevision, &v.InputDigest, &v.PageAssetID, &region, &v.SourceContentDigest, &v.SourceMediaType, &v.SourceSizeBytes, &v.SourcePixelWidth, &v.SourcePixelHeight, &v.SourceOrientationPolicy, &v.SourceOrientationPolicyVersion, &transform, &v.StemRaw, &v.QuestionCanonicalMarkdown, &v.AnswerState, &v.AnswerRaw, &v.AnswerCanonicalMarkdown, &v.AnswerBBoxJSON, &v.Subject, &kp, &confidence, &signals, &evidence, &answerEvidence, &confirm, &reasons, &v.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		if region.Valid {
			v.SourceRegionJSON = json.RawMessage(region.String)
		}
		v.SourceTransformChainJSON = json.RawMessage(transform)
		v.KnowledgePointsJSON = json.RawMessage(kp)
		if confidence.Valid {
			c := confidence.Float64
			v.RecognitionConfidence = &c
		}
		v.OCRSignalsJSON = json.RawMessage(signals)
		v.EvidenceTranscriptionsJSON = json.RawMessage(evidence)
		v.AnswerEvidenceTranscriptionsJSON = json.RawMessage(answerEvidence)
		v.ConfirmationRequired = confirm != 0
		v.ConfirmationReasonsJSON = json.RawMessage(reasons)
		assetIDs[v.PageAssetID] = struct{}{}
		out.RecognitionItems = append(out.RecognitionItems, v)
	}
	if err := rowsDone(rows); err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT p.work_id,p.ordinal,p.parent_invocation_id,p.physical_invocation_id,p.physical_unit,p.result_digest,p.created_at
		FROM k12_problem_source_recognition_physical_results p JOIN k12_problem_source_recognition_results r ON r.work_id=p.work_id
		WHERE r.agent_name=? ORDER BY p.work_id,p.ordinal`, agentName)
	if err != nil {
		return err
	}
	physicalIDs := map[string]struct{}{}
	for rows.Next() {
		var v ProblemSourceArchiveRecognitionPhysicalResult
		if err := rows.Scan(&v.WorkID, &v.Ordinal, &v.ParentInvocationID, &v.PhysicalInvocationID, &v.PhysicalUnit, &v.ResultDigest, &v.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		physicalIDs[v.PhysicalInvocationID] = struct{}{}
		out.RecognitionPhysicalResults = append(out.RecognitionPhysicalResults, v)
	}
	if err := rowsDone(rows); err != nil {
		return err
	}
	parents := make([]string, 0, len(parentIDs))
	for id := range parentIDs {
		parents = append(parents, id)
	}
	sort.Strings(parents)
	for _, id := range parents {
		v, err := getModelInvocationByIDVia(ctx, q, id)
		if err != nil {
			return err
		}
		// Provider control identifiers are neither needed to rebuild typed V73
		// facts nor safe to replay after restore. Keep only internal IDs, route,
		// status and digests in the checksum-covered archive.
		v.ProviderIdempotencyKey = ""
		v.ExternalRequestID = ""
		out.ModelInvocations = append(out.ModelInvocations, v)
	}
	physical := make([]string, 0, len(physicalIDs))
	for id := range physicalIDs {
		physical = append(physical, id)
	}
	sort.Strings(physical)
	for _, id := range physical {
		v, err := getModelPhysicalInvocationByIDVia(ctx, q, agentName, id)
		if err != nil {
			return err
		}
		v.ExternalRequestID = ""
		out.ModelPhysicalInvocations = append(out.ModelPhysicalInvocations, v)
	}
	return nil
}

type problemSourceArchiveStructureIndex struct {
	snapshots           map[string]ProblemSourceArchiveStructureSnapshot
	members             map[string]ProblemSourceArchiveStructureMember
	membersBySnapshot   map[string][]ProblemSourceArchiveStructureMember
	groups              map[string]ProblemSourceArchiveDependencyGroup
	currentBySubmission map[string]ProblemSourceArchiveStructureSnapshot
}

func problemSourceArchiveStructureKey(submissionID string, structureVersion int) string {
	return strings.TrimSpace(submissionID) + "\x00" + fmt.Sprint(structureVersion)
}

func problemSourceArchiveStructureMemberKey(
	submissionID string,
	structureVersion int,
	problemID string,
) string {
	return problemSourceArchiveStructureKey(submissionID, structureVersion) +
		"\x00" + strings.TrimSpace(problemID)
}

func problemSourceArchiveStructureGroupKey(
	submissionID string,
	structureVersion int,
	dependencyGroupID string,
) string {
	return problemSourceArchiveStructureKey(submissionID, structureVersion) +
		"\x00" + strings.TrimSpace(dependencyGroupID)
}

func validateProblemSourceArchiveStructures(
	agentName string,
	archive ProblemSourceArchiveV6,
) (problemSourceArchiveStructureIndex, error) {
	index := problemSourceArchiveStructureIndex{
		snapshots:           make(map[string]ProblemSourceArchiveStructureSnapshot),
		members:             make(map[string]ProblemSourceArchiveStructureMember),
		membersBySnapshot:   make(map[string][]ProblemSourceArchiveStructureMember),
		groups:              make(map[string]ProblemSourceArchiveDependencyGroup),
		currentBySubmission: make(map[string]ProblemSourceArchiveStructureSnapshot),
	}
	for _, snapshot := range archive.StructureSnapshots {
		key := problemSourceArchiveStructureKey(
			snapshot.SubmissionID, snapshot.StructureVersion,
		)
		if snapshot.AgentName != agentName ||
			strings.TrimSpace(snapshot.SubmissionID) == "" ||
			snapshot.StructureVersion < 1 ||
			(snapshot.MappingState != "resolved" &&
				snapshot.MappingState != "fail_closed") ||
			(snapshot.CurrentDisposition != "current" &&
				snapshot.CurrentDisposition != "superseded") {
			return index, fmt.Errorf("problem-source structure snapshot scope/value mismatch")
		}
		if _, duplicate := index.snapshots[key]; duplicate {
			return index, fmt.Errorf(
				"duplicate problem-source structure snapshot %q/%d",
				snapshot.SubmissionID, snapshot.StructureVersion,
			)
		}
		index.snapshots[key] = snapshot
		if snapshot.CurrentDisposition == "current" {
			if _, duplicate := index.currentBySubmission[snapshot.SubmissionID]; duplicate {
				return index, fmt.Errorf(
					"duplicate current problem-source structure for submission %q",
					snapshot.SubmissionID,
				)
			}
			index.currentBySubmission[snapshot.SubmissionID] = snapshot
		}
	}

	ordinalKeys := make(map[string]struct{}, len(archive.StructureMembers))
	for _, member := range archive.StructureMembers {
		snapshotKey := problemSourceArchiveStructureKey(
			member.SubmissionID, member.StructureVersion,
		)
		if member.AgentName != agentName ||
			strings.TrimSpace(member.SubmissionID) == "" ||
			strings.TrimSpace(member.ProblemID) == "" ||
			strings.TrimSpace(member.DependencyGroupID) == "" ||
			member.StructureVersion < 1 || member.Ordinal < 0 ||
			member.InputRevision < 1 {
			return index, fmt.Errorf("problem-source structure member scope/value mismatch")
		}
		if _, ok := index.snapshots[snapshotKey]; !ok {
			return index, fmt.Errorf("problem-source structure member snapshot missing")
		}
		switch member.ProblemKind {
		case "standalone":
			if member.ParentProblemID != "" || member.SubproblemNo != "" {
				return index, fmt.Errorf("problem-source standalone member parent semantics invalid")
			}
		case "compound_parent":
			if member.ParentProblemID != "" {
				return index, fmt.Errorf("problem-source compound parent semantics invalid")
			}
		case "subproblem":
			if strings.TrimSpace(member.ParentProblemID) == "" ||
				strings.TrimSpace(member.SubproblemNo) == "" {
				return index, fmt.Errorf("problem-source subproblem parent semantics invalid")
			}
		default:
			return index, fmt.Errorf("problem-source structure member kind invalid")
		}
		var sourcePath, sectionPath []string
		if err := json.Unmarshal([]byte(member.SourceNumberPathJSON), &sourcePath); err != nil {
			return index, fmt.Errorf("problem-source member source number path invalid: %w", err)
		}
		if err := json.Unmarshal([]byte(member.SourceSectionPathJSON), &sectionPath); err != nil {
			return index, fmt.Errorf("problem-source member source section path invalid: %w", err)
		}
		memberKey := problemSourceArchiveStructureMemberKey(
			member.SubmissionID, member.StructureVersion, member.ProblemID,
		)
		if _, duplicate := index.members[memberKey]; duplicate {
			return index, fmt.Errorf(
				"duplicate problem-source structure member %q/%d/%q",
				member.SubmissionID, member.StructureVersion, member.ProblemID,
			)
		}
		ordinalKey := snapshotKey + "\x00" + fmt.Sprint(member.Ordinal)
		if _, duplicate := ordinalKeys[ordinalKey]; duplicate {
			return index, fmt.Errorf("duplicate problem-source structure member ordinal")
		}
		ordinalKeys[ordinalKey] = struct{}{}
		index.members[memberKey] = member
		index.membersBySnapshot[snapshotKey] = append(
			index.membersBySnapshot[snapshotKey], member,
		)
	}

	for _, group := range archive.DependencyGroups {
		key := problemSourceArchiveStructureGroupKey(
			group.SubmissionID, group.StructureVersion, group.DependencyGroupID,
		)
		if group.AgentName != agentName ||
			strings.TrimSpace(group.SubmissionID) == "" ||
			strings.TrimSpace(group.DependencyGroupID) == "" ||
			group.StructureVersion < 1 || group.StateRevision < 1 {
			return index, fmt.Errorf("problem-source dependency group scope/value mismatch")
		}
		switch group.State {
		case "pending", "ready", "blocked", "processing", "completed", "failed":
		default:
			return index, fmt.Errorf("problem-source dependency group state invalid")
		}
		if _, ok := index.snapshots[problemSourceArchiveStructureKey(
			group.SubmissionID, group.StructureVersion,
		)]; !ok {
			return index, fmt.Errorf("problem-source dependency group snapshot missing")
		}
		if _, duplicate := index.groups[key]; duplicate {
			return index, fmt.Errorf(
				"duplicate problem-source dependency group %q/%d/%q",
				group.SubmissionID, group.StructureVersion, group.DependencyGroupID,
			)
		}
		index.groups[key] = group
	}

	for key, snapshot := range index.snapshots {
		members := index.membersBySnapshot[key]
		if len(members) == 0 {
			return index, fmt.Errorf("problem-source structure snapshot has no members")
		}
		facts := make([]problemStructureMember, 0, len(members))
		for _, member := range members {
			if _, ok := index.groups[problemSourceArchiveStructureGroupKey(
				member.SubmissionID,
				member.StructureVersion,
				member.DependencyGroupID,
			)]; !ok {
				return index, fmt.Errorf("problem-source structure member dependency group missing")
			}
			if member.ProblemKind == "subproblem" {
				parent, ok := index.members[problemSourceArchiveStructureMemberKey(
					member.SubmissionID,
					member.StructureVersion,
					member.ParentProblemID,
				)]
				if !ok || parent.ProblemKind != "compound_parent" {
					return index, fmt.Errorf("problem-source subproblem parent binding invalid")
				}
			}
			var sourcePath, sectionPath []string
			_ = json.Unmarshal([]byte(member.SourceNumberPathJSON), &sourcePath)
			_ = json.Unmarshal([]byte(member.SourceSectionPathJSON), &sectionPath)
			facts = append(facts, problemStructureMember{
				ProblemID: member.ProblemID, Ordinal: member.Ordinal,
				ProblemKind: member.ProblemKind, ParentProblemID: member.ParentProblemID,
				SubproblemNo: member.SubproblemNo, SourceNumberPath: sourcePath,
				DisplayLabel: member.DisplayLabel, SourceSectionPath: sectionPath,
				SourceSectionLabel:   member.SourceSectionLabel,
				SystemSectionOrdinal: member.SystemSectionOrdinal,
				SystemDisplayLabel:   member.SystemDisplayLabel,
				DependencyGroupID:    member.DependencyGroupID,
				InputRevision:        member.InputRevision,
			})
		}
		sort.Slice(facts, func(i, j int) bool {
			if facts[i].Ordinal == facts[j].Ordinal {
				return facts[i].ProblemID < facts[j].ProblemID
			}
			return facts[i].Ordinal < facts[j].Ordinal
		})
		raw, err := json.Marshal(facts)
		if err != nil {
			return index, err
		}
		digest := sha256.Sum256(raw)
		if snapshot.StructureDigest != hex.EncodeToString(digest[:]) {
			return index, fmt.Errorf(
				"problem-source structure digest mismatch for %q/%d",
				snapshot.SubmissionID, snapshot.StructureVersion,
			)
		}
	}
	for key := range index.groups {
		found := false
		for _, member := range archive.StructureMembers {
			if key == problemSourceArchiveStructureGroupKey(
				member.SubmissionID,
				member.StructureVersion,
				member.DependencyGroupID,
			) {
				found = true
				break
			}
		}
		if !found {
			return index, fmt.Errorf("unreferenced problem-source dependency group")
		}
	}
	for _, snapshot := range index.snapshots {
		if _, ok := index.currentBySubmission[snapshot.SubmissionID]; !ok {
			return index, fmt.Errorf(
				"problem-source structure submission %q has no current snapshot",
				snapshot.SubmissionID,
			)
		}
	}
	return index, nil
}

func (index problemSourceArchiveStructureIndex) affectedProblemIDs(
	receipt ProblemSourceArchiveActionReceipt,
) ([]string, ProblemSourceArchiveStructureSnapshot, error) {
	var path ProblemSourceArchiveStructureMember
	found := false
	for _, candidate := range index.members {
		if candidate.StructureVersion == receipt.StructureVersion &&
			candidate.ProblemID == receipt.ProblemID {
			if found {
				return nil, ProblemSourceArchiveStructureSnapshot{}, fmt.Errorf(
					"problem-source receipt path member is ambiguous",
				)
			}
			path = candidate
			found = true
		}
	}
	if !found {
		return nil, ProblemSourceArchiveStructureSnapshot{}, fmt.Errorf(
			"problem-source receipt path member is missing",
		)
	}
	snapshot, ok := index.snapshots[problemSourceArchiveStructureKey(
		path.SubmissionID, path.StructureVersion,
	)]
	if !ok || snapshot.MappingState != "resolved" {
		return nil, ProblemSourceArchiveStructureSnapshot{}, fmt.Errorf(
			"problem-source receipt structure is not resolved",
		)
	}
	members := append([]ProblemSourceArchiveStructureMember(nil),
		index.membersBySnapshot[problemSourceArchiveStructureKey(
			path.SubmissionID, path.StructureVersion,
		)]...,
	)
	sort.Slice(members, func(i, j int) bool {
		if members[i].Ordinal == members[j].Ordinal {
			return members[i].ProblemID < members[j].ProblemID
		}
		return members[i].Ordinal < members[j].Ordinal
	})
	affected := make([]string, 0, len(members))
	pathFound := path.ProblemKind == "compound_parent"
	for _, member := range members {
		if member.DependencyGroupID != path.DependencyGroupID ||
			member.ProblemKind == "compound_parent" {
			continue
		}
		affected = append(affected, member.ProblemID)
		if member.ProblemID == path.ProblemID {
			pathFound = true
		}
	}
	if len(affected) == 0 || !pathFound {
		return nil, ProblemSourceArchiveStructureSnapshot{}, fmt.Errorf(
			"problem-source receipt affected structure is invalid",
		)
	}
	return affected, snapshot, nil
}

func sameProblemSourceArchiveStringsOrdered(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if strings.TrimSpace(left[index]) == "" || left[index] != right[index] {
			return false
		}
	}
	return true
}

// ValidateProblemSourceArchiveV6 is side-effect free and rejects partial,
// cross-owner or unsigned/invalid frozen response evidence before restore.
func ValidateProblemSourceArchiveV6(agentName string, archive ProblemSourceArchiveV6) error {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return fmt.Errorf("problem-source archive agent is empty")
	}
	referencedAssets := map[string]struct{}{}
	dispatches := map[string]k12.ImageTaskDispatch{}
	for _, v := range archive.Dispatches {
		if v.AgentName != agentName || v.DispatchID == "" {
			return fmt.Errorf("problem-source dispatch scope mismatch")
		}
		if _, ok := dispatches[v.DispatchID]; ok {
			return fmt.Errorf("duplicate problem-source dispatch %q", v.DispatchID)
		}
		if err := validateProblemSourcePageAssetRefs(referencedAssets, v.SourceAssetRefs); err != nil {
			return fmt.Errorf("problem-source dispatch %q: %w", v.DispatchID, err)
		}
		dispatches[v.DispatchID] = v
	}
	owners := map[string]ProblemSourceArchiveDispatchOwner{}
	for _, v := range archive.DispatchOwners {
		if v.AgentName != agentName || strings.TrimSpace(v.OwnerScope) == "" ||
			strings.TrimSpace(v.DispatchID) == "" {
			return fmt.Errorf("problem-source dispatch owner mismatch")
		}
		if _, duplicate := owners[v.DispatchID]; duplicate {
			return fmt.Errorf("duplicate problem-source dispatch owner %q", v.DispatchID)
		}
		if _, ok := dispatches[v.DispatchID]; !ok {
			return fmt.Errorf("unreferenced problem-source dispatch owner %q", v.DispatchID)
		}
		owners[v.DispatchID] = v
	}
	for id := range dispatches {
		if _, ok := owners[id]; !ok {
			return fmt.Errorf("problem-source dispatch %q has no immutable owner", id)
		}
	}
	homeworkByID := make(map[string]k12.HomeworkSubmission, len(archive.HomeworkSubmissions))
	homeworkByDispatch := make(map[string]k12.HomeworkSubmission, len(archive.HomeworkSubmissions))
	for _, v := range archive.HomeworkSubmissions {
		if v.AgentName != agentName || strings.TrimSpace(v.SubmissionID) == "" ||
			strings.TrimSpace(v.DispatchID) == "" {
			return fmt.Errorf("problem-source homework owner mismatch")
		}
		if _, duplicate := homeworkByID[v.SubmissionID]; duplicate {
			return fmt.Errorf("duplicate problem-source homework %q", v.SubmissionID)
		}
		if _, duplicate := homeworkByDispatch[v.DispatchID]; duplicate {
			return fmt.Errorf("duplicate problem-source homework dispatch %q", v.DispatchID)
		}
		if _, ok := dispatches[v.DispatchID]; !ok {
			return fmt.Errorf("problem-source homework dispatch missing")
		}
		if err := validateProblemSourcePageAssetRefs(referencedAssets, v.SourceAssetRefs); err != nil {
			return fmt.Errorf("problem-source homework %q: %w", v.SubmissionID, err)
		}
		homeworkByID[v.SubmissionID] = v
		homeworkByDispatch[v.DispatchID] = v
	}
	assets := map[string]ProblemSourceArchivePageAsset{}
	for _, v := range archive.PageAssets {
		if v.AgentName != agentName || strings.TrimSpace(v.OwnerScope) == "" ||
			v.PageAssetID == "" || v.StorageState != "ready" || v.ReadyAt <= 0 ||
			v.LastError != "" || v.SizeBytes <= 0 || v.PixelWidth <= 0 ||
			v.PixelHeight <= 0 {
			return fmt.Errorf("problem-source PageAsset is not owner-scoped ready")
		}
		extension := map[string]string{
			"image/png": ".png", "image/jpeg": ".jpg",
			"image/gif": ".gif", "image/webp": ".webp",
		}[v.MediaType]
		if extension == "" || len(v.ContentDigest) != 64 ||
			v.PageAssetID != "asset://"+agentName+"/"+v.ContentDigest+extension {
			return fmt.Errorf("problem-source PageAsset content identity is invalid")
		}
		for _, c := range v.ContentDigest {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return fmt.Errorf("problem-source PageAsset digest is invalid")
			}
		}
		if v.OrientationPolicy != string(PageAssetOrientationUnverified) &&
			v.OrientationPolicy != string(PageAssetOrientationVerified) {
			return fmt.Errorf("problem-source PageAsset orientation policy is invalid")
		}
		if !json.Valid([]byte(v.TransformChainJSON)) {
			return fmt.Errorf("problem-source PageAsset transform is invalid")
		}
		if _, duplicate := assets[v.PageAssetID]; duplicate {
			return fmt.Errorf("duplicate problem-source PageAsset %q", v.PageAssetID)
		}
		assets[v.PageAssetID] = v
	}
	structureIndex, err := validateProblemSourceArchiveStructures(agentName, archive)
	if err != nil {
		return err
	}
	receipts := map[string]ProblemSourceArchiveActionReceipt{}
	receiptAffected := map[string][]string{}
	receiptStructures := map[string]ProblemSourceArchiveStructureSnapshot{}
	receiptSubmissions := map[string]string{}
	receiptIdempotencyKeys := map[string]struct{}{}
	receiptDispatches := map[string]struct{}{}
	receiptJobs := map[string]struct{}{}
	for _, v := range archive.ActionReceipts {
		owner, ok := owners[v.DispatchID]
		if v.AgentName != agentName || !ok || owner.OwnerScope != v.OwnerScope {
			return fmt.Errorf("problem-source receipt scope mismatch")
		}
		if strings.TrimSpace(v.CommandReceiptID) == "" ||
			strings.TrimSpace(v.JobID) == "" ||
			strings.TrimSpace(v.ProblemID) == "" ||
			strings.TrimSpace(v.IdempotencyKey) == "" ||
			!json.Valid(v.RequestJSON) || !json.Valid(v.AffectedProblemIDsJSON) {
			return fmt.Errorf("problem-source receipt JSON invalid")
		}
		if _, duplicate := receipts[v.CommandReceiptID]; duplicate {
			return fmt.Errorf("duplicate problem-source receipt %q", v.CommandReceiptID)
		}
		idempotencyKey := v.OwnerScope + "\x00" + v.IdempotencyKey
		if _, duplicate := receiptIdempotencyKeys[idempotencyKey]; duplicate {
			return fmt.Errorf("duplicate problem-source receipt idempotency identity")
		}
		receiptIdempotencyKeys[idempotencyKey] = struct{}{}
		var request problemSourceArchiveActionRequest
		if err := decodeProblemSourceActionPayloadStrict(v.RequestJSON, &request); err != nil {
			return fmt.Errorf("problem-source receipt request invalid: %w", err)
		}
		if request.Action != v.Action ||
			request.StructureVersion != v.StructureVersion ||
			request.ExpectedInputRevision != v.ExpectedInputRevision {
			return fmt.Errorf("problem-source receipt request identity mismatch")
		}
		canonicalPayload, err := canonicalProblemSourceActionPayload(v.Action, request.Payload)
		if err != nil {
			return fmt.Errorf("problem-source receipt request invalid: %w", err)
		}
		requestDigest, err := problemSourceActionDigest(ProblemSourceActionCommand{
			OwnerScope: v.OwnerScope, DispatchID: v.DispatchID,
			ProblemID: v.ProblemID, Action: v.Action,
			StructureVersion:      v.StructureVersion,
			ExpectedInputRevision: v.ExpectedInputRevision,
			Payload:               canonicalPayload,
		}, agentName)
		if err != nil {
			return fmt.Errorf("problem-source receipt request digest: %w", err)
		}
		if v.RequestDigest != requestDigest {
			return fmt.Errorf("problem-source receipt request digest mismatch")
		}
		var affected []string
		if err := json.Unmarshal(v.AffectedProblemIDsJSON, &affected); err != nil {
			return fmt.Errorf("problem-source receipt affected JSON invalid: %w", err)
		}
		structureAffected, snapshot, err := structureIndex.affectedProblemIDs(v)
		if err != nil {
			return err
		}
		if !sameProblemSourceArchiveStringsOrdered(affected, structureAffected) {
			return fmt.Errorf("problem-source receipt affected exact-set mismatch")
		}
		switch v.Action {
		case "skip":
			if v.ResultInputRevision != v.ExpectedInputRevision {
				return fmt.Errorf("problem-source skip receipt revision mismatch")
			}
		case "correct_text", "select_region", "retake", "resume":
			if v.ResultInputRevision != v.ExpectedInputRevision+1 {
				return fmt.Errorf("problem-source receipt revision mismatch")
			}
		default:
			return fmt.Errorf("problem-source receipt action invalid")
		}
		frozen, err := viewcontract.ParseFrozenProblemSourceActionResponse(v.ResponseJSON)
		if err != nil {
			return fmt.Errorf("problem-source frozen response invalid: %w", err)
		}
		if frozen.CommandReceiptID != v.CommandReceiptID || frozen.DispatchID != v.DispatchID || frozen.ProblemID != v.ProblemID || frozen.Action != v.Action || frozen.StructureVersion != v.StructureVersion || frozen.InputRevision != v.ResultInputRevision {
			return fmt.Errorf("problem-source frozen response identity mismatch")
		}
		pathMember, ok := structureIndex.members[problemSourceArchiveStructureMemberKey(
			snapshot.SubmissionID, v.StructureVersion, v.ProblemID,
		)]
		if !ok {
			return fmt.Errorf("problem-source receipt path member missing")
		}
		if homework, ok := homeworkByDispatch[v.DispatchID]; ok {
			if homework.GradingJobID != v.JobID {
				return fmt.Errorf("problem-source receipt homework job identity mismatch")
			}
			dispatch := dispatches[v.DispatchID]
			if dispatch.TargetObjectType == k12.ImageTaskTargetHomeworkSubmission &&
				dispatch.TargetObjectID != homework.SubmissionID {
				return fmt.Errorf("problem-source dispatch homework target identity mismatch")
			}
		}
		receipts[v.CommandReceiptID] = v
		receiptAffected[v.CommandReceiptID] = affected
		receiptStructures[v.CommandReceiptID] = snapshot
		receiptSubmissions[v.CommandReceiptID] = pathMember.SubmissionID
		receiptDispatches[v.DispatchID] = struct{}{}
		receiptJobs[v.JobID] = struct{}{}
	}
	for dispatchID := range dispatches {
		if _, ok := receiptDispatches[dispatchID]; !ok {
			return fmt.Errorf("unreferenced problem-source dispatch %q", dispatchID)
		}
	}
	referencedSubmissions := make(map[string]struct{}, len(receiptSubmissions))
	for _, submissionID := range receiptSubmissions {
		referencedSubmissions[submissionID] = struct{}{}
	}
	for _, snapshot := range archive.StructureSnapshots {
		if _, ok := referencedSubmissions[snapshot.SubmissionID]; !ok {
			return fmt.Errorf(
				"unreferenced problem-source structure submission %q",
				snapshot.SubmissionID,
			)
		}
	}
	finalizations := map[string]ProblemSourceArchiveFinalizationGeneration{}
	summaryInvocations := map[string]string{}
	for _, v := range archive.FinalizationGenerations {
		if v.AgentName != agentName || v.JobID == "" || v.Generation < 0 {
			return fmt.Errorf("problem-source finalization generation scope/value mismatch")
		}
		if _, duplicate := finalizations[v.JobID]; duplicate {
			return fmt.Errorf("duplicate problem-source finalization job %q", v.JobID)
		}
		if _, ok := receiptJobs[v.JobID]; !ok {
			return fmt.Errorf("unreferenced problem-source finalization job %q", v.JobID)
		}
		if v.Artifact != nil {
			if err := v.Artifact.Validate(); err != nil {
				return fmt.Errorf("problem-source final artifact invalid: %w", err)
			}
			if v.Artifact.AgentName != agentName || v.Artifact.JobID != v.JobID {
				return fmt.Errorf("problem-source final artifact scope mismatch")
			}
			if v.Artifact.SummaryInvocationID != "" {
				summaryInvocations[v.Artifact.SummaryInvocationID] = v.JobID
			}
		}
		finalizations[v.JobID] = v
	}
	if len(finalizations) != len(receiptJobs) {
		return fmt.Errorf("problem-source finalization exact-set mismatch: jobs=%d generations=%d", len(receiptJobs), len(finalizations))
	}
	for jobID := range receiptJobs {
		if _, ok := finalizations[jobID]; !ok {
			return fmt.Errorf("problem-source finalization generation missing for job %q", jobID)
		}
	}
	inputRevisions := make(map[string]ProblemSourceArchiveInputRevision, len(archive.InputRevisions))
	current := map[string]ProblemSourceArchiveInputRevision{}
	for _, v := range archive.InputRevisions {
		if v.AgentName != agentName || strings.TrimSpace(v.SubmissionID) == "" ||
			strings.TrimSpace(v.ProblemID) == "" || v.StructureVersion < 1 ||
			v.InputRevision < 1 ||
			(v.CurrentDisposition != "current" &&
				v.CurrentDisposition != "superseded") {
			return fmt.Errorf("problem input owner mismatch")
		}
		memberKey := problemSourceArchiveStructureMemberKey(
			v.SubmissionID, v.StructureVersion, v.ProblemID,
		)
		if _, ok := structureIndex.members[memberKey]; !ok {
			return fmt.Errorf("problem input structure member missing")
		}
		if _, ok := assets[v.PageAssetID]; !ok {
			return fmt.Errorf("problem input references unpacked PageAsset %q", v.PageAssetID)
		}
		referencedAssets[v.PageAssetID] = struct{}{}
		if len(v.SourceRegionJSON) > 0 && !json.Valid(v.SourceRegionJSON) {
			return fmt.Errorf("problem input source region invalid")
		}
		if v.OriginCommandReceiptID != "" {
			receipt, ok := receipts[v.OriginCommandReceiptID]
			if !ok {
				return fmt.Errorf("problem input origin receipt missing")
			}
			if receiptSubmissions[v.OriginCommandReceiptID] != v.SubmissionID ||
				receipt.StructureVersion != v.StructureVersion {
				return fmt.Errorf("problem input origin receipt scope mismatch")
			}
			foundAffected := false
			for _, problemID := range receiptAffected[v.OriginCommandReceiptID] {
				if problemID == v.ProblemID {
					foundAffected = true
					break
				}
			}
			if !foundAffected {
				return fmt.Errorf("problem input origin receipt affected-set mismatch")
			}
			if v.InputRevision == receipt.ResultInputRevision &&
				v.InputDigest != problemSourceInputDigest(
					receipt.RequestDigest, v.ProblemID, v.InputRevision,
				) {
				return fmt.Errorf("problem input receipt digest mismatch")
			}
		}
		immutableKey := problemSourceArchiveInputKey(
			v.SubmissionID, v.StructureVersion, v.ProblemID, v.InputRevision,
		)
		if _, duplicate := inputRevisions[immutableKey]; duplicate {
			return fmt.Errorf("duplicate problem input revision")
		}
		inputRevisions[immutableKey] = v
		if v.CurrentDisposition == "current" {
			key := fmt.Sprintf("%s\x00%d\x00%s", v.SubmissionID, v.StructureVersion, v.ProblemID)
			if _, ok := current[key]; ok {
				return fmt.Errorf("duplicate current problem input")
			}
			current[key] = v
		}
	}
	for receiptID, affected := range receiptAffected {
		snapshot := receiptStructures[receiptID]
		if snapshot.CurrentDisposition != "current" {
			continue
		}
		for _, problemID := range affected {
			member := structureIndex.members[problemSourceArchiveStructureMemberKey(
				snapshot.SubmissionID, snapshot.StructureVersion, problemID,
			)]
			input, ok := current[fmt.Sprintf(
				"%s\x00%d\x00%s",
				snapshot.SubmissionID, snapshot.StructureVersion, problemID,
			)]
			if !ok || input.InputRevision != member.InputRevision {
				return fmt.Errorf("problem-source structure current input head mismatch")
			}
		}
	}
	works := map[string]ProblemSourceArchiveReprocessJob{}
	workByReceipt := map[string]string{}
	for _, v := range archive.ReprocessJobs {
		receipt, ok := receipts[v.CommandReceiptID]
		if v.AgentName != agentName || !ok || receipt.OwnerScope != v.OwnerScope ||
			receipt.DispatchID != v.DispatchID || receipt.JobID != v.JobID ||
			receipt.ProblemID != v.ProblemID || receipt.Action != v.Action ||
			receipt.StructureVersion != v.StructureVersion ||
			receipt.ResultInputRevision != v.InputRevision {
			return fmt.Errorf("source work scope mismatch")
		}
		if strings.TrimSpace(v.WorkID) == "" || !json.Valid(v.RequestJSON) ||
			len(v.AffectedProblemIDs) == 0 {
			return fmt.Errorf("source work JSON/exact-set invalid")
		}
		if _, duplicate := works[v.WorkID]; duplicate {
			return fmt.Errorf("duplicate source work %q", v.WorkID)
		}
		if _, duplicate := workByReceipt[v.CommandReceiptID]; duplicate {
			return fmt.Errorf("duplicate source work receipt identity")
		}
		if string(v.RequestJSON) != string(receipt.RequestJSON) {
			return fmt.Errorf("source work request identity mismatch")
		}
		if !sameProblemSourceArchiveStringsOrdered(
			v.AffectedProblemIDs, receiptAffected[v.CommandReceiptID],
		) {
			return fmt.Errorf("source work affected exact-set mismatch")
		}
		if v.InputDigest != problemSourceInputDigest(
			receipt.RequestDigest,
			strings.Join(v.AffectedProblemIDs, "\x00"),
			v.InputRevision,
		) {
			return fmt.Errorf("source work input digest mismatch")
		}
		works[v.WorkID] = v
		workByReceipt[v.CommandReceiptID] = v.WorkID
	}
	for receiptID, receipt := range receipts {
		_, hasWork := workByReceipt[receiptID]
		if (receipt.Action == "skip" && hasWork) ||
			(receipt.Action != "skip" && !hasWork) {
			return fmt.Errorf("problem-source receipt/work exact-set mismatch")
		}
	}
	results := map[string]ProblemSourceArchiveRecognitionResult{}
	for _, v := range archive.RecognitionResults {
		work, ok := works[v.WorkID]
		if v.AgentName != agentName || !ok || work.OwnerScope != v.OwnerScope ||
			v.CommandReceiptID != work.CommandReceiptID || v.JobID != work.JobID ||
			v.DispatchID != work.DispatchID || v.PathProblemID != work.ProblemID ||
			v.Action != work.Action || v.StructureVersion != work.StructureVersion ||
			v.SubmissionID != receiptSubmissions[v.CommandReceiptID] {
			return fmt.Errorf("source recognition aggregate scope mismatch")
		}
		if _, duplicate := results[v.WorkID]; duplicate {
			return fmt.Errorf("duplicate source recognition aggregate %q", v.WorkID)
		}
		if v.MappingState != "stable_exact_set" ||
			v.SourceInputRevision != work.InputRevision ||
			v.ResultInputRevision != work.InputRevision+1 {
			return fmt.Errorf(
				"source recognition aggregate revision/mapping mismatch: mapping=%q source=%d result=%d work=%d",
				v.MappingState, v.SourceInputRevision, v.ResultInputRevision, work.InputRevision,
			)
		}
		snapshot, ok := structureIndex.snapshots[problemSourceArchiveStructureKey(
			v.SubmissionID, v.StructureVersion,
		)]
		if !ok || v.StructureDigest != snapshot.StructureDigest {
			return fmt.Errorf("source recognition structure digest mismatch")
		}
		results[v.WorkID] = v
	}
	for workID, work := range works {
		if work.Status == ProblemSourceReprocessSucceeded {
			if _, ok := results[workID]; !ok {
				return fmt.Errorf("succeeded source work has no committed recognition result")
			}
		}
	}
	itemCounts := map[string]int{}
	itemIDs := map[string]struct{}{}
	itemOrdinals := map[string]struct{}{}
	for _, v := range archive.RecognitionItems {
		result, ok := results[v.WorkID]
		if v.AgentName != agentName || !ok || result.OwnerScope != v.OwnerScope ||
			v.SubmissionID != result.SubmissionID ||
			v.StructureVersion != result.StructureVersion {
			return fmt.Errorf("source recognition item scope mismatch")
		}
		if v.SourceInputRevision != result.SourceInputRevision ||
			v.ResultInputRevision != result.ResultInputRevision {
			return fmt.Errorf("source recognition item revision mismatch")
		}
		itemKey := v.WorkID + "\x00" + v.ProblemID
		if _, duplicate := itemIDs[itemKey]; duplicate {
			return fmt.Errorf("duplicate source recognition item")
		}
		ordinalKey := v.WorkID + "\x00" + fmt.Sprint(v.Ordinal)
		if v.Ordinal < 0 {
			return fmt.Errorf("source recognition item ordinal invalid")
		}
		if _, duplicate := itemOrdinals[ordinalKey]; duplicate {
			return fmt.Errorf("duplicate source recognition item ordinal")
		}
		itemIDs[itemKey] = struct{}{}
		itemOrdinals[ordinalKey] = struct{}{}
		asset, ok := assets[v.PageAssetID]
		if !ok {
			return fmt.Errorf("source recognition item PageAsset missing")
		}
		if v.SourceContentDigest != asset.ContentDigest ||
			v.SourceMediaType != asset.MediaType ||
			v.SourceSizeBytes != asset.SizeBytes ||
			v.SourcePixelWidth != asset.PixelWidth ||
			v.SourcePixelHeight != asset.PixelHeight ||
			v.SourceOrientationPolicy != asset.OrientationPolicy ||
			v.SourceOrientationPolicyVersion != asset.OrientationPolicyVersion ||
			string(v.SourceTransformChainJSON) != asset.TransformChainJSON {
			return fmt.Errorf("source recognition item PageAsset metadata drift")
		}
		referencedAssets[v.PageAssetID] = struct{}{}
		for _, raw := range []json.RawMessage{v.SourceTransformChainJSON, v.KnowledgePointsJSON, v.OCRSignalsJSON, v.EvidenceTranscriptionsJSON, v.AnswerEvidenceTranscriptionsJSON, v.ConfirmationReasonsJSON} {
			if !json.Valid(raw) {
				return fmt.Errorf("source recognition item JSON invalid")
			}
		}
		itemCounts[v.WorkID]++
	}
	if len(assets) != len(referencedAssets) {
		return fmt.Errorf("problem-source PageAsset exact-set mismatch: referenced=%d packed=%d", len(referencedAssets), len(assets))
	}
	for id := range referencedAssets {
		if _, ok := assets[id]; !ok {
			return fmt.Errorf("problem-source referenced PageAsset %q is missing", id)
		}
	}
	physicalCounts := map[string]int{}
	physicalResults := map[string]ProblemSourceArchiveRecognitionPhysicalResult{}
	physicalOrdinals := map[string]struct{}{}
	for _, v := range archive.RecognitionPhysicalResults {
		result, ok := results[v.WorkID]
		if !ok {
			return fmt.Errorf("source recognition physical aggregate missing")
		}
		if v.ParentInvocationID != result.ParentInvocationID || v.Ordinal < 0 ||
			strings.TrimSpace(v.PhysicalInvocationID) == "" {
			return fmt.Errorf("source recognition physical lineage mismatch")
		}
		ordinalKey := fmt.Sprintf("%s\x00%d", v.WorkID, v.Ordinal)
		if _, duplicate := physicalOrdinals[ordinalKey]; duplicate {
			return fmt.Errorf("duplicate source recognition physical ordinal")
		}
		if _, duplicate := physicalResults[v.PhysicalInvocationID]; duplicate {
			return fmt.Errorf("duplicate source recognition physical invocation")
		}
		physicalOrdinals[ordinalKey] = struct{}{}
		physicalResults[v.PhysicalInvocationID] = v
		physicalCounts[v.WorkID]++
	}
	authoritativeSubmissions, err := problemSourceArchiveAuthoritativeJobSubmissions(archive)
	if err != nil {
		return err
	}
	recognitionParents := make(map[string]struct{}, len(results))
	for _, result := range results {
		recognitionParents[result.ParentInvocationID] = struct{}{}
	}
	parents := map[string]k12.ModelInvocation{}
	for _, v := range archive.ModelInvocations {
		if v.AgentName != agentName || strings.TrimSpace(v.InvocationID) == "" ||
			v.ProviderIdempotencyKey != "" || v.ExternalRequestID != "" {
			return fmt.Errorf("model invocation owner mismatch")
		}
		if v.ResultJSON != "" {
			if v.Stage != k12.GradingStageProjecting {
				return fmt.Errorf("model invocation contains unsupported raw provider result payload")
			}
			if _, ok := receiptJobs[v.JobID]; !ok {
				return fmt.Errorf("model invocation typed summary payload job is unreferenced")
			}
			if v.Status != k12.ModelInvocationSucceeded &&
				v.Status != k12.ModelInvocationReconciled {
				return fmt.Errorf("model invocation typed summary payload is not terminal")
			}
			if v.ResultDigest != problemSourceArchiveModelResultDigest(v.ResultJSON) {
				return fmt.Errorf("model invocation result payload digest mismatch")
			}
			if err := validateProblemSourceArchiveSummaryResult(
				v.ResultJSON,
				v.JobID,
				authoritativeSubmissions[v.JobID],
			); err != nil {
				return fmt.Errorf("model invocation typed summary payload invalid: %w", err)
			}
		}
		if _, duplicate := parents[v.InvocationID]; duplicate {
			return fmt.Errorf("duplicate model invocation %q", v.InvocationID)
		}
		switch v.Stage {
		case "recognizing":
			if _, ok := recognitionParents[v.InvocationID]; !ok {
				return fmt.Errorf("unreferenced source recognition model invocation")
			}
		case k12.GradingStageProjecting:
			if _, ok := receiptJobs[v.JobID]; !ok {
				return fmt.Errorf("unreferenced source projecting model invocation")
			}
		default:
			return fmt.Errorf("problem-source model invocation stage is unsupported")
		}
		parents[v.InvocationID] = v
	}
	for invocationID, jobID := range summaryInvocations {
		found := false
		for _, invocation := range archive.ModelInvocations {
			if invocation.InvocationID == invocationID && invocation.JobID == jobID &&
				invocation.AgentName == agentName {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("problem-source final artifact summary invocation %q is missing", invocationID)
		}
	}
	physicalInvocations := map[string]k12.ModelPhysicalInvocation{}
	for _, v := range archive.ModelPhysicalInvocations {
		if v.AgentName != agentName || strings.TrimSpace(v.PhysicalInvocationID) == "" ||
			v.ExternalRequestID != "" {
			return fmt.Errorf("physical invocation owner mismatch")
		}
		if _, ok := parents[v.ParentInvocationID]; !ok {
			return fmt.Errorf("physical invocation parent missing")
		}
		if _, duplicate := physicalInvocations[v.PhysicalInvocationID]; duplicate {
			return fmt.Errorf("duplicate physical invocation %q", v.PhysicalInvocationID)
		}
		physicalInvocations[v.PhysicalInvocationID] = v
		if _, ok := physicalResults[v.PhysicalInvocationID]; !ok {
			return fmt.Errorf("unreferenced physical invocation")
		}
	}
	for id, v := range results {
		var affected []string
		if json.Unmarshal(v.AffectedProblemIDsJSON, &affected) != nil || len(affected) == 0 || itemCounts[id] != len(affected) || physicalCounts[id] == 0 {
			return fmt.Errorf("source recognition exact-set incomplete")
		}
		work := works[id]
		if v.DispatchID != work.DispatchID || v.PathProblemID != work.ProblemID ||
			v.Action != work.Action || v.StructureVersion != work.StructureVersion ||
			v.SourceInputRevision != work.InputRevision {
			return fmt.Errorf("source recognition work identity mismatch")
		}
		if !sameProblemSourceArchiveStringsOrdered(affected, work.AffectedProblemIDs) ||
			!sameProblemSourceArchiveStringsOrdered(
				affected, receiptAffected[v.CommandReceiptID],
			) {
			return fmt.Errorf("source recognition affected exact-set mismatch")
		}
		items := make([]ProblemSourceArchiveRecognitionItem, 0, len(affected))
		for _, item := range archive.RecognitionItems {
			if item.WorkID == id {
				items = append(items, item)
			}
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Ordinal == items[j].Ordinal {
				return items[i].ProblemID < items[j].ProblemID
			}
			return items[i].Ordinal < items[j].Ordinal
		})
		for index, item := range items {
			if item.Ordinal != index || item.ProblemID != affected[index] {
				return fmt.Errorf("source recognition item exact order mismatch")
			}
		}
		parent, ok := parents[v.ParentInvocationID]
		if !ok {
			return fmt.Errorf("source recognition parent invocation missing")
		}
		if parent.JobID != v.JobID || parent.Stage != "recognizing" ||
			parent.Attempt != v.ParentInvocationAttempt || parent.Attempt < 1 {
			return fmt.Errorf("source recognition parent invocation identity mismatch")
		}
		wantRequestDigest, err := ProblemSourceRecognitionParentRequestDigest(
			problemSourceArchiveJob(work),
			parent.RouteSnapshot,
			parent.RequestPolicySnapshot,
		)
		if err != nil {
			return fmt.Errorf("source recognition parent request digest: %w", err)
		}
		if v.ParentRequestDigest != wantRequestDigest ||
			parent.RequestDigest != wantRequestDigest {
			return fmt.Errorf("source recognition parent request digest mismatch")
		}
		input, err := problemSourceArchiveRecognitionInput(
			v,
			archive.RecognitionItems,
			archive.RecognitionPhysicalResults,
		)
		if err != nil {
			return fmt.Errorf("source recognition typed result invalid: %w", err)
		}
		_, aggregateDigest, err := normalizeProblemSourceRecognitionResult(input)
		if err != nil {
			return fmt.Errorf("source recognition aggregate result invalid: %w", err)
		}
		if v.ResultDigest != aggregateDigest {
			return fmt.Errorf("source recognition aggregate result digest mismatch")
		}
		typedDigest, err := ProblemSourceRecognitionTypedResultDigest(input)
		if err != nil {
			return fmt.Errorf("source recognition typed result digest: %w", err)
		}
		if parent.ResultDigest != typedDigest {
			return fmt.Errorf("source recognition parent typed result digest mismatch")
		}
		for _, item := range items {
			if item.InputDigest != problemSourceInputDigest(
				v.ResultDigest, item.ProblemID, item.ResultInputRevision,
			) {
				return fmt.Errorf("source recognition item input digest mismatch")
			}
			input, ok := inputRevisions[problemSourceArchiveInputKey(
				item.SubmissionID,
				item.StructureVersion,
				item.ProblemID,
				item.ResultInputRevision,
			)]
			if !ok {
				return fmt.Errorf("source recognition item input revision missing")
			}
			if input.InputDigest != item.InputDigest ||
				input.PageAssetID != item.PageAssetID ||
				string(input.SourceRegionJSON) != string(item.SourceRegionJSON) ||
				input.StemRaw != item.StemRaw || input.AnswerRaw != item.AnswerRaw ||
				input.AnswerBBoxJSON != item.AnswerBBoxJSON ||
				input.QuestionCanonicalMarkdown != item.QuestionCanonicalMarkdown ||
				input.AnswerCanonicalMarkdown != item.AnswerCanonicalMarkdown {
				return fmt.Errorf("source recognition item/input revision facts mismatch")
			}
		}
		for _, ref := range archive.RecognitionPhysicalResults {
			if ref.WorkID != id {
				continue
			}
			physical, ok := physicalInvocations[ref.PhysicalInvocationID]
			if !ok {
				return fmt.Errorf("source recognition physical invocation missing")
			}
			if physical.ParentInvocationID != v.ParentInvocationID ||
				physical.JobID != v.JobID || physical.Stage != "recognizing" ||
				physical.Attempt != 1 || string(physical.PhysicalUnit) != ref.PhysicalUnit ||
				physical.ResultDigest != ref.ResultDigest {
				return fmt.Errorf("source recognition physical invocation lineage mismatch")
			}
		}
	}
	return nil
}

type problemSourceArchiveTutoringTipsSection struct {
	Title       string `json:"title"`
	Content     string `json:"content"`
	SourceLabel string `json:"source_label"`
}

type problemSourceArchiveTutoringTips struct {
	GradingJobID    string                                    `json:"GradingJobID"`
	SubmissionID    string                                    `json:"SubmissionID"`
	Grade           string                                    `json:"Grade"`
	Subject         string                                    `json:"Subject"`
	KnowledgePoints []string                                  `json:"knowledge_points"`
	Sections        []problemSourceArchiveTutoringTipsSection `json:"sections"`
}

func problemSourceArchiveModelResultDigest(raw string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(raw))
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func problemSourceArchiveAuthoritativeJobSubmissions(
	archive ProblemSourceArchiveV6,
) (map[string]string, error) {
	out := make(map[string]string)
	bind := func(jobID, submissionID string) error {
		jobID = strings.TrimSpace(jobID)
		submissionID = strings.TrimSpace(submissionID)
		if jobID == "" || submissionID == "" {
			return nil
		}
		if prior := out[jobID]; prior != "" && prior != submissionID {
			return fmt.Errorf(
				"problem-source job %q spans authoritative submissions %q and %q",
				jobID,
				prior,
				submissionID,
			)
		}
		out[jobID] = submissionID
		return nil
	}
	for _, result := range archive.RecognitionResults {
		if err := bind(result.JobID, result.SubmissionID); err != nil {
			return nil, err
		}
	}
	for _, receipt := range archive.ActionReceipts {
		for _, member := range archive.StructureMembers {
			if member.ProblemID == receipt.ProblemID &&
				member.StructureVersion == receipt.StructureVersion {
				if err := bind(receipt.JobID, member.SubmissionID); err != nil {
					return nil, err
				}
			}
		}
		for _, input := range archive.InputRevisions {
			if input.ProblemID == receipt.ProblemID &&
				input.StructureVersion == receipt.StructureVersion {
				if err := bind(receipt.JobID, input.SubmissionID); err != nil {
					return nil, err
				}
			}
		}
	}
	return out, nil
}

func decodeProblemSourceArchiveSummaryResult(
	raw string,
) (problemSourceArchiveTutoringTips, error) {
	if len(raw) > 4<<20 {
		return problemSourceArchiveTutoringTips{}, fmt.Errorf(
			"typed summary payload exceeds the archive bound",
		)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var summary problemSourceArchiveTutoringTips
	if err := decoder.Decode(&summary); err != nil {
		return problemSourceArchiveTutoringTips{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return problemSourceArchiveTutoringTips{}, fmt.Errorf(
				"typed summary payload has trailing JSON",
			)
		}
		return problemSourceArchiveTutoringTips{}, err
	}
	return summary, nil
}

// validateProblemSourceArchiveSummaryResult mirrors the storage-independent
// shape consumed by finalizer crash recovery. A v6 archive may carry this
// typed projection, but never an arbitrary provider response envelope.
func validateProblemSourceArchiveSummaryResult(
	raw string,
	jobID string,
	submissionID string,
) error {
	summary, err := decodeProblemSourceArchiveSummaryResult(raw)
	if err != nil {
		return err
	}
	if summary.GradingJobID != jobID ||
		strings.TrimSpace(submissionID) == "" || summary.SubmissionID != submissionID ||
		strings.TrimSpace(summary.Grade) == "" ||
		strings.TrimSpace(summary.Subject) == "" ||
		len(summary.KnowledgePoints) == 0 || len(summary.Sections) != 3 {
		return fmt.Errorf("typed summary payload identity/facts are incomplete")
	}
	for _, knowledgePoint := range summary.KnowledgePoints {
		if strings.TrimSpace(knowledgePoint) == "" {
			return fmt.Errorf("typed summary payload has an empty knowledge point")
		}
	}
	for _, section := range summary.Sections {
		if strings.TrimSpace(section.Title) == "" ||
			strings.TrimSpace(section.Content) == "" ||
			strings.TrimSpace(section.SourceLabel) == "" {
			return fmt.Errorf("typed summary payload has an incomplete section")
		}
	}
	if strings.TrimSpace(summary.Sections[0].Title) != "这页在练什么" ||
		(summary.Sections[0].SourceLabel != "📖 依据课本" &&
			summary.Sections[0].SourceLabel != "🤖 AI 归纳·供参考") {
		return fmt.Errorf("typed summary overview contract changed")
	}
	attentionTitle := strings.TrimSpace(summary.Sections[1].Title)
	if attentionTitle == "要留意" || !strings.HasSuffix(attentionTitle, "要留意") ||
		summary.Sections[1].SourceLabel != "🧠 学情信号" {
		return fmt.Errorf("typed summary learning-evidence contract changed")
	}
	if strings.TrimSpace(summary.Sections[2].Title) != "每道题怎么带（不直接给答案）" ||
		summary.Sections[2].SourceLabel != "🤖 AI 归纳·供参考" {
		return fmt.Errorf("typed summary per-problem contract changed")
	}
	return nil
}

func validateProblemSourcePageAssetRefs(ids map[string]struct{}, refs []string) error {
	for _, ref := range refs {
		if ref == "" || !strings.HasPrefix(ref, "asset://") {
			return fmt.Errorf("non-canonical PageAsset reference %q", ref)
		}
		ids[ref] = struct{}{}
	}
	return nil
}

// NormalizeProblemSourceArchiveV6ForRestore parks ambiguous in-flight calls.
// A running work with a committed V73 result is safely re-queued for local
// finalization because the processor reads that result before provider IO; a
// running work without such evidence becomes outcome_unknown.
func NormalizeProblemSourceArchiveV6ForRestore(source ProblemSourceArchiveV6) ProblemSourceArchiveV6 {
	out := cloneProblemSourceArchiveV6(source)
	hasResult := map[string]struct{}{}
	for _, v := range out.RecognitionResults {
		hasResult[v.WorkID] = struct{}{}
	}
	for i := range out.ReprocessJobs {
		v := &out.ReprocessJobs[i]
		// Process-local reconciliation leases never survive a restore. Epoch and
		// attempt counters remain immutable audit evidence, while the due time is
		// retained for already outcome-unknown work.
		v.ReconciliationOwner = ""
		v.ReconciliationExpiresAtMilli = 0
		if v.Status == ProblemSourceReprocessRunning {
			v.LeaseOwner = ""
			v.LeaseExpiresAtMilli = 0
			v.NextAttemptAtMilli = 0
			if _, ok := hasResult[v.WorkID]; ok {
				v.Status = ProblemSourceReprocessQueued
				v.FailureCode = ""
				v.FailureDetail = ""
			} else {
				v.Status = ProblemSourceReprocessOutcomeUnknown
				v.NextReconcileAtMilli = 0
				v.FailureCode = "hexbak_restore_ambiguous_provider_state"
				v.FailureDetail = "restored running source work has no conclusive durable recognition result"
			}
		}
	}
	for i := range out.ModelInvocations {
		v := &out.ModelInvocations[i]
		// A succeeded projecting invocation is safely replayable only because its
		// exact typed result bytes and digest survived V75. Keep that terminal
		// state so a crash before final-artifact commit can finish locally without
		// another provider call. Every payload-less/ambiguous call is parked.
		if v.Stage != k12.GradingStageProjecting ||
			v.Status != k12.ModelInvocationSucceeded || v.ResultJSON == "" {
			v.Status = k12.ModelInvocationReconciled
		}
		v.ProviderIdempotencyKey = ""
		v.ExternalRequestID = ""
		v.FailureKind = ""
	}
	for i := range out.ModelPhysicalInvocations {
		v := &out.ModelPhysicalInvocations[i]
		v.Status = k12.ModelInvocationReconciled
		v.ExternalRequestID = ""
		v.FailureKind = ""
	}
	return out
}

func cloneProblemSourceArchiveV6(source ProblemSourceArchiveV6) ProblemSourceArchiveV6 {
	raw, _ := json.Marshal(source)
	var out ProblemSourceArchiveV6
	_ = json.Unmarshal(raw, &out)
	return out
}

// ImportProblemSourceArchiveV6Tx merges a verified archive inside the caller's
// records/profile/assets transaction. Conflicting immutable identities fail
// closed; exact replay is idempotent.
func (s *Store) ImportProblemSourceArchiveV6Tx(ctx context.Context, tx *sql.Tx, agentName string, source ProblemSourceArchiveV6) error {
	if tx == nil {
		return fmt.Errorf("k12storage: nil problem-source import transaction")
	}
	if err := ValidateProblemSourceArchiveV6(agentName, source); err != nil {
		return err
	}
	archive := NormalizeProblemSourceArchiveV6ForRestore(source)
	if err := s.insertProblemSourceArchiveV6(ctx, tx, archive); err != nil {
		return err
	}
	stored, err := s.exportProblemSourceArchiveV6Via(ctx, tx, agentName)
	if err != nil {
		return err
	}
	stored = NormalizeProblemSourceArchiveV6ForRestore(stored)
	if err := ensureProblemSourceArchiveSubset(archive, stored); err != nil {
		return err
	}
	return nil
}

// DeleteProblemSourceArchiveV6Tx removes only the currently exported
// source-action closure for an Agent. PageAsset metadata is retained because a
// content address may predate or be shared with another canonical Problem;
// restore-as removes migration-created PageAssets separately using its asset
// creation journal. The caller owns the transaction and may rebuild a snapshot
// after restoring its record/Problem parents.
func (s *Store) DeleteProblemSourceArchiveV6Tx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
) error {
	if tx == nil {
		return fmt.Errorf("k12storage: nil problem-source delete transaction")
	}
	archive, err := s.exportProblemSourceArchiveV6Via(ctx, tx, agentName)
	if err != nil {
		return err
	}
	if archive.IsEmpty() {
		return nil
	}
	for _, item := range archive.FinalizationGenerations {
		if item.Artifact == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM k12_grading_final_artifacts
			WHERE agent_name=? AND job_id=? AND artifact_id=?`,
			item.AgentName, item.JobID, item.Artifact.ArtifactID,
		); err != nil {
			return fmt.Errorf("delete source final artifact: %w", err)
		}
	}
	for _, item := range archive.RecognitionResults {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM k12_problem_source_recognition_results WHERE work_id=?`,
			item.WorkID,
		); err != nil {
			return fmt.Errorf("delete source recognition result: %w", err)
		}
	}
	for _, item := range archive.InputRevisions {
		if _, err := tx.ExecContext(ctx, `DELETE FROM k12_problem_input_revisions
			WHERE agent_name=? AND submission_id=? AND structure_version=?
			  AND problem_id=? AND input_revision=?`,
			item.AgentName, item.SubmissionID, item.StructureVersion,
			item.ProblemID, item.InputRevision,
		); err != nil {
			return fmt.Errorf("delete source input revision: %w", err)
		}
	}
	for _, item := range archive.ReprocessJobs {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM k12_problem_source_reprocess_jobs WHERE work_id=?`,
			item.WorkID,
		); err != nil {
			return fmt.Errorf("delete source work: %w", err)
		}
	}
	for _, item := range archive.ActionReceipts {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM k12_problem_source_action_receipts WHERE command_receipt_id=?`,
			item.CommandReceiptID,
		); err != nil {
			return fmt.Errorf("delete source receipt: %w", err)
		}
	}
	for _, item := range archive.StructureSnapshots {
		if _, err := tx.ExecContext(ctx, `DELETE FROM k12_problem_structure_snapshots
			WHERE agent_name=? AND submission_id=? AND structure_version=?`,
			item.AgentName, item.SubmissionID, item.StructureVersion,
		); err != nil {
			return fmt.Errorf("delete source structure snapshot: %w", err)
		}
	}
	for _, item := range archive.Dispatches {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM k12_image_task_dispatches WHERE agent_name=? AND dispatch_id=?`,
			item.AgentName, item.DispatchID,
		); err != nil {
			return fmt.Errorf("delete source dispatch: %w", err)
		}
	}
	for _, item := range archive.ModelPhysicalInvocations {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM k12_model_physical_invocations WHERE physical_invocation_id=?`,
			item.PhysicalInvocationID,
		); err != nil {
			return fmt.Errorf("delete source physical invocation: %w", err)
		}
	}
	for _, item := range archive.ModelInvocations {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM k12_model_invocations WHERE invocation_id=?`,
			item.InvocationID,
		); err != nil {
			return fmt.Errorf("delete source model invocation: %w", err)
		}
	}
	return nil
}

// DeleteUnreferencedProblemSourcePageAssetsTx removes restore-as-created
// metadata only after all source facts that referenced those IDs are gone.
func (s *Store) DeleteUnreferencedProblemSourcePageAssetsTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
	pageAssetIDs []string,
) error {
	if tx == nil {
		return fmt.Errorf("k12storage: nil PageAsset cleanup transaction")
	}
	for _, pageAssetID := range pageAssetIDs {
		pageAssetID = strings.TrimSpace(pageAssetID)
		if pageAssetID == "" {
			continue
		}
		var references int
		if err := tx.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM k12_problem_input_revisions
			 WHERE agent_name=? AND page_asset_id=?) +
			(SELECT COUNT(*) FROM k12_problem_source_recognition_items
			 WHERE agent_name=? AND page_asset_id=?) +
			(SELECT COUNT(*) FROM k12_problems
			 WHERE agent_name=? AND page_asset_id=?) +
			(SELECT COUNT(*) FROM k12_image_task_dispatches d,json_each(d.source_asset_refs_json) r
			 WHERE d.agent_name=? AND r.value=?) +
			(SELECT COUNT(*) FROM k12_homework_submissions h,json_each(h.source_asset_refs_json) r
			 WHERE h.agent_name=? AND r.value=?)`,
			agentName, pageAssetID, agentName, pageAssetID,
			agentName, pageAssetID, agentName, pageAssetID,
			agentName, pageAssetID,
		).Scan(&references); err != nil {
			return fmt.Errorf("count PageAsset references: %w", err)
		}
		if references != 0 {
			return fmt.Errorf("PageAsset %q remains referenced after source rollback", pageAssetID)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM k12_page_assets WHERE agent_name=? AND page_asset_id=?`,
			agentName, pageAssetID,
		); err != nil {
			return fmt.Errorf("delete restored PageAsset metadata: %w", err)
		}
	}
	return nil
}

func (s *Store) insertProblemSourceArchiveV6(ctx context.Context, tx *sql.Tx, a ProblemSourceArchiveV6) error {
	for _, v := range a.Dispatches {
		assets, _ := json.Marshal(v.SourceAssetRefs)
		evidence, _ := json.Marshal(v.IntentEvidence)
		candidates, _ := json.Marshal(v.ConfirmationCandidates)
		classification, _ := json.Marshal(v.ClassificationRouteSnapshot)
		policy, _ := json.Marshal(v.RoutePolicySnapshot)
		creative := ""
		if v.CreativeEntry != nil {
			raw, _ := json.Marshal(v.CreativeEntry)
			creative = string(raw)
		}
		operation, _ := json.Marshal(v.OperationRouteRequest)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_image_task_dispatches (dispatch_id,agent_name,learner_id,source_kind,source_ref,source_session_id,source_asset_refs_json,source_digest,message_intent,task_intent,intent_evidence_json,intent_confidence,confirmation_candidates_json,status,target_object_type,target_object_id,classification_route_snapshot_json,classification_invocation_id,route_policy_snapshot_json,idempotency_key,request_digest,attempt_generation,retry_safe,failure_kind,version,created_at,updated_at,routing_provenance,creative_entry_json,operation_route_request_json,automatic_budget_seconds,automatic_started_at,automatic_deadline_at,automatic_remaining_seconds) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.DispatchID, v.AgentName, v.LearnerID, v.SourceKind, v.SourceRef, v.SourceSessionID, string(assets), v.SourceDigest, v.MessageIntent, v.TaskIntent, string(evidence), v.IntentConfidence, string(candidates), v.Status, v.TargetObjectType, v.TargetObjectID, string(classification), v.ClassificationInvocationID, string(policy), v.IdempotencyKey, v.RequestDigest, v.AttemptGeneration, boolIntStorage(v.RetrySafe), v.FailureKind, v.Version, v.CreatedAt, v.UpdatedAt, v.RoutingProvenance, creative, string(operation), v.AutomaticBudgetSeconds, v.AutomaticStartedAt, v.AutomaticDeadlineAt, v.AutomaticRemainingSeconds); err != nil {
			return fmt.Errorf("import source dispatch: %w", err)
		}
	}
	for _, v := range a.DispatchOwners {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_image_task_owner_scopes(dispatch_id,owner_scope,agent_name,created_at) VALUES(?,?,?,?)`, v.DispatchID, v.OwnerScope, v.AgentName, v.CreatedAt); err != nil {
			return err
		}
	}
	for _, v := range a.HomeworkSubmissions {
		assets, _ := json.Marshal(v.SourceAssetRefs)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_homework_submissions(submission_id,dispatch_id,agent_name,learner_id,source_kind,source_ref,source_asset_refs_json,task_intent,status,grading_job_id,idempotency_key,version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.SubmissionID, v.DispatchID, v.AgentName, v.LearnerID, v.SourceKind, v.SourceRef, string(assets), v.TaskIntent, v.Status, v.GradingJobID, v.IdempotencyKey, v.Version, v.CreatedAt, v.UpdatedAt); err != nil {
			return err
		}
	}
	for _, v := range a.PageAssets {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_page_assets(owner_scope,page_asset_id,agent_name,content_digest,media_type,size_bytes,pixel_width,pixel_height,orientation_policy,orientation_policy_version,transform_chain_json,storage_state,ready_at,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.OwnerScope, v.PageAssetID, v.AgentName, v.ContentDigest, v.MediaType, v.SizeBytes, v.PixelWidth, v.PixelHeight, v.OrientationPolicy, v.OrientationPolicyVersion, v.TransformChainJSON, v.StorageState, v.ReadyAt, v.LastError, v.CreatedAt, v.UpdatedAt); err != nil {
			return err
		}
	}
	for _, v := range a.StructureSnapshots {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_problem_structure_snapshots(agent_name,submission_id,structure_version,structure_digest,mapping_state,current_disposition,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, v.AgentName, v.SubmissionID, v.StructureVersion, v.StructureDigest, v.MappingState, v.CurrentDisposition, v.CreatedAt, v.UpdatedAt); err != nil {
			return err
		}
	}
	for _, v := range a.StructureMembers {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_problem_structure_members(agent_name,submission_id,structure_version,problem_id,ordinal,problem_kind,parent_problem_id,subproblem_no,source_number_path_json,display_label,source_section_path_json,source_section_label,system_section_ordinal,system_display_label,dependency_group_id,input_revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.AgentName, v.SubmissionID, v.StructureVersion, v.ProblemID, v.Ordinal, v.ProblemKind, v.ParentProblemID, v.SubproblemNo, v.SourceNumberPathJSON, v.DisplayLabel, v.SourceSectionPathJSON, v.SourceSectionLabel, v.SystemSectionOrdinal, v.SystemDisplayLabel, v.DependencyGroupID, v.InputRevision); err != nil {
			return err
		}
	}
	for _, v := range a.DependencyGroups {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_problem_dependency_groups(agent_name,submission_id,structure_version,dependency_group_id,state,state_revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, v.AgentName, v.SubmissionID, v.StructureVersion, v.DependencyGroupID, v.State, v.StateRevision, v.CreatedAt, v.UpdatedAt); err != nil {
			return err
		}
	}
	for _, v := range a.ActionReceipts {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_problem_source_action_receipts(command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,problem_id,idempotency_key,request_digest,action,structure_version,expected_input_revision,result_input_revision,request_json,affected_problem_ids_json,response_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.CommandReceiptID, v.OwnerScope, v.AgentName, v.DispatchID, v.JobID, v.ProblemID, v.IdempotencyKey, v.RequestDigest, v.Action, v.StructureVersion, v.ExpectedInputRevision, v.ResultInputRevision, string(v.RequestJSON), string(v.AffectedProblemIDsJSON), string(v.ResponseJSON), v.CreatedAt, v.UpdatedAt); err != nil {
			return err
		}
	}
	for _, v := range a.InputRevisions {
		var region any
		if len(v.SourceRegionJSON) > 0 {
			region = string(v.SourceRegionJSON)
		}
		var origin any
		if v.OriginCommandReceiptID != "" {
			origin = v.OriginCommandReceiptID
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_problem_input_revisions(agent_name,submission_id,structure_version,problem_id,input_revision,page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,question_canonical_markdown,answer_canonical_markdown,input_digest,current_disposition,origin_command_receipt_id,origin_kind,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.AgentName, v.SubmissionID, v.StructureVersion, v.ProblemID, v.InputRevision, v.PageAssetID, region, v.StemRaw, v.AnswerRaw, v.AnswerBBoxJSON, v.QuestionCanonicalMarkdown, v.AnswerCanonicalMarkdown, v.InputDigest, v.CurrentDisposition, origin, v.OriginKind, v.CreatedAt, v.UpdatedAt); err != nil {
			return err
		}
	}
	for _, v := range a.ModelInvocations {
		route, _ := json.Marshal(v.RouteSnapshot)
		policy, _ := json.Marshal(v.RequestPolicySnapshot)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_model_invocations(invocation_id,agent_name,job_id,stage,request_digest,provider,model,route_snapshot_json,request_policy_snapshot_json,provider_idempotency_key,status,attempt,result_digest,result_json,external_request_id,failure_kind,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.InvocationID, v.AgentName, v.JobID, v.Stage, v.RequestDigest, v.RouteSnapshot.Provider, v.RouteSnapshot.Model, string(route), string(policy), v.ProviderIdempotencyKey, v.Status, v.Attempt, v.ResultDigest, v.ResultJSON, v.ExternalRequestID, v.FailureKind, v.CreatedAt, v.UpdatedAt); err != nil {
			return err
		}
	}
	for _, v := range a.ModelPhysicalInvocations {
		route, _ := json.Marshal(v.RouteSnapshot)
		policy, _ := json.Marshal(v.RequestPolicySnapshot)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_model_physical_invocations(physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,physical_unit,request_digest,route_snapshot_json,request_policy_snapshot_json,status,attempt,result_digest,result_content,external_request_id,failure_kind,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?,?)`, v.PhysicalInvocationID, v.ParentInvocationID, v.AgentName, v.JobID, v.Stage, v.PhysicalUnit, v.RequestDigest, string(route), string(policy), v.Status, v.Attempt, v.ResultDigest, v.ExternalRequestID, v.FailureKind, v.CreatedAt, v.UpdatedAt); err != nil {
			return err
		}
	}
	for _, v := range a.ReprocessJobs {
		affected, _ := json.Marshal(v.AffectedProblemIDs)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_problem_source_reprocess_jobs(work_id,command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,problem_id,action,structure_version,input_revision,input_digest,affected_problem_ids_json,request_json,status,lease_owner,lease_epoch,lease_expires_at,attempt_count,next_attempt_at,reconciliation_owner,reconciliation_epoch,reconciliation_expires_at,reconciliation_attempt_count,next_reconcile_at,failure_code,failure_detail,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.WorkID, v.CommandReceiptID, v.OwnerScope, v.AgentName, v.DispatchID, v.JobID, v.ProblemID, v.Action, v.StructureVersion, v.InputRevision, v.InputDigest, string(affected), string(v.RequestJSON), v.Status, v.LeaseOwner, v.LeaseEpoch, v.LeaseExpiresAtMilli, v.AttemptCount, v.NextAttemptAtMilli, v.ReconciliationOwner, v.ReconciliationEpoch, v.ReconciliationExpiresAtMilli, v.ReconciliationAttemptCount, v.NextReconcileAtMilli, v.FailureCode, v.FailureDetail, v.CreatedAt, v.UpdatedAt); err != nil {
			return err
		}
	}
	for _, v := range a.RecognitionResults {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_problem_source_recognition_results(work_id,command_receipt_id,owner_scope,agent_name,submission_id,dispatch_id,job_id,path_problem_id,parent_invocation_id,parent_request_digest,parent_invocation_attempt,action,structure_version,source_input_revision,result_input_revision,result_digest,mapping_state,structure_digest,affected_problem_ids_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.WorkID, v.CommandReceiptID, v.OwnerScope, v.AgentName, v.SubmissionID, v.DispatchID, v.JobID, v.PathProblemID, v.ParentInvocationID, v.ParentRequestDigest, v.ParentInvocationAttempt, v.Action, v.StructureVersion, v.SourceInputRevision, v.ResultInputRevision, v.ResultDigest, v.MappingState, v.StructureDigest, string(v.AffectedProblemIDsJSON), v.CreatedAt); err != nil {
			return err
		}
	}
	for _, v := range a.RecognitionPhysicalResults {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_problem_source_recognition_physical_results(work_id,ordinal,parent_invocation_id,physical_invocation_id,physical_unit,result_digest,created_at) VALUES(?,?,?,?,?,?,?)`, v.WorkID, v.Ordinal, v.ParentInvocationID, v.PhysicalInvocationID, v.PhysicalUnit, v.ResultDigest, v.CreatedAt); err != nil {
			return err
		}
	}
	for _, v := range a.RecognitionItems {
		var region any
		if len(v.SourceRegionJSON) > 0 {
			region = string(v.SourceRegionJSON)
		}
		var confidence any
		if v.RecognitionConfidence != nil {
			confidence = *v.RecognitionConfidence
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_problem_source_recognition_items(work_id,ordinal,owner_scope,agent_name,submission_id,structure_version,problem_id,source_input_revision,result_input_revision,input_digest,page_asset_id,source_region_json,source_content_digest,source_media_type,source_size_bytes,source_pixel_width,source_pixel_height,source_orientation_policy,source_orientation_policy_version,source_transform_chain_json,stem_raw,question_canonical_markdown,answer_state,answer_raw,answer_canonical_markdown,answer_bbox_json,subject,knowledge_points_json,recognition_confidence,ocr_signals_json,evidence_transcriptions_json,answer_evidence_transcriptions_json,confirmation_required,confirmation_reasons_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.WorkID, v.Ordinal, v.OwnerScope, v.AgentName, v.SubmissionID, v.StructureVersion, v.ProblemID, v.SourceInputRevision, v.ResultInputRevision, v.InputDigest, v.PageAssetID, region, v.SourceContentDigest, v.SourceMediaType, v.SourceSizeBytes, v.SourcePixelWidth, v.SourcePixelHeight, v.SourceOrientationPolicy, v.SourceOrientationPolicyVersion, string(v.SourceTransformChainJSON), v.StemRaw, v.QuestionCanonicalMarkdown, v.AnswerState, v.AnswerRaw, v.AnswerCanonicalMarkdown, v.AnswerBBoxJSON, v.Subject, string(v.KnowledgePointsJSON), confidence, string(v.OCRSignalsJSON), string(v.EvidenceTranscriptionsJSON), string(v.AnswerEvidenceTranscriptionsJSON), boolIntStorage(v.ConfirmationRequired), string(v.ConfirmationReasonsJSON), v.CreatedAt); err != nil {
			return err
		}
	}
	if err := restoreProblemSourceFinalizationsTx(ctx, tx, a.FinalizationGenerations); err != nil {
		return err
	}
	return nil
}

func restoreProblemSourceFinalizationsTx(
	ctx context.Context,
	tx *sql.Tx,
	states []ProblemSourceArchiveFinalizationGeneration,
) error {
	for _, state := range states {
		var currentGeneration int64
		if err := tx.QueryRowContext(ctx, `SELECT finalization_generation
			FROM k12_grading_jobs WHERE agent_name=? AND record_id=?`,
			state.AgentName, state.JobID,
		).Scan(&currentGeneration); err != nil {
			return fmt.Errorf("restore source finalization job %q: %w", state.JobID, err)
		}
		storedArtifact, artifactErr := getGradingFinalArtifactByJobVia(
			ctx, tx, state.AgentName, state.JobID,
		)
		artifactExists := artifactErr == nil
		if artifactErr != nil && !errors.Is(artifactErr, records.ErrNotFound) {
			return artifactErr
		}
		if artifactExists {
			var artifactGeneration int64
			if err := tx.QueryRowContext(ctx, `SELECT finalization_generation
				FROM k12_grading_final_artifacts WHERE agent_name=? AND job_id=?`,
				state.AgentName, state.JobID,
			).Scan(&artifactGeneration); err != nil {
				return err
			}
			if artifactGeneration != currentGeneration {
				return fmt.Errorf("source finalization target invariant mismatch for job %q", state.JobID)
			}
			if state.Artifact == nil {
				if currentGeneration < state.Generation {
					return fmt.Errorf("source finalization generation %d would stale target artifact for job %q", state.Generation, state.JobID)
				}
				continue
			}
			if currentGeneration != state.Generation ||
				!reflect.DeepEqual(storedArtifact, *state.Artifact) {
				return fmt.Errorf("source final artifact conflicts with durable state for job %q", state.JobID)
			}
			continue
		}
		if currentGeneration > state.Generation && state.Artifact != nil {
			return fmt.Errorf("source final artifact generation is stale for job %q", state.JobID)
		}
		if currentGeneration < state.Generation {
			result, err := tx.ExecContext(ctx, `UPDATE k12_grading_jobs
				SET finalization_generation=?
				WHERE agent_name=? AND record_id=? AND finalization_generation=?`,
				state.Generation, state.AgentName, state.JobID, currentGeneration,
			)
			if err != nil {
				return fmt.Errorf("restore source finalization generation: %w", err)
			}
			if changed, err := result.RowsAffected(); err != nil || changed != 1 {
				return fmt.Errorf("restore source finalization generation CAS failed for job %q", state.JobID)
			}
			currentGeneration = state.Generation
		}
		if state.Artifact == nil {
			continue
		}
		if currentGeneration != state.Generation {
			return fmt.Errorf("source final artifact generation conflict for job %q", state.JobID)
		}
		artifact := *state.Artifact
		if _, err := tx.ExecContext(ctx, `INSERT INTO k12_grading_final_artifacts (`+gradingFinalArtifactColumns+`,finalization_generation)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			artifact.ArtifactID, artifact.AgentName, artifact.JobID,
			artifact.StructureVersion, artifact.CoverageStatus,
			artifact.TotalCount, artifact.PublishedCount, artifact.SkippedCount,
			artifact.OrderedCurrentDigestsJSON, artifact.CanonicalMarkdown,
			artifact.ArtifactDigest, artifact.SummaryInvocationID,
			artifact.CreatedAt, artifact.UpdatedAt, state.Generation,
		); err != nil {
			return fmt.Errorf("restore source final artifact: %w", err)
		}
	}
	return nil
}

func boolIntStorage(v bool) int {
	if v {
		return 1
	}
	return 0
}

func ensureProblemSourceArchiveSubset(want, got ProblemSourceArchiveV6) error {
	// Archives are exported in deterministic primary-key order. A restore into
	// an empty scope must be byte-semantic equal; merge restores may contain
	// additional chains, so compare every requested row by its stable key.
	if err := archiveSliceSubset("PageAsset", want.PageAssets, got.PageAssets, func(v ProblemSourceArchivePageAsset) string { return v.PageAssetID }); err != nil {
		return err
	}
	if err := archiveSliceSubset("dispatch", want.Dispatches, got.Dispatches, func(v k12.ImageTaskDispatch) string { return v.DispatchID }); err != nil {
		return err
	}
	if err := archiveSliceSubset("dispatch owner", want.DispatchOwners, got.DispatchOwners, func(v ProblemSourceArchiveDispatchOwner) string { return v.DispatchID }); err != nil {
		return err
	}
	if err := archiveSliceSubset("homework submission", want.HomeworkSubmissions, got.HomeworkSubmissions, func(v k12.HomeworkSubmission) string { return v.SubmissionID }); err != nil {
		return err
	}
	if err := archiveSliceSubset("structure snapshot", want.StructureSnapshots, got.StructureSnapshots, func(v ProblemSourceArchiveStructureSnapshot) string {
		return fmt.Sprintf("%s/%d", v.SubmissionID, v.StructureVersion)
	}); err != nil {
		return err
	}
	if err := archiveSliceSubset("structure member", want.StructureMembers, got.StructureMembers, func(v ProblemSourceArchiveStructureMember) string {
		return fmt.Sprintf("%s/%d/%s", v.SubmissionID, v.StructureVersion, v.ProblemID)
	}); err != nil {
		return err
	}
	if err := archiveSliceSubset("dependency group", want.DependencyGroups, got.DependencyGroups, func(v ProblemSourceArchiveDependencyGroup) string {
		return fmt.Sprintf("%s/%d/%s", v.SubmissionID, v.StructureVersion, v.DependencyGroupID)
	}); err != nil {
		return err
	}
	if err := archiveSliceSubset("receipt", want.ActionReceipts, got.ActionReceipts, func(v ProblemSourceArchiveActionReceipt) string { return v.CommandReceiptID }); err != nil {
		return err
	}
	if err := problemSourceFinalizationSubset(want.FinalizationGenerations, got.FinalizationGenerations); err != nil {
		return err
	}
	if err := archiveSliceSubset("input revision", want.InputRevisions, got.InputRevisions, func(v ProblemSourceArchiveInputRevision) string {
		return fmt.Sprintf("%s/%d/%s/%d", v.SubmissionID, v.StructureVersion, v.ProblemID, v.InputRevision)
	}); err != nil {
		return err
	}
	if err := archiveSliceSubset("work", want.ReprocessJobs, got.ReprocessJobs, func(v ProblemSourceArchiveReprocessJob) string { return v.WorkID }); err != nil {
		return err
	}
	if err := archiveSliceSubset("model invocation", want.ModelInvocations, got.ModelInvocations, func(v k12.ModelInvocation) string { return v.InvocationID }); err != nil {
		return err
	}
	if err := archiveSliceSubset("physical model invocation", want.ModelPhysicalInvocations, got.ModelPhysicalInvocations, func(v k12.ModelPhysicalInvocation) string { return v.PhysicalInvocationID }); err != nil {
		return err
	}
	if err := archiveSliceSubset("recognition result", want.RecognitionResults, got.RecognitionResults, func(v ProblemSourceArchiveRecognitionResult) string { return v.WorkID }); err != nil {
		return err
	}
	if err := archiveSliceSubset("recognition item", want.RecognitionItems, got.RecognitionItems, func(v ProblemSourceArchiveRecognitionItem) string { return v.WorkID + "/" + v.ProblemID }); err != nil {
		return err
	}
	if err := archiveSliceSubset("recognition physical result", want.RecognitionPhysicalResults, got.RecognitionPhysicalResults, func(v ProblemSourceArchiveRecognitionPhysicalResult) string {
		return v.WorkID + "/" + v.PhysicalInvocationID
	}); err != nil {
		return err
	}
	return nil
}

func problemSourceFinalizationSubset(
	want []ProblemSourceArchiveFinalizationGeneration,
	got []ProblemSourceArchiveFinalizationGeneration,
) error {
	byJob := make(map[string]ProblemSourceArchiveFinalizationGeneration, len(got))
	for _, state := range got {
		byJob[state.JobID] = state
	}
	for _, expected := range want {
		stored, ok := byJob[expected.JobID]
		if !ok || stored.AgentName != expected.AgentName ||
			stored.Generation < expected.Generation {
			return fmt.Errorf("problem-source archive finalization %q conflicts with durable state", expected.JobID)
		}
		if expected.Artifact != nil &&
			(stored.Generation != expected.Generation ||
				!reflect.DeepEqual(stored.Artifact, expected.Artifact)) {
			return fmt.Errorf("problem-source archive final artifact %q conflicts with durable state", expected.JobID)
		}
	}
	return nil
}

func archiveSliceSubset[T any](name string, want, got []T, key func(T) string) error {
	byKey := make(map[string]T, len(got))
	for _, v := range got {
		byKey[key(v)] = v
	}
	for _, v := range want {
		stored, ok := byKey[key(v)]
		if !ok || !reflect.DeepEqual(v, stored) {
			return fmt.Errorf("problem-source archive %s %q conflicts with durable state", name, key(v))
		}
	}
	return nil
}
