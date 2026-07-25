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

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

var (
	ErrImageTaskNotFound        = errors.New("image task not found")
	ErrImageTaskConflict        = errors.New("image task immutable identity conflict")
	ErrImageTaskVersionConflict = errors.New("image task version conflict")
	ErrImageTaskInvalidState    = errors.New("image task invalid state")
)

type ImageTaskRoutingDecision struct {
	Intent                   k12.ImageTaskIntent
	Evidence                 []string
	Confidence               float64
	ConfirmationCandidates   []k12.ImageTaskIntent
	WorkTitleCandidate       *k12.FactCandidate
	TaskRequirementCandidate *k12.FactCandidate
	InvocationResultDigest   string
}

type ImageTaskRouteTarget struct {
	HomeworkSubmission *k12.HomeworkSubmission
	CreativeIntake     *k12.CreativeWorkIntake
}

func jsonString(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func validateImageTaskInvocation(inv k12.ImageTaskInvocation) error {
	if strings.TrimSpace(inv.InvocationID) == "" || strings.TrimSpace(inv.AgentName) == "" ||
		strings.TrimSpace(inv.OperationKey) == "" || strings.TrimSpace(inv.RequestDigest) == "" ||
		inv.Attempt < 1 {
		return fmt.Errorf("image task invocation identity 不完整")
	}
	if err := inv.RouteSnapshot.Validate(); err != nil {
		return err
	}
	switch inv.Operation {
	case k12.ImageTaskOperationClassification:
		if inv.DispatchID == "" || inv.IntakeID != "" || inv.WorkRecordID != "" {
			return fmt.Errorf("classification invocation owner 非法")
		}
	case k12.ImageTaskOperationWritingOCR:
		if inv.DispatchID != "" || inv.IntakeID == "" || inv.WorkRecordID != "" {
			return fmt.Errorf("writing OCR invocation owner 非法")
		}
	case k12.ImageTaskOperationWorkFeedback:
		if inv.DispatchID != "" || inv.IntakeID != "" || inv.WorkRecordID == "" {
			return fmt.Errorf("work feedback invocation owner 非法")
		}
	default:
		return fmt.Errorf("image task invocation operation 非法: %q", inv.Operation)
	}
	if inv.Status == "" {
		inv.Status = k12.ImageTaskInvocationPrepared
	}
	if inv.Status != k12.ImageTaskInvocationPrepared {
		return fmt.Errorf("new image task invocation 必须从 prepared 开始")
	}
	return nil
}

// PrepareImageTaskDispatch atomically persists the routing root and its
// classification invocation before any provider request can escape.
func (s *Store) PrepareImageTaskDispatch(
	ctx context.Context,
	dispatch k12.ImageTaskDispatch,
	invocation k12.ImageTaskInvocation,
) (k12.ImageTaskDispatch, bool, error) {
	if err := dispatch.Validate(); err != nil {
		return k12.ImageTaskDispatch{}, false, err
	}
	if err := validateImageTaskInvocation(invocation); err != nil {
		return k12.ImageTaskDispatch{}, false, err
	}
	if invocation.AgentName != dispatch.AgentName ||
		invocation.DispatchID != dispatch.DispatchID ||
		invocation.InvocationID != dispatch.ClassificationInvocationID ||
		invocation.RouteSnapshot != dispatch.ClassificationRouteSnapshot {
		return k12.ImageTaskDispatch{}, false, fmt.Errorf("%w: classification invocation 与 dispatch 不一致", ErrImageTaskConflict)
	}
	if err := ensureAgentRegistered(ctx, s.db, dispatch.AgentName); err != nil {
		return k12.ImageTaskDispatch{}, false, err
	}
	sourceAssetsJSON, err := jsonString(dispatch.SourceAssetRefs)
	if err != nil {
		return k12.ImageTaskDispatch{}, false, err
	}
	evidenceJSON, err := jsonString(dispatch.IntentEvidence)
	if err != nil {
		return k12.ImageTaskDispatch{}, false, err
	}
	candidatesJSON, err := jsonString(dispatch.ConfirmationCandidates)
	if err != nil {
		return k12.ImageTaskDispatch{}, false, err
	}
	classificationRouteJSON, err := jsonString(dispatch.ClassificationRouteSnapshot)
	if err != nil {
		return k12.ImageTaskDispatch{}, false, err
	}
	routePolicyJSON, err := jsonString(dispatch.RoutePolicySnapshot)
	if err != nil {
		return k12.ImageTaskDispatch{}, false, err
	}
	invocationRouteJSON, err := jsonString(invocation.RouteSnapshot)
	if err != nil {
		return k12.ImageTaskDispatch{}, false, err
	}
	if dispatch.CreatedAt == 0 {
		dispatch.CreatedAt = nowUnix()
	}
	if dispatch.UpdatedAt == 0 {
		dispatch.UpdatedAt = dispatch.CreatedAt
	}
	if invocation.CreatedAt == 0 {
		invocation.CreatedAt = dispatch.CreatedAt
	}
	if invocation.UpdatedAt == 0 {
		invocation.UpdatedAt = invocation.CreatedAt
	}
	if invocation.Status == "" {
		invocation.Status = k12.ImageTaskInvocationPrepared
	}
	if dispatch.RoutingProvenance == "" {
		dispatch.RoutingProvenance = k12.ImageTaskRoutingModelClassified
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.ImageTaskDispatch{}, false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO k12_image_task_dispatches
        (dispatch_id,agent_name,learner_id,source_kind,source_ref,source_session_id,
         source_asset_refs_json,source_digest,message_intent,task_intent,intent_evidence_json,
         intent_confidence,confirmation_candidates_json,status,target_object_type,target_object_id,
         classification_route_snapshot_json,classification_invocation_id,route_policy_snapshot_json,
         idempotency_key,request_digest,attempt_generation,retry_safe,failure_kind,version,created_at,updated_at,
         routing_provenance,creative_entry_json,operation_route_request_json)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(agent_name,idempotency_key) DO NOTHING`,
		dispatch.DispatchID, dispatch.AgentName, dispatch.LearnerID, dispatch.SourceKind,
		dispatch.SourceRef, dispatch.SourceSessionID, sourceAssetsJSON, dispatch.SourceDigest,
		dispatch.MessageIntent, dispatch.TaskIntent, evidenceJSON, dispatch.IntentConfidence,
		candidatesJSON, dispatch.Status, dispatch.TargetObjectType, dispatch.TargetObjectID,
		classificationRouteJSON, dispatch.ClassificationInvocationID, routePolicyJSON,
		dispatch.IdempotencyKey, dispatch.RequestDigest, dispatch.AttemptGeneration,
		boolInt(dispatch.RetrySafe), dispatch.FailureKind, dispatch.Version,
		dispatch.CreatedAt, dispatch.UpdatedAt, dispatch.RoutingProvenance, "", "")
	if err != nil {
		return k12.ImageTaskDispatch{}, false, fmt.Errorf("prepare image task dispatch: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		stored, err := getImageTaskDispatch(ctx, tx, dispatch.AgentName, "", dispatch.IdempotencyKey)
		if err != nil {
			return k12.ImageTaskDispatch{}, false, err
		}
		if stored.RequestDigest != dispatch.RequestDigest ||
			stored.SourceDigest != dispatch.SourceDigest ||
			stored.AttemptGeneration != dispatch.AttemptGeneration ||
			stored.ClassificationRouteSnapshot != dispatch.ClassificationRouteSnapshot ||
			stored.RoutePolicySnapshot != dispatch.RoutePolicySnapshot {
			return k12.ImageTaskDispatch{}, false, fmt.Errorf("%w: idempotency key %q already bound to another input/route",
				ErrImageTaskConflict, dispatch.IdempotencyKey)
		}
		var existingInvocation string
		if err := tx.QueryRowContext(ctx, `SELECT invocation_id FROM k12_image_task_invocations
            WHERE agent_name=? AND dispatch_id=? AND operation='classification'`,
			dispatch.AgentName, stored.DispatchID).Scan(&existingInvocation); err != nil {
			return k12.ImageTaskDispatch{}, false, fmt.Errorf("replay classification invocation missing: %w", err)
		}
		if existingInvocation != stored.ClassificationInvocationID {
			return k12.ImageTaskDispatch{}, false, fmt.Errorf("%w: classification invocation drift", ErrImageTaskConflict)
		}
		if err := tx.Commit(); err != nil {
			return k12.ImageTaskDispatch{}, false, err
		}
		return stored, false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_image_task_invocations
        (invocation_id,agent_name,dispatch_id,intake_id,work_record_id,operation,operation_key,
         request_digest,route_snapshot_json,status,attempt,provider_request_key,result_digest,
         result_json,error_kind,retry_safe,started_at,finished_at,created_at,updated_at)
        VALUES(?,?,?,NULL,NULL,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		invocation.InvocationID, invocation.AgentName, invocation.DispatchID,
		invocation.Operation, invocation.OperationKey, invocation.RequestDigest,
		invocationRouteJSON, invocation.Status, invocation.Attempt,
		invocation.ProviderRequestKey, invocation.ResultDigest, invocation.ResultJSON,
		invocation.ErrorKind, boolInt(invocation.RetrySafe), invocation.StartedAt,
		invocation.FinishedAt, invocation.CreatedAt, invocation.UpdatedAt); err != nil {
		return k12.ImageTaskDispatch{}, false, fmt.Errorf("prepare classification invocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return k12.ImageTaskDispatch{}, false, err
	}
	return dispatch, true, nil
}

// PrepareParentSelectedCreativeDispatch atomically persists the manually
// selected dispatch and its CreativeWorkIntake. It deliberately creates no
// classification invocation and stores an empty classification snapshot.
func (s *Store) PrepareParentSelectedCreativeDispatch(
	ctx context.Context,
	dispatch k12.ImageTaskDispatch,
) (k12.ImageTaskDispatch, *k12.CreativeWorkIntake, bool, error) {
	if dispatch.OperationRouteRequest == (k12.ImageTaskRouteSnapshot{}) {
		dispatch.OperationRouteRequest = dispatch.RoutePolicySnapshot
	}
	dispatch.RoutePolicySnapshot = k12.ImageTaskRouteSnapshot{}
	dispatch.RoutingProvenance = k12.ImageTaskRoutingParentSelected
	dispatch.Status = k12.ImageTaskStatusRouted
	dispatch.TargetObjectType = k12.ImageTaskTargetCreativeWorkIntake
	if strings.TrimSpace(dispatch.TargetObjectID) == "" {
		dispatch.TargetObjectID = idgen.NanoID()
	}
	dispatch.ClassificationRouteSnapshot = k12.ImageTaskRouteSnapshot{}
	dispatch.ClassificationInvocationID = ""
	if err := dispatch.Validate(); err != nil {
		return k12.ImageTaskDispatch{}, nil, false, err
	}
	if err := ensureAgentRegistered(ctx, s.db, dispatch.AgentName); err != nil {
		return k12.ImageTaskDispatch{}, nil, false, err
	}
	sourceAssetsJSON, err := jsonString(dispatch.SourceAssetRefs)
	if err != nil {
		return k12.ImageTaskDispatch{}, nil, false, err
	}
	evidenceJSON, _ := jsonString(dispatch.IntentEvidence)
	candidatesJSON, _ := jsonString(dispatch.ConfirmationCandidates)
	routePolicyJSON := ""
	creativeEntryJSON, err := jsonString(dispatch.CreativeEntry)
	if err != nil {
		return k12.ImageTaskDispatch{}, nil, false, err
	}
	operationRouteRequestJSON := ""
	if dispatch.OperationRouteRequest != (k12.ImageTaskRouteSnapshot{}) {
		operationRouteRequestJSON, err = jsonString(dispatch.OperationRouteRequest)
		if err != nil {
			return k12.ImageTaskDispatch{}, nil, false, err
		}
	}
	if dispatch.CreatedAt == 0 {
		dispatch.CreatedAt = nowUnix()
	}
	if dispatch.UpdatedAt == 0 {
		dispatch.UpdatedAt = dispatch.CreatedAt
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.ImageTaskDispatch{}, nil, false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO k12_image_task_dispatches
        (dispatch_id,agent_name,learner_id,source_kind,source_ref,source_session_id,
         source_asset_refs_json,source_digest,message_intent,task_intent,intent_evidence_json,
         intent_confidence,confirmation_candidates_json,status,target_object_type,target_object_id,
         classification_route_snapshot_json,classification_invocation_id,route_policy_snapshot_json,
         idempotency_key,request_digest,attempt_generation,retry_safe,failure_kind,version,created_at,updated_at,
         routing_provenance,creative_entry_json,operation_route_request_json)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(agent_name,idempotency_key) DO NOTHING`,
		dispatch.DispatchID, dispatch.AgentName, dispatch.LearnerID, dispatch.SourceKind,
		dispatch.SourceRef, dispatch.SourceSessionID, sourceAssetsJSON, dispatch.SourceDigest,
		dispatch.MessageIntent, dispatch.TaskIntent, evidenceJSON, dispatch.IntentConfidence,
		candidatesJSON, dispatch.Status, dispatch.TargetObjectType, dispatch.TargetObjectID,
		"", "", routePolicyJSON, dispatch.IdempotencyKey, dispatch.RequestDigest,
		dispatch.AttemptGeneration, boolInt(dispatch.RetrySafe), dispatch.FailureKind,
		dispatch.Version, dispatch.CreatedAt, dispatch.UpdatedAt,
		dispatch.RoutingProvenance, creativeEntryJSON, operationRouteRequestJSON)
	if err != nil {
		return k12.ImageTaskDispatch{}, nil, false,
			fmt.Errorf("prepare parent-selected image task dispatch: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		stored, getErr := getImageTaskDispatch(
			ctx, tx, dispatch.AgentName, "", dispatch.IdempotencyKey,
		)
		if getErr != nil {
			return k12.ImageTaskDispatch{}, nil, false, getErr
		}
		storedEntryJSON, _ := jsonString(stored.CreativeEntry)
		storedRouteRequestJSON, _ := jsonString(stored.OperationRouteRequest)
		if stored.RequestDigest != dispatch.RequestDigest ||
			stored.RoutingProvenance != k12.ImageTaskRoutingParentSelected ||
			storedEntryJSON != creativeEntryJSON ||
			storedRouteRequestJSON != operationRouteRequestJSON {
			return k12.ImageTaskDispatch{}, nil, false,
				fmt.Errorf("%w: manual idempotency key bound to another input", ErrImageTaskConflict)
		}
		target, getErr := getImageTaskRouteTarget(ctx, tx, stored)
		if getErr != nil {
			return k12.ImageTaskDispatch{}, nil, false, getErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return k12.ImageTaskDispatch{}, nil, false, commitErr
		}
		return stored, target.CreativeIntake, false, nil
	}
	if dispatch.CreativeEntry.Kind == k12.CreativeWorkEntryRevision {
		rec, getErr := s.getVia(ctx, tx, dispatch.CreativeEntry.WorkID)
		if getErr != nil || rec == nil || rec.AgentName != dispatch.AgentName ||
			rec.Collection != k12.CollectionCreativeWork {
			return k12.ImageTaskDispatch{}, nil, false,
				fmt.Errorf("%w: revision target not found for owner", ErrImageTaskConflict)
		}
		if rec.Status == k12.WorkStatusArchived {
			return k12.ImageTaskDispatch{}, nil, false,
				fmt.Errorf("%w: archived work cannot accept revision intake", ErrImageTaskInvalidState)
		}
		fields, parseErr := k12.ParseCreativeWorkFields(rec.Fields)
		if parseErr != nil {
			return k12.ImageTaskDispatch{}, nil, false, parseErr
		}
		expectedWorkType := k12.WorkTypeArt
		if dispatch.TaskIntent == k12.ImageTaskIntentWriting {
			expectedWorkType = k12.WorkTypeWriting
		}
		if fields.WorkType != expectedWorkType || len(fields.Versions) == 0 ||
			fields.Versions[len(fields.Versions)-1].VersionID !=
				dispatch.CreativeEntry.BaseVersionID {
			return k12.ImageTaskDispatch{}, nil, false,
				fmt.Errorf("%w: revision type/base version drift", ErrImageTaskVersionConflict)
		}
	}
	target, err := createImageTaskRouteTarget(
		ctx, tx, &dispatch, dispatch.TaskIntent, nil, nil, dispatch.CreatedAt,
	)
	if err != nil {
		return k12.ImageTaskDispatch{}, nil, false, err
	}
	if target.CreativeIntake == nil ||
		target.CreativeIntake.IntakeID != dispatch.TargetObjectID {
		return k12.ImageTaskDispatch{}, nil, false,
			fmt.Errorf("%w: manual creative target drift", ErrImageTaskConflict)
	}
	if err := tx.Commit(); err != nil {
		return k12.ImageTaskDispatch{}, nil, false, err
	}
	return dispatch, target.CreativeIntake, true, nil
}

type imageTaskRowScanner interface {
	Scan(dest ...any) error
}

func scanImageTaskDispatch(row imageTaskRowScanner) (k12.ImageTaskDispatch, error) {
	var d k12.ImageTaskDispatch
	var sourceAssetsJSON, evidenceJSON, candidatesJSON, classificationRouteJSON, routePolicyJSON string
	var creativeEntryJSON, operationRouteRequestJSON string
	var retrySafe int
	err := row.Scan(&d.DispatchID, &d.AgentName, &d.LearnerID, &d.SourceKind,
		&d.SourceRef, &d.SourceSessionID, &sourceAssetsJSON, &d.SourceDigest,
		&d.MessageIntent, &d.TaskIntent, &evidenceJSON, &d.IntentConfidence,
		&candidatesJSON, &d.Status, &d.TargetObjectType, &d.TargetObjectID,
		&classificationRouteJSON, &d.ClassificationInvocationID, &routePolicyJSON,
		&d.IdempotencyKey, &d.RequestDigest, &d.AttemptGeneration, &retrySafe,
		&d.FailureKind, &d.Version, &d.CreatedAt, &d.UpdatedAt,
		&d.RoutingProvenance, &creativeEntryJSON, &operationRouteRequestJSON)
	if err != nil {
		return k12.ImageTaskDispatch{}, err
	}
	if err := json.Unmarshal([]byte(sourceAssetsJSON), &d.SourceAssetRefs); err != nil {
		return d, fmt.Errorf("decode dispatch source assets: %w", err)
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &d.IntentEvidence); err != nil {
		return d, fmt.Errorf("decode dispatch intent evidence: %w", err)
	}
	if err := json.Unmarshal([]byte(candidatesJSON), &d.ConfirmationCandidates); err != nil {
		return d, fmt.Errorf("decode dispatch confirmation candidates: %w", err)
	}
	if classificationRouteJSON != "" {
		if err := json.Unmarshal([]byte(classificationRouteJSON), &d.ClassificationRouteSnapshot); err != nil {
			return d, fmt.Errorf("decode dispatch classification route: %w", err)
		}
	}
	if routePolicyJSON != "" {
		if err := json.Unmarshal([]byte(routePolicyJSON), &d.RoutePolicySnapshot); err != nil {
			return d, fmt.Errorf("decode dispatch route policy: %w", err)
		}
	}
	if creativeEntryJSON != "" {
		d.CreativeEntry = &k12.ImageTaskCreativeEntry{}
		if err := json.Unmarshal([]byte(creativeEntryJSON), d.CreativeEntry); err != nil {
			return d, fmt.Errorf("decode dispatch creative entry: %w", err)
		}
	}
	if operationRouteRequestJSON != "" {
		if err := json.Unmarshal(
			[]byte(operationRouteRequestJSON), &d.OperationRouteRequest,
		); err != nil {
			return d, fmt.Errorf("decode dispatch operation route request: %w", err)
		}
	}
	d.RetrySafe = retrySafe != 0
	return d, nil
}

const imageTaskDispatchSelect = `SELECT dispatch_id,agent_name,learner_id,source_kind,source_ref,
    source_session_id,source_asset_refs_json,source_digest,message_intent,task_intent,
    intent_evidence_json,intent_confidence,confirmation_candidates_json,status,
    target_object_type,target_object_id,classification_route_snapshot_json,
    classification_invocation_id,route_policy_snapshot_json,idempotency_key,request_digest,
    attempt_generation,retry_safe,failure_kind,version,created_at,updated_at,
    routing_provenance,creative_entry_json,operation_route_request_json
    FROM k12_image_task_dispatches`

func getImageTaskDispatch(
	ctx context.Context,
	q dbQueryer,
	agentName, dispatchID, idempotencyKey string,
) (k12.ImageTaskDispatch, error) {
	var row *sql.Row
	if dispatchID != "" {
		row = q.QueryRowContext(ctx, imageTaskDispatchSelect+
			` WHERE agent_name=? AND dispatch_id=?`, agentName, dispatchID)
	} else {
		row = q.QueryRowContext(ctx, imageTaskDispatchSelect+
			` WHERE agent_name=? AND idempotency_key=?`, agentName, idempotencyKey)
	}
	d, err := scanImageTaskDispatch(row)
	if err == sql.ErrNoRows {
		return k12.ImageTaskDispatch{}, ErrImageTaskNotFound
	}
	if err != nil {
		return k12.ImageTaskDispatch{}, fmt.Errorf("get image task dispatch: %w", err)
	}
	return d, nil
}

func (s *Store) GetImageTaskDispatch(
	ctx context.Context,
	agentName, dispatchID string,
) (k12.ImageTaskDispatch, error) {
	return getImageTaskDispatch(ctx, s.db, agentName, dispatchID, "")
}

func (s *Store) GetImageTaskDispatchByIdempotency(
	ctx context.Context,
	agentName, idempotencyKey string,
) (k12.ImageTaskDispatch, error) {
	return getImageTaskDispatch(ctx, s.db, agentName, "", idempotencyKey)
}

// ListImageTaskDispatchesForRecovery returns only non-terminal automatic
// checkpoints. Awaiting parent confirmation and explicit retry failures are
// intentionally excluded: recovery must never turn a restart into consent or
// a blind provider retry.
func (s *Store) ListImageTaskDispatchesForRecovery(
	ctx context.Context,
	agentName string,
) ([]k12.ImageTaskDispatch, error) {
	rows, err := s.db.QueryContext(
		ctx,
		imageTaskDispatchSelect+
			` WHERE agent_name=? AND status IN ('routing','routed')
			  ORDER BY created_at,dispatch_id`,
		strings.TrimSpace(agentName),
	)
	if err != nil {
		return nil, fmt.Errorf("list image task recovery dispatches: %w", err)
	}
	defer rows.Close()
	out := []k12.ImageTaskDispatch{}
	for rows.Next() {
		dispatch, scanErr := scanImageTaskDispatch(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, dispatch)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) GetImageTaskInvocation(
	ctx context.Context,
	agentName, invocationID string,
) (k12.ImageTaskInvocation, error) {
	return getImageTaskInvocation(ctx, s.db, agentName, invocationID)
}

func (s *Store) GetLatestWritingOCRInvocation(
	ctx context.Context,
	agentName, intakeID string,
) (k12.ImageTaskInvocation, error) {
	return getLatestImageTaskInvocation(
		ctx,
		s.db,
		agentName,
		k12.ImageTaskOperationWritingOCR,
		"",
		intakeID,
	)
}

func getImageTaskInvocation(
	ctx context.Context,
	q dbQueryer,
	agentName, invocationID string,
) (k12.ImageTaskInvocation, error) {
	var inv k12.ImageTaskInvocation
	var dispatchID, intakeID, workID sql.NullString
	var routeJSON string
	var retrySafe int
	err := q.QueryRowContext(ctx, `SELECT invocation_id,agent_name,dispatch_id,intake_id,
        work_record_id,operation,operation_key,request_digest,route_snapshot_json,status,
        attempt,provider_request_key,result_digest,result_json,error_kind,retry_safe,
        started_at,finished_at,created_at,updated_at
        FROM k12_image_task_invocations WHERE agent_name=? AND invocation_id=?`,
		agentName, invocationID).Scan(&inv.InvocationID, &inv.AgentName, &dispatchID, &intakeID,
		&workID, &inv.Operation, &inv.OperationKey, &inv.RequestDigest, &routeJSON,
		&inv.Status, &inv.Attempt, &inv.ProviderRequestKey, &inv.ResultDigest,
		&inv.ResultJSON, &inv.ErrorKind, &retrySafe, &inv.StartedAt,
		&inv.FinishedAt, &inv.CreatedAt, &inv.UpdatedAt)
	if err == sql.ErrNoRows {
		return inv, ErrImageTaskNotFound
	}
	if err != nil {
		return inv, err
	}
	inv.DispatchID, inv.IntakeID, inv.WorkRecordID = dispatchID.String, intakeID.String, workID.String
	inv.RetrySafe = retrySafe != 0
	if err := json.Unmarshal([]byte(routeJSON), &inv.RouteSnapshot); err != nil {
		return inv, err
	}
	return inv, nil
}

func getLatestImageTaskInvocation(
	ctx context.Context,
	q dbQueryer,
	agentName string,
	operation k12.ImageTaskOperation,
	dispatchID, intakeID string,
) (k12.ImageTaskInvocation, error) {
	query := `SELECT invocation_id FROM k12_image_task_invocations
        WHERE agent_name=? AND operation=?`
	args := []any{agentName, operation}
	if dispatchID != "" {
		query += ` AND dispatch_id=?`
		args = append(args, dispatchID)
	}
	if intakeID != "" {
		query += ` AND intake_id=?`
		args = append(args, intakeID)
	}
	query += ` ORDER BY attempt DESC,created_at DESC LIMIT 1`
	var invocationID string
	if err := q.QueryRowContext(ctx, query, args...).Scan(&invocationID); err != nil {
		if err == sql.ErrNoRows {
			return k12.ImageTaskInvocation{}, ErrImageTaskNotFound
		}
		return k12.ImageTaskInvocation{}, err
	}
	return getImageTaskInvocation(ctx, q, agentName, invocationID)
}

// CommitImageTaskRouting stores the classifier result and creates exactly one
// target aggregate in the same transaction. An ambiguous decision only moves
// to awaiting_confirmation and creates no target.
func (s *Store) CommitImageTaskRouting(
	ctx context.Context,
	agentName, dispatchID string,
	expectedVersion int,
	decision ImageTaskRoutingDecision,
) (k12.ImageTaskDispatch, ImageTaskRouteTarget, error) {
	if !validRoutingDecision(decision) {
		return k12.ImageTaskDispatch{}, ImageTaskRouteTarget{},
			fmt.Errorf("%w: invalid routing decision", ErrImageTaskInvalidState)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.ImageTaskDispatch{}, ImageTaskRouteTarget{}, err
	}
	defer tx.Rollback()
	dispatch, err := getImageTaskDispatch(ctx, tx, agentName, dispatchID, "")
	if err != nil {
		return dispatch, ImageTaskRouteTarget{}, err
	}
	if dispatch.Status == k12.ImageTaskStatusRouted {
		if dispatch.TaskIntent != decision.Intent {
			return dispatch, ImageTaskRouteTarget{}, fmt.Errorf("%w: dispatch already routed", ErrImageTaskConflict)
		}
		target, err := getImageTaskRouteTarget(ctx, tx, dispatch)
		if err != nil {
			return dispatch, ImageTaskRouteTarget{}, err
		}
		return dispatch, target, tx.Commit()
	}
	if dispatch.Status != k12.ImageTaskStatusRouting &&
		dispatch.Status != k12.ImageTaskStatusAwaitingConfirmation {
		return dispatch, ImageTaskRouteTarget{}, fmt.Errorf("%w: dispatch status=%s", ErrImageTaskInvalidState, dispatch.Status)
	}
	if dispatch.Version != expectedVersion {
		return dispatch, ImageTaskRouteTarget{}, ErrImageTaskVersionConflict
	}
	evidenceJSON, _ := jsonString(decision.Evidence)
	candidatesJSON, _ := jsonString(decision.ConfirmationCandidates)
	resultJSON, _ := jsonString(decision)
	now := nowUnix()
	resultDigest := strings.TrimSpace(decision.InvocationResultDigest)
	if resultDigest == "" {
		sum := sha256.Sum256([]byte(resultJSON))
		resultDigest = "sha256:" + hex.EncodeToString(sum[:])
	}
	activeInvocation, err := getLatestImageTaskInvocation(
		ctx, tx, agentName, k12.ImageTaskOperationClassification, dispatchID, "",
	)
	if err != nil {
		return dispatch, ImageTaskRouteTarget{}, err
	}
	invRes, err := tx.ExecContext(ctx, `UPDATE k12_image_task_invocations
        SET status='succeeded',result_digest=?,result_json=?,retry_safe=0,
            finished_at=?,updated_at=?
        WHERE agent_name=? AND invocation_id=? AND operation='classification'
          AND status IN ('prepared','sent')`,
		resultDigest, resultJSON, now, now, agentName, activeInvocation.InvocationID)
	if err != nil {
		return dispatch, ImageTaskRouteTarget{}, fmt.Errorf("commit classification invocation: %w", err)
	}
	if n, _ := invRes.RowsAffected(); n != 1 {
		inv, getErr := getImageTaskInvocation(ctx, tx, agentName, activeInvocation.InvocationID)
		if getErr != nil || inv.Status != k12.ImageTaskInvocationSucceeded ||
			inv.ResultDigest != resultDigest {
			return dispatch, ImageTaskRouteTarget{}, fmt.Errorf("%w: classification invocation state conflict", ErrImageTaskConflict)
		}
	}

	dispatch.TaskIntent = decision.Intent
	dispatch.IntentEvidence = append([]string(nil), decision.Evidence...)
	dispatch.IntentConfidence = decision.Confidence
	dispatch.ConfirmationCandidates = append([]k12.ImageTaskIntent(nil), decision.ConfirmationCandidates...)
	dispatch.UpdatedAt = now

	var target ImageTaskRouteTarget
	if len(decision.ConfirmationCandidates) >= 2 || decision.Intent == k12.ImageTaskIntentUnknown {
		dispatch.Status = k12.ImageTaskStatusAwaitingConfirmation
	} else {
		dispatch.Status = k12.ImageTaskStatusRouted
		target, err = createImageTaskRouteTarget(
			ctx, tx, &dispatch, decision.Intent,
			decision.WorkTitleCandidate, decision.TaskRequirementCandidate, now,
		)
		if err != nil {
			return dispatch, target, err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE k12_image_task_dispatches
        SET task_intent=?,intent_evidence_json=?,intent_confidence=?,
            confirmation_candidates_json=?,status=?,target_object_type=?,target_object_id=?,
            retry_safe=0,failure_kind='',version=version+1,updated_at=?
        WHERE agent_name=? AND dispatch_id=? AND version=?`,
		dispatch.TaskIntent, evidenceJSON, dispatch.IntentConfidence, candidatesJSON,
		dispatch.Status, dispatch.TargetObjectType, dispatch.TargetObjectID, now,
		agentName, dispatchID, expectedVersion)
	if err != nil {
		return dispatch, target, fmt.Errorf("commit image task route: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return dispatch, target, ErrImageTaskVersionConflict
	}
	dispatch.Version++
	if err := tx.Commit(); err != nil {
		return dispatch, target, err
	}
	return dispatch, target, nil
}

func createImageTaskRouteTarget(
	ctx context.Context,
	tx *sql.Tx,
	dispatch *k12.ImageTaskDispatch,
	intent k12.ImageTaskIntent,
	workTitle, taskRequirement *k12.FactCandidate,
	now int64,
) (ImageTaskRouteTarget, error) {
	var target ImageTaskRouteTarget
	switch intent {
	case k12.ImageTaskIntentCompletedHomework, k12.ImageTaskIntentBlankWorksheet:
		submission := k12.HomeworkSubmission{
			SubmissionID: idgen.NanoID(), DispatchID: dispatch.DispatchID,
			AgentName: dispatch.AgentName, LearnerID: dispatch.LearnerID,
			SourceKind: dispatch.SourceKind, SourceRef: dispatch.SourceRef,
			SourceAssetRefs: append([]string(nil), dispatch.SourceAssetRefs...),
			TaskIntent:      intent, Status: k12.HomeworkSubmissionReceived,
			IdempotencyKey: "dispatch:" + dispatch.DispatchID,
			CreatedAt:      now, UpdatedAt: now,
		}
		if err := insertHomeworkSubmission(ctx, tx, submission); err != nil {
			return target, err
		}
		dispatch.TargetObjectType = k12.ImageTaskTargetHomeworkSubmission
		dispatch.TargetObjectID = submission.SubmissionID
		target.HomeworkSubmission = &submission
	case k12.ImageTaskIntentWriting, k12.ImageTaskIntentArtwork:
		workType := k12.WorkTypeWriting
		status := k12.CreativeWorkIntakePreparing
		if intent == k12.ImageTaskIntentArtwork {
			workType = k12.WorkTypeArt
			status = k12.CreativeWorkIntakeReady
		}
		intakeID := strings.TrimSpace(dispatch.TargetObjectID)
		if intakeID == "" {
			intakeID = idgen.NanoID()
		}
		entryKind := k12.CreativeWorkEntryAuto
		promotionPolicy := k12.CreativeWorkPromotionAutomatic
		targetWorkID, baseVersionID := "", ""
		if dispatch.CreativeEntry != nil {
			entryKind = dispatch.CreativeEntry.Kind
			promotionPolicy = k12.CreativeWorkPromotionExplicitCommit
			targetWorkID = strings.TrimSpace(dispatch.CreativeEntry.WorkID)
			baseVersionID = strings.TrimSpace(dispatch.CreativeEntry.BaseVersionID)
		}
		intake := k12.CreativeWorkIntake{
			IntakeID: intakeID, DispatchID: dispatch.DispatchID,
			AgentName: dispatch.AgentName, LearnerID: dispatch.LearnerID,
			WorkType: workType, SourceAssetRefs: append([]string(nil), dispatch.SourceAssetRefs...),
			SourceDigest:             dispatch.SourceDigest,
			WorkTitleCandidate:       workTitle,
			TaskRequirementCandidate: taskRequirement,
			RoutePolicySnapshot:      dispatch.RoutePolicySnapshot,
			EntryKind:                entryKind,
			PromotionPolicy:          promotionPolicy,
			TargetWorkID:             targetWorkID,
			BaseVersionID:            baseVersionID,
			Status:                   status, IdempotencyKey: "dispatch:" + dispatch.DispatchID + ":" + workType,
			RequestDigest: dispatch.RequestDigest, AttemptGeneration: dispatch.AttemptGeneration,
			CreatedAt: now, UpdatedAt: now,
		}
		for _, candidate := range []*k12.FactCandidate{
			intake.WorkTitleCandidate,
			intake.TaskRequirementCandidate,
		} {
			if candidate == nil {
				continue
			}
			if candidate.Source == k12.FactCandidateSourceImageVision ||
				candidate.Source == k12.FactCandidateSourceImageOCR {
				if !strings.HasPrefix(
					strings.TrimSpace(candidate.EvidenceRef),
					"asset_index:0#",
				) {
					return target, fmt.Errorf(
						"%w: image candidate evidence 未绑定 source asset",
						ErrImageTaskConflict,
					)
				}
			}
		}
		if err := intake.Validate(); err != nil {
			return target, err
		}
		if err := insertCreativeWorkIntake(ctx, tx, intake); err != nil {
			return target, err
		}
		dispatch.TargetObjectType = k12.ImageTaskTargetCreativeWorkIntake
		dispatch.TargetObjectID = intake.IntakeID
		target.CreativeIntake = &intake
	default:
		return target, fmt.Errorf("%w: confirmed intent 非法", ErrImageTaskConflict)
	}
	return target, nil
}

// ConfirmImageTaskIntent is a distinct parent command. It never rewrites the
// classifier invocation receipt: it only selects one member of the immutable
// candidate exact-set and creates that target atomically.
func (s *Store) ConfirmImageTaskIntent(
	ctx context.Context,
	agentName, dispatchID string,
	expectedVersion int,
	intent k12.ImageTaskIntent,
) (k12.ImageTaskDispatch, ImageTaskRouteTarget, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.ImageTaskDispatch{}, ImageTaskRouteTarget{}, err
	}
	defer tx.Rollback()
	dispatch, err := getImageTaskDispatch(ctx, tx, agentName, dispatchID, "")
	if err != nil {
		return dispatch, ImageTaskRouteTarget{}, err
	}
	if dispatch.Status == k12.ImageTaskStatusRouted {
		if dispatch.TaskIntent != intent {
			return dispatch, ImageTaskRouteTarget{}, ErrImageTaskConflict
		}
		target, targetErr := getImageTaskRouteTarget(ctx, tx, dispatch)
		if targetErr != nil {
			return dispatch, target, targetErr
		}
		return dispatch, target, tx.Commit()
	}
	if dispatch.Status != k12.ImageTaskStatusAwaitingConfirmation {
		return dispatch, ImageTaskRouteTarget{}, ErrImageTaskInvalidState
	}
	if dispatch.Version != expectedVersion {
		return dispatch, ImageTaskRouteTarget{}, ErrImageTaskVersionConflict
	}
	allowed := false
	for _, candidate := range dispatch.ConfirmationCandidates {
		if candidate == intent {
			allowed = true
			break
		}
	}
	if !allowed {
		return dispatch, ImageTaskRouteTarget{}, ErrImageTaskConflict
	}
	invocation, err := getLatestImageTaskInvocation(
		ctx, tx, agentName, k12.ImageTaskOperationClassification, dispatchID, "",
	)
	if err != nil || invocation.Status != k12.ImageTaskInvocationSucceeded {
		return dispatch, ImageTaskRouteTarget{}, ErrImageTaskConflict
	}
	var classified ImageTaskRoutingDecision
	if err := json.Unmarshal([]byte(invocation.ResultJSON), &classified); err != nil {
		return dispatch, ImageTaskRouteTarget{}, fmt.Errorf("%w: classifier result receipt invalid", ErrImageTaskConflict)
	}
	now := nowUnix()
	dispatch.TaskIntent = intent
	dispatch.Status = k12.ImageTaskStatusRouted
	target, err := createImageTaskRouteTarget(
		ctx, tx, &dispatch, intent,
		classified.WorkTitleCandidate, classified.TaskRequirementCandidate, now,
	)
	if err != nil {
		return dispatch, target, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE k12_image_task_dispatches
        SET task_intent=?,status='routed',target_object_type=?,target_object_id=?,
            retry_safe=0,failure_kind='',version=version+1,updated_at=?
        WHERE agent_name=? AND dispatch_id=? AND status='awaiting_confirmation' AND version=?`,
		intent, dispatch.TargetObjectType, dispatch.TargetObjectID, now,
		agentName, dispatchID, expectedVersion)
	if err != nil {
		return dispatch, target, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return dispatch, target, ErrImageTaskVersionConflict
	}
	dispatch.Version++
	dispatch.UpdatedAt = now
	if err := tx.Commit(); err != nil {
		return dispatch, target, err
	}
	return dispatch, target, nil
}

func validRoutingDecision(d ImageTaskRoutingDecision) bool {
	if d.Confidence < 0 || d.Confidence > 1 {
		return false
	}
	switch d.Intent {
	case k12.ImageTaskIntentCompletedHomework, k12.ImageTaskIntentBlankWorksheet,
		k12.ImageTaskIntentWriting, k12.ImageTaskIntentArtwork:
		return len(d.ConfirmationCandidates) == 0
	case k12.ImageTaskIntentUnknown:
		if len(d.ConfirmationCandidates) < 2 {
			return false
		}
		for _, candidate := range d.ConfirmationCandidates {
			if candidate == k12.ImageTaskIntentUnknown {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func insertHomeworkSubmission(ctx context.Context, tx *sql.Tx, submission k12.HomeworkSubmission) error {
	assetsJSON, _ := jsonString(submission.SourceAssetRefs)
	_, err := tx.ExecContext(ctx, `INSERT INTO k12_homework_submissions
        (submission_id,dispatch_id,agent_name,learner_id,source_kind,source_ref,
         source_asset_refs_json,task_intent,status,grading_job_id,idempotency_key,
         version,created_at,updated_at)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		submission.SubmissionID, submission.DispatchID, submission.AgentName,
		submission.LearnerID, submission.SourceKind, submission.SourceRef,
		assetsJSON, submission.TaskIntent, submission.Status, submission.GradingJobID,
		submission.IdempotencyKey, submission.Version, submission.CreatedAt, submission.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create HomeworkSubmission: %w", err)
	}
	return nil
}

func insertCreativeWorkIntake(ctx context.Context, tx *sql.Tx, intake k12.CreativeWorkIntake) error {
	assetsJSON, _ := jsonString(intake.SourceAssetRefs)
	titleJSON, taskJSON, ocrJSON := "", "", ""
	var err error
	if intake.WorkTitleCandidate != nil {
		titleJSON, err = jsonString(intake.WorkTitleCandidate)
		if err != nil {
			return err
		}
	}
	if intake.TaskRequirementCandidate != nil {
		taskJSON, err = jsonString(intake.TaskRequirementCandidate)
		if err != nil {
			return err
		}
	}
	if intake.OCREvidence != nil {
		ocrJSON, err = jsonString(intake.OCREvidence)
		if err != nil {
			return err
		}
	}
	routeJSON := ""
	if intake.RoutePolicySnapshot != (k12.ImageTaskRouteSnapshot{}) {
		routeJSON, _ = jsonString(intake.RoutePolicySnapshot)
	}
	invocationsJSON, _ := jsonString(intake.OperationInvocations)
	commitReceiptJSON := ""
	if intake.CommitReceipt != nil {
		commitReceiptJSON, err = jsonString(intake.CommitReceipt)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO k12_creative_work_intakes
        (intake_id,dispatch_id,agent_name,learner_id,work_type,source_asset_refs_json,
         source_digest,work_title_candidate_json,task_requirement_candidate_json,
         ocr_evidence_json,route_policy_snapshot_json,operation_invocations_json,status,
         confirmation_provenance,promoted_work_id,idempotency_key,request_digest,
         attempt_generation,retry_safe,failure_kind,version,created_at,updated_at,
         entry_kind,promotion_policy,target_work_id,base_version_id,promoted_version_id,
         commit_receipt_json)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		intake.IntakeID, intake.DispatchID, intake.AgentName, intake.LearnerID, intake.WorkType,
		assetsJSON, intake.SourceDigest, titleJSON, taskJSON, ocrJSON, routeJSON,
		invocationsJSON, intake.Status, intake.ConfirmationProvenance,
		intake.PromotedWorkID, intake.IdempotencyKey, intake.RequestDigest,
		intake.AttemptGeneration, boolInt(intake.RetrySafe), intake.FailureKind,
		intake.Version, intake.CreatedAt, intake.UpdatedAt, intake.EntryKind,
		intake.PromotionPolicy, intake.TargetWorkID, intake.BaseVersionID,
		intake.PromotedVersionID, commitReceiptJSON)
	if err != nil {
		return fmt.Errorf("create CreativeWorkIntake: %w", err)
	}
	return nil
}

func getImageTaskRouteTarget(
	ctx context.Context,
	q dbQueryer,
	dispatch k12.ImageTaskDispatch,
) (ImageTaskRouteTarget, error) {
	switch dispatch.TargetObjectType {
	case k12.ImageTaskTargetHomeworkSubmission:
		submission, err := getHomeworkSubmission(ctx, q, dispatch.AgentName, dispatch.TargetObjectID)
		return ImageTaskRouteTarget{HomeworkSubmission: &submission}, err
	case k12.ImageTaskTargetCreativeWorkIntake:
		intake, err := getCreativeWorkIntake(ctx, q, dispatch.AgentName, dispatch.TargetObjectID)
		return ImageTaskRouteTarget{CreativeIntake: &intake}, err
	default:
		return ImageTaskRouteTarget{}, ErrImageTaskNotFound
	}
}

func getHomeworkSubmission(
	ctx context.Context,
	q dbQueryer,
	agentName, submissionID string,
) (k12.HomeworkSubmission, error) {
	var s k12.HomeworkSubmission
	var assetsJSON string
	err := q.QueryRowContext(ctx, `SELECT submission_id,dispatch_id,agent_name,learner_id,
        source_kind,source_ref,source_asset_refs_json,task_intent,status,grading_job_id,
        idempotency_key,version,created_at,updated_at FROM k12_homework_submissions
        WHERE agent_name=? AND submission_id=?`, agentName, submissionID).
		Scan(&s.SubmissionID, &s.DispatchID, &s.AgentName, &s.LearnerID, &s.SourceKind,
			&s.SourceRef, &assetsJSON, &s.TaskIntent, &s.Status, &s.GradingJobID,
			&s.IdempotencyKey, &s.Version, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return s, ErrImageTaskNotFound
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal([]byte(assetsJSON), &s.SourceAssetRefs); err != nil {
		return s, err
	}
	return s, nil
}

func (s *Store) GetHomeworkSubmission(
	ctx context.Context,
	agentName, submissionID string,
) (k12.HomeworkSubmission, error) {
	return getHomeworkSubmission(ctx, s.db, agentName, submissionID)
}

func getCreativeWorkIntake(
	ctx context.Context,
	q dbQueryer,
	agentName, intakeID string,
) (k12.CreativeWorkIntake, error) {
	var i k12.CreativeWorkIntake
	var assetsJSON, titleJSON, taskJSON, ocrJSON, routeJSON, invocationsJSON string
	var commitReceiptJSON string
	var retrySafe int
	err := q.QueryRowContext(ctx, `SELECT intake_id,dispatch_id,agent_name,learner_id,
        work_type,source_asset_refs_json,source_digest,work_title_candidate_json,
        task_requirement_candidate_json,ocr_evidence_json,route_policy_snapshot_json,
        operation_invocations_json,status,confirmation_provenance,promoted_work_id,
        idempotency_key,request_digest,attempt_generation,retry_safe,failure_kind,
        version,created_at,updated_at,entry_kind,promotion_policy,target_work_id,
        base_version_id,promoted_version_id,commit_receipt_json FROM k12_creative_work_intakes
        WHERE agent_name=? AND intake_id=?`, agentName, intakeID).
		Scan(&i.IntakeID, &i.DispatchID, &i.AgentName, &i.LearnerID, &i.WorkType,
			&assetsJSON, &i.SourceDigest, &titleJSON, &taskJSON, &ocrJSON, &routeJSON,
			&invocationsJSON, &i.Status, &i.ConfirmationProvenance, &i.PromotedWorkID,
			&i.IdempotencyKey, &i.RequestDigest, &i.AttemptGeneration, &retrySafe,
			&i.FailureKind, &i.Version, &i.CreatedAt, &i.UpdatedAt, &i.EntryKind,
			&i.PromotionPolicy, &i.TargetWorkID, &i.BaseVersionID,
			&i.PromotedVersionID, &commitReceiptJSON)
	if err == sql.ErrNoRows {
		return i, ErrImageTaskNotFound
	}
	if err != nil {
		return i, err
	}
	if err := json.Unmarshal([]byte(assetsJSON), &i.SourceAssetRefs); err != nil {
		return i, err
	}
	if titleJSON != "" {
		i.WorkTitleCandidate = &k12.FactCandidate{}
		if err := json.Unmarshal([]byte(titleJSON), i.WorkTitleCandidate); err != nil {
			return i, err
		}
	}
	if taskJSON != "" {
		i.TaskRequirementCandidate = &k12.FactCandidate{}
		if err := json.Unmarshal([]byte(taskJSON), i.TaskRequirementCandidate); err != nil {
			return i, err
		}
	}
	if ocrJSON != "" {
		i.OCREvidence = &k12.CreativeWorkIntakeOCREvidence{}
		if err := json.Unmarshal([]byte(ocrJSON), i.OCREvidence); err != nil {
			return i, err
		}
	}
	if routeJSON != "" {
		if err := json.Unmarshal([]byte(routeJSON), &i.RoutePolicySnapshot); err != nil {
			return i, err
		}
	}
	if err := json.Unmarshal([]byte(invocationsJSON), &i.OperationInvocations); err != nil {
		return i, err
	}
	if commitReceiptJSON != "" {
		i.CommitReceipt = &k12.CreativeWorkCommitReceipt{}
		if err := json.Unmarshal([]byte(commitReceiptJSON), i.CommitReceipt); err != nil {
			return i, err
		}
	}
	i.RetrySafe = retrySafe != 0
	return i, nil
}

func (s *Store) GetCreativeWorkIntake(
	ctx context.Context,
	agentName, intakeID string,
) (k12.CreativeWorkIntake, error) {
	return getCreativeWorkIntake(ctx, s.db, agentName, intakeID)
}

// PromoteCreativeWorkIntake creates the formal work and v1 and advances the
// intake in one short transaction. A replay after commit returns the same work.
func (s *Store) PromoteCreativeWorkIntake(
	ctx context.Context,
	agentName, intakeID string,
	expectedVersion int,
) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	intake, err := getCreativeWorkIntake(ctx, tx, agentName, intakeID)
	if err != nil {
		return "", false, err
	}
	if intake.Status == k12.CreativeWorkIntakePromoted {
		if err := tx.Commit(); err != nil {
			return "", false, err
		}
		return intake.PromotedWorkID, false, nil
	}
	if intake.Status != k12.CreativeWorkIntakeReady {
		return "", false, fmt.Errorf("%w: intake status=%s", ErrImageTaskInvalidState, intake.Status)
	}
	if intake.PromotionPolicy == k12.CreativeWorkPromotionExplicitCommit {
		return "", false, fmt.Errorf("%w: explicit_commit intake requires commit command", ErrImageTaskInvalidState)
	}
	if intake.Version != expectedVersion {
		return "", false, ErrImageTaskVersionConflict
	}
	if err := intake.Validate(); err != nil {
		return "", false, err
	}
	for _, ref := range intake.SourceAssetRefs {
		owner, _, parseErr := assetstore.Parse(ref)
		if parseErr != nil || owner != agentName {
			return "", false, fmt.Errorf("%w: source asset owner mismatch", ErrImageTaskConflict)
		}
	}
	var title, task string
	provenance := k12.TitleTaskProvenance{}
	if intake.WorkTitleCandidate != nil && intake.WorkTitleCandidate.ParentAuthored() {
		title = strings.TrimSpace(intake.WorkTitleCandidate.Value)
		candidate := *intake.WorkTitleCandidate
		provenance.WorkTitle = &candidate
	}
	if intake.TaskRequirementCandidate != nil &&
		intake.TaskRequirementCandidate.ParentAuthored() {
		task = strings.TrimSpace(intake.TaskRequirementCandidate.Value)
		candidate := *intake.TaskRequirementCandidate
		provenance.TaskRequirement = &candidate
	}
	version := k12.CreativeWorkVersion{
		VersionID:     "v1",
		SourceAssetID: intake.SourceAssetRefs[0],
	}
	if intake.WorkType == k12.WorkTypeWriting && intake.OCREvidence != nil {
		version.ContentMarkdown = intake.OCREvidence.CanonicalContent
		version.OCRRaw = intake.OCREvidence.Raw
		version.OCRVersion = intake.OCREvidence.CanonicalVersion
		version.OCRConfirmedDigest = intake.OCREvidence.CanonicalDigest
		version.ContentConfirmedAt = intake.OCREvidence.FrozenAt
		if version.ContentConfirmedAt == 0 {
			version.ContentConfirmedAt = nowUnix()
		}
	}
	fields := k12.NormalizeCreativeWorkFields(k12.CreativeWorkFields{
		WorkType: intake.WorkType, WorkTitle: title, TaskRequirement: task,
		TitleTaskProvenance: provenance, SourceIntakeID: intake.IntakeID,
		Versions: []k12.CreativeWorkVersion{version},
	})
	sourceSession := ""
	dispatch, err := getImageTaskDispatch(ctx, tx, agentName, intake.DispatchID, "")
	if err != nil {
		return "", false, err
	}
	sourceSession = dispatch.SourceSessionID
	rec, err := k12.NewCreativeWorkRecord(agentName, sourceSession, fields)
	if err != nil {
		return "", false, err
	}
	schema, err := s.registry.Get(k12.CollectionCreativeWork)
	if err != nil {
		return "", false, err
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(rec.Fields); err != nil {
			return "", false, fmt.Errorf("%w: %v", records.ErrInvalidFields, err)
		}
	}
	rec.RecordID = idgen.NanoID()
	rec.SchemaVersion = schema.Version
	rec.DedupeKey = schema.DedupeKey(rec)
	rec.Status = schema.InitialStatus
	rec.Tags = "[]"
	now := nowUnix()
	rec.Version, rec.CreatedAt, rec.UpdatedAt = 0, now, now
	mapper := creativeWorkMapper{}
	domainVals, err := mapper.encode(rec.Fields)
	if err != nil {
		return "", false, err
	}
	cols := mapper.domainCols()
	query := fmt.Sprintf(`INSERT INTO %s (%s, %s) VALUES (%s)`,
		mapper.table(), baseCols, strings.Join(cols, ", "), placeholders(11+len(cols)))
	args := append([]any{rec.RecordID, rec.AgentName, rec.SchemaVersion, rec.Status,
		rec.DedupeKey, rec.Tags, rec.DueAt, rec.SourceSession, rec.Version,
		rec.CreatedAt, rec.UpdatedAt}, domainVals...)
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return "", false, fmt.Errorf("create promoted CreativeWork: %w", err)
	}
	if err := mapper.syncChildren(ctx, tx, rec.RecordID, rec.Fields); err != nil {
		return "", false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE k12_creative_work_intakes
        SET status='promoted',promoted_work_id=?,promoted_version_id='v1',
            retry_safe=0,failure_kind='',
            version=version+1,updated_at=?
        WHERE agent_name=? AND intake_id=? AND status='ready' AND version=?`,
		rec.RecordID, now, agentName, intakeID, expectedVersion)
	if err != nil {
		return "", false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", false, ErrImageTaskVersionConflict
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return rec.RecordID, true, nil
}

func creativeVersionFromIntake(
	intake k12.CreativeWorkIntake,
	versionID, contentMarkdown string,
) k12.CreativeWorkVersion {
	version := k12.CreativeWorkVersion{
		VersionID: versionID, SourceAssetID: intake.SourceAssetRefs[0],
		ContentMarkdown: strings.TrimSpace(contentMarkdown),
	}
	if intake.WorkType == k12.WorkTypeWriting && intake.OCREvidence != nil {
		version.ContentMarkdown = intake.OCREvidence.CanonicalContent
		version.OCRRaw = intake.OCREvidence.Raw
		version.OCRVersion = intake.OCREvidence.CanonicalVersion
		version.OCRConfirmedDigest = intake.OCREvidence.CanonicalDigest
		version.ContentConfirmedAt = intake.OCREvidence.FrozenAt
		if version.ContentConfirmedAt == 0 {
			version.ContentConfirmedAt = nowUnix()
		}
	}
	return version
}

func applyManualCommitFacts(
	intakeID string,
	command k12.CreativeWorkCommitCommand,
	fields *k12.CreativeWorkFields,
) {
	evidencePrefix := "intake:" + intakeID + "#commit:" + command.CommandDigest
	if title := strings.TrimSpace(command.WorkTitle); title != "" {
		candidate := k12.FactCandidate{
			Value: title, Source: k12.FactCandidateSourceUser, Confidence: 1,
			EvidenceRef: evidencePrefix + ":work_title",
		}
		fields.WorkTitle = title
		fields.TitleTaskProvenance.WorkTitle = &candidate
	}
	if task := strings.TrimSpace(command.TaskRequirement); task != "" {
		candidate := k12.FactCandidate{
			Value: task, Source: k12.FactCandidateSourceUser, Confidence: 1,
			EvidenceRef: evidencePrefix + ":task_requirement",
		}
		fields.TaskRequirement = task
		fields.TitleTaskProvenance.TaskRequirement = &candidate
	}
	if intent := strings.TrimSpace(command.Intent); intent != "" {
		fields.Intent = intent
	}
}

// CommitManualCreativeWorkIntake is the only explicit_commit promotion path.
// New work creation, revision append, intake link and receipt are committed in
// one transaction. A same-digest replay returns the prior receipt.
func (s *Store) CommitManualCreativeWorkIntake(
	ctx context.Context,
	agentName, intakeID string,
	expectedVersion int,
	command k12.CreativeWorkCommitCommand,
) (k12.CreativeWorkIntake, error) {
	command.CommandDigest = strings.TrimSpace(command.CommandDigest)
	if command.CommandDigest == "" {
		return k12.CreativeWorkIntake{},
			fmt.Errorf("%w: commit command digest required", ErrImageTaskInvalidState)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.CreativeWorkIntake{}, err
	}
	defer tx.Rollback()
	intake, err := getCreativeWorkIntake(ctx, tx, agentName, intakeID)
	if err != nil {
		return intake, err
	}
	if intake.Status == k12.CreativeWorkIntakePromoted {
		if intake.CommitReceipt == nil ||
			intake.CommitReceipt.CommandDigest != command.CommandDigest {
			return intake, fmt.Errorf("%w: commit already bound to another command", ErrImageTaskConflict)
		}
		if err := tx.Commit(); err != nil {
			return intake, err
		}
		return intake, nil
	}
	if intake.WorkType == k12.WorkTypeWriting &&
		strings.TrimSpace(command.ContentMarkdown) != "" &&
		(intake.OCREvidence == nil ||
			strings.TrimSpace(command.ContentMarkdown) != intake.OCREvidence.CanonicalContent) {
		return intake, fmt.Errorf("%w: writing content changed after freeze_ocr", ErrImageTaskConflict)
	}
	if intake.PromotionPolicy != k12.CreativeWorkPromotionExplicitCommit ||
		intake.Status != k12.CreativeWorkIntakeReady {
		return intake, fmt.Errorf("%w: intake is not commit-ready", ErrImageTaskInvalidState)
	}
	if intake.Version != expectedVersion {
		return intake, ErrImageTaskVersionConflict
	}
	if err := intake.Validate(); err != nil {
		return intake, err
	}
	for _, ref := range intake.SourceAssetRefs {
		owner, _, parseErr := assetstore.Parse(ref)
		if parseErr != nil || owner != agentName {
			return intake, fmt.Errorf("%w: source asset owner mismatch", ErrImageTaskConflict)
		}
	}
	now := nowUnix()
	workID, promotedVersionID := "", ""
	mapper := creativeWorkMapper{}

	switch intake.EntryKind {
	case k12.CreativeWorkEntryNewWork:
		fields := k12.CreativeWorkFields{
			WorkType: intake.WorkType, SourceIntakeID: intake.IntakeID,
		}
		applyManualCommitFacts(intake.IntakeID, command, &fields)
		promotedVersionID = "v1"
		fields.Versions = []k12.CreativeWorkVersion{
			creativeVersionFromIntake(intake, promotedVersionID, command.ContentMarkdown),
		}
		fields = k12.NormalizeCreativeWorkFields(fields)
		dispatch, getErr := getImageTaskDispatch(ctx, tx, agentName, intake.DispatchID, "")
		if getErr != nil {
			return intake, getErr
		}
		rec, newErr := k12.NewCreativeWorkRecord(agentName, dispatch.SourceSessionID, fields)
		if newErr != nil {
			return intake, newErr
		}
		schema, schemaErr := s.registry.Get(k12.CollectionCreativeWork)
		if schemaErr != nil {
			return intake, schemaErr
		}
		rec.RecordID = idgen.NanoID()
		rec.SchemaVersion = schema.Version
		rec.DedupeKey = schema.DedupeKey(rec)
		rec.Status = schema.InitialStatus
		rec.Tags = "[]"
		rec.Version, rec.CreatedAt, rec.UpdatedAt = 0, now, now
		domainVals, encodeErr := mapper.encode(rec.Fields)
		if encodeErr != nil {
			return intake, encodeErr
		}
		cols := mapper.domainCols()
		query := fmt.Sprintf(`INSERT INTO %s (%s, %s) VALUES (%s)`,
			mapper.table(), baseCols, strings.Join(cols, ", "), placeholders(11+len(cols)))
		args := append([]any{rec.RecordID, rec.AgentName, rec.SchemaVersion, rec.Status,
			rec.DedupeKey, rec.Tags, rec.DueAt, rec.SourceSession, rec.Version,
			rec.CreatedAt, rec.UpdatedAt}, domainVals...)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return intake, fmt.Errorf("create manual CreativeWork: %w", err)
		}
		if err := mapper.syncChildren(ctx, tx, rec.RecordID, rec.Fields); err != nil {
			return intake, err
		}
		workID = rec.RecordID
	case k12.CreativeWorkEntryRevision:
		rec, getErr := s.getVia(ctx, tx, intake.TargetWorkID)
		if getErr != nil || rec == nil || rec.AgentName != agentName ||
			rec.Collection != k12.CollectionCreativeWork {
			return intake, fmt.Errorf("%w: revision target not found for owner", ErrImageTaskConflict)
		}
		if rec.Status == k12.WorkStatusArchived {
			return intake, fmt.Errorf("%w: archived work cannot be revised", ErrImageTaskInvalidState)
		}
		fields, parseErr := k12.ParseCreativeWorkFields(rec.Fields)
		if parseErr != nil {
			return intake, parseErr
		}
		if fields.WorkType != intake.WorkType || len(fields.Versions) == 0 ||
			fields.Versions[len(fields.Versions)-1].VersionID != intake.BaseVersionID {
			return intake, fmt.Errorf("%w: revision type/base version drift", ErrImageTaskVersionConflict)
		}
		applyManualCommitFacts(intake.IntakeID, command, &fields)
		promotedVersionID = fmt.Sprintf("v%d", len(fields.Versions)+1)
		fields.Versions = append(
			fields.Versions,
			creativeVersionFromIntake(intake, promotedVersionID, command.ContentMarkdown),
		)
		fields = k12.NormalizeCreativeWorkFields(fields)
		fieldsJSON, marshalErr := jsonString(fields)
		if marshalErr != nil {
			return intake, marshalErr
		}
		domainVals, encodeErr := mapper.encode(fieldsJSON)
		if encodeErr != nil {
			return intake, encodeErr
		}
		set := []string{"status=?", "version=version+1", "updated_at=?"}
		args := []any{k12.WorkStatusRevised, now}
		for _, col := range mapper.domainCols() {
			set = append(set, col+"=?")
		}
		args = append(args, domainVals...)
		args = append(args, rec.RecordID, agentName, rec.Version)
		res, updateErr := tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE %s SET %s WHERE record_id=? AND agent_name=? AND version=?`,
			mapper.table(), strings.Join(set, ","),
		), args...)
		if updateErr != nil {
			return intake, updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return intake, ErrImageTaskVersionConflict
		}
		if err := mapper.syncChildren(ctx, tx, rec.RecordID, fieldsJSON); err != nil {
			return intake, err
		}
		workID = rec.RecordID
	default:
		return intake, fmt.Errorf("%w: unsupported entry_kind", ErrImageTaskInvalidState)
	}

	receipt := k12.CreativeWorkCommitReceipt{
		CommandDigest: command.CommandDigest, CommittedAt: now,
		WorkID: workID, VersionID: promotedVersionID,
	}
	receiptJSON, err := jsonString(receipt)
	if err != nil {
		return intake, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE k12_creative_work_intakes
        SET status='promoted',promoted_work_id=?,promoted_version_id=?,
            commit_receipt_json=?,retry_safe=0,failure_kind='',
            version=version+1,updated_at=?
        WHERE agent_name=? AND intake_id=? AND status='ready' AND version=?`,
		workID, promotedVersionID, receiptJSON, now,
		agentName, intakeID, expectedVersion)
	if err != nil {
		return intake, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return intake, ErrImageTaskVersionConflict
	}
	if err := tx.Commit(); err != nil {
		return intake, err
	}
	return getCreativeWorkIntake(ctx, s.db, agentName, intakeID)
}

// PrepareImageTaskInvocation adds a per-operation immutable call ledger. It is
// used after routing for writing OCR and after promotion for work feedback.
func (s *Store) PrepareImageTaskInvocation(
	ctx context.Context,
	invocation k12.ImageTaskInvocation,
) (k12.ImageTaskInvocation, bool, error) {
	if err := validateImageTaskInvocation(invocation); err != nil {
		return invocation, false, err
	}
	if err := ensureAgentRegistered(ctx, s.db, invocation.AgentName); err != nil {
		return invocation, false, err
	}
	routeJSON, _ := jsonString(invocation.RouteSnapshot)
	if invocation.CreatedAt == 0 {
		invocation.CreatedAt = nowUnix()
	}
	if invocation.UpdatedAt == 0 {
		invocation.UpdatedAt = invocation.CreatedAt
	}
	var dispatchID, intakeID, workID any
	if invocation.DispatchID != "" {
		dispatchID = invocation.DispatchID
	}
	if invocation.IntakeID != "" {
		intakeID = invocation.IntakeID
	}
	if invocation.WorkRecordID != "" {
		workID = invocation.WorkRecordID
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO k12_image_task_invocations
        (invocation_id,agent_name,dispatch_id,intake_id,work_record_id,operation,operation_key,
         request_digest,route_snapshot_json,status,attempt,provider_request_key,result_digest,
         result_json,error_kind,retry_safe,started_at,finished_at,created_at,updated_at)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(agent_name,operation_key,attempt) DO NOTHING`,
		invocation.InvocationID, invocation.AgentName, dispatchID, intakeID, workID,
		invocation.Operation, invocation.OperationKey, invocation.RequestDigest, routeJSON,
		k12.ImageTaskInvocationPrepared, invocation.Attempt, invocation.ProviderRequestKey,
		invocation.ResultDigest, invocation.ResultJSON, invocation.ErrorKind,
		boolInt(invocation.RetrySafe), invocation.StartedAt, invocation.FinishedAt,
		invocation.CreatedAt, invocation.UpdatedAt)
	if err != nil {
		return invocation, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		invocation.Status = k12.ImageTaskInvocationPrepared
		return invocation, true, nil
	}
	var existingID string
	if err := s.db.QueryRowContext(ctx, `SELECT invocation_id FROM k12_image_task_invocations
        WHERE agent_name=? AND operation_key=? AND attempt=?`, invocation.AgentName,
		invocation.OperationKey, invocation.Attempt).Scan(&existingID); err != nil {
		return invocation, false, err
	}
	existing, err := getImageTaskInvocation(ctx, s.db, invocation.AgentName, existingID)
	if err != nil {
		return invocation, false, err
	}
	if existing.RequestDigest != invocation.RequestDigest ||
		existing.RouteSnapshot != invocation.RouteSnapshot ||
		existing.Operation != invocation.Operation {
		return invocation, false, ErrImageTaskConflict
	}
	return existing, false, nil
}

func (s *Store) MarkImageTaskInvocationSent(
	ctx context.Context,
	agentName, invocationID, providerKey string,
) (k12.ImageTaskInvocation, error) {
	now := nowUnix()
	res, err := s.db.ExecContext(ctx, `UPDATE k12_image_task_invocations
        SET status='sent',provider_request_key=?,started_at=?,updated_at=?
        WHERE agent_name=? AND invocation_id=? AND status='prepared'`,
		providerKey, now, now, agentName, invocationID)
	if err != nil {
		return k12.ImageTaskInvocation{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		inv, getErr := getImageTaskInvocation(ctx, s.db, agentName, invocationID)
		if getErr != nil || inv.Status != k12.ImageTaskInvocationSent {
			return inv, ErrImageTaskInvalidState
		}
		return inv, nil
	}
	return getImageTaskInvocation(ctx, s.db, agentName, invocationID)
}

func (s *Store) GetLatestWorkFeedbackInvocation(
	ctx context.Context,
	agentName, workRecordID, operationKey string,
) (k12.ImageTaskInvocation, error) {
	var invocationID string
	err := s.db.QueryRowContext(ctx, `SELECT invocation_id
        FROM k12_image_task_invocations
        WHERE agent_name=? AND work_record_id=? AND operation='work_feedback'
          AND operation_key=?
        ORDER BY attempt DESC,created_at DESC LIMIT 1`,
		agentName, workRecordID, operationKey).Scan(&invocationID)
	if err == sql.ErrNoRows {
		return k12.ImageTaskInvocation{}, ErrImageTaskNotFound
	}
	if err != nil {
		return k12.ImageTaskInvocation{}, err
	}
	return getImageTaskInvocation(ctx, s.db, agentName, invocationID)
}

func (s *Store) CompleteWorkFeedbackInvocation(
	ctx context.Context,
	agentName, invocationID, resultDigest, resultJSON string,
) error {
	now := nowUnix()
	res, err := s.db.ExecContext(ctx, `UPDATE k12_image_task_invocations
        SET status='succeeded',result_digest=?,result_json=?,error_kind='',
            retry_safe=0,finished_at=?,updated_at=?
        WHERE agent_name=? AND invocation_id=? AND operation='work_feedback'
          AND status IN ('prepared','sent')`,
		resultDigest, resultJSON, now, now, agentName, invocationID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrImageTaskInvalidState
	}
	return nil
}

func (s *Store) FailWorkFeedbackInvocation(
	ctx context.Context,
	agentName, invocationID, failureKind string,
	outcomeUnknown, retrySafe bool,
) error {
	failureKind = strings.TrimSpace(failureKind)
	if failureKind == "" {
		return ErrImageTaskInvalidState
	}
	if outcomeUnknown {
		retrySafe = false
	}
	status := k12.ImageTaskInvocationFailed
	if outcomeUnknown {
		status = k12.ImageTaskInvocationOutcomeUnknown
	}
	now := nowUnix()
	res, err := s.db.ExecContext(ctx, `UPDATE k12_image_task_invocations
        SET status=?,error_kind=?,retry_safe=?,finished_at=?,updated_at=?
        WHERE agent_name=? AND invocation_id=? AND operation='work_feedback'
          AND status IN ('prepared','sent')`,
		status, failureKind, boolInt(retrySafe), now, now, agentName, invocationID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrImageTaskInvalidState
	}
	return nil
}

// FailImageTaskInvocation atomically parks the invocation and its owning
// aggregate. outcomeUnknown is never retry-safe because a blind resend could
// duplicate an already accepted provider operation.
func (s *Store) FailImageTaskInvocation(
	ctx context.Context,
	agentName, invocationID, failureKind string,
	outcomeUnknown, retrySafe bool,
) error {
	failureKind = strings.TrimSpace(failureKind)
	if failureKind == "" {
		return fmt.Errorf("%w: failure_kind required", ErrImageTaskInvalidState)
	}
	if outcomeUnknown {
		retrySafe = false
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	invocation, err := getImageTaskInvocation(ctx, tx, agentName, invocationID)
	if err != nil {
		return err
	}
	if invocation.Status != k12.ImageTaskInvocationSent &&
		invocation.Status != k12.ImageTaskInvocationPrepared {
		return ErrImageTaskInvalidState
	}
	status := k12.ImageTaskInvocationFailed
	if outcomeUnknown {
		status = k12.ImageTaskInvocationOutcomeUnknown
	}
	now := nowUnix()
	res, err := tx.ExecContext(ctx, `UPDATE k12_image_task_invocations
        SET status=?,error_kind=?,retry_safe=?,finished_at=?,updated_at=?
        WHERE agent_name=? AND invocation_id=? AND status IN ('prepared','sent')`,
		status, failureKind, boolInt(retrySafe), now, now, agentName, invocationID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrImageTaskInvalidState
	}
	switch invocation.Operation {
	case k12.ImageTaskOperationClassification:
		res, err = tx.ExecContext(ctx, `UPDATE k12_image_task_dispatches
            SET status='failed',failure_kind=?,retry_safe=?,version=version+1,updated_at=?
            WHERE agent_name=? AND dispatch_id=? AND status='routing'`,
			failureKind, boolInt(retrySafe), now, agentName, invocation.DispatchID)
	case k12.ImageTaskOperationWritingOCR:
		res, err = tx.ExecContext(ctx, `UPDATE k12_creative_work_intakes
            SET status='failed',failure_kind=?,retry_safe=?,version=version+1,updated_at=?
            WHERE agent_name=? AND intake_id=? AND status='preparing'`,
			failureKind, boolInt(retrySafe), now, agentName, invocation.IntakeID)
	default:
		return ErrImageTaskInvalidState
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrImageTaskInvalidState
	}
	return tx.Commit()
}

// PrepareImageTaskRetry creates a new immutable invocation attempt from the
// previously frozen route. It never resolves provider/model defaults again.
func (s *Store) PrepareImageTaskRetry(
	ctx context.Context,
	agentName, dispatchID string,
	expectedVersion int,
	newInvocationID string,
) (k12.ImageTaskDispatch, k12.ImageTaskInvocation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.ImageTaskDispatch{}, k12.ImageTaskInvocation{}, err
	}
	defer tx.Rollback()
	dispatch, err := getImageTaskDispatch(ctx, tx, agentName, dispatchID, "")
	if err != nil {
		return dispatch, k12.ImageTaskInvocation{}, err
	}
	if dispatch.Version != expectedVersion {
		return dispatch, k12.ImageTaskInvocation{}, ErrImageTaskVersionConflict
	}
	var prior k12.ImageTaskInvocation
	var intake *k12.CreativeWorkIntake
	switch {
	case dispatch.Status == k12.ImageTaskStatusFailed:
		if !dispatch.RetrySafe {
			return dispatch, prior, ErrImageTaskInvalidState
		}
		prior, err = getLatestImageTaskInvocation(
			ctx, tx, agentName, k12.ImageTaskOperationClassification, dispatchID, "",
		)
	case dispatch.Status == k12.ImageTaskStatusRouted &&
		dispatch.TargetObjectType == k12.ImageTaskTargetCreativeWorkIntake:
		value, getErr := getCreativeWorkIntake(ctx, tx, agentName, dispatch.TargetObjectID)
		if getErr != nil {
			return dispatch, prior, getErr
		}
		intake = &value
		if intake.Status != k12.CreativeWorkIntakeFailed || !intake.RetrySafe {
			return dispatch, prior, ErrImageTaskInvalidState
		}
		prior, err = getLatestImageTaskInvocation(
			ctx, tx, agentName, k12.ImageTaskOperationWritingOCR, "", intake.IntakeID,
		)
	default:
		return dispatch, prior, ErrImageTaskInvalidState
	}
	if err != nil {
		return dispatch, prior, err
	}
	if prior.Status != k12.ImageTaskInvocationFailed || !prior.RetrySafe {
		return dispatch, prior, ErrImageTaskInvalidState
	}
	now := nowUnix()
	next := k12.ImageTaskInvocation{
		InvocationID: strings.TrimSpace(newInvocationID), AgentName: agentName,
		DispatchID: prior.DispatchID, IntakeID: prior.IntakeID, WorkRecordID: prior.WorkRecordID,
		Operation: prior.Operation, OperationKey: prior.OperationKey,
		RequestDigest: prior.RequestDigest, RouteSnapshot: prior.RouteSnapshot,
		Status: k12.ImageTaskInvocationPrepared, Attempt: prior.Attempt + 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := validateImageTaskInvocation(next); err != nil {
		return dispatch, next, err
	}
	routeJSON, _ := jsonString(next.RouteSnapshot)
	var dispatchRef, intakeRef, workRef any
	if next.DispatchID != "" {
		dispatchRef = next.DispatchID
	}
	if next.IntakeID != "" {
		intakeRef = next.IntakeID
	}
	if next.WorkRecordID != "" {
		workRef = next.WorkRecordID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO k12_image_task_invocations
        (invocation_id,agent_name,dispatch_id,intake_id,work_record_id,operation,operation_key,
         request_digest,route_snapshot_json,status,attempt,provider_request_key,result_digest,
         result_json,error_kind,retry_safe,started_at,finished_at,created_at,updated_at)
        VALUES(?,?,?,?,?,?,?,?,?,'prepared',?,'','','','',0,0,0,?,?)`,
		next.InvocationID, next.AgentName, dispatchRef, intakeRef, workRef,
		next.Operation, next.OperationKey, next.RequestDigest, routeJSON, next.Attempt,
		next.CreatedAt, next.UpdatedAt)
	if err != nil {
		return dispatch, next, err
	}
	if intake == nil {
		_, err = tx.ExecContext(ctx, `UPDATE k12_image_task_dispatches
            SET status='routing',failure_kind='',retry_safe=0,version=version+1,updated_at=?
            WHERE agent_name=? AND dispatch_id=? AND version=? AND status='failed'`,
			now, agentName, dispatchID, expectedVersion)
		dispatch.Status = k12.ImageTaskStatusRouting
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE k12_creative_work_intakes
            SET status='preparing',failure_kind='',retry_safe=0,version=version+1,updated_at=?
            WHERE agent_name=? AND intake_id=? AND status='failed'`,
			now, agentName, intake.IntakeID)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE k12_image_task_dispatches
                SET version=version+1,updated_at=?
                WHERE agent_name=? AND dispatch_id=? AND version=?`,
				now, agentName, dispatchID, expectedVersion)
		}
	}
	if err != nil {
		return dispatch, next, err
	}
	if err := tx.Commit(); err != nil {
		return dispatch, next, err
	}
	dispatch, err = s.GetImageTaskDispatch(ctx, agentName, dispatchID)
	return dispatch, next, err
}

// FreezeCreativeWorkIntakeOCR commits the successful OCR invocation and the
// canonical evidence pointer together. It never overwrites immutable raw data.
func (s *Store) FreezeCreativeWorkIntakeOCR(
	ctx context.Context,
	agentName, intakeID string,
	expectedVersion int,
	invocationID string,
	evidence k12.CreativeWorkIntakeOCREvidence,
	provenance k12.CreativeWorkConfirmationProvenance,
) (k12.CreativeWorkIntake, error) {
	sum := sha256.Sum256([]byte(evidence.CanonicalContent))
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])
	if strings.TrimSpace(evidence.CanonicalContent) == "" || evidence.CanonicalVersion < 1 ||
		evidence.CanonicalDigest != wantDigest {
		return k12.CreativeWorkIntake{}, fmt.Errorf("%w: invalid canonical OCR evidence", ErrImageTaskInvalidState)
	}
	evidence.ConfirmationProvenance = provenance
	if evidence.FrozenAt == 0 {
		evidence.FrozenAt = nowUnix()
	}
	evidenceJSON, _ := jsonString(evidence)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.CreativeWorkIntake{}, err
	}
	defer tx.Rollback()
	intake, err := getCreativeWorkIntake(ctx, tx, agentName, intakeID)
	if err != nil {
		return intake, err
	}
	if intake.WorkType != k12.WorkTypeWriting ||
		(intake.Status != k12.CreativeWorkIntakePreparing &&
			intake.Status != k12.CreativeWorkIntakeAwaitingConfirmation) {
		return intake, ErrImageTaskInvalidState
	}
	if intake.Version != expectedVersion {
		return intake, ErrImageTaskVersionConflict
	}
	inv, err := getImageTaskInvocation(ctx, tx, agentName, invocationID)
	if err != nil || inv.IntakeID != intakeID || inv.Operation != k12.ImageTaskOperationWritingOCR {
		return intake, ErrImageTaskConflict
	}
	resultJSON, _ := jsonString(map[string]any{
		"canonical_digest":  evidence.CanonicalDigest,
		"canonical_version": evidence.CanonicalVersion,
	})
	now := nowUnix()
	res, err := tx.ExecContext(ctx, `UPDATE k12_image_task_invocations
        SET status='succeeded',result_digest=?,result_json=?,retry_safe=0,
            finished_at=?,updated_at=?
        WHERE agent_name=? AND invocation_id=? AND status IN ('prepared','sent')`,
		evidence.CanonicalDigest, resultJSON, now, now, agentName, invocationID)
	if err != nil {
		return intake, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return intake, ErrImageTaskInvalidState
	}
	invocations := append([]string(nil), intake.OperationInvocations...)
	found := false
	for _, id := range invocations {
		found = found || id == invocationID
	}
	if !found {
		invocations = append(invocations, invocationID)
	}
	invocationsJSON, _ := jsonString(invocations)
	res, err = tx.ExecContext(ctx, `UPDATE k12_creative_work_intakes
        SET ocr_evidence_json=?,operation_invocations_json=?,status='ready',
            confirmation_provenance=?,retry_safe=0,failure_kind='',
            version=version+1,updated_at=?
        WHERE agent_name=? AND intake_id=? AND version=?`,
		evidenceJSON, invocationsJSON, provenance, now, agentName, intakeID, expectedVersion)
	if err != nil {
		return intake, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return intake, ErrImageTaskVersionConflict
	}
	if err := tx.Commit(); err != nil {
		return intake, err
	}
	return getCreativeWorkIntake(ctx, s.db, agentName, intakeID)
}

// HoldCreativeWorkIntakeOCRConfirmation persists a successful but uncertain
// OCR receipt and parks only the minimum risky segments for parent review.
// The model result is immutable; a later confirmation creates a new canonical
// version instead of rewriting this evidence.
func (s *Store) HoldCreativeWorkIntakeOCRConfirmation(
	ctx context.Context,
	agentName, intakeID string,
	expectedVersion int,
	invocationID string,
	evidence k12.CreativeWorkIntakeOCREvidence,
) (k12.CreativeWorkIntake, error) {
	if strings.TrimSpace(evidence.Raw) == "" || strings.TrimSpace(evidence.CanonicalContent) == "" ||
		evidence.CanonicalVersion < 1 ||
		evidence.Confidence < 0 || evidence.Confidence > 1 {
		return k12.CreativeWorkIntake{}, fmt.Errorf("%w: invalid risky OCR evidence", ErrImageTaskInvalidState)
	}
	sum := sha256.Sum256([]byte(evidence.CanonicalContent))
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])
	if evidence.CanonicalDigest != wantDigest {
		return k12.CreativeWorkIntake{}, fmt.Errorf("%w: invalid risky OCR digest", ErrImageTaskInvalidState)
	}
	evidence.ConfirmationProvenance = ""
	evidence.FrozenAt = 0
	evidenceJSON, _ := jsonString(evidence)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.CreativeWorkIntake{}, err
	}
	defer tx.Rollback()
	intake, err := getCreativeWorkIntake(ctx, tx, agentName, intakeID)
	if err != nil {
		return intake, err
	}
	if intake.WorkType != k12.WorkTypeWriting || intake.Status != k12.CreativeWorkIntakePreparing ||
		intake.Version != expectedVersion {
		return intake, ErrImageTaskInvalidState
	}
	if len(evidence.RiskSegments) == 0 &&
		intake.PromotionPolicy != k12.CreativeWorkPromotionExplicitCommit {
		return intake, fmt.Errorf("%w: automatic OCR confirmation requires risk evidence",
			ErrImageTaskInvalidState)
	}
	invocation, err := getImageTaskInvocation(ctx, tx, agentName, invocationID)
	if err != nil || invocation.IntakeID != intakeID ||
		invocation.Operation != k12.ImageTaskOperationWritingOCR {
		return intake, ErrImageTaskConflict
	}
	resultJSON, _ := jsonString(map[string]any{
		"canonical_digest": evidence.CanonicalDigest,
		"risk_segments":    evidence.RiskSegments,
		"confidence":       evidence.Confidence,
	})
	now := nowUnix()
	res, err := tx.ExecContext(ctx, `UPDATE k12_image_task_invocations
        SET status='succeeded',result_digest=?,result_json=?,retry_safe=0,
            finished_at=?,updated_at=?
        WHERE agent_name=? AND invocation_id=? AND status IN ('prepared','sent')`,
		evidence.CanonicalDigest, resultJSON, now, now, agentName, invocationID)
	if err != nil {
		return intake, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return intake, ErrImageTaskInvalidState
	}
	invocations := append([]string(nil), intake.OperationInvocations...)
	invocations = append(invocations, invocationID)
	invocationsJSON, _ := jsonString(invocations)
	res, err = tx.ExecContext(ctx, `UPDATE k12_creative_work_intakes
        SET ocr_evidence_json=?,operation_invocations_json=?,
            status='awaiting_confirmation',confirmation_provenance='',
            retry_safe=0,failure_kind='',version=version+1,updated_at=?
        WHERE agent_name=? AND intake_id=? AND status='preparing' AND version=?`,
		evidenceJSON, invocationsJSON, now, agentName, intakeID, expectedVersion)
	if err != nil {
		return intake, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return intake, ErrImageTaskVersionConflict
	}
	if err := tx.Commit(); err != nil {
		return intake, err
	}
	return getCreativeWorkIntake(ctx, s.db, agentName, intakeID)
}

func (s *Store) ConfirmCreativeWorkIntakeOCR(
	ctx context.Context,
	agentName, intakeID string,
	expectedVersion, canonicalVersion int,
	canonicalContent string,
	segmentCorrections []k12.CreativeWorkIntakeOCRCorrection,
) (k12.CreativeWorkIntake, error) {
	canonicalContent = strings.TrimSpace(canonicalContent)
	if canonicalContent == "" || canonicalVersion < 1 {
		return k12.CreativeWorkIntake{}, ErrImageTaskInvalidState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.CreativeWorkIntake{}, err
	}
	defer tx.Rollback()
	intake, err := getCreativeWorkIntake(ctx, tx, agentName, intakeID)
	if err != nil {
		return intake, err
	}
	if intake.Status != k12.CreativeWorkIntakeAwaitingConfirmation ||
		intake.WorkType != k12.WorkTypeWriting || intake.OCREvidence == nil ||
		intake.Version != expectedVersion ||
		intake.OCREvidence.CanonicalVersion != canonicalVersion {
		return intake, ErrImageTaskVersionConflict
	}
	correctionsBySegment := make(map[string]k12.CreativeWorkIntakeOCRCorrection, len(segmentCorrections))
	for _, correction := range segmentCorrections {
		correction.SegmentID = strings.TrimSpace(correction.SegmentID)
		correction.CanonicalText = strings.TrimSpace(correction.CanonicalText)
		if correction.SegmentID == "" || correction.CanonicalText == "" {
			return intake, fmt.Errorf("%w: empty OCR segment correction", ErrImageTaskInvalidState)
		}
		if _, duplicated := correctionsBySegment[correction.SegmentID]; duplicated {
			return intake, fmt.Errorf(
				"%w: duplicate OCR segment correction %q",
				ErrImageTaskInvalidState,
				correction.SegmentID,
			)
		}
		correctionsBySegment[correction.SegmentID] = correction
	}
	if len(intake.OCREvidence.RiskSegments) == 0 && len(correctionsBySegment) != 0 {
		return intake, fmt.Errorf(
			"%w: OCR correction has no matching risk segment",
			ErrImageTaskInvalidState,
		)
	}
	expectedCanonical := strings.TrimSpace(intake.OCREvidence.CanonicalContent)
	orderedCorrections := make(
		[]k12.CreativeWorkIntakeOCRCorrection,
		0,
		len(intake.OCREvidence.RiskSegments),
	)
	correctedSegment := false
	for _, risk := range intake.OCREvidence.RiskSegments {
		segmentID := strings.TrimSpace(risk.SegmentID)
		rawText := strings.TrimSpace(risk.RawText)
		correction, ok := correctionsBySegment[segmentID]
		if !ok {
			return intake, fmt.Errorf(
				"%w: unresolved OCR risk segment %q",
				ErrImageTaskInvalidState,
				segmentID,
			)
		}
		delete(correctionsBySegment, segmentID)
		if rawText == "" || strings.Count(expectedCanonical, rawText) != 1 {
			return intake, fmt.Errorf(
				"%w: OCR risk segment %q cannot be located unambiguously",
				ErrImageTaskInvalidState,
				segmentID,
			)
		}
		expectedCanonical = strings.Replace(
			expectedCanonical,
			rawText,
			correction.CanonicalText,
			1,
		)
		if correction.CanonicalText != rawText {
			correctedSegment = true
		}
		orderedCorrections = append(orderedCorrections, correction)
	}
	if len(correctionsBySegment) != 0 {
		return intake, fmt.Errorf(
			"%w: OCR correction references unknown risk segment",
			ErrImageTaskInvalidState,
		)
	}
	if len(intake.OCREvidence.RiskSegments) != 0 &&
		expectedCanonical != canonicalContent {
		return intake, fmt.Errorf(
			"%w: canonical content does not match segment corrections",
			ErrImageTaskInvalidState,
		)
	}
	provenance := k12.CreativeWorkParentConfirmed
	if canonicalContent != strings.TrimSpace(intake.OCREvidence.CanonicalContent) ||
		correctedSegment {
		provenance = k12.CreativeWorkParentCorrected
	}
	evidence := *intake.OCREvidence
	evidence.CanonicalContent = canonicalContent
	evidence.SegmentCorrections = orderedCorrections
	evidence.CanonicalVersion++
	sum := sha256.Sum256([]byte(canonicalContent))
	evidence.CanonicalDigest = "sha256:" + hex.EncodeToString(sum[:])
	evidence.ConfirmationProvenance = provenance
	evidence.FrozenAt = nowUnix()
	evidenceJSON, _ := jsonString(evidence)
	res, err := tx.ExecContext(ctx, `UPDATE k12_creative_work_intakes
        SET ocr_evidence_json=?,status='ready',confirmation_provenance=?,
            version=version+1,updated_at=?
        WHERE agent_name=? AND intake_id=? AND status='awaiting_confirmation' AND version=?`,
		evidenceJSON, provenance, evidence.FrozenAt, agentName, intakeID, expectedVersion)
	if err != nil {
		return intake, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return intake, ErrImageTaskVersionConflict
	}
	if err := tx.Commit(); err != nil {
		return intake, err
	}
	return getCreativeWorkIntake(ctx, s.db, agentName, intakeID)
}

func (s *Store) BindHomeworkSubmissionGradingJob(
	ctx context.Context,
	agentName, submissionID, gradingJobID string,
	expectedVersion int,
) (k12.HomeworkSubmission, error) {
	now := nowUnix()
	res, err := s.db.ExecContext(ctx, `UPDATE k12_homework_submissions
        SET grading_job_id=?,status='processing',version=version+1,updated_at=?
        WHERE agent_name=? AND submission_id=? AND version=?
          AND (grading_job_id='' OR grading_job_id=?)`,
		gradingJobID, now, agentName, submissionID, expectedVersion, gradingJobID)
	if err != nil {
		return k12.HomeworkSubmission{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		existing, getErr := getHomeworkSubmission(ctx, s.db, agentName, submissionID)
		if getErr == nil && existing.GradingJobID == gradingJobID {
			return existing, nil
		}
		return existing, ErrImageTaskVersionConflict
	}
	return getHomeworkSubmission(ctx, s.db, agentName, submissionID)
}

func (s *Store) CancelImageTaskDispatch(
	ctx context.Context,
	agentName, dispatchID string,
	expectedVersion int,
) (k12.ImageTaskDispatch, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.ImageTaskDispatch{}, err
	}
	defer tx.Rollback()
	dispatch, err := getImageTaskDispatch(ctx, tx, agentName, dispatchID, "")
	if err != nil {
		return dispatch, err
	}
	if dispatch.Status == k12.ImageTaskStatusCancelled {
		if err := tx.Commit(); err != nil {
			return dispatch, err
		}
		return dispatch, nil
	}
	if dispatch.Version != expectedVersion {
		return dispatch, ErrImageTaskVersionConflict
	}
	switch dispatch.Status {
	case k12.ImageTaskStatusRouting, k12.ImageTaskStatusAwaitingConfirmation:
		// No target exists.
	case k12.ImageTaskStatusFailed:
		if dispatch.TargetObjectID != "" {
			return dispatch, ErrImageTaskInvalidState
		}
	case k12.ImageTaskStatusRouted:
		switch dispatch.TargetObjectType {
		case k12.ImageTaskTargetHomeworkSubmission:
			submission, getErr := getHomeworkSubmission(ctx, tx, agentName, dispatch.TargetObjectID)
			if getErr != nil {
				return dispatch, getErr
			}
			switch submission.Status {
			case k12.HomeworkSubmissionReceived, k12.HomeworkSubmissionProcessing,
				k12.HomeworkSubmissionAwaitingConfirmation, k12.HomeworkSubmissionFailed:
			default:
				return dispatch, ErrImageTaskInvalidState
			}
			if _, err := tx.ExecContext(ctx, `UPDATE k12_homework_submissions
                SET status='cancelled',version=version+1,updated_at=?
                WHERE agent_name=? AND submission_id=? AND version=?`,
				nowUnix(), agentName, submission.SubmissionID, submission.Version); err != nil {
				return dispatch, err
			}
		case k12.ImageTaskTargetCreativeWorkIntake:
			intake, getErr := getCreativeWorkIntake(ctx, tx, agentName, dispatch.TargetObjectID)
			if getErr != nil {
				return dispatch, getErr
			}
			switch intake.Status {
			case k12.CreativeWorkIntakePreparing, k12.CreativeWorkIntakeAwaitingConfirmation,
				k12.CreativeWorkIntakeFailed:
			default:
				return dispatch, ErrImageTaskInvalidState
			}
			if _, err := tx.ExecContext(ctx, `UPDATE k12_creative_work_intakes
                SET status='cancelled',retry_safe=0,version=version+1,updated_at=?
                WHERE agent_name=? AND intake_id=? AND version=?`,
				nowUnix(), agentName, intake.IntakeID, intake.Version); err != nil {
				return dispatch, err
			}
		default:
			return dispatch, ErrImageTaskInvalidState
		}
	default:
		return dispatch, ErrImageTaskInvalidState
	}
	now := nowUnix()
	res, err := tx.ExecContext(ctx, `UPDATE k12_image_task_dispatches
        SET status='cancelled',retry_safe=0,version=version+1,updated_at=?
        WHERE agent_name=? AND dispatch_id=? AND version=? AND status!='cancelled'`,
		now, agentName, dispatchID, expectedVersion)
	if err != nil {
		return dispatch, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return dispatch, ErrImageTaskVersionConflict
	}
	if err := tx.Commit(); err != nil {
		return dispatch, err
	}
	return s.GetImageTaskDispatch(ctx, agentName, dispatchID)
}
