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
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/internal/sqliteutil"
	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

var ErrModelPhysicalInvocationConflict = errors.New(
	"model physical invocation immutable identity conflict",
)

const modelPhysicalInvocationColumns = `physical_invocation_id,parent_invocation_id,
    agent_name,job_id,stage,physical_unit,request_digest,route_snapshot_json,
    request_policy_snapshot_json,status,attempt,result_digest,external_request_id,
    failure_kind,created_at,updated_at,recognition_plan_version,plan_digest,
    candidate_exact_set_digest`

func scanModelPhysicalInvocation(
	row rowScanner,
) (k12.ModelPhysicalInvocation, error) {
	var invocation k12.ModelPhysicalInvocation
	var routeJSON, requestPolicyJSON, status, physicalUnit, planVersion string
	err := row.Scan(
		&invocation.PhysicalInvocationID,
		&invocation.ParentInvocationID,
		&invocation.AgentName,
		&invocation.JobID,
		&invocation.Stage,
		&physicalUnit,
		&invocation.RequestDigest,
		&routeJSON,
		&requestPolicyJSON,
		&status,
		&invocation.Attempt,
		&invocation.ResultDigest,
		&invocation.ExternalRequestID,
		&invocation.FailureKind,
		&invocation.CreatedAt,
		&invocation.UpdatedAt,
		&planVersion,
		&invocation.PlanDigest,
		&invocation.CandidateExactSetDigest,
	)
	if err != nil {
		return k12.ModelPhysicalInvocation{}, err
	}
	if err := json.Unmarshal(
		[]byte(routeJSON),
		&invocation.RouteSnapshot,
	); err != nil {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"k12storage: parse physical invocation route snapshot: %w",
			err,
		)
	}
	invocation.RouteSnapshot = k12.NormalizeGradingModelSnapshot(
		invocation.RouteSnapshot,
	)
	if strings.TrimSpace(requestPolicyJSON) != "" {
		if err := json.Unmarshal(
			[]byte(requestPolicyJSON),
			&invocation.RequestPolicySnapshot,
		); err != nil {
			return k12.ModelPhysicalInvocation{}, fmt.Errorf(
				"k12storage: parse physical invocation request policy snapshot: %w",
				err,
			)
		}
	}
	invocation.RequestPolicySnapshot = k12.NormalizeModelRequestPolicySnapshot(
		invocation.RequestPolicySnapshot,
	)
	invocation.PhysicalUnit = k12.RecognitionPhysicalUnit(physicalUnit)
	switch planVersion {
	case "v1":
		invocation.RecognitionPlanVersion = k12.RecognitionPlanVersionV1
	case "v2":
		invocation.RecognitionPlanVersion = k12.RecognitionPlanVersionV2
	default:
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"k12storage: parse physical invocation plan version %q",
			planVersion,
		)
	}
	invocation.Status = k12.ModelInvocationStatus(status)
	return invocation, nil
}

func validateModelPhysicalInvocation(
	invocation *k12.ModelPhysicalInvocation,
) error {
	if invocation == nil {
		return fmt.Errorf("k12storage: model physical invocation is nil")
	}
	invocation.PhysicalInvocationID = strings.TrimSpace(
		invocation.PhysicalInvocationID,
	)
	invocation.ParentInvocationID = strings.TrimSpace(
		invocation.ParentInvocationID,
	)
	invocation.AgentName = strings.TrimSpace(invocation.AgentName)
	invocation.JobID = strings.TrimSpace(invocation.JobID)
	invocation.Stage = strings.TrimSpace(invocation.Stage)
	invocation.RequestDigest = strings.TrimSpace(invocation.RequestDigest)
	invocation.PlanDigest = strings.TrimSpace(invocation.PlanDigest)
	invocation.CandidateExactSetDigest = strings.TrimSpace(
		invocation.CandidateExactSetDigest,
	)
	if invocation.RecognitionPlanVersion == 0 {
		invocation.RecognitionPlanVersion = k12.RecognitionPlanVersionV1
	}
	invocation.RouteSnapshot = k12.NormalizeGradingModelSnapshot(
		invocation.RouteSnapshot,
	)
	invocation.RequestPolicySnapshot = k12.NormalizeModelRequestPolicySnapshot(
		invocation.RequestPolicySnapshot,
	)
	if invocation.PhysicalInvocationID == "" ||
		invocation.ParentInvocationID == "" ||
		invocation.AgentName == "" ||
		invocation.JobID == "" ||
		invocation.Stage == "" ||
		invocation.RequestDigest == "" ||
		invocation.RouteSnapshot.Provider == "" ||
		invocation.RouteSnapshot.Model == "" ||
		invocation.RouteSnapshot.Route == "" {
		return fmt.Errorf(
			"k12storage: physical invocation missing id/parent/owner/job/stage/unit/digest/route",
		)
	}
	if !invocation.PhysicalUnit.Valid() {
		return fmt.Errorf(
			"k12storage: invalid recognition physical unit %q",
			invocation.PhysicalUnit,
		)
	}
	if invocation.Attempt != 1 {
		return fmt.Errorf(
			"k12storage: physical invocation attempt=%d, want exactly 1",
			invocation.Attempt,
		)
	}
	switch invocation.RecognitionPlanVersion {
	case k12.RecognitionPlanVersionV1:
		if !legacyRecognitionPhysicalUnit(invocation.PhysicalUnit) ||
			invocation.PlanDigest != "" ||
			invocation.CandidateExactSetDigest != "" {
			return fmt.Errorf(
				"k12storage: V1 physical invocation carries V2 plan identity",
			)
		}
	case k12.RecognitionPlanVersionV2:
		if invocation.PlanDigest == "" {
			return fmt.Errorf(
				"k12storage: V2 physical invocation requires plan digest",
			)
		}
		if invocation.PhysicalUnit == k12.RecognitionPhysicalUnitWholePage {
			if invocation.CandidateExactSetDigest != "" {
				return fmt.Errorf(
					"k12storage: V2 manifest must not carry a candidate exact-set",
				)
			}
		} else if !layoutRecognitionPhysicalUnit(invocation.PhysicalUnit) ||
			invocation.CandidateExactSetDigest == "" {
			return fmt.Errorf(
				"k12storage: V2 child requires a layout unit and candidate exact-set",
			)
		}
	default:
		return fmt.Errorf(
			"k12storage: unsupported recognition plan version %d",
			invocation.RecognitionPlanVersion,
		)
	}
	return nil
}

func legacyRecognitionPhysicalUnit(unit k12.RecognitionPhysicalUnit) bool {
	switch unit {
	case k12.RecognitionPhysicalUnitWholePage,
		k12.RecognitionPhysicalUnitSegment1,
		k12.RecognitionPhysicalUnitSegment2,
		k12.RecognitionPhysicalUnitSegment3,
		k12.RecognitionPhysicalUnitSegment4,
		k12.RecognitionPhysicalUnitSegment5,
		k12.RecognitionPhysicalUnitPrintedInventory:
		return true
	default:
		return false
	}
}

func layoutRecognitionPhysicalUnit(unit k12.RecognitionPhysicalUnit) bool {
	return strings.HasPrefix(string(unit), "layout_batch_") ||
		strings.HasPrefix(string(unit), "layout_repair_")
}

func recognitionPlanVersionSQL(version int) (string, error) {
	if version == 0 || version == k12.RecognitionPlanVersionV1 {
		return "v1", nil
	}
	if version == k12.RecognitionPlanVersionV2 {
		return "v2", nil
	}
	return "", fmt.Errorf(
		"k12storage: unsupported recognition plan version %d",
		version,
	)
}

func (s *Store) getModelInvocationByID(
	ctx context.Context,
	invocationID string,
) (k12.ModelInvocation, error) {
	return getModelInvocationByIDVia(ctx, s.db, invocationID)
}

func getModelInvocationByIDVia(
	ctx context.Context,
	q dbQueryer,
	invocationID string,
) (k12.ModelInvocation, error) {
	invocation, err := scanModelInvocation(q.QueryRowContext(
		ctx,
		`SELECT `+modelInvocationColumns+`
         FROM k12_model_invocations WHERE invocation_id=?`,
		invocationID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.ModelInvocation{}, records.ErrNotFound
	}
	if err != nil {
		return k12.ModelInvocation{}, fmt.Errorf(
			"k12storage: get parent model invocation: %w",
			err,
		)
	}
	return invocation, nil
}

func getModelPhysicalInvocationByIDVia(
	ctx context.Context,
	q dbQueryer,
	agentName string,
	physicalInvocationID string,
) (k12.ModelPhysicalInvocation, error) {
	invocation, err := scanModelPhysicalInvocation(q.QueryRowContext(
		ctx,
		`SELECT `+modelPhysicalInvocationColumns+`
         FROM k12_model_physical_invocations
         WHERE physical_invocation_id=? AND agent_name=?`,
		strings.TrimSpace(physicalInvocationID),
		strings.TrimSpace(agentName),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.ModelPhysicalInvocation{}, records.ErrNotFound
	}
	if err != nil {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"k12storage: get model physical invocation: %w",
			err,
		)
	}
	return invocation, nil
}

func (s *Store) getModelPhysicalInvocationByUnit(
	ctx context.Context,
	parentInvocationID string,
	physicalUnit k12.RecognitionPhysicalUnit,
) (k12.ModelPhysicalInvocation, error) {
	invocation, err := scanModelPhysicalInvocation(s.db.QueryRowContext(
		ctx,
		`SELECT `+modelPhysicalInvocationColumns+`
         FROM k12_model_physical_invocations
         WHERE parent_invocation_id=? AND physical_unit=?`,
		parentInvocationID,
		physicalUnit,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.ModelPhysicalInvocation{}, records.ErrNotFound
	}
	if err != nil {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"k12storage: get model physical invocation by unit: %w",
			err,
		)
	}
	return invocation, nil
}

func validatePhysicalInvocationParent(
	invocation k12.ModelPhysicalInvocation,
	parent k12.ModelInvocation,
) error {
	if invocation.ParentInvocationID != parent.InvocationID ||
		invocation.AgentName != parent.AgentName ||
		invocation.JobID != parent.JobID ||
		invocation.Stage != parent.Stage ||
		invocation.RouteSnapshot != parent.RouteSnapshot ||
		invocation.RequestPolicySnapshot != parent.RequestPolicySnapshot {
		return fmt.Errorf(
			"%w: parent=%s unit=%s",
			ErrModelPhysicalInvocationConflict,
			invocation.ParentInvocationID,
			invocation.PhysicalUnit,
		)
	}
	if invocation.Stage != k12.GradingStageRecognizing {
		return fmt.Errorf(
			"k12storage: physical invocation stage %q is not recognizing",
			invocation.Stage,
		)
	}
	if err := k12.ValidateModelInvocationRequestPolicy(
		invocation.Stage,
		invocation.RouteSnapshot,
		invocation.RequestPolicySnapshot,
	); err != nil {
		return fmt.Errorf(
			"%w: invalid inherited request policy: %v",
			ErrModelPhysicalInvocationConflict,
			err,
		)
	}
	return nil
}

func sameModelPhysicalInvocationIdentity(
	stored k12.ModelPhysicalInvocation,
	requested k12.ModelPhysicalInvocation,
) bool {
	return stored.PhysicalInvocationID == requested.PhysicalInvocationID &&
		stored.ParentInvocationID == requested.ParentInvocationID &&
		stored.AgentName == requested.AgentName &&
		stored.JobID == requested.JobID &&
		stored.Stage == requested.Stage &&
		stored.PhysicalUnit == requested.PhysicalUnit &&
		stored.RequestDigest == requested.RequestDigest &&
		stored.RouteSnapshot == requested.RouteSnapshot &&
		stored.RequestPolicySnapshot == requested.RequestPolicySnapshot &&
		stored.RecognitionPlanVersion == requested.RecognitionPlanVersion &&
		stored.PlanDigest == requested.PlanDigest &&
		stored.CandidateExactSetDigest == requested.CandidateExactSetDigest &&
		stored.Attempt == requested.Attempt
}

func physicalInvocationResultDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func getModelInvocationByAttemptVia(
	ctx context.Context,
	q dbQueryer,
	jobID string,
	stage string,
	attempt int,
) (k12.ModelInvocation, error) {
	invocation, err := scanModelInvocation(q.QueryRowContext(
		ctx,
		`SELECT `+modelInvocationColumns+`
         FROM k12_model_invocations
         WHERE job_id=? AND stage=? AND attempt=?`,
		jobID,
		stage,
		attempt,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.ModelInvocation{}, records.ErrNotFound
	}
	if err != nil {
		return k12.ModelInvocation{}, fmt.Errorf(
			"k12storage: get model invocation by attempt: %w",
			err,
		)
	}
	return invocation, nil
}

func getModelPhysicalInvocationByUnitVia(
	ctx context.Context,
	q dbQueryer,
	parentInvocationID string,
	physicalUnit k12.RecognitionPhysicalUnit,
) (k12.ModelPhysicalInvocation, error) {
	invocation, err := scanModelPhysicalInvocation(q.QueryRowContext(
		ctx,
		`SELECT `+modelPhysicalInvocationColumns+`
         FROM k12_model_physical_invocations
         WHERE parent_invocation_id=? AND physical_unit=?`,
		parentInvocationID,
		physicalUnit,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.ModelPhysicalInvocation{}, records.ErrNotFound
	}
	if err != nil {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"k12storage: get model physical invocation by unit: %w",
			err,
		)
	}
	return invocation, nil
}

func recognitionFallbackPredecessors(
	unit k12.RecognitionPhysicalUnit,
) []k12.RecognitionPhysicalUnit {
	switch unit {
	case k12.RecognitionPhysicalUnitSegment1:
		return nil
	case k12.RecognitionPhysicalUnitSegment2:
		return []k12.RecognitionPhysicalUnit{
			k12.RecognitionPhysicalUnitSegment1,
		}
	case k12.RecognitionPhysicalUnitSegment3:
		return []k12.RecognitionPhysicalUnit{
			k12.RecognitionPhysicalUnitSegment1,
			k12.RecognitionPhysicalUnitSegment2,
		}
	case k12.RecognitionPhysicalUnitSegment4:
		return []k12.RecognitionPhysicalUnit{
			k12.RecognitionPhysicalUnitSegment1,
			k12.RecognitionPhysicalUnitSegment2,
			k12.RecognitionPhysicalUnitSegment3,
		}
	case k12.RecognitionPhysicalUnitSegment5:
		return []k12.RecognitionPhysicalUnit{
			k12.RecognitionPhysicalUnitSegment1,
			k12.RecognitionPhysicalUnitSegment2,
			k12.RecognitionPhysicalUnitSegment3,
			k12.RecognitionPhysicalUnitSegment4,
		}
	case k12.RecognitionPhysicalUnitPrintedInventory:
		return []k12.RecognitionPhysicalUnit{
			k12.RecognitionPhysicalUnitSegment1,
			k12.RecognitionPhysicalUnitSegment2,
			k12.RecognitionPhysicalUnitSegment3,
			k12.RecognitionPhysicalUnitSegment4,
			k12.RecognitionPhysicalUnitSegment5,
		}
	default:
		return nil
	}
}

// recognitionPhysicalFallbackGateSQL returns a predicate that is evaluated in
// the same INSERT/UPDATE statement as the child state change. parentAlias is
// an internal SQL alias, never caller input.
func recognitionPhysicalFallbackGateSQL(
	parentAlias string,
	unit k12.RecognitionPhysicalUnit,
) (string, []any) {
	if unit == k12.RecognitionPhysicalUnitWholePage {
		return "1=1", nil
	}
	predicate := fmt.Sprintf(
		`EXISTS (
             SELECT 1
             FROM k12_recognition_fallback_authorizations AS authorization
             JOIN k12_model_physical_invocations AS whole
               ON whole.physical_invocation_id =
                    authorization.whole_physical_invocation_id
             WHERE authorization.parent_invocation_id = %[1]s.invocation_id
               AND authorization.agent_name = %[1]s.agent_name
               AND authorization.job_id = %[1]s.job_id
               AND whole.parent_invocation_id = %[1]s.invocation_id
               AND whole.agent_name = %[1]s.agent_name
               AND whole.job_id = %[1]s.job_id
               AND whole.physical_unit = 'whole_page'
               AND whole.status = 'succeeded'
               AND whole.result_digest =
                    authorization.whole_result_digest
               AND whole.result_content =
                    authorization.whole_result_content
         )`,
		parentAlias,
	)
	args := make([]any, 0, len(recognitionFallbackPredecessors(unit)))
	for _, predecessor := range recognitionFallbackPredecessors(unit) {
		predicate += fmt.Sprintf(
			` AND EXISTS (
                 SELECT 1
                 FROM k12_model_physical_invocations AS predecessor
                 WHERE predecessor.parent_invocation_id = %[1]s.invocation_id
                   AND predecessor.agent_name = %[1]s.agent_name
                   AND predecessor.job_id = %[1]s.job_id
                   AND predecessor.physical_unit = ?
                   AND predecessor.status = 'succeeded'
             )`,
			parentAlias,
		)
		args = append(args, predecessor)
	}
	return predicate, args
}

func validateRecognitionFallbackFactsVia(
	ctx context.Context,
	q dbQueryer,
	parent k12.ModelInvocation,
	invocation k12.ModelPhysicalInvocation,
) error {
	if parent.Status != k12.ModelInvocationSent {
		return fmt.Errorf(
			"%w: fallback parent %s status=%s",
			records.ErrIllegalTransition,
			parent.InvocationID,
			parent.Status,
		)
	}
	var (
		authorizationAgent   string
		authorizationJob     string
		authorizationWholeID string
		authorizationDigest  string
		authorizationContent string
		wholeParentID        string
		wholeAgent           string
		wholeJob             string
		wholeStage           string
		wholeUnit            k12.RecognitionPhysicalUnit
		wholeStatus          k12.ModelInvocationStatus
		wholeDigest          string
		wholeContent         sql.NullString
	)
	err := q.QueryRowContext(
		ctx,
		`SELECT authorization.agent_name,
                authorization.job_id,
                authorization.whole_physical_invocation_id,
                authorization.whole_result_digest,
                authorization.whole_result_content,
                whole.parent_invocation_id,
                whole.agent_name,
                whole.job_id,
                whole.stage,
                whole.physical_unit,
                whole.status,
                whole.result_digest,
                whole.result_content
         FROM k12_recognition_fallback_authorizations AS authorization
         JOIN k12_model_physical_invocations AS whole
           ON whole.physical_invocation_id =
                authorization.whole_physical_invocation_id
         WHERE authorization.parent_invocation_id=?`,
		parent.InvocationID,
	).Scan(
		&authorizationAgent,
		&authorizationJob,
		&authorizationWholeID,
		&authorizationDigest,
		&authorizationContent,
		&wholeParentID,
		&wholeAgent,
		&wholeJob,
		&wholeStage,
		&wholeUnit,
		&wholeStatus,
		&wholeDigest,
		&wholeContent,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(
			"%w: fallback authorization for parent %s is missing",
			records.ErrIllegalTransition,
			parent.InvocationID,
		)
	}
	if err != nil {
		return fmt.Errorf(
			"k12storage: validate fallback authorization in transaction: %w",
			err,
		)
	}
	if authorizationAgent != invocation.AgentName ||
		authorizationJob != invocation.JobID ||
		authorizationWholeID == "" ||
		authorizationDigest !=
			physicalInvocationResultDigest(authorizationContent) ||
		wholeParentID != invocation.ParentInvocationID ||
		wholeAgent != invocation.AgentName ||
		wholeJob != invocation.JobID ||
		wholeStage != invocation.Stage ||
		wholeUnit != k12.RecognitionPhysicalUnitWholePage ||
		wholeStatus != k12.ModelInvocationSucceeded ||
		!wholeContent.Valid ||
		wholeDigest != physicalInvocationResultDigest(wholeContent.String) ||
		authorizationDigest != wholeDigest ||
		authorizationContent != wholeContent.String {
		return fmt.Errorf(
			"%w: fallback authorization for parent %s drifted",
			ErrModelPhysicalInvocationConflict,
			parent.InvocationID,
		)
	}

	for _, predecessorUnit := range recognitionFallbackPredecessors(
		invocation.PhysicalUnit,
	) {
		var (
			predecessorParent  string
			predecessorAgent   string
			predecessorJob     string
			predecessorStage   string
			predecessorStatus  k12.ModelInvocationStatus
			predecessorDigest  string
			predecessorContent sql.NullString
		)
		err := q.QueryRowContext(
			ctx,
			`SELECT parent_invocation_id,agent_name,job_id,stage,
                    status,result_digest,result_content
             FROM k12_model_physical_invocations
             WHERE parent_invocation_id=? AND physical_unit=?`,
			parent.InvocationID,
			predecessorUnit,
		).Scan(
			&predecessorParent,
			&predecessorAgent,
			&predecessorJob,
			&predecessorStage,
			&predecessorStatus,
			&predecessorDigest,
			&predecessorContent,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf(
				"%w: fallback unit %s lacks predecessor %s",
				records.ErrIllegalTransition,
				invocation.PhysicalUnit,
				predecessorUnit,
			)
		}
		if err != nil {
			return fmt.Errorf(
				"k12storage: validate fallback predecessor %s: %w",
				predecessorUnit,
				err,
			)
		}
		if predecessorParent != invocation.ParentInvocationID ||
			predecessorAgent != invocation.AgentName ||
			predecessorJob != invocation.JobID ||
			predecessorStage != invocation.Stage ||
			predecessorStatus != k12.ModelInvocationSucceeded ||
			!predecessorContent.Valid ||
			predecessorDigest != physicalInvocationResultDigest(
				predecessorContent.String,
			) {
			return fmt.Errorf(
				"%w: fallback predecessor %s drifted",
				ErrModelPhysicalInvocationConflict,
				predecessorUnit,
			)
		}
	}
	return nil
}

func sameRecognizingInitialParentIdentity(
	stored k12.ModelInvocation,
	requested k12.ModelInvocation,
) bool {
	return stored.InvocationID == requested.InvocationID &&
		stored.AgentName == requested.AgentName &&
		stored.JobID == requested.JobID &&
		stored.Stage == requested.Stage &&
		stored.RequestDigest == requested.RequestDigest &&
		stored.RouteSnapshot == requested.RouteSnapshot &&
		stored.RequestPolicySnapshot == requested.RequestPolicySnapshot &&
		stored.Attempt == requested.Attempt
}

// PrepareRecognizingInvocationWithInitialWholePage atomically publishes the
// recognizing parent as sent together with its exact prepared whole_page
// child. A prepared parent is not provider authorization; the transaction
// changes it to sent only after the child insert is durable.
func (s *Store) PrepareRecognizingInvocationWithInitialWholePage(
	ctx context.Context,
	parent k12.ModelInvocation,
	child k12.ModelPhysicalInvocation,
) (
	k12.ModelInvocation,
	k12.ModelPhysicalInvocation,
	bool,
	error,
) {
	var (
		storedParent k12.ModelInvocation
		storedChild  k12.ModelPhysicalInvocation
		created      bool
	)
	err := sqliteutil.RetryOnBusy(ctx, func() error {
		var attemptErr error
		storedParent, storedChild, created, attemptErr =
			s.prepareRecognizingInvocationWithInitialWholePageOnce(
				ctx,
				parent,
				child,
				nil,
			)
		return attemptErr
	})
	if err != nil {
		return k12.ModelInvocation{},
			k12.ModelPhysicalInvocation{},
			false,
			err
	}
	return storedParent, storedChild, created, nil
}

// PrepareRecognizingInvocationWithInitialLayoutPlanV2 在发布 parent 为 sent 前，
// 原子持久化不可变的 V2 计划头、recognizing parent 和 compact-manifest
// whole_page 子调用。精确重放保持幂等；任何事务都不能在缺少任一 V2 行时暴露 sent parent。
func (s *Store) PrepareRecognizingInvocationWithInitialLayoutPlanV2(
	ctx context.Context,
	parent k12.ModelInvocation,
	child k12.ModelPhysicalInvocation,
	header k12.RecognitionLayoutPlanHeaderV2,
) (
	k12.ModelInvocation,
	k12.ModelPhysicalInvocation,
	bool,
	error,
) {
	var (
		storedParent k12.ModelInvocation
		storedChild  k12.ModelPhysicalInvocation
		created      bool
	)
	err := sqliteutil.RetryOnBusy(ctx, func() error {
		var attemptErr error
		storedParent, storedChild, created, attemptErr =
			s.prepareRecognizingInvocationWithInitialWholePageOnce(
				ctx,
				parent,
				child,
				&header,
			)
		return attemptErr
	})
	if err != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, err
	}
	return storedParent, storedChild, created, nil
}

// prepareRecognizingInvocationWithInitialWholePageOnce owns one complete
// deferred SQLite transaction. A BUSY/BUSY_SNAPSHOT retry must restart this
// whole function so it never reuses a stale WAL read snapshot.
func (s *Store) prepareRecognizingInvocationWithInitialWholePageOnce(
	ctx context.Context,
	parent k12.ModelInvocation,
	child k12.ModelPhysicalInvocation,
	layoutHeader *k12.RecognitionLayoutPlanHeaderV2,
) (
	k12.ModelInvocation,
	k12.ModelPhysicalInvocation,
	bool,
	error,
) {
	if err := validateModelInvocation(&parent); err != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, err
	}
	if err := validateModelPhysicalInvocation(&child); err != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, err
	}
	if parent.Stage != k12.GradingStageRecognizing ||
		child.PhysicalUnit != k12.RecognitionPhysicalUnitWholePage {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
			fmt.Errorf(
				"k12storage: atomic recognizing publication requires recognizing parent and whole_page child",
			)
	}
	if err := validatePhysicalInvocationParent(child, parent); err != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, err
	}
	var (
		layoutHeaderJSON   []byte
		layoutHeaderDigest string
	)
	if layoutHeader == nil {
		if child.RecognitionPlanVersion != k12.RecognitionPlanVersionV1 {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				fmt.Errorf(
					"k12storage: V2 initial child requires an atomic layout-plan header",
				)
		}
	} else {
		if child.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				fmt.Errorf(
					"k12storage: layout-plan header requires a V2 initial child",
				)
		}
		if layoutHeader.ParentInvocationID != parent.InvocationID ||
			layoutHeader.AgentName != parent.AgentName ||
			layoutHeader.JobID != parent.JobID ||
			layoutHeader.ParentRequestDigest != parent.RequestDigest ||
			layoutHeader.RouteSnapshot != parent.RouteSnapshot ||
			layoutHeader.RequestPolicySnapshot != parent.RequestPolicySnapshot {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				fmt.Errorf(
					"%w: layout-plan header drifted from parent identity",
					ErrModelPhysicalInvocationConflict,
				)
		}
		var headerErr error
		layoutHeaderJSON, layoutHeaderDigest, headerErr =
			k12.CanonicalRecognitionLayoutPlanHeaderV2(*layoutHeader)
		if headerErr != nil {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				headerErr
		}
		if child.PlanDigest != layoutHeaderDigest {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				fmt.Errorf(
					"%w: manifest child is detached from layout-plan header digest",
					ErrModelPhysicalInvocationConflict,
				)
		}
	}
	if err := ensureAgentRegistered(ctx, s.db, parent.AgentName); err != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, err
	}
	if parent.CreatedAt <= 0 {
		parent.CreatedAt = nowUnix()
	}
	parent.UpdatedAt = parent.CreatedAt
	parent.Status = k12.ModelInvocationPrepared
	if child.CreatedAt <= 0 {
		child.CreatedAt = nowUnix()
	}
	child.UpdatedAt = child.CreatedAt
	child.Status = k12.ModelInvocationPrepared

	parentRouteJSON, opErr := json.Marshal(parent.RouteSnapshot)
	if opErr != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, opErr
	}
	parentPolicyJSON := ""
	if !parent.RequestPolicySnapshot.IsZero() {
		raw, marshalErr := json.Marshal(parent.RequestPolicySnapshot)
		if marshalErr != nil {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				marshalErr
		}
		parentPolicyJSON = string(raw)
	}
	childRouteJSON, opErr := json.Marshal(child.RouteSnapshot)
	if opErr != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, opErr
	}
	childPolicyJSON := ""
	if !child.RequestPolicySnapshot.IsZero() {
		raw, marshalErr := json.Marshal(child.RequestPolicySnapshot)
		if marshalErr != nil {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				marshalErr
		}
		childPolicyJSON = string(raw)
	}
	childPlanVersion, opErr := recognitionPlanVersionSQL(
		child.RecognitionPlanVersion,
	)
	if opErr != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, opErr
	}

	tx, opErr := s.db.BeginTx(ctx, nil)
	if opErr != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
			fmt.Errorf(
				"k12storage: begin atomic recognizing publication: %w",
				opErr,
			)
	}
	// 提交成功或主路径失败后，回滚仅用于释放事务；主路径错误保持原样。
	defer func() { _ = tx.Rollback() }()

	parentCreated := false
	storedParent, opErr := getModelInvocationByAttemptVia(
		ctx,
		tx,
		parent.JobID,
		parent.Stage,
		parent.Attempt,
	)
	if errors.Is(opErr, records.ErrNotFound) {
		res, insertErr := tx.ExecContext(
			ctx,
			`INSERT INTO k12_model_invocations (`+
				modelInvocationColumns+
				`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
                 ON CONFLICT(job_id,stage,attempt) DO NOTHING`,
			parent.InvocationID,
			parent.AgentName,
			parent.JobID,
			parent.Stage,
			parent.RequestDigest,
			parent.RouteSnapshot.Provider,
			parent.RouteSnapshot.Model,
			string(parentRouteJSON),
			parentPolicyJSON,
			parent.ProviderIdempotencyKey,
			parent.Status,
			parent.Attempt,
			"",
			"",
			"",
			"",
			parent.CreatedAt,
			parent.UpdatedAt,
		)
		if insertErr != nil {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				fmt.Errorf(
					"k12storage: prepare recognizing parent in atomic publication: %w",
					insertErr,
				)
		}
		affected, _ := res.RowsAffected()
		parentCreated = affected > 0
		storedParent, opErr = getModelInvocationByAttemptVia(
			ctx,
			tx,
			parent.JobID,
			parent.Stage,
			parent.Attempt,
		)
	}
	if opErr != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, opErr
	}
	if !sameRecognizingInitialParentIdentity(storedParent, parent) {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
			fmt.Errorf(
				"%w: job=%s stage=%s attempt=%d",
				ErrModelInvocationConflict,
				parent.JobID,
				parent.Stage,
				parent.Attempt,
			)
	}

	storedChild, opErr := getModelPhysicalInvocationByUnitVia(
		ctx,
		tx,
		parent.InvocationID,
		k12.RecognitionPhysicalUnitWholePage,
	)
	childCreated := false
	if errors.Is(opErr, records.ErrNotFound) {
		if storedParent.Status != k12.ModelInvocationPrepared {
			return storedParent, k12.ModelPhysicalInvocation{}, false,
				fmt.Errorf(
					"%w: sent/terminal recognizing parent %s has no whole_page child",
					records.ErrIllegalTransition,
					storedParent.InvocationID,
				)
		}
		res, insertErr := tx.ExecContext(
			ctx,
			`INSERT INTO k12_model_physical_invocations (`+
				modelPhysicalInvocationColumns+
				`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
                 ON CONFLICT DO NOTHING`,
			child.PhysicalInvocationID,
			child.ParentInvocationID,
			child.AgentName,
			child.JobID,
			child.Stage,
			child.PhysicalUnit,
			child.RequestDigest,
			string(childRouteJSON),
			childPolicyJSON,
			child.Status,
			child.Attempt,
			"",
			"",
			"",
			child.CreatedAt,
			child.UpdatedAt,
			childPlanVersion,
			child.PlanDigest,
			child.CandidateExactSetDigest,
		)
		if insertErr != nil {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				fmt.Errorf(
					"k12storage: prepare initial whole_page child: %w",
					insertErr,
				)
		}
		affected, _ := res.RowsAffected()
		childCreated = affected > 0
		storedChild, opErr = getModelPhysicalInvocationByUnitVia(
			ctx,
			tx,
			parent.InvocationID,
			k12.RecognitionPhysicalUnitWholePage,
		)
	}
	if opErr != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, opErr
	}
	if !sameModelPhysicalInvocationIdentity(storedChild, child) {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
			fmt.Errorf(
				"%w: parent=%s unit=%s",
				ErrModelPhysicalInvocationConflict,
				parent.InvocationID,
				k12.RecognitionPhysicalUnitWholePage,
			)
	}
	layoutHeaderCreated := false
	if layoutHeader != nil {
		if storedParent.Status == k12.ModelInvocationPrepared {
			headerCreatedAt := nowUnix()
			res, insertErr := tx.ExecContext(
				ctx,
				`INSERT INTO k12_recognition_layout_plans (
                    plan_id,parent_invocation_id,agent_name,job_id,stage,
                    manifest_physical_invocation_id,page_digest,header_digest,
                    layout_header_json,stage_started_at,stage_deadline_at,
                    effective_concurrency,status,created_at,updated_at
                 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
                 ON CONFLICT(parent_invocation_id) DO NOTHING`,
				layoutHeader.PlanID,
				parent.InvocationID,
				parent.AgentName,
				parent.JobID,
				parent.Stage,
				child.PhysicalInvocationID,
				layoutHeader.PageDigest,
				layoutHeaderDigest,
				string(layoutHeaderJSON),
				layoutHeader.StageStartedAtUnixMillis,
				0,
				layoutHeader.EffectiveConcurrency,
				"prepared_manifest",
				headerCreatedAt,
				headerCreatedAt,
			)
			if insertErr != nil {
				return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
					fmt.Errorf(
						"k12storage: persist initial V2 layout-plan header: %w",
						insertErr,
					)
			}
			affected, _ := res.RowsAffected()
			layoutHeaderCreated = affected > 0
		}
		var (
			storedPlanID, storedParentID, storedAgent, storedJob string
			storedStage, storedManifestID, storedPageDigest      string
			storedHeaderDigest, storedHeaderJSON, storedStatus   string
			storedAuthorizedPlanJSON                             string
			storedStageStarted, storedStageDeadline              int64
			storedConcurrency, storedSelectedBucket              int
		)
		opErr = tx.QueryRowContext(
			ctx,
			`SELECT plan_id,parent_invocation_id,agent_name,job_id,stage,
                    manifest_physical_invocation_id,page_digest,header_digest,
                    layout_header_json,stage_started_at,stage_deadline_at,
                    effective_concurrency,status,authorized_plan_json,
                    selected_bucket_max_problems
               FROM k12_recognition_layout_plans
              WHERE parent_invocation_id=?`,
			parent.InvocationID,
		).Scan(
			&storedPlanID,
			&storedParentID,
			&storedAgent,
			&storedJob,
			&storedStage,
			&storedManifestID,
			&storedPageDigest,
			&storedHeaderDigest,
			&storedHeaderJSON,
			&storedStageStarted,
			&storedStageDeadline,
			&storedConcurrency,
			&storedStatus,
			&storedAuthorizedPlanJSON,
			&storedSelectedBucket,
		)
		if errors.Is(opErr, sql.ErrNoRows) {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				fmt.Errorf(
					"%w: sent V2 parent has no durable layout-plan header",
					records.ErrIllegalTransition,
				)
		}
		if opErr != nil {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				fmt.Errorf("k12storage: read V2 layout-plan header: %w", opErr)
		}
		if storedPlanID != layoutHeader.PlanID ||
			storedParentID != parent.InvocationID ||
			storedAgent != parent.AgentName ||
			storedJob != parent.JobID ||
			storedStage != parent.Stage ||
			storedManifestID != child.PhysicalInvocationID ||
			storedPageDigest != layoutHeader.PageDigest ||
			storedHeaderDigest != layoutHeaderDigest ||
			storedHeaderJSON != string(layoutHeaderJSON) ||
			storedStageStarted != layoutHeader.StageStartedAtUnixMillis ||
			storedConcurrency != layoutHeader.EffectiveConcurrency ||
			(storedStatus != "prepared_manifest" &&
				storedStatus != "manifest_sent" &&
				storedStatus != "manifest_succeeded" &&
				storedStatus != "authorized" && storedStatus != "running" &&
				storedStatus != "succeeded") {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				fmt.Errorf(
					"%w: immutable layout-plan header changed",
					ErrModelPhysicalInvocationConflict,
				)
		}
		switch storedStatus {
		case "prepared_manifest", "manifest_sent", "manifest_succeeded":
			if storedStageDeadline != 0 || storedSelectedBucket != 0 ||
				storedAuthorizedPlanJSON != "" {
				return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
					fmt.Errorf(
						"%w: pre-authorization layout budget was selected early",
						ErrModelPhysicalInvocationConflict,
					)
			}
		case "authorized", "running", "succeeded":
			var authorizedPlan k12.RecognitionLayoutPlanV2
			if err := json.Unmarshal(
				[]byte(storedAuthorizedPlanJSON),
				&authorizedPlan,
			); err != nil {
				return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
					fmt.Errorf(
						"%w: parse authorized layout plan during replay: %v",
						ErrModelPhysicalInvocationConflict,
						err,
					)
			}
			wantBucket, durationMillis, selectErr :=
				layoutHeader.BudgetBuckets.Select(len(authorizedPlan.Targets))
			if selectErr != nil || storedSelectedBucket != wantBucket ||
				storedStageDeadline !=
					layoutHeader.StageStartedAtUnixMillis+durationMillis {
				return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
					fmt.Errorf(
						"%w: selected layout budget changed during replay",
						ErrModelPhysicalInvocationConflict,
					)
			}
		}
	}
	if storedParent.Status == k12.ModelInvocationPrepared {
		if storedChild.Status != k12.ModelInvocationPrepared {
			return storedParent, storedChild, false, fmt.Errorf(
				"%w: prepared recognizing parent has whole_page status %s",
				records.ErrIllegalTransition,
				storedChild.Status,
			)
		}
		res, updateErr := tx.ExecContext(
			ctx,
			`UPDATE k12_model_invocations
             SET status=?,updated_at=?
             WHERE invocation_id=? AND agent_name=? AND status=?`,
			k12.ModelInvocationSent,
			nowUnix(),
			storedParent.InvocationID,
			storedParent.AgentName,
			k12.ModelInvocationPrepared,
		)
		if updateErr != nil {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				fmt.Errorf(
					"k12storage: publish recognizing parent sent: %w",
					updateErr,
				)
		}
		affected, _ := res.RowsAffected()
		if affected != 1 {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				fmt.Errorf(
					"%w: recognizing parent %s was concurrently published",
					records.ErrIllegalTransition,
					storedParent.InvocationID,
				)
		}
		storedParent, opErr = getModelInvocationByAttemptVia(
			ctx,
			tx,
			parent.JobID,
			parent.Stage,
			parent.Attempt,
		)
		if opErr != nil {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				opErr
		}
	}
	if err := tx.Commit(); err != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
			fmt.Errorf(
				"k12storage: commit atomic recognizing publication: %w",
				err,
			)
	}
	return storedParent, storedChild,
		parentCreated || childCreated || layoutHeaderCreated,
		nil
}

// PrepareModelPhysicalInvocation establishes the durable before-send point for
// one actual recognition request. The immutable identity is keyed by the stage
// parent plus the explicit algorithm-owned physical unit.
func (s *Store) PrepareModelPhysicalInvocation(
	ctx context.Context,
	invocation k12.ModelPhysicalInvocation,
) (k12.ModelPhysicalInvocation, bool, error) {
	if err := validateModelPhysicalInvocation(&invocation); err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	if invocation.PhysicalUnit != k12.RecognitionPhysicalUnitWholePage {
		if invocation.RecognitionPlanVersion == k12.RecognitionPlanVersionV2 {
			return s.prepareLayoutModelPhysicalInvocation(ctx, invocation)
		}
		return s.prepareFallbackModelPhysicalInvocation(ctx, invocation)
	}
	if err := ensureAgentRegistered(ctx, s.db, invocation.AgentName); err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	parent, parentErr := s.getModelInvocationByID(ctx, invocation.ParentInvocationID)
	if parentErr != nil {
		return k12.ModelPhysicalInvocation{}, false, parentErr
	}
	if err := validatePhysicalInvocationParent(invocation, parent); err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	existing, err := s.getModelPhysicalInvocationByUnit(
		ctx,
		invocation.ParentInvocationID,
		invocation.PhysicalUnit,
	)
	if err == nil {
		if !sameModelPhysicalInvocationIdentity(existing, invocation) {
			return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
				"%w: parent=%s unit=%s",
				ErrModelPhysicalInvocationConflict,
				invocation.ParentInvocationID,
				invocation.PhysicalUnit,
			)
		}
		return existing, false, nil
	}
	if !errors.Is(err, records.ErrNotFound) {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	if parent.Status != k12.ModelInvocationSent {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"%w: physical child requires sent parent %s, got %s",
			records.ErrIllegalTransition,
			parent.InvocationID,
			parent.Status,
		)
	}
	if invocation.CreatedAt <= 0 {
		invocation.CreatedAt = nowUnix()
	}
	invocation.UpdatedAt = invocation.CreatedAt
	invocation.Status = k12.ModelInvocationPrepared
	routeJSON, err := json.Marshal(invocation.RouteSnapshot)
	if err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	requestPolicyJSON := ""
	if !invocation.RequestPolicySnapshot.IsZero() {
		raw, marshalErr := json.Marshal(invocation.RequestPolicySnapshot)
		if marshalErr != nil {
			return k12.ModelPhysicalInvocation{}, false, marshalErr
		}
		requestPolicyJSON = string(raw)
	}
	planVersion, err := recognitionPlanVersionSQL(
		invocation.RecognitionPlanVersion,
	)
	if err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	fallbackGateSQL, fallbackGateArgs :=
		recognitionPhysicalFallbackGateSQL(
			"parent",
			invocation.PhysicalUnit,
		)
	insertArgs := []any{
		invocation.PhysicalInvocationID,
		invocation.ParentInvocationID,
		invocation.AgentName,
		invocation.JobID,
		invocation.Stage,
		invocation.PhysicalUnit,
		invocation.RequestDigest,
		string(routeJSON),
		requestPolicyJSON,
		invocation.Status,
		invocation.Attempt,
		"",
		"",
		"",
		invocation.CreatedAt,
		invocation.UpdatedAt,
		planVersion,
		invocation.PlanDigest,
		invocation.CandidateExactSetDigest,
		invocation.ParentInvocationID,
		k12.ModelInvocationSent,
	}
	insertArgs = append(insertArgs, fallbackGateArgs...)
	res, err := s.db.ExecContext(
		ctx,
		`INSERT INTO k12_model_physical_invocations (`+
			modelPhysicalInvocationColumns+
			`) SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?
             FROM k12_model_invocations AS parent
             WHERE invocation_id=? AND status=?
               AND (`+fallbackGateSQL+`)
             ON CONFLICT DO NOTHING`,
		insertArgs...,
	)
	if err != nil {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: prepare model physical invocation: %w",
			err,
		)
	}
	created, _ := res.RowsAffected()
	stored, err := s.getModelPhysicalInvocationByUnit(
		ctx,
		invocation.ParentInvocationID,
		invocation.PhysicalUnit,
	)
	if err != nil {
		if created == 0 && errors.Is(err, records.ErrNotFound) {
			currentParent, parentErr := s.getModelInvocationByID(
				ctx,
				invocation.ParentInvocationID,
			)
			if parentErr != nil {
				return k12.ModelPhysicalInvocation{}, false, parentErr
			}
			if currentParent.Status != k12.ModelInvocationSent {
				return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
					"%w: physical child requires sent parent %s, got %s",
					records.ErrIllegalTransition,
					currentParent.InvocationID,
					currentParent.Status,
				)
			}
			if invocation.PhysicalUnit !=
				k12.RecognitionPhysicalUnitWholePage {
				return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
					"%w: fallback unit %s lacks durable authorization or succeeded predecessors",
					records.ErrIllegalTransition,
					invocation.PhysicalUnit,
				)
			}
			return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
				"%w: physical_invocation_id=%s",
				ErrModelPhysicalInvocationConflict,
				invocation.PhysicalInvocationID,
			)
		}
		return k12.ModelPhysicalInvocation{}, false, err
	}
	if !sameModelPhysicalInvocationIdentity(stored, invocation) {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"%w: parent=%s unit=%s",
			ErrModelPhysicalInvocationConflict,
			invocation.ParentInvocationID,
			invocation.PhysicalUnit,
		)
	}
	return stored, created > 0, nil
}

func (s *Store) prepareLayoutModelPhysicalInvocation(
	ctx context.Context,
	invocation k12.ModelPhysicalInvocation,
) (k12.ModelPhysicalInvocation, bool, error) {
	var (
		stored  k12.ModelPhysicalInvocation
		created bool
	)
	err := sqliteutil.RetryOnBusy(ctx, func() error {
		var attemptErr error
		stored, created, attemptErr =
			s.prepareLayoutModelPhysicalInvocationOnce(ctx, invocation)
		return attemptErr
	})
	if err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	return stored, created, nil
}

func (s *Store) prepareLayoutModelPhysicalInvocationOnce(
	ctx context.Context,
	invocation k12.ModelPhysicalInvocation,
) (k12.ModelPhysicalInvocation, bool, error) {
	tx, opErr := s.db.BeginTx(ctx, nil)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: begin layout physical prepare: %w",
			opErr,
		)
	}
	// 提交成功或主路径失败后，回滚仅用于释放事务；主路径错误保持原样。
	defer func() { _ = tx.Rollback() }()
	if err := ensureAgentRegistered(ctx, tx, invocation.AgentName); err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	parent, opErr := getModelInvocationByIDVia(
		ctx,
		tx,
		invocation.ParentInvocationID,
	)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, opErr
	}
	if err := validatePhysicalInvocationParent(invocation, parent); err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	if err := validateRecognitionLayoutBatchAuthorizationVia(
		ctx,
		tx,
		parent,
		invocation,
	); err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}

	existing, opErr := getModelPhysicalInvocationByUnitVia(
		ctx,
		tx,
		invocation.ParentInvocationID,
		invocation.PhysicalUnit,
	)
	if opErr == nil {
		if !sameModelPhysicalInvocationIdentity(existing, invocation) {
			return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
				"%w: parent=%s unit=%s",
				ErrModelPhysicalInvocationConflict,
				invocation.ParentInvocationID,
				invocation.PhysicalUnit,
			)
		}
		if err := tx.Commit(); err != nil {
			return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
				"k12storage: commit layout physical replay: %w",
				err,
			)
		}
		return existing, false, nil
	}
	if !errors.Is(opErr, records.ErrNotFound) {
		return k12.ModelPhysicalInvocation{}, false, opErr
	}
	if invocation.CreatedAt <= 0 {
		invocation.CreatedAt = nowUnix()
	}
	invocation.UpdatedAt = invocation.CreatedAt
	invocation.Status = k12.ModelInvocationPrepared
	routeJSON, opErr := json.Marshal(invocation.RouteSnapshot)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, opErr
	}
	policyJSON, opErr := json.Marshal(invocation.RequestPolicySnapshot)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, opErr
	}
	res, opErr := tx.ExecContext(
		ctx,
		`INSERT INTO k12_model_physical_invocations (`+
			modelPhysicalInvocationColumns+
			`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
             ON CONFLICT DO NOTHING`,
		invocation.PhysicalInvocationID,
		invocation.ParentInvocationID,
		invocation.AgentName,
		invocation.JobID,
		invocation.Stage,
		invocation.PhysicalUnit,
		invocation.RequestDigest,
		string(routeJSON),
		string(policyJSON),
		invocation.Status,
		invocation.Attempt,
		"",
		"",
		"",
		invocation.CreatedAt,
		invocation.UpdatedAt,
		"v2",
		invocation.PlanDigest,
		invocation.CandidateExactSetDigest,
	)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: prepare authorized layout physical invocation: %w",
			opErr,
		)
	}
	affected, _ := res.RowsAffected()
	stored, opErr := getModelPhysicalInvocationByUnitVia(
		ctx,
		tx,
		invocation.ParentInvocationID,
		invocation.PhysicalUnit,
	)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, opErr
	}
	if !sameModelPhysicalInvocationIdentity(stored, invocation) {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"%w: parent=%s unit=%s",
			ErrModelPhysicalInvocationConflict,
			invocation.ParentInvocationID,
			invocation.PhysicalUnit,
		)
	}
	if err := tx.Commit(); err != nil {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: commit layout physical prepare: %w",
			err,
		)
	}
	return stored, affected > 0, nil
}

func validateRecognitionLayoutBatchAuthorizationVia(
	ctx context.Context,
	q dbQueryer,
	parent k12.ModelInvocation,
	invocation k12.ModelPhysicalInvocation,
) error {
	if parent.Status != k12.ModelInvocationSent {
		return fmt.Errorf(
			"%w: layout child requires sent parent %s, got %s",
			records.ErrIllegalTransition,
			parent.InvocationID,
			parent.Status,
		)
	}
	isPrimary := strings.HasPrefix(string(invocation.PhysicalUnit), "layout_batch_")
	isRepair := strings.HasPrefix(string(invocation.PhysicalUnit), "layout_repair_")
	if invocation.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
		(!isPrimary && !isRepair) {
		return fmt.Errorf(
			"%w: only an authorized V2 layout batch or repair may use this gate",
			records.ErrIllegalTransition,
		)
	}
	var (
		planID, planDigest, status string
		stageDeadline              int64
	)
	queryErr := q.QueryRowContext(
		ctx,
		`SELECT plan_id,authorized_plan_digest,status,stage_deadline_at
           FROM k12_recognition_layout_plans
          WHERE parent_invocation_id=? AND agent_name=? AND job_id=?`,
		parent.InvocationID,
		invocation.AgentName,
		invocation.JobID,
	).Scan(&planID, &planDigest, &status, &stageDeadline)
	if errors.Is(queryErr, sql.ErrNoRows) {
		return fmt.Errorf(
			"%w: V2 layout plan is not durably authorized",
			records.ErrIllegalTransition,
		)
	}
	if queryErr != nil {
		return fmt.Errorf(
			"k12storage: read layout authorization: %w",
			queryErr,
		)
	}
	if (status != "authorized" && status != "running") ||
		planDigest == "" || planDigest != invocation.PlanDigest ||
		stageDeadline <= time.Now().UnixMilli() {
		return fmt.Errorf(
			"%w: layout plan authorization changed or its frozen deadline elapsed",
			ErrModelPhysicalInvocationConflict,
		)
	}
	if isRepair {
		return validateRecognitionLayoutRepairAuthorizationVia(
			ctx,
			q,
			parent,
			invocation,
			planID,
			planDigest,
		)
	}
	var batchID string
	queryErr = q.QueryRowContext(
		ctx,
		`SELECT batch_id
           FROM k12_recognition_layout_batches
          WHERE plan_id=? AND physical_unit=?`,
		planID,
		invocation.PhysicalUnit,
	).Scan(&batchID)
	if errors.Is(queryErr, sql.ErrNoRows) {
		return fmt.Errorf(
			"%w: physical unit %s is not in the authorized plan",
			records.ErrIllegalTransition,
			invocation.PhysicalUnit,
		)
	}
	if queryErr != nil {
		return fmt.Errorf("k12storage: read authorized layout batch: %w", queryErr)
	}
	rows, queryErr := q.QueryContext(
		ctx,
		`SELECT candidate_id
           FROM k12_recognition_layout_batch_members
          WHERE plan_id=? AND batch_id=?
          ORDER BY slot`,
		planID,
		batchID,
	)
	if queryErr != nil {
		return fmt.Errorf("k12storage: read authorized batch members: %w", queryErr)
	}
	// 遍历失败时保留主路径错误，延迟关闭仅用于释放游标。
	defer func() { _ = rows.Close() }()
	targetIDs := make([]string, 0, k12.RecognitionLayoutBatchTargetLimitV2)
	for rows.Next() {
		var targetID string
		if err := rows.Scan(&targetID); err != nil {
			return fmt.Errorf("k12storage: scan authorized batch member: %w", err)
		}
		targetIDs = append(targetIDs, targetID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("k12storage: list authorized batch members: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("k12storage: close authorized batch members: %w", err)
	}
	exactSetDigest, digestErr := k12.RecognitionLayoutTargetExactSetDigestV2(targetIDs)
	if digestErr != nil || exactSetDigest != invocation.CandidateExactSetDigest {
		return fmt.Errorf(
			"%w: layout batch candidate exact-set drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	return nil
}

func validateRecognitionLayoutRepairAuthorizationVia(
	ctx context.Context,
	q dbQueryer,
	parent k12.ModelInvocation,
	invocation k12.ModelPhysicalInvocation,
	planID string,
	planDigest string,
) error {
	return validateRecognitionLayoutRepairAuthorizationEvidenceVia(
		ctx,
		q,
		parent,
		invocation,
		planID,
		planDigest,
		true,
	)
}

func validateRecognitionLayoutRepairAuthorizationEvidenceVia(
	ctx context.Context,
	q dbQueryer,
	parent k12.ModelInvocation,
	invocation k12.ModelPhysicalInvocation,
	planID string,
	planDigest string,
	requireUnfrozenCandidate bool,
) error {
	var (
		authorizationID, candidateID, sourceBatchID string
		sourcePhysicalID, sourceResultDigest        string
		authorizationDigest                         string
		repairRound                                 int
	)
	opErr := q.QueryRowContext(
		ctx,
		`SELECT repair_authorization_id,candidate_id,source_batch_id,
                source_batch_physical_invocation_id,
                source_batch_result_digest,repair_round,authorization_digest
           FROM k12_recognition_layout_repair_authorizations
          WHERE plan_id=? AND repair_physical_unit=?`,
		planID,
		invocation.PhysicalUnit,
	).Scan(
		&authorizationID,
		&candidateID,
		&sourceBatchID,
		&sourcePhysicalID,
		&sourceResultDigest,
		&repairRound,
		&authorizationDigest,
	)
	if errors.Is(opErr, sql.ErrNoRows) {
		return fmt.Errorf(
			"%w: singleton repair lacks durable round-one authorization",
			records.ErrIllegalTransition,
		)
	}
	if opErr != nil {
		return fmt.Errorf("k12storage: read singleton repair authorization: %w", opErr)
	}
	var candidateOrdinal int
	if err := q.QueryRowContext(
		ctx,
		`SELECT ordinal FROM k12_recognition_layout_candidates
          WHERE plan_id=? AND candidate_id=?`,
		planID,
		candidateID,
	).Scan(&candidateOrdinal); err != nil {
		return fmt.Errorf("k12storage: read repair candidate ordinal: %w", err)
	}
	wantUnit, opErr := k12.RecognitionLayoutRepairUnitV2(candidateOrdinal)
	if opErr != nil || invocation.PhysicalUnit != wantUnit || repairRound != 1 {
		return fmt.Errorf(
			"%w: repair physical identity or round drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	exactSetDigest, opErr := k12.RecognitionLayoutTargetExactSetDigestV2(
		[]string{candidateID},
	)
	if opErr != nil || exactSetDigest != invocation.CandidateExactSetDigest {
		return fmt.Errorf(
			"%w: repair singleton candidate exact-set drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	if requireUnfrozenCandidate {
		var frozenCount int
		if err := q.QueryRowContext(
			ctx,
			`SELECT COUNT(*) FROM k12_recognition_layout_candidate_results
              WHERE plan_id=? AND candidate_id=?`,
			planID,
			candidateID,
		).Scan(&frozenCount); err != nil || frozenCount != 0 {
			return fmt.Errorf(
				"%w: already-frozen candidate cannot enter repair",
				ErrModelPhysicalInvocationConflict,
			)
		}
	}
	var sourceUnit k12.RecognitionPhysicalUnit
	if err := q.QueryRowContext(
		ctx,
		`SELECT physical_unit FROM k12_recognition_layout_batches
          WHERE plan_id=? AND batch_id=?`,
		planID,
		sourceBatchID,
	).Scan(&sourceUnit); err != nil {
		return fmt.Errorf("k12storage: read repair source batch: %w", err)
	}
	rows, opErr := q.QueryContext(
		ctx,
		`SELECT candidate_id FROM k12_recognition_layout_batch_members
          WHERE plan_id=? AND batch_id=? ORDER BY slot`,
		planID,
		sourceBatchID,
	)
	if opErr != nil {
		return fmt.Errorf("k12storage: list repair source members: %w", opErr)
	}
	sourceTargetIDs := make([]string, 0, k12.RecognitionLayoutBatchTargetLimitV2)
	candidateBelongs := false
	for rows.Next() {
		var targetID string
		if err := rows.Scan(&targetID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("k12storage: scan repair source member: %w", err)
		}
		sourceTargetIDs = append(sourceTargetIDs, targetID)
		candidateBelongs = candidateBelongs || targetID == candidateID
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("k12storage: close repair source members: %w", err)
	}
	sourceExactSetDigest, opErr := k12.RecognitionLayoutTargetExactSetDigestV2(
		sourceTargetIDs,
	)
	if opErr != nil || !candidateBelongs {
		return fmt.Errorf(
			"%w: repair candidate is detached from source batch membership",
			ErrModelPhysicalInvocationConflict,
		)
	}
	var (
		storedParent, storedAgent, storedJob, storedStage string
		storedUnit                                        k12.RecognitionPhysicalUnit
		storedStatus                                      k12.ModelInvocationStatus
		storedDigest, resultContent, planVersion          string
		storedPlanDigest, storedExactSetDigest            string
	)
	opErr = q.QueryRowContext(
		ctx,
		`SELECT parent_invocation_id,agent_name,job_id,stage,physical_unit,
                status,result_digest,result_content,recognition_plan_version,
                plan_digest,candidate_exact_set_digest
           FROM k12_model_physical_invocations
          WHERE physical_invocation_id=?`,
		sourcePhysicalID,
	).Scan(
		&storedParent,
		&storedAgent,
		&storedJob,
		&storedStage,
		&storedUnit,
		&storedStatus,
		&storedDigest,
		&resultContent,
		&planVersion,
		&storedPlanDigest,
		&storedExactSetDigest,
	)
	if opErr != nil || storedParent != parent.InvocationID ||
		storedAgent != parent.AgentName || storedJob != parent.JobID ||
		storedStage != parent.Stage || storedUnit != sourceUnit ||
		storedStatus != k12.ModelInvocationSucceeded ||
		storedDigest != sourceResultDigest ||
		storedDigest != physicalInvocationResultDigest(resultContent) ||
		planVersion != "v2" || storedPlanDigest != planDigest ||
		storedExactSetDigest != sourceExactSetDigest {
		return fmt.Errorf(
			"%w: repair authorization source evidence drifted: %v",
			ErrModelPhysicalInvocationConflict,
			opErr,
		)
	}
	var (
		settlementClass k12.RecognitionLayoutBatchClassificationV2
		ambiguityKind   k12.RecognitionLayoutBatchAmbiguityKindV2
		settlementHash  string
	)
	opErr = q.QueryRowContext(
		ctx,
		`SELECT classification,ambiguity_kind,settlement_digest
           FROM k12_recognition_layout_batch_settlements
          WHERE plan_id=? AND batch_id=?
            AND parent_invocation_id=?
            AND source_physical_invocation_id=?
            AND source_physical_unit=?
            AND source_physical_result_digest=?`,
		planID,
		sourceBatchID,
		parent.InvocationID,
		sourcePhysicalID,
		sourceUnit,
		sourceResultDigest,
	).Scan(&settlementClass, &ambiguityKind, &settlementHash)
	if opErr != nil || settlementClass != k12.RecognitionLayoutBatchClassifiedV2 ||
		ambiguityKind != "" || !validPrefixedSHA256DigestV2(settlementHash) {
		return fmt.Errorf(
			"%w: repair is detached from classified batch receipt: %v",
			ErrModelPhysicalInvocationConflict,
			opErr,
		)
	}
	baseSettlement := k12.RecognitionLayoutPrimaryBatchSettlementV2{
		PlanDigest:                 planDigest,
		SourcePhysicalInvocationID: sourcePhysicalID,
		SourcePhysicalUnit:         sourceUnit,
		SourcePhysicalResultDigest: sourceResultDigest,
		Classification:             k12.RecognitionLayoutBatchClassifiedV2,
	}
	digestMatches := false
	for _, classification := range []k12.RecognitionLayoutCandidateClassificationV2{
		k12.RecognitionLayoutCandidateMissingV2,
		k12.RecognitionLayoutCandidateInvalidV2,
	} {
		candidate := k12.RecognitionLayoutCandidateSettlementV2{
			CandidateID:    candidateID,
			Classification: classification,
		}
		wantDigest, digestErr := recognitionLayoutRepairAuthorizationDigestV2(
			baseSettlement,
			sourceBatchID,
			candidate,
			invocation.PhysicalUnit,
		)
		if digestErr == nil && wantDigest == authorizationDigest &&
			authorizationID == recognitionLayoutRepairAuthorizationIDV2(wantDigest) {
			digestMatches = true
			break
		}
	}
	if !digestMatches {
		return fmt.Errorf(
			"%w: repair authorization digest is not reproducible",
			ErrModelPhysicalInvocationConflict,
		)
	}
	return nil
}

func (s *Store) prepareFallbackModelPhysicalInvocation(
	ctx context.Context,
	invocation k12.ModelPhysicalInvocation,
) (k12.ModelPhysicalInvocation, bool, error) {
	var (
		stored  k12.ModelPhysicalInvocation
		created bool
	)
	err := sqliteutil.RetryOnBusy(ctx, func() error {
		var attemptErr error
		stored, created, attemptErr =
			s.prepareFallbackModelPhysicalInvocationOnce(ctx, invocation)
		return attemptErr
	})
	if err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	return stored, created, nil
}

// prepareFallbackModelPhysicalInvocationOnce 持有从私有内容校验到精确重放/插入的
// 完整 SQLite 快照。因此 BUSY_SNAPSHOT 重试不会复用并发写入提交前观察到的授权事实。
func (s *Store) prepareFallbackModelPhysicalInvocationOnce(
	ctx context.Context,
	invocation k12.ModelPhysicalInvocation,
) (k12.ModelPhysicalInvocation, bool, error) {
	tx, opErr := s.db.BeginTx(ctx, nil)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: begin fallback physical prepare: %w",
			opErr,
		)
	}
	// 提交成功或主路径失败后，回滚仅用于释放事务；主路径错误保持原样。
	defer func() { _ = tx.Rollback() }()

	if err := ensureAgentRegistered(ctx, tx, invocation.AgentName); err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	parent, opErr := getModelInvocationByIDVia(
		ctx,
		tx,
		invocation.ParentInvocationID,
	)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, opErr
	}
	if err := validatePhysicalInvocationParent(invocation, parent); err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	if err := validateRecognitionFallbackFactsVia(
		ctx,
		tx,
		parent,
		invocation,
	); err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}

	existing, opErr := getModelPhysicalInvocationByUnitVia(
		ctx,
		tx,
		invocation.ParentInvocationID,
		invocation.PhysicalUnit,
	)
	if opErr == nil {
		if !sameModelPhysicalInvocationIdentity(existing, invocation) {
			return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
				"%w: parent=%s unit=%s",
				ErrModelPhysicalInvocationConflict,
				invocation.ParentInvocationID,
				invocation.PhysicalUnit,
			)
		}
		if err := tx.Commit(); err != nil {
			return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
				"k12storage: commit fallback physical prepare replay: %w",
				err,
			)
		}
		return existing, false, nil
	}
	if !errors.Is(opErr, records.ErrNotFound) {
		return k12.ModelPhysicalInvocation{}, false, opErr
	}

	if invocation.CreatedAt <= 0 {
		invocation.CreatedAt = nowUnix()
	}
	invocation.UpdatedAt = invocation.CreatedAt
	invocation.Status = k12.ModelInvocationPrepared
	routeJSON, opErr := json.Marshal(invocation.RouteSnapshot)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, opErr
	}
	requestPolicyJSON := ""
	if !invocation.RequestPolicySnapshot.IsZero() {
		raw, marshalErr := json.Marshal(invocation.RequestPolicySnapshot)
		if marshalErr != nil {
			return k12.ModelPhysicalInvocation{}, false, marshalErr
		}
		requestPolicyJSON = string(raw)
	}
	planVersion, opErr := recognitionPlanVersionSQL(
		invocation.RecognitionPlanVersion,
	)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, opErr
	}
	fallbackGateSQL, fallbackGateArgs :=
		recognitionPhysicalFallbackGateSQL(
			"parent",
			invocation.PhysicalUnit,
		)
	insertArgs := []any{
		invocation.PhysicalInvocationID,
		invocation.ParentInvocationID,
		invocation.AgentName,
		invocation.JobID,
		invocation.Stage,
		invocation.PhysicalUnit,
		invocation.RequestDigest,
		string(routeJSON),
		requestPolicyJSON,
		invocation.Status,
		invocation.Attempt,
		"",
		"",
		"",
		invocation.CreatedAt,
		invocation.UpdatedAt,
		planVersion,
		invocation.PlanDigest,
		invocation.CandidateExactSetDigest,
		invocation.ParentInvocationID,
		k12.ModelInvocationSent,
	}
	insertArgs = append(insertArgs, fallbackGateArgs...)
	res, opErr := tx.ExecContext(
		ctx,
		`INSERT INTO k12_model_physical_invocations (`+
			modelPhysicalInvocationColumns+
			`) SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?
             FROM k12_model_invocations AS parent
             WHERE invocation_id=? AND status=?
               AND (`+fallbackGateSQL+`)
             ON CONFLICT DO NOTHING`,
		insertArgs...,
	)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: prepare fallback model physical invocation: %w",
			opErr,
		)
	}
	affected, _ := res.RowsAffected()
	stored, opErr := getModelPhysicalInvocationByUnitVia(
		ctx,
		tx,
		invocation.ParentInvocationID,
		invocation.PhysicalUnit,
	)
	if opErr != nil {
		if affected == 0 && errors.Is(opErr, records.ErrNotFound) {
			return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
				"%w: fallback unit %s lost its authorization gate",
				records.ErrIllegalTransition,
				invocation.PhysicalUnit,
			)
		}
		return k12.ModelPhysicalInvocation{}, false, opErr
	}
	if !sameModelPhysicalInvocationIdentity(stored, invocation) {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"%w: parent=%s unit=%s",
			ErrModelPhysicalInvocationConflict,
			invocation.ParentInvocationID,
			invocation.PhysicalUnit,
		)
	}
	if err := tx.Commit(); err != nil {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: commit fallback physical prepare: %w",
			err,
		)
	}
	return stored, affected > 0, nil
}

func (s *Store) GetModelPhysicalInvocation(
	ctx context.Context,
	agentName string,
	physicalInvocationID string,
) (k12.ModelPhysicalInvocation, error) {
	return getModelPhysicalInvocationByIDVia(
		ctx,
		s.db,
		agentName,
		physicalInvocationID,
	)
}

func (s *Store) ListModelPhysicalInvocations(
	ctx context.Context,
	agentName string,
	jobID string,
) ([]k12.ModelPhysicalInvocation, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+modelPhysicalInvocationColumns+`
         FROM k12_model_physical_invocations
         WHERE agent_name=? AND job_id=?
         ORDER BY parent_invocation_id,
             CASE physical_unit
                 WHEN 'whole_page' THEN 0
                 WHEN 'segment_1' THEN 1
                 WHEN 'segment_2' THEN 2
                 WHEN 'segment_3' THEN 3
                 WHEN 'segment_4' THEN 4
                 WHEN 'segment_5' THEN 5
                 WHEN 'printed_inventory' THEN 6
                 ELSE 7
             END`,
		strings.TrimSpace(agentName),
		strings.TrimSpace(jobID),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"k12storage: list model physical invocations: %w",
			err,
		)
	}
	// 遍历失败时保留主路径错误，延迟关闭仅用于释放游标。
	defer func() { _ = rows.Close() }()
	out := make([]k12.ModelPhysicalInvocation, 0)
	for rows.Next() {
		invocation, scanErr := scanModelPhysicalInvocation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, invocation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"k12storage: list model physical invocations: %w",
			err,
		)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf(
			"k12storage: close model physical invocations: %w",
			err,
		)
	}
	return out, nil
}

func (s *Store) transitionModelPhysicalInvocation(
	ctx context.Context,
	agentName string,
	physicalInvocationID string,
	from []k12.ModelInvocationStatus,
	to k12.ModelInvocationStatus,
	resultDigest string,
	externalRequestID string,
	failureKind string,
) (k12.ModelPhysicalInvocation, error) {
	agentName = strings.TrimSpace(agentName)
	physicalInvocationID = strings.TrimSpace(physicalInvocationID)
	resultDigest = strings.TrimSpace(resultDigest)
	externalRequestID = strings.TrimSpace(externalRequestID)
	failureKind = strings.TrimSpace(failureKind)
	placeholders := make([]string, len(from))
	for index := range from {
		placeholders[index] = "?"
	}
	args := []any{
		to,
		resultDigest,
		externalRequestID,
		failureKind,
		nowUnix(),
		physicalInvocationID,
		agentName,
	}
	args = append(args, statusArgs(from)...)
	res, err := s.db.ExecContext(
		ctx,
		`UPDATE k12_model_physical_invocations SET
             status=?,result_digest=?,external_request_id=?,failure_kind=?,updated_at=?
         WHERE physical_invocation_id=? AND agent_name=? AND status IN (`+
			strings.Join(placeholders, ",")+
			`)`,
		args...,
	)
	if err != nil {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"k12storage: transition model physical invocation to %s: %w",
			to,
			err,
		)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		current, getErr := s.GetModelPhysicalInvocation(
			ctx,
			agentName,
			physicalInvocationID,
		)
		if getErr != nil {
			return k12.ModelPhysicalInvocation{}, getErr
		}
		if current.Status != to {
			return k12.ModelPhysicalInvocation{}, fmt.Errorf(
				"%w: physical invocation %s status %s -> %s",
				records.ErrIllegalTransition,
				physicalInvocationID,
				current.Status,
				to,
			)
		}
		if current.ResultDigest != resultDigest ||
			current.ExternalRequestID != externalRequestID ||
			current.FailureKind != failureKind {
			return k12.ModelPhysicalInvocation{}, fmt.Errorf(
				"%w: physical invocation %s terminal facts changed",
				ErrModelPhysicalInvocationConflict,
				physicalInvocationID,
			)
		}
		return current, nil
	}
	return s.GetModelPhysicalInvocation(ctx, agentName, physicalInvocationID)
}

func (s *Store) MarkModelPhysicalInvocationSent(
	ctx context.Context,
	agentName string,
	physicalInvocationID string,
) (k12.ModelPhysicalInvocation, error) {
	invocation, _, err := s.ClaimModelPhysicalInvocationSent(
		ctx,
		agentName,
		physicalInvocationID,
	)
	return invocation, err
}

// ClaimModelPhysicalInvocationSent 是 Provider POST 前的单赢家 CAS。
// 只有 claimed=true 才授权调用方发送请求。
func (s *Store) ClaimModelPhysicalInvocationSent(
	ctx context.Context,
	agentName string,
	physicalInvocationID string,
) (k12.ModelPhysicalInvocation, bool, error) {
	agentName = strings.TrimSpace(agentName)
	physicalInvocationID = strings.TrimSpace(physicalInvocationID)
	var (
		invocation k12.ModelPhysicalInvocation
		claimed    bool
	)
	err := sqliteutil.RetryOnBusy(ctx, func() error {
		var attemptErr error
		invocation, claimed, attemptErr =
			s.claimModelPhysicalInvocationSentOnce(
				ctx,
				agentName,
				physicalInvocationID,
			)
		return attemptErr
	})
	if err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	return invocation, claimed, nil
}

// claimModelPhysicalInvocationSentOnce 将 fallback 授权、私有内容校验、前驱校验和
// prepared→sent CAS 保持在同一个 SQLite 快照中。WAL 快照失效后，RetryOnBusy
// 会重新执行整个事务。
func (s *Store) claimModelPhysicalInvocationSentOnce(
	ctx context.Context,
	agentName string,
	physicalInvocationID string,
) (k12.ModelPhysicalInvocation, bool, error) {
	tx, opErr := s.db.BeginTx(ctx, nil)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: begin physical invocation send claim: %w",
			opErr,
		)
	}
	// 提交成功或主路径失败后，回滚仅用于释放事务；主路径错误保持原样。
	defer func() { _ = tx.Rollback() }()

	before, opErr := getModelPhysicalInvocationByIDVia(
		ctx,
		tx,
		agentName,
		physicalInvocationID,
	)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, opErr
	}
	if before.RecognitionPlanVersion == k12.RecognitionPlanVersionV2 &&
		before.PhysicalUnit != k12.RecognitionPhysicalUnitWholePage {
		parent, parentErr := getModelInvocationByIDVia(
			ctx,
			tx,
			before.ParentInvocationID,
		)
		if parentErr != nil {
			return k12.ModelPhysicalInvocation{}, false, parentErr
		}
		if parentErr = validatePhysicalInvocationParent(
			before,
			parent,
		); parentErr != nil {
			return k12.ModelPhysicalInvocation{}, false, parentErr
		}
		if parentErr = validateRecognitionLayoutBatchAuthorizationVia(
			ctx,
			tx,
			parent,
			before,
		); parentErr != nil {
			return k12.ModelPhysicalInvocation{}, false, parentErr
		}
	} else if before.PhysicalUnit != k12.RecognitionPhysicalUnitWholePage {
		parent, parentErr := getModelInvocationByIDVia(
			ctx,
			tx,
			before.ParentInvocationID,
		)
		if parentErr != nil {
			return k12.ModelPhysicalInvocation{}, false, parentErr
		}
		if parentErr = validatePhysicalInvocationParent(
			before,
			parent,
		); parentErr != nil {
			return k12.ModelPhysicalInvocation{}, false, parentErr
		}
		if parentErr = validateRecognitionFallbackFactsVia(
			ctx,
			tx,
			parent,
			before,
		); parentErr != nil {
			return k12.ModelPhysicalInvocation{}, false, parentErr
		}
	}
	fallbackGateSQL := "1=1"
	var fallbackGateArgs []any
	if before.RecognitionPlanVersion == k12.RecognitionPlanVersionV1 {
		fallbackGateSQL, fallbackGateArgs =
			recognitionPhysicalFallbackGateSQL(
				"parent",
				before.PhysicalUnit,
			)
	}
	claimArgs := []any{
		k12.ModelInvocationSent,
		nowUnix(),
		physicalInvocationID,
		agentName,
		k12.ModelInvocationPrepared,
		k12.ModelInvocationSent,
	}
	claimArgs = append(claimArgs, fallbackGateArgs...)
	invocation, opErr := scanModelPhysicalInvocation(tx.QueryRowContext(
		ctx,
		`UPDATE k12_model_physical_invocations
         SET status=?,updated_at=?
         WHERE physical_invocation_id=? AND agent_name=? AND status=?
           AND EXISTS (
             SELECT 1
             FROM k12_model_invocations AS parent
             WHERE parent.invocation_id =
                       k12_model_physical_invocations.parent_invocation_id
               AND parent.agent_name =
                       k12_model_physical_invocations.agent_name
               AND parent.job_id =
                       k12_model_physical_invocations.job_id
               AND parent.stage =
                       k12_model_physical_invocations.stage
               AND parent.status=?
               AND (`+fallbackGateSQL+`)
           )
         RETURNING `+modelPhysicalInvocationColumns,
		claimArgs...,
	))
	if opErr == nil {
		if invocation.RecognitionPlanVersion == k12.RecognitionPlanVersionV2 {
			if err := advanceRecognitionLayoutPlanAfterClaim(
				ctx,
				tx,
				invocation,
			); err != nil {
				return k12.ModelPhysicalInvocation{}, false, err
			}
		}
		if err := tx.Commit(); err != nil {
			return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
				"k12storage: commit physical invocation send claim: %w",
				err,
			)
		}
		return invocation, true, nil
	}
	if !errors.Is(opErr, sql.ErrNoRows) {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: claim model physical invocation sent: %w",
			opErr,
		)
	}
	current, opErr := getModelPhysicalInvocationByIDVia(
		ctx,
		tx,
		agentName,
		physicalInvocationID,
	)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, false, opErr
	}
	if current.Status != k12.ModelInvocationSent {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"%w: physical invocation %s status %s -> %s",
			records.ErrIllegalTransition,
			physicalInvocationID,
			current.Status,
			k12.ModelInvocationSent,
		)
	}
	if current.RecognitionPlanVersion == k12.RecognitionPlanVersionV2 {
		if err := validateRecognitionLayoutPlanClaimReplayVia(
			ctx,
			tx,
			current,
		); err != nil {
			return k12.ModelPhysicalInvocation{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: commit physical invocation send claim replay: %w",
			err,
		)
	}
	return current, false, nil
}

func advanceRecognitionLayoutPlanAfterClaim(
	ctx context.Context,
	tx *sql.Tx,
	invocation k12.ModelPhysicalInvocation,
) error {
	if invocation.PhysicalUnit == k12.RecognitionPhysicalUnitWholePage {
		res, err := tx.ExecContext(
			ctx,
			`UPDATE k12_recognition_layout_plans
                SET status='manifest_sent',updated_at=?
              WHERE parent_invocation_id=? AND agent_name=?
                AND manifest_physical_invocation_id=?
                AND header_digest=? AND status='prepared_manifest'`,
			nowUnix(),
			invocation.ParentInvocationID,
			invocation.AgentName,
			invocation.PhysicalInvocationID,
			invocation.PlanDigest,
		)
		if err != nil {
			return fmt.Errorf("k12storage: mark layout manifest sent: %w", err)
		}
		if affected, _ := res.RowsAffected(); affected != 1 {
			return fmt.Errorf(
				"%w: V2 manifest has no prepared plan header",
				records.ErrIllegalTransition,
			)
		}
		return nil
	}
	res, err := tx.ExecContext(
		ctx,
		`UPDATE k12_recognition_layout_plans
            SET status='running',updated_at=?
          WHERE parent_invocation_id=? AND agent_name=?
            AND authorized_plan_digest=? AND status='authorized'`,
		nowUnix(),
		invocation.ParentInvocationID,
		invocation.AgentName,
		invocation.PlanDigest,
	)
	if err != nil {
		return fmt.Errorf("k12storage: mark authorized layout plan running: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 1 {
		return nil
	}
	var status string
	err = tx.QueryRowContext(
		ctx,
		`SELECT status FROM k12_recognition_layout_plans
          WHERE parent_invocation_id=? AND agent_name=?
            AND authorized_plan_digest=?`,
		invocation.ParentInvocationID,
		invocation.AgentName,
		invocation.PlanDigest,
	).Scan(&status)
	if err != nil || status != "running" {
		return fmt.Errorf(
			"%w: layout plan cannot enter running state",
			records.ErrIllegalTransition,
		)
	}
	return nil
}

func validateRecognitionLayoutPlanClaimReplayVia(
	ctx context.Context,
	q dbQueryer,
	invocation k12.ModelPhysicalInvocation,
) error {
	var wantStatuses map[string]bool
	wantDigestColumn := "authorized_plan_digest"
	if invocation.PhysicalUnit == k12.RecognitionPhysicalUnitWholePage {
		wantStatuses = map[string]bool{
			"manifest_sent":      true,
			"manifest_succeeded": true,
			"authorized":         true,
			"running":            true,
		}
		wantDigestColumn = "header_digest"
	} else {
		wantStatuses = map[string]bool{"running": true}
	}
	var status, digest string
	err := q.QueryRowContext(
		ctx,
		`SELECT status,`+wantDigestColumn+`
           FROM k12_recognition_layout_plans
          WHERE parent_invocation_id=? AND agent_name=?`,
		invocation.ParentInvocationID,
		invocation.AgentName,
	).Scan(&status, &digest)
	if err != nil || !wantStatuses[status] || digest != invocation.PlanDigest {
		return fmt.Errorf(
			"%w: V2 physical sent replay is detached from plan state",
			ErrModelPhysicalInvocationConflict,
		)
	}
	return nil
}

// MarkModelPhysicalInvocationSucceededWithContent 是唯一的物理调用成功转换。
// Store 而非调用方根据精确的 Provider 内容计算摘要，并私有保留该内容以支持重启安全的
// 对账。ModelPhysicalInvocation 及其 JSON 表示仅暴露摘要。
func (s *Store) MarkModelPhysicalInvocationSucceededWithContent(
	ctx context.Context,
	agentName string,
	physicalInvocationID string,
	resultContent string,
	externalRequestID string,
) (k12.ModelPhysicalInvocation, error) {
	agentName = strings.TrimSpace(agentName)
	physicalInvocationID = strings.TrimSpace(physicalInvocationID)
	externalRequestID = strings.TrimSpace(externalRequestID)
	resultDigest := physicalInvocationResultDigest(resultContent)
	before, opErr := s.GetModelPhysicalInvocation(
		ctx,
		agentName,
		physicalInvocationID,
	)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, opErr
	}
	if before.RecognitionPlanVersion == k12.RecognitionPlanVersionV2 &&
		before.PhysicalUnit == k12.RecognitionPhysicalUnitWholePage {
		var stored k12.ModelPhysicalInvocation
		retryErr := sqliteutil.RetryOnBusy(ctx, func() error {
			var attemptErr error
			stored, attemptErr = s.markRecognitionLayoutManifestSucceededOnce(
				ctx,
				agentName,
				physicalInvocationID,
				resultContent,
				resultDigest,
				externalRequestID,
			)
			return attemptErr
		})
		if retryErr != nil {
			return k12.ModelPhysicalInvocation{}, retryErr
		}
		return stored, nil
	}
	res, opErr := s.db.ExecContext(
		ctx,
		`UPDATE k12_model_physical_invocations
         SET status=?,result_digest=?,result_content=?,
             external_request_id=?,failure_kind='',updated_at=?
         WHERE physical_invocation_id=? AND agent_name=? AND status=?`,
		k12.ModelInvocationSucceeded,
		resultDigest,
		resultContent,
		externalRequestID,
		nowUnix(),
		physicalInvocationID,
		agentName,
		k12.ModelInvocationSent,
	)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"k12storage: mark physical invocation succeeded with content: %w",
			opErr,
		)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		current, getErr := s.GetModelPhysicalInvocation(
			ctx,
			agentName,
			physicalInvocationID,
		)
		if getErr != nil {
			return k12.ModelPhysicalInvocation{}, getErr
		}
		if current.Status != k12.ModelInvocationSucceeded {
			return k12.ModelPhysicalInvocation{}, fmt.Errorf(
				"%w: physical invocation %s status %s -> %s",
				records.ErrIllegalTransition,
				physicalInvocationID,
				current.Status,
				k12.ModelInvocationSucceeded,
			)
		}
		var storedContent sql.NullString
		if queryErr := s.db.QueryRowContext(
			ctx,
			`SELECT result_content
             FROM k12_model_physical_invocations
             WHERE physical_invocation_id=? AND agent_name=?`,
			physicalInvocationID,
			agentName,
		).Scan(&storedContent); queryErr != nil {
			return k12.ModelPhysicalInvocation{}, fmt.Errorf(
				"k12storage: read private physical result content: %w",
				queryErr,
			)
		}
		if current.ResultDigest != resultDigest ||
			current.ExternalRequestID != externalRequestID ||
			current.FailureKind != "" ||
			!storedContent.Valid ||
			storedContent.String != resultContent {
			return k12.ModelPhysicalInvocation{}, fmt.Errorf(
				"%w: physical invocation %s terminal facts changed",
				ErrModelPhysicalInvocationConflict,
				physicalInvocationID,
			)
		}
		return current, nil
	}
	return s.GetModelPhysicalInvocation(ctx, agentName, physicalInvocationID)
}

func (s *Store) markRecognitionLayoutManifestSucceededOnce(
	ctx context.Context,
	agentName string,
	physicalInvocationID string,
	resultContent string,
	resultDigest string,
	externalRequestID string,
) (k12.ModelPhysicalInvocation, error) {
	tx, opErr := s.db.BeginTx(ctx, nil)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"k12storage: begin V2 manifest success: %w",
			opErr,
		)
	}
	// 提交成功或主路径失败后，回滚仅用于释放事务；主路径错误保持原样。
	defer func() { _ = tx.Rollback() }()
	before, opErr := getModelPhysicalInvocationByIDVia(
		ctx,
		tx,
		agentName,
		physicalInvocationID,
	)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, opErr
	}
	if before.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
		before.PhysicalUnit != k12.RecognitionPhysicalUnitWholePage {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"%w: physical invocation is not a V2 manifest",
			ErrModelPhysicalInvocationConflict,
		)
	}
	if before.Status == k12.ModelInvocationSent {
		res, updateErr := tx.ExecContext(
			ctx,
			`UPDATE k12_model_physical_invocations
                SET status=?,result_digest=?,result_content=?,
                    external_request_id=?,failure_kind='',updated_at=?
              WHERE physical_invocation_id=? AND agent_name=? AND status=?`,
			k12.ModelInvocationSucceeded,
			resultDigest,
			resultContent,
			externalRequestID,
			nowUnix(),
			physicalInvocationID,
			agentName,
			k12.ModelInvocationSent,
		)
		if updateErr != nil {
			return k12.ModelPhysicalInvocation{}, fmt.Errorf(
				"k12storage: persist V2 manifest content: %w",
				updateErr,
			)
		}
		if affected, _ := res.RowsAffected(); affected != 1 {
			return k12.ModelPhysicalInvocation{}, fmt.Errorf(
				"%w: V2 manifest lost sent state",
				records.ErrIllegalTransition,
			)
		}
	} else if before.Status != k12.ModelInvocationSucceeded {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"%w: V2 manifest status %s -> succeeded",
			records.ErrIllegalTransition,
			before.Status,
		)
	}
	stored, opErr := getModelPhysicalInvocationByIDVia(
		ctx,
		tx,
		agentName,
		physicalInvocationID,
	)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, opErr
	}
	var storedContent sql.NullString
	if err := tx.QueryRowContext(
		ctx,
		`SELECT result_content FROM k12_model_physical_invocations
          WHERE physical_invocation_id=? AND agent_name=?`,
		physicalInvocationID,
		agentName,
	).Scan(&storedContent); err != nil {
		return k12.ModelPhysicalInvocation{}, err
	}
	if stored.ResultDigest != resultDigest ||
		stored.ExternalRequestID != externalRequestID ||
		!storedContent.Valid || storedContent.String != resultContent {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"%w: V2 manifest terminal facts changed",
			ErrModelPhysicalInvocationConflict,
		)
	}
	res, opErr := tx.ExecContext(
		ctx,
		`UPDATE k12_recognition_layout_plans
            SET manifest_result_digest=?,status='manifest_succeeded',updated_at=?
          WHERE parent_invocation_id=? AND agent_name=?
            AND manifest_physical_invocation_id=? AND header_digest=?
            AND status='manifest_sent'`,
		resultDigest,
		nowUnix(),
		stored.ParentInvocationID,
		agentName,
		physicalInvocationID,
		stored.PlanDigest,
	)
	if opErr != nil {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"k12storage: bind succeeded manifest to layout header: %w",
			opErr,
		)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		var status, manifestDigest string
		err := tx.QueryRowContext(
			ctx,
			`SELECT status,manifest_result_digest
               FROM k12_recognition_layout_plans
              WHERE parent_invocation_id=? AND agent_name=?
                AND manifest_physical_invocation_id=? AND header_digest=?`,
			stored.ParentInvocationID,
			agentName,
			physicalInvocationID,
			stored.PlanDigest,
		).Scan(&status, &manifestDigest)
		if err != nil || manifestDigest != resultDigest ||
			(status != "manifest_succeeded" && status != "authorized" &&
				status != "running") {
			return k12.ModelPhysicalInvocation{}, fmt.Errorf(
				"%w: V2 manifest success is detached from plan header",
				ErrModelPhysicalInvocationConflict,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"k12storage: commit V2 manifest success: %w",
			err,
		)
	}
	return stored, nil
}

// ValidateModelPhysicalInvocationResultContent 在不暴露私有内容的前提下，证明成功物理回执中
// 存储的摘要是其精确持久化 Provider 响应的普通 SHA-256。
func (s *Store) ValidateModelPhysicalInvocationResultContent(
	ctx context.Context,
	agentName string,
	physicalInvocationID string,
) error {
	agentName = strings.TrimSpace(agentName)
	physicalInvocationID = strings.TrimSpace(physicalInvocationID)
	var status k12.ModelInvocationStatus
	var resultDigest string
	var resultContent sql.NullString
	opErr := s.db.QueryRowContext(
		ctx,
		`SELECT status,result_digest,result_content
         FROM k12_model_physical_invocations
         WHERE physical_invocation_id=? AND agent_name=?`,
		physicalInvocationID,
		agentName,
	).Scan(&status, &resultDigest, &resultContent)
	if errors.Is(opErr, sql.ErrNoRows) {
		return records.ErrNotFound
	}
	if opErr != nil {
		return fmt.Errorf(
			"k12storage: validate private physical result content: %w",
			opErr,
		)
	}
	if status != k12.ModelInvocationSucceeded ||
		!resultContent.Valid ||
		resultDigest != physicalInvocationResultDigest(resultContent.String) {
		return fmt.Errorf(
			"%w: physical invocation %s result content does not bind its digest",
			ErrModelPhysicalInvocationConflict,
			physicalInvocationID,
		)
	}
	return nil
}

// LoadSucceededModelPhysicalInvocationResultContent 是仅供重启恢复使用的执行器接缝，
// 可在不发起另一次物理请求的情况下重放精确 Provider 内容。该内容始终限制在
// storage/usecase 边界内，仅在 owner、终态、调用方期望摘要和 Store 计算摘要全部一致时返回。
func (s *Store) LoadSucceededModelPhysicalInvocationResultContent(
	ctx context.Context,
	agentName string,
	physicalInvocationID string,
	expectedResultDigest string,
) (string, error) {
	agentName = strings.TrimSpace(agentName)
	physicalInvocationID = strings.TrimSpace(physicalInvocationID)
	expectedResultDigest = strings.TrimSpace(expectedResultDigest)
	if agentName == "" || physicalInvocationID == "" || expectedResultDigest == "" {
		return "", fmt.Errorf(
			"k12storage: private physical replay requires owner/id/digest",
		)
	}
	var (
		status        k12.ModelInvocationStatus
		storedDigest  string
		storedContent sql.NullString
	)
	opErr := s.db.QueryRowContext(
		ctx,
		`SELECT status,result_digest,result_content
           FROM k12_model_physical_invocations
          WHERE physical_invocation_id=? AND agent_name=?`,
		physicalInvocationID,
		agentName,
	).Scan(&status, &storedDigest, &storedContent)
	if errors.Is(opErr, sql.ErrNoRows) {
		return "", records.ErrNotFound
	}
	if opErr != nil {
		return "", fmt.Errorf(
			"k12storage: load private physical result content: %w",
			opErr,
		)
	}
	if status != k12.ModelInvocationSucceeded ||
		!storedContent.Valid || storedDigest != expectedResultDigest ||
		storedDigest != physicalInvocationResultDigest(storedContent.String) {
		return "", fmt.Errorf(
			"%w: physical invocation %s private replay facts drifted",
			ErrModelPhysicalInvocationConflict,
			physicalInvocationID,
		)
	}
	return storedContent.String, nil
}

// LoadRecognitionLayoutPlanRuntimeV2 恢复编排与故障恢复所需的非敏感 V2 控制面快照。
// 返回任何投影前都会校验规范计划头和已授权计划摘要。
func (s *Store) LoadRecognitionLayoutPlanRuntimeV2(
	ctx context.Context,
	agentName string,
	parentInvocationID string,
) (k12.RecognitionLayoutPlanRuntimeV2, error) {
	agentName = strings.TrimSpace(agentName)
	parentInvocationID = strings.TrimSpace(parentInvocationID)
	if agentName == "" || parentInvocationID == "" {
		return k12.RecognitionLayoutPlanRuntimeV2{}, fmt.Errorf(
			"k12storage: load layout runtime requires owner and parent",
		)
	}
	var (
		storedParentID, storedAgent, headerJSON, headerDigest string
		manifestID, manifestResultDigest, exactSetDigest      string
		authorizedPlanDigest, authorizedPlanJSON, status      string
		stageStartedAt, stageDeadlineAt                       int64
		selectedBucket                                        int
	)
	opErr := s.db.QueryRowContext(
		ctx,
		`SELECT parent_invocation_id,agent_name,layout_header_json,
                header_digest,manifest_physical_invocation_id,
                manifest_result_digest,candidate_exact_set_digest,
                authorized_plan_digest,authorized_plan_json,status,
                stage_started_at,stage_deadline_at,
                selected_bucket_max_problems
           FROM k12_recognition_layout_plans
          WHERE parent_invocation_id=? AND agent_name=?`,
		parentInvocationID,
		agentName,
	).Scan(
		&storedParentID,
		&storedAgent,
		&headerJSON,
		&headerDigest,
		&manifestID,
		&manifestResultDigest,
		&exactSetDigest,
		&authorizedPlanDigest,
		&authorizedPlanJSON,
		&status,
		&stageStartedAt,
		&stageDeadlineAt,
		&selectedBucket,
	)
	if errors.Is(opErr, sql.ErrNoRows) {
		return k12.RecognitionLayoutPlanRuntimeV2{}, records.ErrNotFound
	}
	if opErr != nil {
		return k12.RecognitionLayoutPlanRuntimeV2{}, fmt.Errorf(
			"k12storage: load layout runtime: %w",
			opErr,
		)
	}
	var canonicalHeader struct {
		Contract string `json:"contract"`
		k12.RecognitionLayoutPlanHeaderV2
	}
	if err := json.Unmarshal([]byte(headerJSON), &canonicalHeader); err != nil {
		return k12.RecognitionLayoutPlanRuntimeV2{}, fmt.Errorf(
			"k12storage: parse layout runtime header: %w",
			err,
		)
	}
	reencodedHeader, recomputedHeaderDigest, opErr :=
		k12.CanonicalRecognitionLayoutPlanHeaderV2(
			canonicalHeader.RecognitionLayoutPlanHeaderV2,
		)
	if opErr != nil || canonicalHeader.Contract != "recognition_layout_plan_header_v2" ||
		string(reencodedHeader) != headerJSON ||
		recomputedHeaderDigest != headerDigest ||
		storedParentID != parentInvocationID || storedAgent != agentName ||
		canonicalHeader.ParentInvocationID != parentInvocationID ||
		canonicalHeader.AgentName != agentName ||
		canonicalHeader.StageStartedAtUnixMillis != stageStartedAt {
		return k12.RecognitionLayoutPlanRuntimeV2{}, fmt.Errorf(
			"%w: persisted layout runtime header drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	runtime := k12.RecognitionLayoutPlanRuntimeV2{
		Header:                       canonicalHeader.RecognitionLayoutPlanHeaderV2,
		HeaderDigest:                 headerDigest,
		ManifestPhysicalInvocationID: manifestID,
		ManifestResultDigest:         manifestResultDigest,
		CandidateExactSetDigest:      exactSetDigest,
		SelectedBucketMaxProblems:    selectedBucket,
		StageDeadlineAtUnixMillis:    stageDeadlineAt,
		Status:                       status,
	}
	if authorizedPlanJSON == "" {
		if authorizedPlanDigest != "" || exactSetDigest != "" ||
			selectedBucket != 0 || stageDeadlineAt != 0 {
			return k12.RecognitionLayoutPlanRuntimeV2{}, fmt.Errorf(
				"%w: unapproved layout runtime carries authorization facts",
				ErrModelPhysicalInvocationConflict,
			)
		}
		return runtime, nil
	}
	var plan k12.RecognitionLayoutPlanV2
	if err := json.Unmarshal([]byte(authorizedPlanJSON), &plan); err != nil {
		return k12.RecognitionLayoutPlanRuntimeV2{}, fmt.Errorf(
			"k12storage: parse authorized layout runtime plan: %w",
			err,
		)
	}
	reencodedPlan, opErr := json.Marshal(plan)
	if opErr != nil || string(reencodedPlan) != authorizedPlanJSON ||
		k12.ValidateRecognitionLayoutPlanV2(plan) != nil ||
		plan.AuthorizedPlanDigest != authorizedPlanDigest ||
		plan.PageDigest != runtime.Header.PageDigest ||
		plan.ManifestInvocationID != manifestID ||
		plan.ManifestResultDigest != manifestResultDigest {
		return k12.RecognitionLayoutPlanRuntimeV2{}, fmt.Errorf(
			"%w: persisted authorized layout runtime drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	targetIDs := make([]string, len(plan.Targets))
	for index := range plan.Targets {
		targetIDs[index] = plan.Targets[index].TargetID
	}
	wantExactSetDigest, opErr :=
		k12.RecognitionLayoutTargetExactSetDigestV2(targetIDs)
	if opErr != nil || wantExactSetDigest != exactSetDigest {
		return k12.RecognitionLayoutPlanRuntimeV2{}, fmt.Errorf(
			"%w: persisted authorized target exact-set drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	wantBucket, durationMillis, opErr := runtime.Header.BudgetBuckets.Select(
		len(plan.Targets),
	)
	wantDeadline := runtime.Header.StageStartedAtUnixMillis + durationMillis
	if opErr != nil || wantBucket != selectedBucket ||
		wantDeadline != stageDeadlineAt {
		return k12.RecognitionLayoutPlanRuntimeV2{}, fmt.Errorf(
			"%w: persisted selected layout budget drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	runtime.AuthorizedPlan = &plan
	return runtime, nil
}

// AuthorizeRecognitionLayoutPlanV2 将本地校验后的计划绑定到精确成功的 compact manifest。
// candidates、primary batches、有序成员和计划头授权更新在同一事务中提交。
// 精确重放只读且幂等。
func (s *Store) AuthorizeRecognitionLayoutPlanV2(
	ctx context.Context,
	agentName string,
	parentInvocationID string,
	manifest k12.RecognitionLayoutManifestSuccessV2,
	plan k12.RecognitionLayoutPlanV2,
) error {
	agentName = strings.TrimSpace(agentName)
	parentInvocationID = strings.TrimSpace(parentInvocationID)
	if agentName == "" || parentInvocationID == "" {
		return fmt.Errorf(
			"k12storage: layout authorization missing owner or parent",
		)
	}
	if err := k12.ValidateRecognitionLayoutPlanV2(plan); err != nil {
		return err
	}
	if manifest.InvocationID != plan.ManifestInvocationID ||
		manifest.ResultDigest != plan.ManifestResultDigest {
		return fmt.Errorf(
			"%w: caller manifest identity differs from authorized plan",
			ErrModelPhysicalInvocationConflict,
		)
	}
	return sqliteutil.RetryOnBusy(ctx, func() error {
		return s.authorizeRecognitionLayoutPlanV2Once(
			ctx,
			agentName,
			parentInvocationID,
			manifest,
			plan,
		)
	})
}

type recognitionLayoutPrimaryBatchAuthorityV2 struct {
	PlanID         string
	BatchID        string
	MemberIDs      []string
	MemberOrdinals []int
}

// SettleRecognitionLayoutPrimaryBatchV2 冻结一个精确成功 primary batch 的有效子集，
// 并为 missing/invalid 补集授权唯一一轮修复。摘要由 Store 根据规范调用方事实和
// 私有持久化 Provider 结果推导。
func (s *Store) SettleRecognitionLayoutPrimaryBatchV2(
	ctx context.Context,
	agentName string,
	parentInvocationID string,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
) (k12.RecognitionLayoutPrimaryBatchSettlementResultV2, bool, error) {
	agentName = strings.TrimSpace(agentName)
	parentInvocationID = strings.TrimSpace(parentInvocationID)
	if agentName == "" || parentInvocationID == "" {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false,
			fmt.Errorf("k12storage: batch settlement requires owner and parent")
	}
	if err := validateRecognitionLayoutPrimaryBatchSettlementV2(settlement); err != nil {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false, err
	}
	var (
		result  k12.RecognitionLayoutPrimaryBatchSettlementResultV2
		created bool
	)
	err := sqliteutil.RetryOnBusy(ctx, func() error {
		var attemptErr error
		result, created, attemptErr = s.settleRecognitionLayoutPrimaryBatchV2Once(
			ctx,
			agentName,
			parentInvocationID,
			settlement,
		)
		return attemptErr
	})
	if err != nil {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false, err
	}
	return result, created, nil
}

func (s *Store) settleRecognitionLayoutPrimaryBatchV2Once(
	ctx context.Context,
	agentName string,
	parentInvocationID string,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
) (k12.RecognitionLayoutPrimaryBatchSettlementResultV2, bool, error) {
	tx, opErr := s.db.BeginTx(ctx, nil)
	if opErr != nil {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false,
			fmt.Errorf("k12storage: begin primary-batch settlement: %w", opErr)
	}
	// 提交成功或主路径失败后，回滚仅用于释放事务；主路径错误保持原样。
	defer func() { _ = tx.Rollback() }()
	authority, opErr := loadRecognitionLayoutPrimaryBatchAuthorityV2(
		ctx,
		tx,
		agentName,
		parentInvocationID,
		settlement,
	)
	if opErr != nil {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false, opErr
	}
	settlementDigest, opErr := recognitionLayoutPrimaryBatchSettlementDigestV2(
		parentInvocationID,
		settlement,
		authority.BatchID,
	)
	if opErr != nil {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false, opErr
	}
	receiptExists, opErr := validateStoredRecognitionLayoutBatchSettlementReceiptV2(
		ctx,
		tx,
		parentInvocationID,
		settlement,
		authority,
		settlementDigest,
	)
	if opErr != nil {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false, opErr
	}
	if settlement.Classification == k12.RecognitionLayoutBatchTerminalAmbiguousV2 {
		count, countErr := countRecognitionLayoutBatchSettlementRowsV2(
			ctx,
			tx,
			authority.PlanID,
			authority.BatchID,
			settlement.SourcePhysicalInvocationID,
		)
		if countErr != nil {
			return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false, countErr
		}
		if count != 0 {
			return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false,
				fmt.Errorf(
					"%w: terminal ambiguity conflicts with frozen batch members",
					ErrModelPhysicalInvocationConflict,
				)
		}
		if !receiptExists {
			if err := insertRecognitionLayoutBatchSettlementReceiptV2(
				ctx,
				tx,
				parentInvocationID,
				settlement,
				authority,
				settlementDigest,
				nowUnix(),
			); err != nil {
				return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false, err
			}
		}
		if err := tx.Commit(); err != nil {
			return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false,
				fmt.Errorf("k12storage: commit ambiguous batch classification: %w", err)
		}
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{
			Classification:         settlement.Classification,
			SettlementDigest:       settlementDigest,
			UnresolvedCandidateIDs: append([]string(nil), authority.MemberIDs...),
		}, !receiptExists, nil
	}
	if len(settlement.Candidates) != len(authority.MemberIDs) {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false,
			fmt.Errorf(
				"%w: classified candidates do not cover the source batch",
				ErrModelPhysicalInvocationConflict,
			)
	}
	for index := range authority.MemberIDs {
		if settlement.Candidates[index].CandidateID != authority.MemberIDs[index] {
			return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false,
				fmt.Errorf(
					"%w: classified candidate order or membership drifted",
					ErrModelPhysicalInvocationConflict,
				)
		}
	}

	projection, opErr := buildRecognitionLayoutPrimaryBatchProjectionV2(
		parentInvocationID,
		settlement,
		authority,
	)
	if opErr != nil {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false, opErr
	}
	projection.SettlementDigest = settlementDigest
	existingCount, opErr := countRecognitionLayoutBatchSettlementRowsV2(
		ctx,
		tx,
		authority.PlanID,
		authority.BatchID,
		settlement.SourcePhysicalInvocationID,
	)
	if opErr != nil {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false, opErr
	}
	created := !receiptExists
	if created && existingCount != 0 {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false,
			fmt.Errorf(
				"%w: primary batch has orphan settlement rows",
				ErrModelPhysicalInvocationConflict,
			)
	}
	if created {
		createdAt := nowUnix()
		if err := insertRecognitionLayoutBatchSettlementReceiptV2(
			ctx,
			tx,
			parentInvocationID,
			settlement,
			authority,
			settlementDigest,
			createdAt,
		); err != nil {
			return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false, err
		}
		for index, candidate := range settlement.Candidates {
			switch candidate.Classification {
			case k12.RecognitionLayoutCandidateValidV2:
				receipt := nextRecognitionLayoutCandidateReceiptV2(
					projection.FrozenResults,
					candidate.CandidateID,
				)
				if _, err := tx.ExecContext(
					ctx,
					`INSERT INTO k12_recognition_layout_candidate_results (
                            plan_id,candidate_id,parent_invocation_id,
                            source_physical_invocation_id,
                            source_physical_result_digest,result_kind,
                            result_digest,result_json,created_at
                         ) VALUES(?,?,?,?,?,?,?,?,?)`,
					authority.PlanID,
					candidate.CandidateID,
					parentInvocationID,
					settlement.SourcePhysicalInvocationID,
					settlement.SourcePhysicalResultDigest,
					candidate.ResultKind,
					receipt.ResultDigest,
					string(candidate.ResultJSON),
					createdAt,
				); err != nil {
					return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false,
						fmt.Errorf("k12storage: freeze candidate result %d: %w", index+1, err)
				}
			case k12.RecognitionLayoutCandidateMissingV2,
				k12.RecognitionLayoutCandidateInvalidV2:
				var frozenCount int
				if err := tx.QueryRowContext(
					ctx,
					`SELECT COUNT(*)
                       FROM k12_recognition_layout_candidate_results
                      WHERE plan_id=? AND candidate_id=?`,
					authority.PlanID,
					candidate.CandidateID,
				).Scan(&frozenCount); err != nil || frozenCount != 0 {
					return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false,
						fmt.Errorf(
							"%w: frozen candidate cannot receive repair authorization",
							ErrModelPhysicalInvocationConflict,
						)
				}
				authorization := nextRecognitionLayoutRepairAuthorizationV2(
					projection.RepairAuthorizations,
					candidate.CandidateID,
				)
				if _, err := tx.ExecContext(
					ctx,
					`INSERT INTO k12_recognition_layout_repair_authorizations (
                            plan_id,repair_authorization_id,
                            repair_physical_unit,candidate_id,source_batch_id,
                            source_batch_physical_invocation_id,
                            source_batch_result_digest,repair_round,
                            authorization_digest,created_at
                         ) VALUES(?,?,?,?,?,?,?,?,?,?)`,
					authority.PlanID,
					authorization.AuthorizationID,
					authorization.PhysicalUnit,
					candidate.CandidateID,
					authority.BatchID,
					settlement.SourcePhysicalInvocationID,
					settlement.SourcePhysicalResultDigest,
					authorization.RepairRound,
					authorization.AuthorizationDigest,
					createdAt,
				); err != nil {
					return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false,
						fmt.Errorf("k12storage: authorize singleton repair %d: %w", index+1, err)
				}
			}
		}
	}
	if err := validateStoredRecognitionLayoutPrimaryBatchSettlementV2(
		ctx,
		tx,
		parentInvocationID,
		settlement,
		authority,
		projection,
	); err != nil {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false,
			fmt.Errorf("k12storage: commit primary-batch settlement: %w", err)
	}
	return projection, created, nil
}

func validateRecognitionLayoutPrimaryBatchSettlementV2(
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
) error {
	if !validPrefixedSHA256DigestV2(settlement.PlanDigest) ||
		settlement.SourcePhysicalInvocationID == "" ||
		strings.TrimSpace(settlement.SourcePhysicalInvocationID) !=
			settlement.SourcePhysicalInvocationID ||
		!strings.HasPrefix(string(settlement.SourcePhysicalUnit), "layout_batch_") ||
		!settlement.SourcePhysicalUnit.Valid() ||
		!validPrefixedSHA256DigestV2(settlement.SourcePhysicalResultDigest) {
		return fmt.Errorf(
			"%w: invalid primary-batch settlement identity",
			k12.ErrRecognitionLayoutPlanInvalid,
		)
	}
	switch settlement.Classification {
	case k12.RecognitionLayoutBatchTerminalAmbiguousV2:
		if len(settlement.Candidates) != 0 || !validRecognitionLayoutAmbiguityKindV2(
			settlement.AmbiguityKind,
		) {
			return fmt.Errorf(
				"%w: terminal ambiguity must carry one closed reason and no members",
				k12.ErrRecognitionLayoutPlanInvalid,
			)
		}
		return nil
	case k12.RecognitionLayoutBatchClassifiedV2:
		if settlement.AmbiguityKind != "" || len(settlement.Candidates) < 1 ||
			len(settlement.Candidates) > k12.RecognitionLayoutBatchTargetLimitV2 {
			return fmt.Errorf(
				"%w: classified batch must contain 1..%d exact members",
				k12.ErrRecognitionLayoutPlanInvalid,
				k12.RecognitionLayoutBatchTargetLimitV2,
			)
		}
	default:
		return fmt.Errorf(
			"%w: unknown primary-batch classification",
			k12.ErrRecognitionLayoutPlanInvalid,
		)
	}
	seen := make(map[string]struct{}, len(settlement.Candidates))
	for _, candidate := range settlement.Candidates {
		if candidate.CandidateID == "" ||
			strings.TrimSpace(candidate.CandidateID) != candidate.CandidateID {
			return fmt.Errorf(
				"%w: non-canonical candidate identity",
				k12.ErrRecognitionLayoutPlanInvalid,
			)
		}
		if _, duplicate := seen[candidate.CandidateID]; duplicate {
			return fmt.Errorf(
				"%w: duplicate candidate classification",
				k12.ErrRecognitionLayoutPlanInvalid,
			)
		}
		seen[candidate.CandidateID] = struct{}{}
		switch candidate.Classification {
		case k12.RecognitionLayoutCandidateValidV2:
			if candidate.ResultKind != k12.RecognitionLayoutCandidateQuestionV2 &&
				candidate.ResultKind != k12.RecognitionLayoutCandidateNonQuestionV2 {
				return fmt.Errorf(
					"%w: valid candidate has unknown result kind",
					k12.ErrRecognitionLayoutPlanInvalid,
				)
			}
			if _, err := canonicalRecognitionLayoutResultJSONV2(candidate.ResultJSON); err != nil {
				return err
			}
		case k12.RecognitionLayoutCandidateMissingV2,
			k12.RecognitionLayoutCandidateInvalidV2:
			if candidate.ResultKind != "" || len(candidate.ResultJSON) != 0 {
				return fmt.Errorf(
					"%w: repairable candidate must not carry a result",
					k12.ErrRecognitionLayoutPlanInvalid,
				)
			}
		default:
			return fmt.Errorf(
				"%w: unknown candidate classification",
				k12.ErrRecognitionLayoutPlanInvalid,
			)
		}
	}
	return nil
}

func validRecognitionLayoutAmbiguityKindV2(
	kind k12.RecognitionLayoutBatchAmbiguityKindV2,
) bool {
	switch kind {
	case k12.RecognitionLayoutAmbiguityExtraCandidateV2,
		k12.RecognitionLayoutAmbiguityDuplicateCandidateV2,
		k12.RecognitionLayoutAmbiguitySourceConflictV2,
		k12.RecognitionLayoutAmbiguityUnattributableV2:
		return true
	default:
		return false
	}
}

func validPrefixedSHA256DigestV2(value string) bool {
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	raw := strings.TrimPrefix(value, "sha256:")
	if len(raw) != sha256.Size*2 || raw != strings.ToLower(raw) {
		return false
	}
	decoded, err := hex.DecodeString(raw)
	return err == nil && len(decoded) == sha256.Size
}

func canonicalRecognitionLayoutResultJSONV2(raw json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, fmt.Errorf(
			"%w: candidate result must be a JSON object",
			k12.ErrRecognitionLayoutPlanInvalid,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf(
			"%w: candidate result contains trailing JSON",
			k12.ErrRecognitionLayoutPlanInvalid,
		)
	}
	canonical, err := json.Marshal(object)
	if err != nil || !bytes.Equal(canonical, raw) {
		return nil, fmt.Errorf(
			"%w: candidate result JSON is not canonical",
			k12.ErrRecognitionLayoutPlanInvalid,
		)
	}
	return canonical, nil
}

func loadRecognitionLayoutPrimaryBatchAuthorityV2(
	ctx context.Context,
	q dbQueryer,
	agentName string,
	parentInvocationID string,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
) (recognitionLayoutPrimaryBatchAuthorityV2, error) {
	var (
		planID, jobID, storedPlanDigest, planStatus string
	)
	opErr := q.QueryRowContext(
		ctx,
		`SELECT plan_id,job_id,authorized_plan_digest,status
           FROM k12_recognition_layout_plans
          WHERE parent_invocation_id=? AND agent_name=?`,
		parentInvocationID,
		agentName,
	).Scan(&planID, &jobID, &storedPlanDigest, &planStatus)
	if errors.Is(opErr, sql.ErrNoRows) {
		return recognitionLayoutPrimaryBatchAuthorityV2{}, records.ErrNotFound
	}
	if opErr != nil {
		return recognitionLayoutPrimaryBatchAuthorityV2{},
			fmt.Errorf("k12storage: read settlement plan: %w", opErr)
	}
	if (planStatus != "authorized" && planStatus != "running" &&
		planStatus != "succeeded") ||
		storedPlanDigest == "" || storedPlanDigest != settlement.PlanDigest {
		return recognitionLayoutPrimaryBatchAuthorityV2{}, fmt.Errorf(
			"%w: primary-batch plan authorization drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	var authority recognitionLayoutPrimaryBatchAuthorityV2
	authority.PlanID = planID
	var memberCount int
	opErr = q.QueryRowContext(
		ctx,
		`SELECT batch_id,member_count
           FROM k12_recognition_layout_batches
          WHERE plan_id=? AND physical_unit=?`,
		planID,
		settlement.SourcePhysicalUnit,
	).Scan(&authority.BatchID, &memberCount)
	if errors.Is(opErr, sql.ErrNoRows) {
		return recognitionLayoutPrimaryBatchAuthorityV2{}, fmt.Errorf(
			"%w: source unit is not an authorized primary batch",
			records.ErrIllegalTransition,
		)
	}
	if opErr != nil {
		return recognitionLayoutPrimaryBatchAuthorityV2{},
			fmt.Errorf("k12storage: read source layout batch: %w", opErr)
	}
	rows, opErr := q.QueryContext(
		ctx,
		`SELECT member.candidate_id,candidate.ordinal
           FROM k12_recognition_layout_batch_members member
           JOIN k12_recognition_layout_candidates candidate
             ON candidate.plan_id=member.plan_id
            AND candidate.candidate_id=member.candidate_id
          WHERE member.plan_id=? AND member.batch_id=?
          ORDER BY member.slot`,
		planID,
		authority.BatchID,
	)
	if opErr != nil {
		return recognitionLayoutPrimaryBatchAuthorityV2{},
			fmt.Errorf("k12storage: list source batch members: %w", opErr)
	}
	for rows.Next() {
		var candidateID string
		var ordinal int
		if err := rows.Scan(&candidateID, &ordinal); err != nil {
			_ = rows.Close()
			return recognitionLayoutPrimaryBatchAuthorityV2{},
				fmt.Errorf("k12storage: scan source batch member: %w", err)
		}
		authority.MemberIDs = append(authority.MemberIDs, candidateID)
		authority.MemberOrdinals = append(authority.MemberOrdinals, ordinal)
	}
	if err := rows.Close(); err != nil {
		return recognitionLayoutPrimaryBatchAuthorityV2{},
			fmt.Errorf("k12storage: close source batch members: %w", err)
	}
	if len(authority.MemberIDs) != memberCount {
		return recognitionLayoutPrimaryBatchAuthorityV2{}, fmt.Errorf(
			"%w: source batch membership cardinality drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	exactSetDigest, opErr := k12.RecognitionLayoutTargetExactSetDigestV2(
		authority.MemberIDs,
	)
	if opErr != nil {
		return recognitionLayoutPrimaryBatchAuthorityV2{}, opErr
	}
	var (
		storedParentID, storedAgent, storedJob, storedStage string
		storedUnit                                          k12.RecognitionPhysicalUnit
		storedStatus                                        k12.ModelInvocationStatus
		storedResultDigest, storedPlanVersion               string
		sourcePlanDigest, storedExactSetDigest              string
		storedResultContent                                 sql.NullString
	)
	opErr = q.QueryRowContext(
		ctx,
		`SELECT parent_invocation_id,agent_name,job_id,stage,physical_unit,
                status,result_digest,result_content,recognition_plan_version,
                plan_digest,candidate_exact_set_digest
           FROM k12_model_physical_invocations
          WHERE physical_invocation_id=?`,
		settlement.SourcePhysicalInvocationID,
	).Scan(
		&storedParentID,
		&storedAgent,
		&storedJob,
		&storedStage,
		&storedUnit,
		&storedStatus,
		&storedResultDigest,
		&storedResultContent,
		&storedPlanVersion,
		&sourcePlanDigest,
		&storedExactSetDigest,
	)
	if errors.Is(opErr, sql.ErrNoRows) {
		return recognitionLayoutPrimaryBatchAuthorityV2{}, records.ErrNotFound
	}
	if opErr != nil {
		return recognitionLayoutPrimaryBatchAuthorityV2{},
			fmt.Errorf("k12storage: read succeeded source batch: %w", opErr)
	}
	if storedParentID != parentInvocationID || storedAgent != agentName ||
		storedJob != jobID || storedStage != k12.GradingStageRecognizing ||
		storedUnit != settlement.SourcePhysicalUnit ||
		storedStatus != k12.ModelInvocationSucceeded ||
		storedResultDigest != settlement.SourcePhysicalResultDigest ||
		!storedResultContent.Valid ||
		storedResultDigest != physicalInvocationResultDigest(storedResultContent.String) ||
		storedPlanVersion != "v2" || sourcePlanDigest != settlement.PlanDigest ||
		storedExactSetDigest != exactSetDigest {
		return recognitionLayoutPrimaryBatchAuthorityV2{}, fmt.Errorf(
			"%w: source primary batch is detached from exact private evidence",
			ErrModelPhysicalInvocationConflict,
		)
	}
	return authority, nil
}

func countRecognitionLayoutBatchSettlementRowsV2(
	ctx context.Context,
	q dbQueryer,
	planID string,
	batchID string,
	sourcePhysicalInvocationID string,
) (int, error) {
	var count int
	err := q.QueryRowContext(
		ctx,
		`SELECT
            (SELECT COUNT(*)
               FROM k12_recognition_layout_candidate_results result
              WHERE result.plan_id=?
                AND result.source_physical_invocation_id=?)
            +
            (SELECT COUNT(*)
               FROM k12_recognition_layout_repair_authorizations repair
              WHERE repair.plan_id=? AND repair.source_batch_id=?
                AND repair.source_batch_physical_invocation_id=?)`,
		planID,
		sourcePhysicalInvocationID,
		planID,
		batchID,
		sourcePhysicalInvocationID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("k12storage: count primary settlement rows: %w", err)
	}
	return count, nil
}

func buildRecognitionLayoutPrimaryBatchProjectionV2(
	parentInvocationID string,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
	authority recognitionLayoutPrimaryBatchAuthorityV2,
) (k12.RecognitionLayoutPrimaryBatchSettlementResultV2, error) {
	projection := k12.RecognitionLayoutPrimaryBatchSettlementResultV2{
		Classification: settlement.Classification,
	}
	for index, candidate := range settlement.Candidates {
		switch candidate.Classification {
		case k12.RecognitionLayoutCandidateValidV2:
			digest, err := recognitionLayoutCandidateResultDigestV2(
				parentInvocationID,
				settlement,
				candidate,
			)
			if err != nil {
				return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, err
			}
			projection.FrozenResults = append(
				projection.FrozenResults,
				k12.RecognitionLayoutCandidateResultReceiptV2{
					CandidateID:  candidate.CandidateID,
					ResultKind:   candidate.ResultKind,
					ResultDigest: digest,
				},
			)
		case k12.RecognitionLayoutCandidateMissingV2,
			k12.RecognitionLayoutCandidateInvalidV2:
			unit, err := k12.RecognitionLayoutRepairUnitV2(
				authority.MemberOrdinals[index],
			)
			if err != nil {
				return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, err
			}
			digest, err := recognitionLayoutRepairAuthorizationDigestV2(
				settlement,
				authority.BatchID,
				candidate,
				unit,
			)
			if err != nil {
				return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, err
			}
			projection.RepairAuthorizations = append(
				projection.RepairAuthorizations,
				k12.RecognitionLayoutRepairAuthorizationV2{
					AuthorizationID:     recognitionLayoutRepairAuthorizationIDV2(digest),
					AuthorizationDigest: digest,
					CandidateID:         candidate.CandidateID,
					PhysicalUnit:        unit,
					RepairRound:         1,
				},
			)
			projection.UnresolvedCandidateIDs = append(
				projection.UnresolvedCandidateIDs,
				candidate.CandidateID,
			)
		}
	}
	return projection, nil
}

func recognitionLayoutPrimaryBatchSettlementDigestV2(
	parentInvocationID string,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
	batchID string,
) (string, error) {
	candidates := append(
		make([]k12.RecognitionLayoutCandidateSettlementV2, 0, len(settlement.Candidates)),
		settlement.Candidates...,
	)
	encoded, err := json.Marshal(struct {
		Contract                   string                                       `json:"contract"`
		ParentInvocationID         string                                       `json:"parent_invocation_id"`
		PlanDigest                 string                                       `json:"plan_digest"`
		BatchID                    string                                       `json:"batch_id"`
		SourcePhysicalInvocationID string                                       `json:"source_physical_invocation_id"`
		SourcePhysicalUnit         k12.RecognitionPhysicalUnit                  `json:"source_physical_unit"`
		SourcePhysicalResultDigest string                                       `json:"source_physical_result_digest"`
		Classification             k12.RecognitionLayoutBatchClassificationV2   `json:"classification"`
		AmbiguityKind              k12.RecognitionLayoutBatchAmbiguityKindV2    `json:"ambiguity_kind"`
		Candidates                 []k12.RecognitionLayoutCandidateSettlementV2 `json:"candidates"`
	}{
		Contract:                   "recognition_layout_primary_batch_settlement_v2",
		ParentInvocationID:         parentInvocationID,
		PlanDigest:                 settlement.PlanDigest,
		BatchID:                    batchID,
		SourcePhysicalInvocationID: settlement.SourcePhysicalInvocationID,
		SourcePhysicalUnit:         settlement.SourcePhysicalUnit,
		SourcePhysicalResultDigest: settlement.SourcePhysicalResultDigest,
		Classification:             settlement.Classification,
		AmbiguityKind:              settlement.AmbiguityKind,
		Candidates:                 candidates,
	})
	if err != nil {
		return "", fmt.Errorf("k12storage: encode primary settlement digest: %w", err)
	}
	return physicalInvocationResultDigest(string(encoded)), nil
}

func validateStoredRecognitionLayoutBatchSettlementReceiptV2(
	ctx context.Context,
	q dbQueryer,
	parentInvocationID string,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
	authority recognitionLayoutPrimaryBatchAuthorityV2,
	settlementDigest string,
) (bool, error) {
	var (
		storedParent, sourceID, sourceResultDigest, storedDigest string
		storedUnit                                               k12.RecognitionPhysicalUnit
		classification                                           k12.RecognitionLayoutBatchClassificationV2
		ambiguityKind                                            k12.RecognitionLayoutBatchAmbiguityKindV2
	)
	err := q.QueryRowContext(
		ctx,
		`SELECT parent_invocation_id,source_physical_invocation_id,
                source_physical_unit,source_physical_result_digest,
                classification,ambiguity_kind,settlement_digest
           FROM k12_recognition_layout_batch_settlements
          WHERE plan_id=? AND batch_id=?`,
		authority.PlanID,
		authority.BatchID,
	).Scan(
		&storedParent,
		&sourceID,
		&storedUnit,
		&sourceResultDigest,
		&classification,
		&ambiguityKind,
		&storedDigest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("k12storage: read batch-settlement receipt: %w", err)
	}
	if storedParent != parentInvocationID ||
		sourceID != settlement.SourcePhysicalInvocationID ||
		storedUnit != settlement.SourcePhysicalUnit ||
		sourceResultDigest != settlement.SourcePhysicalResultDigest ||
		classification != settlement.Classification ||
		ambiguityKind != settlement.AmbiguityKind ||
		storedDigest != settlementDigest {
		return false, fmt.Errorf(
			"%w: batch-settlement replay changed immutable facts",
			ErrModelPhysicalInvocationConflict,
		)
	}
	return true, nil
}

func insertRecognitionLayoutBatchSettlementReceiptV2(
	ctx context.Context,
	tx *sql.Tx,
	parentInvocationID string,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
	authority recognitionLayoutPrimaryBatchAuthorityV2,
	settlementDigest string,
	createdAt int64,
) error {
	_, err := tx.ExecContext(
		ctx,
		`INSERT INTO k12_recognition_layout_batch_settlements (
            plan_id,batch_id,parent_invocation_id,
            source_physical_invocation_id,source_physical_unit,
            source_physical_result_digest,classification,ambiguity_kind,
            settlement_digest,created_at
         ) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		authority.PlanID,
		authority.BatchID,
		parentInvocationID,
		settlement.SourcePhysicalInvocationID,
		settlement.SourcePhysicalUnit,
		settlement.SourcePhysicalResultDigest,
		settlement.Classification,
		settlement.AmbiguityKind,
		settlementDigest,
		createdAt,
	)
	if err != nil {
		return fmt.Errorf("k12storage: append batch-settlement receipt: %w", err)
	}
	return nil
}

func recognitionLayoutCandidateResultDigestV2(
	parentInvocationID string,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
	candidate k12.RecognitionLayoutCandidateSettlementV2,
) (string, error) {
	canonical, err := canonicalRecognitionLayoutResultJSONV2(candidate.ResultJSON)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(struct {
		Contract                   string                                     `json:"contract"`
		ParentInvocationID         string                                     `json:"parent_invocation_id"`
		PlanDigest                 string                                     `json:"plan_digest"`
		CandidateID                string                                     `json:"candidate_id"`
		SourcePhysicalInvocationID string                                     `json:"source_physical_invocation_id"`
		SourcePhysicalResultDigest string                                     `json:"source_physical_result_digest"`
		SourcePhysicalUnit         k12.RecognitionPhysicalUnit                `json:"source_physical_unit"`
		ResultKind                 k12.RecognitionLayoutCandidateResultKindV2 `json:"result_kind"`
		Result                     json.RawMessage                            `json:"result"`
	}{
		Contract:                   "recognition_layout_candidate_result_v2",
		ParentInvocationID:         parentInvocationID,
		PlanDigest:                 settlement.PlanDigest,
		CandidateID:                candidate.CandidateID,
		SourcePhysicalInvocationID: settlement.SourcePhysicalInvocationID,
		SourcePhysicalResultDigest: settlement.SourcePhysicalResultDigest,
		SourcePhysicalUnit:         settlement.SourcePhysicalUnit,
		ResultKind:                 candidate.ResultKind,
		Result:                     canonical,
	})
	if err != nil {
		return "", fmt.Errorf("k12storage: encode candidate result digest: %w", err)
	}
	return physicalInvocationResultDigest(string(encoded)), nil
}

func recognitionLayoutRepairAuthorizationDigestV2(
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
	batchID string,
	candidate k12.RecognitionLayoutCandidateSettlementV2,
	repairUnit k12.RecognitionPhysicalUnit,
) (string, error) {
	encoded, err := json.Marshal(struct {
		Contract                   string                                         `json:"contract"`
		PlanDigest                 string                                         `json:"plan_digest"`
		CandidateID                string                                         `json:"candidate_id"`
		CandidateClassification    k12.RecognitionLayoutCandidateClassificationV2 `json:"candidate_classification"`
		SourceBatchID              string                                         `json:"source_batch_id"`
		SourcePhysicalInvocationID string                                         `json:"source_physical_invocation_id"`
		SourcePhysicalResultDigest string                                         `json:"source_physical_result_digest"`
		SourcePhysicalUnit         k12.RecognitionPhysicalUnit                    `json:"source_physical_unit"`
		RepairPhysicalUnit         k12.RecognitionPhysicalUnit                    `json:"repair_physical_unit"`
		RepairRound                int                                            `json:"repair_round"`
	}{
		Contract:                   "recognition_layout_repair_authorization_v2",
		PlanDigest:                 settlement.PlanDigest,
		CandidateID:                candidate.CandidateID,
		CandidateClassification:    candidate.Classification,
		SourceBatchID:              batchID,
		SourcePhysicalInvocationID: settlement.SourcePhysicalInvocationID,
		SourcePhysicalResultDigest: settlement.SourcePhysicalResultDigest,
		SourcePhysicalUnit:         settlement.SourcePhysicalUnit,
		RepairPhysicalUnit:         repairUnit,
		RepairRound:                1,
	})
	if err != nil {
		return "", fmt.Errorf("k12storage: encode repair authorization digest: %w", err)
	}
	return physicalInvocationResultDigest(string(encoded)), nil
}

func recognitionLayoutRepairAuthorizationIDV2(digest string) string {
	return "repair-auth-v2-" + strings.TrimPrefix(digest, "sha256:")
}

func nextRecognitionLayoutCandidateReceiptV2(
	receipts []k12.RecognitionLayoutCandidateResultReceiptV2,
	candidateID string,
) k12.RecognitionLayoutCandidateResultReceiptV2 {
	for _, receipt := range receipts {
		if receipt.CandidateID == candidateID {
			return receipt
		}
	}
	return k12.RecognitionLayoutCandidateResultReceiptV2{}
}

func nextRecognitionLayoutRepairAuthorizationV2(
	authorizations []k12.RecognitionLayoutRepairAuthorizationV2,
	candidateID string,
) k12.RecognitionLayoutRepairAuthorizationV2 {
	for _, authorization := range authorizations {
		if authorization.CandidateID == candidateID {
			return authorization
		}
	}
	return k12.RecognitionLayoutRepairAuthorizationV2{}
}

func validateStoredRecognitionLayoutPrimaryBatchSettlementV2(
	ctx context.Context,
	q dbQueryer,
	parentInvocationID string,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
	authority recognitionLayoutPrimaryBatchAuthorityV2,
	projection k12.RecognitionLayoutPrimaryBatchSettlementResultV2,
) error {
	count, err := countRecognitionLayoutBatchSettlementRowsV2(
		ctx,
		q,
		authority.PlanID,
		authority.BatchID,
		settlement.SourcePhysicalInvocationID,
	)
	if err != nil {
		return err
	}
	if count != len(settlement.Candidates) {
		return fmt.Errorf(
			"%w: primary settlement cardinality drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	for _, candidate := range settlement.Candidates {
		switch candidate.Classification {
		case k12.RecognitionLayoutCandidateValidV2:
			receipt := nextRecognitionLayoutCandidateReceiptV2(
				projection.FrozenResults,
				candidate.CandidateID,
			)
			var (
				storedParent, sourceID, sourceDigest string
				storedKind                           k12.RecognitionLayoutCandidateResultKindV2
				storedDigest, storedJSON             string
			)
			err := q.QueryRowContext(
				ctx,
				`SELECT parent_invocation_id,source_physical_invocation_id,
                        source_physical_result_digest,result_kind,
                        result_digest,result_json
                   FROM k12_recognition_layout_candidate_results
                  WHERE plan_id=? AND candidate_id=?`,
				authority.PlanID,
				candidate.CandidateID,
			).Scan(
				&storedParent,
				&sourceID,
				&sourceDigest,
				&storedKind,
				&storedDigest,
				&storedJSON,
			)
			if err != nil || storedParent != parentInvocationID ||
				sourceID != settlement.SourcePhysicalInvocationID ||
				sourceDigest != settlement.SourcePhysicalResultDigest ||
				storedKind != candidate.ResultKind ||
				storedDigest != receipt.ResultDigest ||
				storedJSON != string(candidate.ResultJSON) {
				return fmt.Errorf(
					"%w: frozen candidate result %s drifted: %v",
					ErrModelPhysicalInvocationConflict,
					candidate.CandidateID,
					err,
				)
			}
			var repairCount int
			if err := q.QueryRowContext(
				ctx,
				`SELECT COUNT(*)
                   FROM k12_recognition_layout_repair_authorizations
                  WHERE plan_id=? AND candidate_id=?`,
				authority.PlanID,
				candidate.CandidateID,
			).Scan(&repairCount); err != nil || repairCount != 0 {
				return fmt.Errorf(
					"%w: frozen candidate also carries repair authorization",
					ErrModelPhysicalInvocationConflict,
				)
			}
		case k12.RecognitionLayoutCandidateMissingV2,
			k12.RecognitionLayoutCandidateInvalidV2:
			authorization := nextRecognitionLayoutRepairAuthorizationV2(
				projection.RepairAuthorizations,
				candidate.CandidateID,
			)
			var (
				authorizationID, physicalUnit, sourceBatchID string
				sourceID, sourceDigest, authorizationDigest  string
				repairRound                                  int
			)
			err := q.QueryRowContext(
				ctx,
				`SELECT repair_authorization_id,repair_physical_unit,
                        source_batch_id,source_batch_physical_invocation_id,
                        source_batch_result_digest,repair_round,
                        authorization_digest
                   FROM k12_recognition_layout_repair_authorizations
                  WHERE plan_id=? AND candidate_id=?`,
				authority.PlanID,
				candidate.CandidateID,
			).Scan(
				&authorizationID,
				&physicalUnit,
				&sourceBatchID,
				&sourceID,
				&sourceDigest,
				&repairRound,
				&authorizationDigest,
			)
			if err != nil || authorizationID != authorization.AuthorizationID ||
				k12.RecognitionPhysicalUnit(physicalUnit) != authorization.PhysicalUnit ||
				sourceBatchID != authority.BatchID ||
				sourceID != settlement.SourcePhysicalInvocationID ||
				sourceDigest != settlement.SourcePhysicalResultDigest ||
				repairRound != 1 ||
				authorizationDigest != authorization.AuthorizationDigest {
				return fmt.Errorf(
					"%w: repair authorization %s drifted: %v",
					ErrModelPhysicalInvocationConflict,
					candidate.CandidateID,
					err,
				)
			}
		}
	}
	return nil
}

type recognitionLayoutRepairSettlementAuthorityV2 struct {
	PlanID     string
	PlanStatus string
	Parent     k12.ModelInvocation
}

// SettleRecognitionLayoutRepairV2 冻结一个精确成功的第一轮单候选修复终态分类。
// 修复回执与可选的有效候选结果原子提交；精确重放只读。
func (s *Store) SettleRecognitionLayoutRepairV2(
	ctx context.Context,
	agentName string,
	parentInvocationID string,
	settlement k12.RecognitionLayoutRepairSettlementV2,
) (k12.RecognitionLayoutRepairSettlementResultV2, bool, error) {
	agentName = strings.TrimSpace(agentName)
	parentInvocationID = strings.TrimSpace(parentInvocationID)
	if agentName == "" || parentInvocationID == "" {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false,
			fmt.Errorf("k12storage: repair settlement requires owner and parent")
	}
	if err := validateRecognitionLayoutRepairSettlementV2(settlement); err != nil {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false, err
	}
	var (
		result  k12.RecognitionLayoutRepairSettlementResultV2
		created bool
	)
	err := sqliteutil.RetryOnBusy(ctx, func() error {
		var attemptErr error
		result, created, attemptErr = s.settleRecognitionLayoutRepairV2Once(
			ctx,
			agentName,
			parentInvocationID,
			settlement,
		)
		return attemptErr
	})
	if err != nil {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false, err
	}
	return result, created, nil
}

func (s *Store) settleRecognitionLayoutRepairV2Once(
	ctx context.Context,
	agentName string,
	parentInvocationID string,
	settlement k12.RecognitionLayoutRepairSettlementV2,
) (k12.RecognitionLayoutRepairSettlementResultV2, bool, error) {
	tx, opErr := s.db.BeginTx(ctx, nil)
	if opErr != nil {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false,
			fmt.Errorf("k12storage: begin repair settlement: %w", opErr)
	}
	// 提交成功或主路径失败后，回滚仅用于释放事务；主路径错误保持原样。
	defer func() { _ = tx.Rollback() }()

	// 在读取回执是否存在前先获取一个有作用域的 SQLite 写锁。并发精确重放会在赢家提交后
	// 重新开始完整快照，而不会向调用方暴露 SQLITE_BUSY_SNAPSHOT。
	res, opErr := tx.ExecContext(
		ctx,
		`UPDATE k12_recognition_layout_plans
            SET updated_at=updated_at
          WHERE parent_invocation_id=? AND agent_name=?`,
		parentInvocationID,
		agentName,
	)
	if opErr != nil {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false,
			fmt.Errorf("k12storage: serialize repair settlement: %w", opErr)
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false,
			records.ErrNotFound
	}

	authority, opErr := loadRecognitionLayoutRepairSettlementAuthorityV2(
		ctx,
		tx,
		agentName,
		parentInvocationID,
		settlement,
	)
	if opErr != nil {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false, opErr
	}
	projection, resultDigest, opErr := buildRecognitionLayoutRepairProjectionV2(
		parentInvocationID,
		settlement,
	)
	if opErr != nil {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false, opErr
	}
	settlementDigest, opErr := recognitionLayoutRepairSettlementDigestV2(
		parentInvocationID,
		settlement,
	)
	if opErr != nil {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false, opErr
	}
	projection.SettlementDigest = settlementDigest
	receiptExists, opErr := validateStoredRecognitionLayoutRepairSettlementV2(
		ctx,
		tx,
		parentInvocationID,
		settlement,
		authority,
		projection,
		resultDigest,
	)
	if opErr != nil {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false, opErr
	}
	if receiptExists {
		if err := tx.Commit(); err != nil {
			return k12.RecognitionLayoutRepairSettlementResultV2{}, false,
				fmt.Errorf("k12storage: commit repair settlement replay: %w", err)
		}
		return projection, false, nil
	}
	if authority.PlanStatus != "authorized" && authority.PlanStatus != "running" {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false,
			fmt.Errorf(
				"%w: repair settlement plan status=%s",
				records.ErrIllegalTransition,
				authority.PlanStatus,
			)
	}
	if authority.Parent.Status != k12.ModelInvocationSent {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false,
			fmt.Errorf(
				"%w: repair settlement parent status=%s",
				records.ErrIllegalTransition,
				authority.Parent.Status,
			)
	}

	createdAt := nowUnix()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO k12_recognition_layout_repair_settlements (
            plan_id,repair_authorization_id,authorization_digest,candidate_id,
            parent_invocation_id,source_physical_invocation_id,
            source_physical_unit,source_physical_result_digest,
            classification,result_kind,result_digest,settlement_digest,created_at
         ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		authority.PlanID,
		settlement.AuthorizationID,
		settlement.AuthorizationDigest,
		settlement.CandidateID,
		parentInvocationID,
		settlement.SourcePhysicalInvocationID,
		settlement.SourcePhysicalUnit,
		settlement.SourcePhysicalResultDigest,
		settlement.Classification,
		settlement.ResultKind,
		resultDigest,
		settlementDigest,
		createdAt,
	); err != nil {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false,
			fmt.Errorf("k12storage: append repair-settlement receipt: %w", err)
	}
	if settlement.Classification == k12.RecognitionLayoutCandidateValidV2 {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO k12_recognition_layout_candidate_results (
                plan_id,candidate_id,parent_invocation_id,
                source_physical_invocation_id,source_physical_result_digest,
                result_kind,result_digest,result_json,created_at
             ) VALUES(?,?,?,?,?,?,?,?,?)`,
			authority.PlanID,
			settlement.CandidateID,
			parentInvocationID,
			settlement.SourcePhysicalInvocationID,
			settlement.SourcePhysicalResultDigest,
			settlement.ResultKind,
			resultDigest,
			string(settlement.ResultJSON),
			createdAt,
		); err != nil {
			return k12.RecognitionLayoutRepairSettlementResultV2{}, false,
				fmt.Errorf("k12storage: freeze repaired candidate result: %w", err)
		}
	}
	stored, opErr := validateStoredRecognitionLayoutRepairSettlementV2(
		ctx,
		tx,
		parentInvocationID,
		settlement,
		authority,
		projection,
		resultDigest,
	)
	if opErr != nil || !stored {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false,
			fmt.Errorf(
				"%w: repair settlement did not become durable: %v",
				ErrModelPhysicalInvocationConflict,
				opErr,
			)
	}
	if err := tx.Commit(); err != nil {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false,
			fmt.Errorf("k12storage: commit repair settlement: %w", err)
	}
	return projection, true, nil
}

func validateRecognitionLayoutRepairSettlementV2(
	settlement k12.RecognitionLayoutRepairSettlementV2,
) error {
	if !validPrefixedSHA256DigestV2(settlement.PlanDigest) ||
		settlement.AuthorizationID == "" ||
		strings.TrimSpace(settlement.AuthorizationID) != settlement.AuthorizationID ||
		!validPrefixedSHA256DigestV2(settlement.AuthorizationDigest) ||
		settlement.CandidateID == "" ||
		strings.TrimSpace(settlement.CandidateID) != settlement.CandidateID ||
		settlement.SourcePhysicalInvocationID == "" ||
		strings.TrimSpace(settlement.SourcePhysicalInvocationID) !=
			settlement.SourcePhysicalInvocationID ||
		!strings.HasPrefix(string(settlement.SourcePhysicalUnit), "layout_repair_") ||
		!settlement.SourcePhysicalUnit.Valid() ||
		!validPrefixedSHA256DigestV2(settlement.SourcePhysicalResultDigest) {
		return fmt.Errorf(
			"%w: invalid repair-settlement identity",
			k12.ErrRecognitionLayoutPlanInvalid,
		)
	}
	switch settlement.Classification {
	case k12.RecognitionLayoutCandidateValidV2:
		if settlement.ResultKind != k12.RecognitionLayoutCandidateQuestionV2 &&
			settlement.ResultKind != k12.RecognitionLayoutCandidateNonQuestionV2 {
			return fmt.Errorf(
				"%w: valid repair has unknown result kind",
				k12.ErrRecognitionLayoutPlanInvalid,
			)
		}
		if _, err := canonicalRecognitionLayoutResultJSONV2(
			settlement.ResultJSON,
		); err != nil {
			return err
		}
	case k12.RecognitionLayoutCandidateInvalidV2:
		if settlement.ResultKind != "" || len(settlement.ResultJSON) != 0 {
			return fmt.Errorf(
				"%w: invalid repair must not carry a result",
				k12.ErrRecognitionLayoutPlanInvalid,
			)
		}
	default:
		return fmt.Errorf(
			"%w: repair settlement must be valid or terminal invalid",
			k12.ErrRecognitionLayoutPlanInvalid,
		)
	}
	return nil
}

func loadRecognitionLayoutRepairSettlementAuthorityV2(
	ctx context.Context,
	q dbQueryer,
	agentName string,
	parentInvocationID string,
	settlement k12.RecognitionLayoutRepairSettlementV2,
) (recognitionLayoutRepairSettlementAuthorityV2, error) {
	parent, opErr := getModelInvocationByIDVia(ctx, q, parentInvocationID)
	if opErr != nil {
		return recognitionLayoutRepairSettlementAuthorityV2{}, opErr
	}
	if parent.AgentName != agentName || parent.Stage != k12.GradingStageRecognizing {
		return recognitionLayoutRepairSettlementAuthorityV2{}, fmt.Errorf(
			"%w: repair-settlement parent ownership drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	authority := recognitionLayoutRepairSettlementAuthorityV2{Parent: parent}
	var jobID, planDigest string
	opErr = q.QueryRowContext(
		ctx,
		`SELECT plan_id,job_id,authorized_plan_digest,status
           FROM k12_recognition_layout_plans
          WHERE parent_invocation_id=? AND agent_name=?`,
		parentInvocationID,
		agentName,
	).Scan(
		&authority.PlanID,
		&jobID,
		&planDigest,
		&authority.PlanStatus,
	)
	if errors.Is(opErr, sql.ErrNoRows) {
		return recognitionLayoutRepairSettlementAuthorityV2{}, records.ErrNotFound
	}
	if opErr != nil {
		return recognitionLayoutRepairSettlementAuthorityV2{},
			fmt.Errorf("k12storage: read repair-settlement plan: %w", opErr)
	}
	if jobID != parent.JobID || planDigest == "" ||
		planDigest != settlement.PlanDigest {
		return recognitionLayoutRepairSettlementAuthorityV2{}, fmt.Errorf(
			"%w: repair-settlement plan authorization drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	var (
		authorizationID, candidateID, authorizationDigest string
		repairUnit                                        k12.RecognitionPhysicalUnit
		repairRound                                       int
	)
	opErr = q.QueryRowContext(
		ctx,
		`SELECT repair_authorization_id,repair_physical_unit,
                candidate_id,repair_round,authorization_digest
           FROM k12_recognition_layout_repair_authorizations
          WHERE plan_id=? AND candidate_id=?`,
		authority.PlanID,
		settlement.CandidateID,
	).Scan(
		&authorizationID,
		&repairUnit,
		&candidateID,
		&repairRound,
		&authorizationDigest,
	)
	if errors.Is(opErr, sql.ErrNoRows) {
		return recognitionLayoutRepairSettlementAuthorityV2{}, fmt.Errorf(
			"%w: singleton repair authorization is missing",
			records.ErrIllegalTransition,
		)
	}
	if opErr != nil {
		return recognitionLayoutRepairSettlementAuthorityV2{},
			fmt.Errorf("k12storage: read repair authorization: %w", opErr)
	}
	if authorizationID != settlement.AuthorizationID ||
		authorizationDigest != settlement.AuthorizationDigest ||
		candidateID != settlement.CandidateID ||
		repairUnit != settlement.SourcePhysicalUnit || repairRound != 1 {
		return recognitionLayoutRepairSettlementAuthorityV2{}, fmt.Errorf(
			"%w: repair authorization identity drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	var candidateOrdinal int
	if err := q.QueryRowContext(
		ctx,
		`SELECT ordinal FROM k12_recognition_layout_candidates
          WHERE plan_id=? AND candidate_id=?`,
		authority.PlanID,
		settlement.CandidateID,
	).Scan(&candidateOrdinal); err != nil {
		return recognitionLayoutRepairSettlementAuthorityV2{},
			fmt.Errorf("k12storage: read repair-settlement candidate: %w", err)
	}
	wantUnit, opErr := k12.RecognitionLayoutRepairUnitV2(candidateOrdinal)
	if opErr != nil || wantUnit != settlement.SourcePhysicalUnit {
		return recognitionLayoutRepairSettlementAuthorityV2{}, fmt.Errorf(
			"%w: repair-settlement unit is not the candidate's canonical unit",
			ErrModelPhysicalInvocationConflict,
		)
	}
	source, opErr := getModelPhysicalInvocationByIDVia(
		ctx,
		q,
		agentName,
		settlement.SourcePhysicalInvocationID,
	)
	if opErr != nil {
		return recognitionLayoutRepairSettlementAuthorityV2{}, opErr
	}
	if err := validatePhysicalInvocationParent(source, parent); err != nil {
		return recognitionLayoutRepairSettlementAuthorityV2{}, err
	}
	exactSetDigest, opErr := k12.RecognitionLayoutTargetExactSetDigestV2(
		[]string{settlement.CandidateID},
	)
	if opErr != nil {
		return recognitionLayoutRepairSettlementAuthorityV2{}, opErr
	}
	if source.PhysicalUnit != settlement.SourcePhysicalUnit ||
		source.Status != k12.ModelInvocationSucceeded || source.Attempt != 1 ||
		source.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
		source.PlanDigest != settlement.PlanDigest ||
		source.CandidateExactSetDigest != exactSetDigest ||
		source.ResultDigest != settlement.SourcePhysicalResultDigest {
		return recognitionLayoutRepairSettlementAuthorityV2{}, fmt.Errorf(
			"%w: repair settlement is detached from exact succeeded singleton child",
			ErrModelPhysicalInvocationConflict,
		)
	}
	var resultContent sql.NullString
	if err := q.QueryRowContext(
		ctx,
		`SELECT result_content FROM k12_model_physical_invocations
          WHERE physical_invocation_id=? AND agent_name=?`,
		settlement.SourcePhysicalInvocationID,
		agentName,
	).Scan(&resultContent); err != nil || !resultContent.Valid ||
		physicalInvocationResultDigest(resultContent.String) != source.ResultDigest {
		return recognitionLayoutRepairSettlementAuthorityV2{}, fmt.Errorf(
			"%w: repair source private result content drifted: %v",
			ErrModelPhysicalInvocationConflict,
			err,
		)
	}
	if err := validateRecognitionLayoutRepairAuthorizationEvidenceVia(
		ctx,
		q,
		parent,
		source,
		authority.PlanID,
		planDigest,
		false,
	); err != nil {
		return recognitionLayoutRepairSettlementAuthorityV2{}, err
	}
	return authority, nil
}

func buildRecognitionLayoutRepairProjectionV2(
	parentInvocationID string,
	settlement k12.RecognitionLayoutRepairSettlementV2,
) (k12.RecognitionLayoutRepairSettlementResultV2, string, error) {
	projection := k12.RecognitionLayoutRepairSettlementResultV2{
		Classification: settlement.Classification,
	}
	if settlement.Classification == k12.RecognitionLayoutCandidateInvalidV2 {
		projection.UnresolvedCandidateID = settlement.CandidateID
		return projection, "", nil
	}
	resultDigest, err := recognitionLayoutRepairCandidateResultDigestV2(
		parentInvocationID,
		settlement,
	)
	if err != nil {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, "", err
	}
	projection.FrozenResult = &k12.RecognitionLayoutCandidateResultReceiptV2{
		CandidateID:  settlement.CandidateID,
		ResultKind:   settlement.ResultKind,
		ResultDigest: resultDigest,
	}
	return projection, resultDigest, nil
}

func recognitionLayoutRepairCandidateResultDigestV2(
	parentInvocationID string,
	settlement k12.RecognitionLayoutRepairSettlementV2,
) (string, error) {
	return recognitionLayoutCandidateResultDigestV2(
		parentInvocationID,
		k12.RecognitionLayoutPrimaryBatchSettlementV2{
			PlanDigest:                 settlement.PlanDigest,
			SourcePhysicalInvocationID: settlement.SourcePhysicalInvocationID,
			SourcePhysicalUnit:         settlement.SourcePhysicalUnit,
			SourcePhysicalResultDigest: settlement.SourcePhysicalResultDigest,
		},
		k12.RecognitionLayoutCandidateSettlementV2{
			CandidateID:    settlement.CandidateID,
			Classification: settlement.Classification,
			ResultKind:     settlement.ResultKind,
			ResultJSON:     settlement.ResultJSON,
		},
	)
}

func recognitionLayoutRepairSettlementDigestV2(
	parentInvocationID string,
	settlement k12.RecognitionLayoutRepairSettlementV2,
) (string, error) {
	encoded, err := json.Marshal(struct {
		Contract                   string                                         `json:"contract"`
		ParentInvocationID         string                                         `json:"parent_invocation_id"`
		PlanDigest                 string                                         `json:"plan_digest"`
		AuthorizationID            string                                         `json:"authorization_id"`
		AuthorizationDigest        string                                         `json:"authorization_digest"`
		CandidateID                string                                         `json:"candidate_id"`
		SourcePhysicalInvocationID string                                         `json:"source_physical_invocation_id"`
		SourcePhysicalUnit         k12.RecognitionPhysicalUnit                    `json:"source_physical_unit"`
		SourcePhysicalResultDigest string                                         `json:"source_physical_result_digest"`
		Classification             k12.RecognitionLayoutCandidateClassificationV2 `json:"classification"`
		ResultKind                 k12.RecognitionLayoutCandidateResultKindV2     `json:"result_kind"`
		Result                     json.RawMessage                                `json:"result"`
	}{
		Contract:                   "recognition_layout_repair_settlement_v2",
		ParentInvocationID:         parentInvocationID,
		PlanDigest:                 settlement.PlanDigest,
		AuthorizationID:            settlement.AuthorizationID,
		AuthorizationDigest:        settlement.AuthorizationDigest,
		CandidateID:                settlement.CandidateID,
		SourcePhysicalInvocationID: settlement.SourcePhysicalInvocationID,
		SourcePhysicalUnit:         settlement.SourcePhysicalUnit,
		SourcePhysicalResultDigest: settlement.SourcePhysicalResultDigest,
		Classification:             settlement.Classification,
		ResultKind:                 settlement.ResultKind,
		Result:                     settlement.ResultJSON,
	})
	if err != nil {
		return "", fmt.Errorf("k12storage: encode repair settlement digest: %w", err)
	}
	return physicalInvocationResultDigest(string(encoded)), nil
}

func validateStoredRecognitionLayoutRepairSettlementV2(
	ctx context.Context,
	q dbQueryer,
	parentInvocationID string,
	settlement k12.RecognitionLayoutRepairSettlementV2,
	authority recognitionLayoutRepairSettlementAuthorityV2,
	projection k12.RecognitionLayoutRepairSettlementResultV2,
	resultDigest string,
) (bool, error) {
	var (
		storedAuthorizationID, storedAuthorizationDigest string
		storedCandidateID, storedParentID                string
		storedSourceID, storedSourceDigest               string
		storedUnit                                       k12.RecognitionPhysicalUnit
		storedClassification                             k12.RecognitionLayoutCandidateClassificationV2
		storedResultKind                                 k12.RecognitionLayoutCandidateResultKindV2
		storedResultDigest, storedSettlementDigest       string
	)
	opErr := q.QueryRowContext(
		ctx,
		`SELECT repair_authorization_id,authorization_digest,candidate_id,
                parent_invocation_id,source_physical_invocation_id,
                source_physical_unit,source_physical_result_digest,
                classification,result_kind,result_digest,settlement_digest
           FROM k12_recognition_layout_repair_settlements
          WHERE plan_id=? AND candidate_id=?`,
		authority.PlanID,
		settlement.CandidateID,
	).Scan(
		&storedAuthorizationID,
		&storedAuthorizationDigest,
		&storedCandidateID,
		&storedParentID,
		&storedSourceID,
		&storedUnit,
		&storedSourceDigest,
		&storedClassification,
		&storedResultKind,
		&storedResultDigest,
		&storedSettlementDigest,
	)
	if errors.Is(opErr, sql.ErrNoRows) {
		return false, nil
	}
	if opErr != nil {
		return false, fmt.Errorf("k12storage: read repair-settlement receipt: %w", opErr)
	}
	if storedAuthorizationID != settlement.AuthorizationID ||
		storedAuthorizationDigest != settlement.AuthorizationDigest ||
		storedCandidateID != settlement.CandidateID ||
		storedParentID != parentInvocationID ||
		storedSourceID != settlement.SourcePhysicalInvocationID ||
		storedUnit != settlement.SourcePhysicalUnit ||
		storedSourceDigest != settlement.SourcePhysicalResultDigest ||
		storedClassification != settlement.Classification ||
		storedResultKind != settlement.ResultKind ||
		storedResultDigest != resultDigest ||
		storedSettlementDigest != projection.SettlementDigest {
		return false, fmt.Errorf(
			"%w: repair-settlement replay changed immutable facts",
			ErrModelPhysicalInvocationConflict,
		)
	}
	var candidateResultCount int
	if err := q.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_candidate_results
          WHERE plan_id=? AND candidate_id=?`,
		authority.PlanID,
		settlement.CandidateID,
	).Scan(&candidateResultCount); err != nil {
		return false, fmt.Errorf("k12storage: count repaired candidate result: %w", err)
	}
	if settlement.Classification == k12.RecognitionLayoutCandidateInvalidV2 {
		if candidateResultCount != 0 || projection.FrozenResult != nil ||
			projection.UnresolvedCandidateID != settlement.CandidateID {
			return false, fmt.Errorf(
				"%w: terminal invalid repair unexpectedly froze a candidate",
				ErrModelPhysicalInvocationConflict,
			)
		}
		return true, nil
	}
	if candidateResultCount != 1 || projection.FrozenResult == nil {
		return false, fmt.Errorf(
			"%w: valid repair candidate result cardinality drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	var (
		storedResultParent, storedResultSourceID, storedResultSourceDigest string
		storedCandidateResultKind                                          k12.RecognitionLayoutCandidateResultKindV2
		storedCandidateResultDigest, storedResultJSON                      string
	)
	opErr = q.QueryRowContext(
		ctx,
		`SELECT parent_invocation_id,source_physical_invocation_id,
                source_physical_result_digest,result_kind,result_digest,
                result_json
           FROM k12_recognition_layout_candidate_results
          WHERE plan_id=? AND candidate_id=?`,
		authority.PlanID,
		settlement.CandidateID,
	).Scan(
		&storedResultParent,
		&storedResultSourceID,
		&storedResultSourceDigest,
		&storedCandidateResultKind,
		&storedCandidateResultDigest,
		&storedResultJSON,
	)
	if opErr != nil || storedResultParent != parentInvocationID ||
		storedResultSourceID != settlement.SourcePhysicalInvocationID ||
		storedResultSourceDigest != settlement.SourcePhysicalResultDigest ||
		storedCandidateResultKind != settlement.ResultKind ||
		storedCandidateResultDigest != resultDigest ||
		storedResultJSON != string(settlement.ResultJSON) {
		return false, fmt.Errorf(
			"%w: frozen repair candidate result drifted: %v",
			ErrModelPhysicalInvocationConflict,
			opErr,
		)
	}
	return true, nil
}

type recognitionLayoutFinalizationAuthorityV2 struct {
	PlanID                  string
	Status                  string
	HeaderDigest            string
	CandidateExactSetDigest string
	Header                  k12.RecognitionLayoutPlanHeaderV2
	Plan                    k12.RecognitionLayoutPlanV2
	Parent                  k12.ModelInvocation
}

type recognitionLayoutFinalRepairAuthorizationV2 struct {
	AuthorizationID        string
	AuthorizationDigest    string
	CandidateID            string
	CandidateOrdinal       int
	PhysicalUnit           k12.RecognitionPhysicalUnit
	SourceBatchID          string
	SourcePhysicalID       string
	SourceResultDigest     string
	OriginalClassification k12.RecognitionLayoutCandidateClassificationV2
}

// FinalizeRecognitionLayoutPlanV2 根据私有持久证据重建完整的候选和物理调用精确集合。
// 调用方只提供 owner 与 parent 身份，因此无法注入 Provider 结果或遗漏已授权物理子调用。
func (s *Store) FinalizeRecognitionLayoutPlanV2(
	ctx context.Context,
	agentName string,
	parentInvocationID string,
) (k12.RecognitionLayoutPlanFinalizationResultV2, bool, error) {
	agentName = strings.TrimSpace(agentName)
	parentInvocationID = strings.TrimSpace(parentInvocationID)
	if agentName == "" || parentInvocationID == "" {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
			fmt.Errorf("k12storage: layout finalization missing owner or parent")
	}
	var (
		result  k12.RecognitionLayoutPlanFinalizationResultV2
		created bool
	)
	err := sqliteutil.RetryOnBusy(ctx, func() error {
		var attemptErr error
		result, created, attemptErr = s.finalizeRecognitionLayoutPlanV2Once(
			ctx,
			agentName,
			parentInvocationID,
		)
		return attemptErr
	})
	if err != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false, err
	}
	return result, created, nil
}

func (s *Store) finalizeRecognitionLayoutPlanV2Once(
	ctx context.Context,
	agentName string,
	parentInvocationID string,
) (k12.RecognitionLayoutPlanFinalizationResultV2, bool, error) {
	tx, opErr := s.db.BeginTx(ctx, nil)
	if opErr != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
			fmt.Errorf("k12storage: begin layout finalization: %w", opErr)
	}
	// 提交成功或主路径失败后，回滚仅用于释放事务；主路径错误保持原样。
	defer func() { _ = tx.Rollback() }()
	authority, opErr := loadRecognitionLayoutFinalizationAuthorityV2(
		ctx,
		tx,
		agentName,
		parentInvocationID,
	)
	if opErr != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false, opErr
	}
	if authority.Status == "succeeded" {
		projection, finalizationJSON, found, err :=
			loadStoredRecognitionLayoutFinalizationV2(ctx, tx, authority)
		if err != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, false, err
		}
		if !found {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
				fmt.Errorf(
					"%w: succeeded layout plan is missing its finalization receipt",
					ErrModelPhysicalInvocationConflict,
				)
		}
		if authority.Parent.Status != k12.ModelInvocationSent &&
			authority.Parent.Status != k12.ModelInvocationSucceeded {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
				fmt.Errorf(
					"%w: finalized layout parent status=%s",
					ErrModelPhysicalInvocationConflict,
					authority.Parent.Status,
				)
		}
		if _, err := validateStoredRecognitionLayoutFinalizationV2(
			ctx,
			tx,
			parentInvocationID,
			projection,
			finalizationJSON,
		); err != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
				fmt.Errorf("k12storage: commit stored layout finalization replay: %w", err)
		}
		return projection, false, nil
	}
	projection, finalizationJSON, opErr := reconstructRecognitionLayoutFinalizationV2(
		ctx,
		tx,
		authority,
	)
	if opErr != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false, opErr
	}
	receiptExists, opErr := validateStoredRecognitionLayoutFinalizationV2(
		ctx,
		tx,
		parentInvocationID,
		projection,
		finalizationJSON,
	)
	if opErr != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false, opErr
	}
	if receiptExists {
		if authority.Status != "succeeded" {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
				fmt.Errorf(
					"%w: finalization receipt exists while plan status=%s",
					ErrModelPhysicalInvocationConflict,
					authority.Status,
				)
		}
		if authority.Parent.Status != k12.ModelInvocationSent &&
			authority.Parent.Status != k12.ModelInvocationSucceeded {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
				fmt.Errorf(
					"%w: finalized layout parent status=%s",
					ErrModelPhysicalInvocationConflict,
					authority.Parent.Status,
				)
		}
		if err := tx.Commit(); err != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
				fmt.Errorf("k12storage: commit layout finalization replay: %w", err)
		}
		return projection, false, nil
	}
	if authority.Status == "succeeded" {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
			fmt.Errorf(
				"%w: succeeded layout plan is missing its finalization receipt",
				ErrModelPhysicalInvocationConflict,
			)
	}
	if authority.Status != "running" ||
		authority.Parent.Status != k12.ModelInvocationSent {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
			fmt.Errorf(
				"%w: layout plan status=%s parent status=%s cannot finalize",
				records.ErrIllegalTransition,
				authority.Status,
				authority.Parent.Status,
			)
	}
	createdAt := nowUnix()
	inserted, opErr := tx.ExecContext(
		ctx,
		`INSERT INTO k12_recognition_layout_finalizations (
            plan_id,parent_invocation_id,authorized_plan_digest,
            candidate_exact_set_digest,candidate_results_exact_set_digest,
            physical_results_exact_set_digest,candidate_result_count,
            physical_result_count,finalization_json,finalization_digest,created_at
         ) VALUES(?,?,?,?,?,?,?,?,?,?,?)
         ON CONFLICT(plan_id) DO NOTHING`,
		projection.PlanID,
		parentInvocationID,
		projection.PlanDigest,
		projection.CandidateExactSetDigest,
		projection.CandidateResultsExactSetDigest,
		projection.PhysicalResultsExactSetDigest,
		projection.CandidateResultCount,
		projection.PhysicalResultCount,
		string(finalizationJSON),
		projection.FinalizationDigest,
		createdAt,
	)
	if opErr != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
			fmt.Errorf("k12storage: append layout finalization receipt: %w", opErr)
	}
	insertedCount, _ := inserted.RowsAffected()
	if insertedCount != 1 {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
			fmt.Errorf(
				"%w: concurrent layout finalization receipt was not visible",
				ErrModelPhysicalInvocationConflict,
			)
	}
	updated, opErr := tx.ExecContext(
		ctx,
		`UPDATE k12_recognition_layout_plans
            SET status='succeeded',updated_at=?
          WHERE plan_id=? AND parent_invocation_id=? AND status='running'`,
		createdAt,
		projection.PlanID,
		parentInvocationID,
	)
	if opErr != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
			fmt.Errorf("k12storage: finalize layout-plan state: %w", opErr)
	}
	if affected, _ := updated.RowsAffected(); affected != 1 {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
			fmt.Errorf(
				"%w: layout plan lost running-to-succeeded CAS",
				records.ErrIllegalTransition,
			)
	}
	if err := tx.Commit(); err != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false,
			fmt.Errorf("k12storage: commit layout finalization: %w", err)
	}
	return projection, true, nil
}

func loadRecognitionLayoutFinalizationAuthorityV2(
	ctx context.Context,
	q dbQueryer,
	agentName string,
	parentInvocationID string,
) (recognitionLayoutFinalizationAuthorityV2, error) {
	parent, opErr := getModelInvocationByIDVia(ctx, q, parentInvocationID)
	if opErr != nil {
		return recognitionLayoutFinalizationAuthorityV2{}, opErr
	}
	if parent.AgentName != agentName || parent.Stage != k12.GradingStageRecognizing {
		return recognitionLayoutFinalizationAuthorityV2{}, fmt.Errorf(
			"%w: layout finalization parent ownership drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	var (
		authority                                     recognitionLayoutFinalizationAuthorityV2
		jobID, manifestID, pageDigest, manifestDigest string
		authorizedPlanJSON, layoutHeaderJSON          string
		stageStartedAt, stageDeadlineAt               int64
		selectedBucket, effectiveConcurrency          int
	)
	authority.Parent = parent
	opErr = q.QueryRowContext(
		ctx,
		`SELECT plan_id,job_id,manifest_physical_invocation_id,page_digest,
                header_digest,manifest_result_digest,authorized_plan_digest,
                candidate_exact_set_digest,authorized_plan_json,
                layout_header_json,stage_started_at,stage_deadline_at,
                selected_bucket_max_problems,effective_concurrency,status
           FROM k12_recognition_layout_plans
          WHERE parent_invocation_id=? AND agent_name=?`,
		parentInvocationID,
		agentName,
	).Scan(
		&authority.PlanID,
		&jobID,
		&manifestID,
		&pageDigest,
		&authority.HeaderDigest,
		&manifestDigest,
		&authority.Plan.AuthorizedPlanDigest,
		&authority.CandidateExactSetDigest,
		&authorizedPlanJSON,
		&layoutHeaderJSON,
		&stageStartedAt,
		&stageDeadlineAt,
		&selectedBucket,
		&effectiveConcurrency,
		&authority.Status,
	)
	if errors.Is(opErr, sql.ErrNoRows) {
		return recognitionLayoutFinalizationAuthorityV2{}, records.ErrNotFound
	}
	if opErr != nil {
		return recognitionLayoutFinalizationAuthorityV2{},
			fmt.Errorf("k12storage: read layout finalization authority: %w", opErr)
	}
	if jobID != parent.JobID || authority.Status == "" ||
		authority.Plan.AuthorizedPlanDigest == "" ||
		authority.CandidateExactSetDigest == "" || authorizedPlanJSON == "" {
		return recognitionLayoutFinalizationAuthorityV2{}, fmt.Errorf(
			"%w: incomplete layout finalization authority",
			ErrModelPhysicalInvocationConflict,
		)
	}
	if err := json.Unmarshal([]byte(authorizedPlanJSON), &authority.Plan); err != nil {
		return recognitionLayoutFinalizationAuthorityV2{},
			fmt.Errorf("k12storage: parse finalization authorized plan: %w", err)
	}
	canonicalPlanJSON, opErr := json.Marshal(authority.Plan)
	if opErr != nil || string(canonicalPlanJSON) != authorizedPlanJSON ||
		k12.ValidateRecognitionLayoutPlanV2(authority.Plan) != nil ||
		authority.Plan.AuthorizedPlanDigest == "" ||
		authority.Plan.ManifestInvocationID != manifestID ||
		authority.Plan.ManifestResultDigest != manifestDigest ||
		authority.Plan.PageDigest != pageDigest {
		return recognitionLayoutFinalizationAuthorityV2{}, fmt.Errorf(
			"%w: authorized layout plan is not canonical",
			ErrModelPhysicalInvocationConflict,
		)
	}
	var canonicalHeader struct {
		Contract string `json:"contract"`
		k12.RecognitionLayoutPlanHeaderV2
	}
	if err := json.Unmarshal([]byte(layoutHeaderJSON), &canonicalHeader); err != nil {
		return recognitionLayoutFinalizationAuthorityV2{},
			fmt.Errorf("k12storage: parse finalization layout header: %w", err)
	}
	headerJSON, headerDigest, opErr := k12.CanonicalRecognitionLayoutPlanHeaderV2(
		canonicalHeader.RecognitionLayoutPlanHeaderV2,
	)
	if opErr != nil || canonicalHeader.Contract != "recognition_layout_plan_header_v2" ||
		string(headerJSON) != layoutHeaderJSON || headerDigest != authority.HeaderDigest ||
		canonicalHeader.PlanID != authority.PlanID ||
		canonicalHeader.ParentInvocationID != parentInvocationID ||
		canonicalHeader.AgentName != agentName || canonicalHeader.JobID != parent.JobID ||
		canonicalHeader.PageDigest != pageDigest ||
		canonicalHeader.ParentRequestDigest != parent.RequestDigest ||
		canonicalHeader.StageStartedAtUnixMillis != stageStartedAt ||
		canonicalHeader.EffectiveConcurrency != effectiveConcurrency {
		return recognitionLayoutFinalizationAuthorityV2{}, fmt.Errorf(
			"%w: canonical layout header drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	authority.Header = canonicalHeader.RecognitionLayoutPlanHeaderV2
	bucket, durationMillis, opErr := authority.Header.BudgetBuckets.Select(
		len(authority.Plan.Targets),
	)
	if opErr != nil || bucket != selectedBucket ||
		stageDeadlineAt != stageStartedAt+durationMillis {
		return recognitionLayoutFinalizationAuthorityV2{}, fmt.Errorf(
			"%w: selected layout budget drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	targetIDs := make([]string, len(authority.Plan.Targets))
	for index, target := range authority.Plan.Targets {
		targetIDs[index] = target.TargetID
	}
	exactSetDigest, opErr := k12.RecognitionLayoutTargetExactSetDigestV2(targetIDs)
	if opErr != nil || exactSetDigest != authority.CandidateExactSetDigest {
		return recognitionLayoutFinalizationAuthorityV2{}, fmt.Errorf(
			"%w: layout candidate exact-set drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	if err := validateStoredRecognitionLayoutPlanV2(
		ctx,
		q,
		authority.PlanID,
		authority.Plan,
	); err != nil {
		return recognitionLayoutFinalizationAuthorityV2{}, err
	}
	return authority, nil
}

func reconstructRecognitionLayoutFinalizationV2(
	ctx context.Context,
	q dbQueryer,
	authority recognitionLayoutFinalizationAuthorityV2,
) (k12.RecognitionLayoutPlanFinalizationResultV2, []byte, error) {
	plan := authority.Plan
	physicalResults := make([]k12.RecognitionLayoutPhysicalResultEvidenceV2, 0)
	physicalByID := make(map[string]k12.RecognitionLayoutPhysicalResultEvidenceV2)
	appendPhysical := func(evidence k12.RecognitionLayoutPhysicalResultEvidenceV2) error {
		if _, duplicate := physicalByID[evidence.PhysicalInvocationID]; duplicate {
			return fmt.Errorf(
				"%w: duplicate physical evidence identity",
				ErrModelPhysicalInvocationConflict,
			)
		}
		physicalByID[evidence.PhysicalInvocationID] = evidence
		physicalResults = append(physicalResults, evidence)
		return nil
	}
	manifest, manifestErr := loadRecognitionLayoutFinalPhysicalEvidenceV2(
		ctx,
		q,
		authority.Parent,
		plan.ManifestInvocationID,
		k12.RecognitionPhysicalUnitWholePage,
		authority.HeaderDigest,
		"",
		plan.ManifestResultDigest,
	)
	if manifestErr != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, manifestErr
	}
	if err := appendPhysical(manifest); err != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, err
	}

	candidateExpectedSource := make(map[string]string, len(plan.Targets))
	repairs := make([]recognitionLayoutFinalRepairAuthorizationV2, 0)
	for _, batch := range plan.Batches {
		var (
			batchID, sourceID, sourceResultDigest, settlementDigest string
			sourceUnit                                              k12.RecognitionPhysicalUnit
			classification                                          k12.RecognitionLayoutBatchClassificationV2
			ambiguity                                               k12.RecognitionLayoutBatchAmbiguityKindV2
		)
		batchQueryErr := q.QueryRowContext(
			ctx,
			`SELECT batch_id,source_physical_invocation_id,
                    source_physical_unit,source_physical_result_digest,
                    classification,ambiguity_kind,settlement_digest
               FROM k12_recognition_layout_batch_settlements
              WHERE plan_id=? AND batch_id=?`,
			authority.PlanID,
			batch.Unit,
		).Scan(
			&batchID,
			&sourceID,
			&sourceUnit,
			&sourceResultDigest,
			&classification,
			&ambiguity,
			&settlementDigest,
		)
		if errors.Is(batchQueryErr, sql.ErrNoRows) {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
				fmt.Errorf(
					"%w: authorized primary batch has no classified settlement",
					records.ErrIllegalTransition,
				)
		}
		if batchQueryErr != nil || batchID != string(batch.Unit) || sourceUnit != batch.Unit ||
			classification != k12.RecognitionLayoutBatchClassifiedV2 || ambiguity != "" ||
			!validPrefixedSHA256DigestV2(settlementDigest) {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
				fmt.Errorf(
					"%w: primary batch settlement is terminal or drifted: %v",
					ErrModelPhysicalInvocationConflict,
					batchQueryErr,
				)
		}
		batchExactSet, batchExactSetErr := k12.RecognitionLayoutTargetExactSetDigestV2(batch.TargetIDs)
		if batchExactSetErr != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, batchExactSetErr
		}
		physical, physicalErr := loadRecognitionLayoutFinalPhysicalEvidenceV2(
			ctx,
			q,
			authority.Parent,
			sourceID,
			batch.Unit,
			plan.AuthorizedPlanDigest,
			batchExactSet,
			sourceResultDigest,
		)
		if physicalErr != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, physicalErr
		}
		if err := appendPhysical(physical); err != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, err
		}
		settlement := k12.RecognitionLayoutPrimaryBatchSettlementV2{
			PlanDigest:                 plan.AuthorizedPlanDigest,
			SourcePhysicalInvocationID: sourceID,
			SourcePhysicalUnit:         sourceUnit,
			SourcePhysicalResultDigest: sourceResultDigest,
			Classification:             k12.RecognitionLayoutBatchClassifiedV2,
			Candidates:                 make([]k12.RecognitionLayoutCandidateSettlementV2, 0, len(batch.TargetIDs)),
		}
		for _, candidateID := range batch.TargetIDs {
			repair, found, repairErr := loadRecognitionLayoutFinalRepairAuthorizationV2(
				ctx,
				q,
				authority.PlanID,
				candidateID,
			)
			if repairErr != nil {
				return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, repairErr
			}
			if found {
				if repair.SourceBatchID != batchID || repair.SourcePhysicalID != sourceID ||
					repair.SourceResultDigest != sourceResultDigest {
					return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
						fmt.Errorf(
							"%w: repair authorization changed source batch",
							ErrModelPhysicalInvocationConflict,
						)
				}
				repair.OriginalClassification, repairErr =
					inferRecognitionLayoutFinalRepairClassificationV2(
						settlement,
						repair,
					)
				if repairErr != nil {
					return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, repairErr
				}
				settlement.Candidates = append(
					settlement.Candidates,
					k12.RecognitionLayoutCandidateSettlementV2{
						CandidateID:    candidateID,
						Classification: repair.OriginalClassification,
					},
				)
				repairs = append(repairs, repair)
				continue
			}
			result, found, resultErr := loadRecognitionLayoutFinalCandidateResultV2(
				ctx,
				q,
				authority.PlanID,
				candidateID,
			)
			if resultErr != nil {
				return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, resultErr
			}
			if !found || result.SourcePhysicalInvocationID != sourceID ||
				result.SourcePhysicalUnit != batch.Unit ||
				result.SourcePhysicalResultDigest != sourceResultDigest {
				return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
					fmt.Errorf(
						"%w: primary candidate lacks exact frozen result",
						ErrModelPhysicalInvocationConflict,
					)
			}
			settlement.Candidates = append(
				settlement.Candidates,
				k12.RecognitionLayoutCandidateSettlementV2{
					CandidateID:    candidateID,
					Classification: k12.RecognitionLayoutCandidateValidV2,
					ResultKind:     result.ResultKind,
					ResultJSON:     append(json.RawMessage(nil), result.ResultJSON...),
				},
			)
			candidateExpectedSource[candidateID] = sourceID
		}
		if err := validateRecognitionLayoutPrimaryBatchSettlementV2(settlement); err != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, err
		}
		batchAuthority, authorityErr := loadRecognitionLayoutPrimaryBatchAuthorityV2(
			ctx,
			q,
			authority.Parent.AgentName,
			authority.Parent.InvocationID,
			settlement,
		)
		if authorityErr != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, authorityErr
		}
		projection, projectionErr := buildRecognitionLayoutPrimaryBatchProjectionV2(
			authority.Parent.InvocationID,
			settlement,
			batchAuthority,
		)
		if projectionErr != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, projectionErr
		}
		wantSettlementDigest, settlementDigestErr := recognitionLayoutPrimaryBatchSettlementDigestV2(
			authority.Parent.InvocationID,
			settlement,
			batchID,
		)
		if settlementDigestErr != nil || wantSettlementDigest != settlementDigest {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
				fmt.Errorf(
					"%w: primary settlement digest is not reproducible",
					ErrModelPhysicalInvocationConflict,
				)
		}
		projection.SettlementDigest = wantSettlementDigest
		if err := validateStoredRecognitionLayoutPrimaryBatchSettlementV2(
			ctx,
			q,
			authority.Parent.InvocationID,
			settlement,
			batchAuthority,
			projection,
		); err != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, err
		}
	}
	var primarySettlementCount int
	if err := q.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_batch_settlements WHERE plan_id=?`,
		authority.PlanID,
	).Scan(&primarySettlementCount); err != nil || primarySettlementCount != len(plan.Batches) {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
			fmt.Errorf(
				"%w: primary settlement exact-set cardinality drifted: %v",
				ErrModelPhysicalInvocationConflict,
				err,
			)
	}
	var repairAuthorizationCount int
	if err := q.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_repair_authorizations WHERE plan_id=?`,
		authority.PlanID,
	).Scan(&repairAuthorizationCount); err != nil || repairAuthorizationCount != len(repairs) {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
			fmt.Errorf(
				"%w: repair authorization exact-set cardinality drifted: %v",
				ErrModelPhysicalInvocationConflict,
				err,
			)
	}
	for _, repair := range repairs {
		var (
			authorizationID, authorizationDigest, candidateID string
			sourceID, sourceResultDigest, resultDigest        string
			settlementDigest                                  string
			sourceUnit                                        k12.RecognitionPhysicalUnit
			classification                                    k12.RecognitionLayoutCandidateClassificationV2
			resultKind                                        k12.RecognitionLayoutCandidateResultKindV2
		)
		settlementQueryErr := q.QueryRowContext(
			ctx,
			`SELECT repair_authorization_id,authorization_digest,candidate_id,
                    source_physical_invocation_id,source_physical_unit,
                    source_physical_result_digest,classification,result_kind,
                    result_digest,settlement_digest
               FROM k12_recognition_layout_repair_settlements
              WHERE plan_id=? AND candidate_id=?`,
			authority.PlanID,
			repair.CandidateID,
		).Scan(
			&authorizationID,
			&authorizationDigest,
			&candidateID,
			&sourceID,
			&sourceUnit,
			&sourceResultDigest,
			&classification,
			&resultKind,
			&resultDigest,
			&settlementDigest,
		)
		if errors.Is(settlementQueryErr, sql.ErrNoRows) {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
				fmt.Errorf(
					"%w: authorized singleton repair has no settlement",
					records.ErrIllegalTransition,
				)
		}
		if settlementQueryErr != nil || authorizationID != repair.AuthorizationID ||
			authorizationDigest != repair.AuthorizationDigest ||
			candidateID != repair.CandidateID || sourceUnit != repair.PhysicalUnit ||
			classification != k12.RecognitionLayoutCandidateValidV2 ||
			(resultKind != k12.RecognitionLayoutCandidateQuestionV2 &&
				resultKind != k12.RecognitionLayoutCandidateNonQuestionV2) {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
				fmt.Errorf(
					"%w: repair settlement is invalid or drifted: %v",
					ErrModelPhysicalInvocationConflict,
					settlementQueryErr,
				)
		}
		result, found, resultErr := loadRecognitionLayoutFinalCandidateResultV2(
			ctx,
			q,
			authority.PlanID,
			repair.CandidateID,
		)
		if resultErr != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, resultErr
		}
		if !found || result.SourcePhysicalInvocationID != sourceID ||
			result.SourcePhysicalUnit != sourceUnit ||
			result.SourcePhysicalResultDigest != sourceResultDigest ||
			result.ResultKind != resultKind || result.ResultDigest != resultDigest {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
				fmt.Errorf(
					"%w: repaired candidate result is missing or detached",
					ErrModelPhysicalInvocationConflict,
				)
		}
		settlement := k12.RecognitionLayoutRepairSettlementV2{
			PlanDigest:                 plan.AuthorizedPlanDigest,
			AuthorizationID:            authorizationID,
			AuthorizationDigest:        authorizationDigest,
			CandidateID:                candidateID,
			SourcePhysicalInvocationID: sourceID,
			SourcePhysicalUnit:         sourceUnit,
			SourcePhysicalResultDigest: sourceResultDigest,
			Classification:             classification,
			ResultKind:                 resultKind,
			ResultJSON:                 append(json.RawMessage(nil), result.ResultJSON...),
		}
		if err := validateRecognitionLayoutRepairSettlementV2(settlement); err != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, err
		}
		repairAuthority, authorityErr := loadRecognitionLayoutRepairSettlementAuthorityV2(
			ctx,
			q,
			authority.Parent.AgentName,
			authority.Parent.InvocationID,
			settlement,
		)
		if authorityErr != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, authorityErr
		}
		repairProjection, wantResultDigest, projectionErr := buildRecognitionLayoutRepairProjectionV2(
			authority.Parent.InvocationID,
			settlement,
		)
		if projectionErr != nil || wantResultDigest != resultDigest {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
				fmt.Errorf(
					"%w: repair candidate result digest is not reproducible",
					ErrModelPhysicalInvocationConflict,
				)
		}
		wantSettlementDigest, settlementDigestErr := recognitionLayoutRepairSettlementDigestV2(
			authority.Parent.InvocationID,
			settlement,
		)
		if settlementDigestErr != nil || wantSettlementDigest != settlementDigest {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
				fmt.Errorf(
					"%w: repair settlement digest is not reproducible",
					ErrModelPhysicalInvocationConflict,
				)
		}
		repairProjection.SettlementDigest = wantSettlementDigest
		stored, receiptErr := validateStoredRecognitionLayoutRepairSettlementV2(
			ctx,
			q,
			authority.Parent.InvocationID,
			settlement,
			repairAuthority,
			repairProjection,
			wantResultDigest,
		)
		if receiptErr != nil || !stored {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
				fmt.Errorf(
					"%w: repair settlement receipt is incomplete: %v",
					ErrModelPhysicalInvocationConflict,
					receiptErr,
				)
		}
		repairExactSet, exactSetErr := k12.RecognitionLayoutTargetExactSetDigestV2(
			[]string{repair.CandidateID},
		)
		if exactSetErr != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, exactSetErr
		}
		physical, physicalErr := loadRecognitionLayoutFinalPhysicalEvidenceV2(
			ctx,
			q,
			authority.Parent,
			sourceID,
			sourceUnit,
			plan.AuthorizedPlanDigest,
			repairExactSet,
			sourceResultDigest,
		)
		if physicalErr != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, physicalErr
		}
		if err := appendPhysical(physical); err != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, err
		}
		candidateExpectedSource[repair.CandidateID] = sourceID
	}
	var repairSettlementCount int
	if err := q.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_repair_settlements WHERE plan_id=?`,
		authority.PlanID,
	).Scan(&repairSettlementCount); err != nil || repairSettlementCount != len(repairs) {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
			fmt.Errorf(
				"%w: repair settlement exact-set cardinality drifted: %v",
				ErrModelPhysicalInvocationConflict,
				err,
			)
	}
	var physicalCount int
	if err := q.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_model_physical_invocations
          WHERE parent_invocation_id=? AND recognition_plan_version='v2'`,
		authority.Parent.InvocationID,
	).Scan(&physicalCount); err != nil || physicalCount != len(physicalResults) {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
			fmt.Errorf(
				"%w: V2 physical child exact-set cardinality drifted: %v",
				ErrModelPhysicalInvocationConflict,
				err,
			)
	}
	candidateResults := make([]k12.RecognitionLayoutCandidateFinalResultV2, 0, len(plan.Targets))
	for _, target := range plan.Targets {
		result, found, resultErr := loadRecognitionLayoutFinalCandidateResultV2(
			ctx,
			q,
			authority.PlanID,
			target.TargetID,
		)
		if resultErr != nil {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, resultErr
		}
		expectedSourceID, hasExpectedSource := candidateExpectedSource[target.TargetID]
		physical, sourceExists := physicalByID[result.SourcePhysicalInvocationID]
		if !found || !hasExpectedSource || !sourceExists ||
			result.SourcePhysicalInvocationID != expectedSourceID ||
			result.SourcePhysicalUnit != physical.PhysicalUnit ||
			result.SourcePhysicalResultDigest != physical.ResultDigest {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
				fmt.Errorf(
					"%w: candidate result exact-set or source order drifted",
					ErrModelPhysicalInvocationConflict,
				)
		}
		candidate := k12.RecognitionLayoutCandidateSettlementV2{
			CandidateID:    result.CandidateID,
			Classification: k12.RecognitionLayoutCandidateValidV2,
			ResultKind:     result.ResultKind,
			ResultJSON:     append(json.RawMessage(nil), result.ResultJSON...),
		}
		wantResultDigest, resultDigestErr := recognitionLayoutCandidateResultDigestV2(
			authority.Parent.InvocationID,
			k12.RecognitionLayoutPrimaryBatchSettlementV2{
				PlanDigest:                 plan.AuthorizedPlanDigest,
				SourcePhysicalInvocationID: physical.PhysicalInvocationID,
				SourcePhysicalUnit:         physical.PhysicalUnit,
				SourcePhysicalResultDigest: physical.ResultDigest,
			},
			candidate,
		)
		if resultDigestErr != nil || wantResultDigest != result.ResultDigest {
			return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
				fmt.Errorf(
					"%w: candidate result digest is not reproducible",
					ErrModelPhysicalInvocationConflict,
				)
		}
		candidateResults = append(candidateResults, result)
	}
	var candidateResultCount int
	if err := q.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_candidate_results WHERE plan_id=?`,
		authority.PlanID,
	).Scan(&candidateResultCount); err != nil || candidateResultCount != len(plan.Targets) {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil,
			fmt.Errorf(
				"%w: candidate result exact-set cardinality drifted: %v",
				ErrModelPhysicalInvocationConflict,
				err,
			)
	}
	candidateResultsDigest, candidateDigestErr := k12.RecognitionLayoutCandidateResultsExactSetDigestV2(
		candidateResults,
	)
	if candidateDigestErr != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, candidateDigestErr
	}
	physicalResultsDigest, physicalDigestErr := k12.RecognitionLayoutPhysicalResultsExactSetDigestV2(
		physicalResults,
	)
	if physicalDigestErr != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, physicalDigestErr
	}
	projection := k12.RecognitionLayoutPlanFinalizationResultV2{
		PlanID:                         authority.PlanID,
		PlanDigest:                     plan.AuthorizedPlanDigest,
		CandidateExactSetDigest:        authority.CandidateExactSetDigest,
		CandidateResultsExactSetDigest: candidateResultsDigest,
		PhysicalResultsExactSetDigest:  physicalResultsDigest,
		CandidateResultCount:           len(candidateResults),
		PhysicalResultCount:            len(physicalResults),
		CandidateResults:               candidateResults,
		PhysicalResults:                physicalResults,
	}
	finalizationJSON, finalizationDigest, finalizationErr := k12.CanonicalRecognitionLayoutPlanFinalizationV2(
		authority.Parent.InvocationID,
		projection,
	)
	if finalizationErr != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, nil, finalizationErr
	}
	projection.FinalizationDigest = finalizationDigest
	return projection, finalizationJSON, nil
}

func loadRecognitionLayoutFinalPhysicalEvidenceV2(
	ctx context.Context,
	q dbQueryer,
	parent k12.ModelInvocation,
	physicalInvocationID string,
	physicalUnit k12.RecognitionPhysicalUnit,
	planDigest string,
	candidateExactSetDigest string,
	resultDigest string,
) (k12.RecognitionLayoutPhysicalResultEvidenceV2, error) {
	physical, err := getModelPhysicalInvocationByIDVia(
		ctx,
		q,
		parent.AgentName,
		physicalInvocationID,
	)
	if err != nil {
		return k12.RecognitionLayoutPhysicalResultEvidenceV2{}, err
	}
	if err := validatePhysicalInvocationParent(physical, parent); err != nil {
		return k12.RecognitionLayoutPhysicalResultEvidenceV2{}, err
	}
	var resultContent sql.NullString
	if err := q.QueryRowContext(
		ctx,
		`SELECT result_content FROM k12_model_physical_invocations
          WHERE physical_invocation_id=? AND agent_name=?`,
		physicalInvocationID,
		parent.AgentName,
	).Scan(&resultContent); err != nil || !resultContent.Valid ||
		physicalInvocationResultDigest(resultContent.String) != physical.ResultDigest {
		return k12.RecognitionLayoutPhysicalResultEvidenceV2{}, fmt.Errorf(
			"%w: physical result private content drifted: %v",
			ErrModelPhysicalInvocationConflict,
			err,
		)
	}
	if physical.PhysicalUnit != physicalUnit ||
		physical.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
		physical.PlanDigest != planDigest ||
		physical.CandidateExactSetDigest != candidateExactSetDigest ||
		physical.Status != k12.ModelInvocationSucceeded || physical.Attempt != 1 ||
		physical.ResultDigest != resultDigest {
		return k12.RecognitionLayoutPhysicalResultEvidenceV2{}, fmt.Errorf(
			"%w: physical result is not exact succeeded V2 evidence",
			ErrModelPhysicalInvocationConflict,
		)
	}
	return k12.RecognitionLayoutPhysicalResultEvidenceV2{
		PhysicalInvocationID:    physical.PhysicalInvocationID,
		PhysicalUnit:            physical.PhysicalUnit,
		ResultDigest:            physical.ResultDigest,
		PlanDigest:              physical.PlanDigest,
		CandidateExactSetDigest: physical.CandidateExactSetDigest,
		Attempt:                 physical.Attempt,
	}, nil
}

func loadRecognitionLayoutFinalCandidateResultV2(
	ctx context.Context,
	q dbQueryer,
	planID string,
	candidateID string,
) (k12.RecognitionLayoutCandidateFinalResultV2, bool, error) {
	var result k12.RecognitionLayoutCandidateFinalResultV2
	var resultJSON string
	err := q.QueryRowContext(
		ctx,
		`SELECT result.candidate_id,result.result_kind,result.result_digest,
                result.result_json,result.source_physical_invocation_id,
                child.physical_unit,result.source_physical_result_digest
           FROM k12_recognition_layout_candidate_results result
           JOIN k12_model_physical_invocations child
             ON child.physical_invocation_id=result.source_physical_invocation_id
            AND child.parent_invocation_id=result.parent_invocation_id
          WHERE result.plan_id=? AND result.candidate_id=?`,
		planID,
		candidateID,
	).Scan(
		&result.CandidateID,
		&result.ResultKind,
		&result.ResultDigest,
		&resultJSON,
		&result.SourcePhysicalInvocationID,
		&result.SourcePhysicalUnit,
		&result.SourcePhysicalResultDigest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return k12.RecognitionLayoutCandidateFinalResultV2{}, false, nil
	}
	if err != nil {
		return k12.RecognitionLayoutCandidateFinalResultV2{}, false,
			fmt.Errorf("k12storage: read final candidate result: %w", err)
	}
	result.ResultJSON = json.RawMessage(resultJSON)
	if result.CandidateID != candidateID ||
		(result.ResultKind != k12.RecognitionLayoutCandidateQuestionV2 &&
			result.ResultKind != k12.RecognitionLayoutCandidateNonQuestionV2) ||
		!validPrefixedSHA256DigestV2(result.ResultDigest) ||
		!validPrefixedSHA256DigestV2(result.SourcePhysicalResultDigest) {
		return k12.RecognitionLayoutCandidateFinalResultV2{}, false,
			fmt.Errorf(
				"%w: final candidate result identity drifted",
				ErrModelPhysicalInvocationConflict,
			)
	}
	if _, err := canonicalRecognitionLayoutResultJSONV2(result.ResultJSON); err != nil {
		return k12.RecognitionLayoutCandidateFinalResultV2{}, false, err
	}
	return result, true, nil
}

func loadRecognitionLayoutFinalRepairAuthorizationV2(
	ctx context.Context,
	q dbQueryer,
	planID string,
	candidateID string,
) (recognitionLayoutFinalRepairAuthorizationV2, bool, error) {
	var repair recognitionLayoutFinalRepairAuthorizationV2
	var repairRound int
	err := q.QueryRowContext(
		ctx,
		`SELECT repair.repair_authorization_id,repair.authorization_digest,
                repair.candidate_id,candidate.ordinal,
                repair.repair_physical_unit,repair.source_batch_id,
                repair.source_batch_physical_invocation_id,
                repair.source_batch_result_digest,repair.repair_round
           FROM k12_recognition_layout_repair_authorizations repair
           JOIN k12_recognition_layout_candidates candidate
             ON candidate.plan_id=repair.plan_id
            AND candidate.candidate_id=repair.candidate_id
          WHERE repair.plan_id=? AND repair.candidate_id=?`,
		planID,
		candidateID,
	).Scan(
		&repair.AuthorizationID,
		&repair.AuthorizationDigest,
		&repair.CandidateID,
		&repair.CandidateOrdinal,
		&repair.PhysicalUnit,
		&repair.SourceBatchID,
		&repair.SourcePhysicalID,
		&repair.SourceResultDigest,
		&repairRound,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return recognitionLayoutFinalRepairAuthorizationV2{}, false, nil
	}
	if err != nil {
		return recognitionLayoutFinalRepairAuthorizationV2{}, false,
			fmt.Errorf("k12storage: read final repair authorization: %w", err)
	}
	wantUnit, err := k12.RecognitionLayoutRepairUnitV2(repair.CandidateOrdinal)
	if err != nil || repair.CandidateID != candidateID || repairRound != 1 ||
		repair.PhysicalUnit != wantUnit ||
		!validPrefixedSHA256DigestV2(repair.AuthorizationDigest) ||
		repair.AuthorizationID != recognitionLayoutRepairAuthorizationIDV2(
			repair.AuthorizationDigest,
		) {
		return recognitionLayoutFinalRepairAuthorizationV2{}, false,
			fmt.Errorf(
				"%w: repair authorization identity drifted",
				ErrModelPhysicalInvocationConflict,
			)
	}
	return repair, true, nil
}

func inferRecognitionLayoutFinalRepairClassificationV2(
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
	repair recognitionLayoutFinalRepairAuthorizationV2,
) (k12.RecognitionLayoutCandidateClassificationV2, error) {
	var matched k12.RecognitionLayoutCandidateClassificationV2
	for _, classification := range []k12.RecognitionLayoutCandidateClassificationV2{
		k12.RecognitionLayoutCandidateMissingV2,
		k12.RecognitionLayoutCandidateInvalidV2,
	} {
		candidate := k12.RecognitionLayoutCandidateSettlementV2{
			CandidateID:    repair.CandidateID,
			Classification: classification,
		}
		digest, err := recognitionLayoutRepairAuthorizationDigestV2(
			settlement,
			repair.SourceBatchID,
			candidate,
			repair.PhysicalUnit,
		)
		if err == nil && digest == repair.AuthorizationDigest &&
			repair.AuthorizationID == recognitionLayoutRepairAuthorizationIDV2(digest) {
			if matched != "" {
				return "", fmt.Errorf(
					"%w: repair authorization classification is ambiguous",
					ErrModelPhysicalInvocationConflict,
				)
			}
			matched = classification
		}
	}
	if matched == "" {
		return "", fmt.Errorf(
			"%w: repair authorization digest is not reproducible",
			ErrModelPhysicalInvocationConflict,
		)
	}
	return matched, nil
}

func validateStoredRecognitionLayoutFinalizationV2(
	ctx context.Context,
	q dbQueryer,
	parentInvocationID string,
	result k12.RecognitionLayoutPlanFinalizationResultV2,
	finalizationJSON []byte,
) (bool, error) {
	var (
		storedParentID, planDigest, candidateExactSetDigest string
		candidateResultsDigest, physicalResultsDigest       string
		storedJSON, storedDigest                            string
		candidateCount, physicalCount                       int
	)
	err := q.QueryRowContext(
		ctx,
		`SELECT parent_invocation_id,authorized_plan_digest,
                candidate_exact_set_digest,candidate_results_exact_set_digest,
                physical_results_exact_set_digest,candidate_result_count,
                physical_result_count,finalization_json,finalization_digest
           FROM k12_recognition_layout_finalizations WHERE plan_id=?`,
		result.PlanID,
	).Scan(
		&storedParentID,
		&planDigest,
		&candidateExactSetDigest,
		&candidateResultsDigest,
		&physicalResultsDigest,
		&candidateCount,
		&physicalCount,
		&storedJSON,
		&storedDigest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("k12storage: read layout finalization receipt: %w", err)
	}
	if storedParentID != parentInvocationID || planDigest != result.PlanDigest ||
		candidateExactSetDigest != result.CandidateExactSetDigest ||
		candidateResultsDigest != result.CandidateResultsExactSetDigest ||
		physicalResultsDigest != result.PhysicalResultsExactSetDigest ||
		candidateCount != result.CandidateResultCount ||
		physicalCount != result.PhysicalResultCount ||
		storedJSON != string(finalizationJSON) || storedDigest != result.FinalizationDigest {
		return false, fmt.Errorf(
			"%w: layout finalization receipt drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	return true, nil
}

func (s *Store) authorizeRecognitionLayoutPlanV2Once(
	ctx context.Context,
	agentName string,
	parentInvocationID string,
	manifest k12.RecognitionLayoutManifestSuccessV2,
	plan k12.RecognitionLayoutPlanV2,
) error {
	tx, opErr := s.db.BeginTx(ctx, nil)
	if opErr != nil {
		return fmt.Errorf("k12storage: begin layout authorization: %w", opErr)
	}
	// 提交成功或主路径失败后，回滚仅用于释放事务；主路径错误保持原样。
	defer func() { _ = tx.Rollback() }()
	var (
		planID, jobID, manifestID, pageDigest, headerDigest string
		status, storedManifestDigest, storedPlanDigest      string
		storedExactSetDigest, storedPlanJSON                string
		layoutHeaderJSON                                    string
		stageStartedAt, stageDeadlineAt                     int64
		selectedBucketMaxProblems                           int
	)
	opErr = tx.QueryRowContext(
		ctx,
		`SELECT plan_id,job_id,manifest_physical_invocation_id,page_digest,
                header_digest,status,manifest_result_digest,
                authorized_plan_digest,candidate_exact_set_digest,
                authorized_plan_json,layout_header_json,stage_started_at,
                stage_deadline_at,selected_bucket_max_problems
           FROM k12_recognition_layout_plans
          WHERE parent_invocation_id=? AND agent_name=?`,
		parentInvocationID,
		agentName,
	).Scan(
		&planID,
		&jobID,
		&manifestID,
		&pageDigest,
		&headerDigest,
		&status,
		&storedManifestDigest,
		&storedPlanDigest,
		&storedExactSetDigest,
		&storedPlanJSON,
		&layoutHeaderJSON,
		&stageStartedAt,
		&stageDeadlineAt,
		&selectedBucketMaxProblems,
	)
	if errors.Is(opErr, sql.ErrNoRows) {
		return fmt.Errorf(
			"%w: V2 layout-plan header is missing",
			records.ErrIllegalTransition,
		)
	}
	if opErr != nil {
		return fmt.Errorf("k12storage: read layout-plan header: %w", opErr)
	}
	if jobID == "" || manifestID != manifest.InvocationID ||
		pageDigest != plan.PageDigest {
		return fmt.Errorf(
			"%w: layout plan page or manifest identity drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	var canonicalHeader struct {
		Contract string `json:"contract"`
		k12.RecognitionLayoutPlanHeaderV2
	}
	if err := json.Unmarshal([]byte(layoutHeaderJSON), &canonicalHeader); err != nil {
		return fmt.Errorf("k12storage: parse canonical layout header: %w", err)
	}
	headerJSON, recomputedHeaderDigest, opErr :=
		k12.CanonicalRecognitionLayoutPlanHeaderV2(
			canonicalHeader.RecognitionLayoutPlanHeaderV2,
		)
	if opErr != nil || canonicalHeader.Contract != "recognition_layout_plan_header_v2" ||
		string(headerJSON) != layoutHeaderJSON ||
		recomputedHeaderDigest != headerDigest ||
		canonicalHeader.StageStartedAtUnixMillis != stageStartedAt {
		return fmt.Errorf(
			"%w: canonical layout header digest or start time drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	selectedBucket, selectedDurationMillis, opErr :=
		canonicalHeader.BudgetBuckets.Select(len(plan.Targets))
	if opErr != nil {
		return opErr
	}
	selectedDeadline := stageStartedAt + selectedDurationMillis
	var (
		manifestParent, manifestAgent, manifestJob string
		manifestUnit                               k12.RecognitionPhysicalUnit
		manifestStatus                             k12.ModelInvocationStatus
		manifestResultDigest                       string
		manifestContent                            sql.NullString
		manifestPlanVersion                        string
		manifestPlanDigest                         string
		manifestExactSetDigest                     string
	)
	opErr = tx.QueryRowContext(
		ctx,
		`SELECT parent_invocation_id,agent_name,job_id,physical_unit,status,
                result_digest,result_content,recognition_plan_version,
                plan_digest,candidate_exact_set_digest
           FROM k12_model_physical_invocations
          WHERE physical_invocation_id=?`,
		manifestID,
	).Scan(
		&manifestParent,
		&manifestAgent,
		&manifestJob,
		&manifestUnit,
		&manifestStatus,
		&manifestResultDigest,
		&manifestContent,
		&manifestPlanVersion,
		&manifestPlanDigest,
		&manifestExactSetDigest,
	)
	if opErr != nil {
		return fmt.Errorf("k12storage: read succeeded V2 manifest: %w", opErr)
	}
	if manifestParent != parentInvocationID || manifestAgent != agentName ||
		manifestJob != jobID ||
		manifestUnit != k12.RecognitionPhysicalUnitWholePage ||
		manifestStatus != k12.ModelInvocationSucceeded ||
		manifestResultDigest != manifest.ResultDigest ||
		!manifestContent.Valid ||
		manifestResultDigest != physicalInvocationResultDigest(manifestContent.String) ||
		manifestPlanVersion != "v2" || manifestPlanDigest != headerDigest ||
		manifestExactSetDigest != "" {
		return fmt.Errorf(
			"%w: layout authorization is detached from exact succeeded manifest content",
			ErrModelPhysicalInvocationConflict,
		)
	}
	targetIDs := make([]string, len(plan.Targets))
	for index := range plan.Targets {
		targetIDs[index] = plan.Targets[index].TargetID
	}
	exactSetDigest, opErr := k12.RecognitionLayoutTargetExactSetDigestV2(targetIDs)
	if opErr != nil {
		return opErr
	}
	planJSON, opErr := json.Marshal(plan)
	if opErr != nil {
		return fmt.Errorf("k12storage: encode authorized layout plan: %w", opErr)
	}
	if status == "authorized" || status == "running" || status == "succeeded" {
		if storedManifestDigest != manifest.ResultDigest ||
			storedPlanDigest != plan.AuthorizedPlanDigest ||
			storedExactSetDigest != exactSetDigest ||
			storedPlanJSON != string(planJSON) ||
			selectedBucketMaxProblems != selectedBucket ||
			stageDeadlineAt != selectedDeadline {
			return fmt.Errorf(
				"%w: authorized layout-plan replay changed immutable facts",
				ErrModelPhysicalInvocationConflict,
			)
		}
		if err := validateStoredRecognitionLayoutPlanV2(
			ctx,
			tx,
			planID,
			plan,
		); err != nil {
			return err
		}
		return tx.Commit()
	}
	if status != "manifest_succeeded" ||
		storedManifestDigest != manifest.ResultDigest ||
		selectedBucketMaxProblems != 0 || stageDeadlineAt != 0 {
		return fmt.Errorf(
			"%w: layout plan status %s cannot be authorized",
			records.ErrIllegalTransition,
			status,
		)
	}
	createdAt := nowUnix()
	for index, target := range plan.Targets {
		candidateJSON, marshalErr := json.Marshal(target)
		if marshalErr != nil {
			return fmt.Errorf("k12storage: encode layout target: %w", marshalErr)
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO k12_recognition_layout_candidates (
                    plan_id,candidate_id,ordinal,bbox_x,bbox_y,bbox_width,
                    bbox_height,crop_digest,candidate_json,created_at
                 ) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			planID,
			target.TargetID,
			index+1,
			target.Region.X,
			target.Region.Y,
			target.Region.Width,
			target.Region.Height,
			target.CropDigest,
			string(candidateJSON),
			createdAt,
		); err != nil {
			return fmt.Errorf("k12storage: persist layout target %d: %w", index+1, err)
		}
	}
	for index, batch := range plan.Batches {
		batchID := string(batch.Unit)
		batchDigest, digestErr := recognitionLayoutBatchDigestV2(batch)
		if digestErr != nil {
			return digestErr
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO k12_recognition_layout_batches (
                    plan_id,batch_id,ordinal,physical_unit,member_count,
                    batch_digest,input_digest,created_at
                 ) VALUES(?,?,?,?,?,?,?,?)`,
			planID,
			batchID,
			index+1,
			batch.Unit,
			len(batch.TargetIDs),
			batchDigest,
			batch.InputDigest,
			createdAt,
		); err != nil {
			return fmt.Errorf("k12storage: persist layout batch %d: %w", index+1, err)
		}
		for slot, targetID := range batch.TargetIDs {
			if _, err := tx.ExecContext(
				ctx,
				`INSERT INTO k12_recognition_layout_batch_members (
                        plan_id,batch_id,slot,candidate_id,created_at
                     ) VALUES(?,?,?,?,?)`,
				planID,
				batchID,
				slot,
				targetID,
				createdAt,
			); err != nil {
				return fmt.Errorf(
					"k12storage: persist layout batch %d member %d: %w",
					index+1,
					slot,
					err,
				)
			}
		}
	}
	res, opErr := tx.ExecContext(
		ctx,
		`UPDATE k12_recognition_layout_plans
            SET authorized_plan_digest=?,candidate_exact_set_digest=?,
                authorized_plan_json=?,selected_bucket_max_problems=?,
                stage_deadline_at=?,status='authorized',updated_at=?
          WHERE plan_id=? AND parent_invocation_id=?
            AND status='manifest_succeeded'
            AND manifest_result_digest=?`,
		plan.AuthorizedPlanDigest,
		exactSetDigest,
		string(planJSON),
		selectedBucket,
		selectedDeadline,
		createdAt,
		planID,
		parentInvocationID,
		manifest.ResultDigest,
	)
	if opErr != nil {
		return fmt.Errorf("k12storage: authorize layout-plan header: %w", opErr)
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return fmt.Errorf(
			"%w: layout-plan header lost manifest authorization",
			records.ErrIllegalTransition,
		)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("k12storage: commit layout authorization: %w", err)
	}
	return nil
}

func recognitionLayoutBatchDigestV2(
	batch k12.RecognitionLayoutBatchV2,
) (string, error) {
	encoded, err := json.Marshal(struct {
		Contract    string                      `json:"contract"`
		Unit        k12.RecognitionPhysicalUnit `json:"unit"`
		TargetIDs   []string                    `json:"target_ids"`
		InputDigest string                      `json:"input_digest"`
	}{
		Contract:    "recognition_layout_batch_v2",
		Unit:        batch.Unit,
		TargetIDs:   batch.TargetIDs,
		InputDigest: batch.InputDigest,
	})
	if err != nil {
		return "", fmt.Errorf("k12storage: encode layout batch: %w", err)
	}
	return physicalInvocationResultDigest(string(encoded)), nil
}

func validateStoredRecognitionLayoutPlanV2(
	ctx context.Context,
	q dbQueryer,
	planID string,
	plan k12.RecognitionLayoutPlanV2,
) error {
	var candidateCount, batchCount, memberCount int
	if err := q.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_candidates WHERE plan_id=?`,
		planID,
	).Scan(&candidateCount); err != nil {
		return fmt.Errorf("k12storage: count layout candidates: %w", err)
	}
	if err := q.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_batches WHERE plan_id=?`,
		planID,
	).Scan(&batchCount); err != nil {
		return fmt.Errorf("k12storage: count layout batches: %w", err)
	}
	if err := q.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_batch_members WHERE plan_id=?`,
		planID,
	).Scan(&memberCount); err != nil {
		return fmt.Errorf("k12storage: count layout batch members: %w", err)
	}
	if candidateCount != len(plan.Targets) || batchCount != len(plan.Batches) ||
		memberCount != len(plan.Targets) {
		return fmt.Errorf(
			"%w: persisted layout-plan cardinality drifted",
			ErrModelPhysicalInvocationConflict,
		)
	}
	for index, target := range plan.Targets {
		candidateJSON, err := json.Marshal(target)
		if err != nil {
			return err
		}
		var (
			ordinal                int
			x, y, width, height    int
			cropDigest, storedJSON string
		)
		err = q.QueryRowContext(
			ctx,
			`SELECT ordinal,bbox_x,bbox_y,bbox_width,bbox_height,
                    crop_digest,candidate_json
               FROM k12_recognition_layout_candidates
              WHERE plan_id=? AND candidate_id=?`,
			planID,
			target.TargetID,
		).Scan(&ordinal, &x, &y, &width, &height, &cropDigest, &storedJSON)
		if err != nil || ordinal != index+1 || x != target.Region.X ||
			y != target.Region.Y || width != target.Region.Width ||
			height != target.Region.Height || cropDigest != target.CropDigest ||
			storedJSON != string(candidateJSON) {
			return fmt.Errorf(
				"%w: persisted layout target %d drifted: %v",
				ErrModelPhysicalInvocationConflict,
				index+1,
				err,
			)
		}
	}
	for index, batch := range plan.Batches {
		batchDigest, err := recognitionLayoutBatchDigestV2(batch)
		if err != nil {
			return err
		}
		var ordinal, count int
		var storedDigest, inputDigest string
		err = q.QueryRowContext(
			ctx,
			`SELECT ordinal,member_count,batch_digest,input_digest
               FROM k12_recognition_layout_batches
              WHERE plan_id=? AND batch_id=? AND physical_unit=?`,
			planID,
			batch.Unit,
			batch.Unit,
		).Scan(&ordinal, &count, &storedDigest, &inputDigest)
		if err != nil || ordinal != index+1 || count != len(batch.TargetIDs) ||
			storedDigest != batchDigest || inputDigest != batch.InputDigest {
			return fmt.Errorf(
				"%w: persisted layout batch %d drifted: %v",
				ErrModelPhysicalInvocationConflict,
				index+1,
				err,
			)
		}
		rows, err := q.QueryContext(
			ctx,
			`SELECT candidate_id FROM k12_recognition_layout_batch_members
              WHERE plan_id=? AND batch_id=? ORDER BY slot`,
			planID,
			batch.Unit,
		)
		if err != nil {
			return fmt.Errorf("k12storage: list persisted batch members: %w", err)
		}
		storedIDs := make([]string, 0, len(batch.TargetIDs))
		for rows.Next() {
			var targetID string
			if scanErr := rows.Scan(&targetID); scanErr != nil {
				return errors.Join(scanErr, rows.Close())
			}
			storedIDs = append(storedIDs, targetID)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil || closeErr != nil || len(storedIDs) != len(batch.TargetIDs) {
			return fmt.Errorf(
				"%w: persisted batch member count drifted: %v",
				ErrModelPhysicalInvocationConflict,
				errors.Join(rowsErr, closeErr),
			)
		}
		for slot := range storedIDs {
			if storedIDs[slot] != batch.TargetIDs[slot] {
				return fmt.Errorf(
					"%w: persisted batch member order drifted",
					ErrModelPhysicalInvocationConflict,
				)
			}
		}
	}
	return nil
}

// ValidateRecognitionFallbackAuthorization proves that the private
// authorization used to unlock a seven-call fallback still binds the exact
// succeeded whole_page child, content, and Store-computed digest. No private
// content is returned to the caller.
func (s *Store) ValidateRecognitionFallbackAuthorization(
	ctx context.Context,
	agentName string,
	parentInvocationID string,
	wholePhysicalInvocationID string,
) error {
	agentName = strings.TrimSpace(agentName)
	parentInvocationID = strings.TrimSpace(parentInvocationID)
	wholePhysicalInvocationID = strings.TrimSpace(
		wholePhysicalInvocationID,
	)
	var (
		authorizationAgent   string
		authorizationJob     string
		authorizationWholeID string
		authorizationDigest  string
		authorizationContent string
		wholeParentID        string
		wholeAgent           string
		wholeJob             string
		wholeUnit            k12.RecognitionPhysicalUnit
		wholeStatus          k12.ModelInvocationStatus
		wholeDigest          string
		wholeContent         sql.NullString
	)
	err := s.db.QueryRowContext(
		ctx,
		`SELECT authorization.agent_name,
                authorization.job_id,
                authorization.whole_physical_invocation_id,
                authorization.whole_result_digest,
                authorization.whole_result_content,
                whole.parent_invocation_id,
                whole.agent_name,
                whole.job_id,
                whole.physical_unit,
                whole.status,
                whole.result_digest,
                whole.result_content
         FROM k12_recognition_fallback_authorizations AS authorization
         JOIN k12_model_physical_invocations AS whole
           ON whole.physical_invocation_id =
                authorization.whole_physical_invocation_id
         WHERE authorization.parent_invocation_id=?`,
		parentInvocationID,
	).Scan(
		&authorizationAgent,
		&authorizationJob,
		&authorizationWholeID,
		&authorizationDigest,
		&authorizationContent,
		&wholeParentID,
		&wholeAgent,
		&wholeJob,
		&wholeUnit,
		&wholeStatus,
		&wholeDigest,
		&wholeContent,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(
			"%w: fallback authorization for parent %s is missing",
			ErrModelPhysicalInvocationConflict,
			parentInvocationID,
		)
	}
	if err != nil {
		return fmt.Errorf(
			"k12storage: validate recognition fallback authorization: %w",
			err,
		)
	}
	if authorizationAgent != agentName ||
		authorizationWholeID != wholePhysicalInvocationID ||
		wholeParentID != parentInvocationID ||
		wholeAgent != agentName ||
		authorizationJob == "" ||
		wholeJob != authorizationJob ||
		wholeUnit != k12.RecognitionPhysicalUnitWholePage ||
		wholeStatus != k12.ModelInvocationSucceeded ||
		!wholeContent.Valid ||
		authorizationDigest != wholeDigest ||
		authorizationContent != wholeContent.String ||
		wholeDigest != physicalInvocationResultDigest(wholeContent.String) {
		return fmt.Errorf(
			"%w: fallback authorization for parent %s drifted",
			ErrModelPhysicalInvocationConflict,
			parentInvocationID,
		)
	}
	return nil
}

// AuthorizeRecognitionFallback freezes the parser-owned protocol-invalid
// decision against the exact succeeded whole_page content. An exact replay is
// idempotent; changed content or a different whole child is an immutable
// conflict.
func (s *Store) AuthorizeRecognitionFallback(
	ctx context.Context,
	agentName string,
	parentInvocationID string,
	wholePhysicalInvocationID string,
	wholeResultContent string,
) error {
	agentName = strings.TrimSpace(agentName)
	parentInvocationID = strings.TrimSpace(parentInvocationID)
	wholePhysicalInvocationID = strings.TrimSpace(
		wholePhysicalInvocationID,
	)
	if agentName == "" || parentInvocationID == "" ||
		wholePhysicalInvocationID == "" {
		return fmt.Errorf(
			"k12storage: fallback authorization missing owner/parent/whole identity",
		)
	}
	resultDigest := physicalInvocationResultDigest(wholeResultContent)
	res, err := s.db.ExecContext(
		ctx,
		`INSERT INTO k12_recognition_fallback_authorizations (
             parent_invocation_id,agent_name,job_id,
             whole_physical_invocation_id,whole_result_digest,
             whole_result_content,created_at
         )
         SELECT parent.invocation_id,parent.agent_name,parent.job_id,
                whole.physical_invocation_id,whole.result_digest,
                whole.result_content,?
         FROM k12_model_invocations AS parent
         JOIN k12_model_physical_invocations AS whole
           ON whole.parent_invocation_id=parent.invocation_id
          AND whole.agent_name=parent.agent_name
          AND whole.job_id=parent.job_id
          AND whole.stage=parent.stage
         WHERE parent.invocation_id=?
           AND parent.agent_name=?
           AND parent.stage='recognizing'
           AND parent.status='sent'
           AND whole.physical_invocation_id=?
           AND whole.physical_unit='whole_page'
           AND whole.status='succeeded'
           AND whole.result_digest=?
           AND whole.result_content=?
         ON CONFLICT(parent_invocation_id) DO NOTHING`,
		nowUnix(),
		parentInvocationID,
		agentName,
		wholePhysicalInvocationID,
		resultDigest,
		wholeResultContent,
	)
	if err != nil {
		return fmt.Errorf(
			"k12storage: authorize recognition fallback: %w",
			err,
		)
	}
	affected, _ := res.RowsAffected()
	if affected > 0 {
		return nil
	}
	var storedAgent, storedWholeID, storedDigest, storedContent string
	err = s.db.QueryRowContext(
		ctx,
		`SELECT agent_name,whole_physical_invocation_id,
                whole_result_digest,whole_result_content
         FROM k12_recognition_fallback_authorizations
         WHERE parent_invocation_id=?`,
		parentInvocationID,
	).Scan(
		&storedAgent,
		&storedWholeID,
		&storedDigest,
		&storedContent,
	)
	if err == nil {
		if storedAgent == agentName &&
			storedWholeID == wholePhysicalInvocationID &&
			storedDigest == resultDigest &&
			storedContent == wholeResultContent {
			return nil
		}
		return fmt.Errorf(
			"%w: fallback authorization for parent %s changed",
			ErrModelPhysicalInvocationConflict,
			parentInvocationID,
		)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf(
			"k12storage: read recognition fallback authorization: %w",
			err,
		)
	}
	return fmt.Errorf(
		"%w: fallback requires exact succeeded whole_page content",
		records.ErrIllegalTransition,
	)
}

func (s *Store) MarkModelPhysicalInvocationFailed(
	ctx context.Context,
	agentName string,
	physicalInvocationID string,
	failureKind string,
) (k12.ModelPhysicalInvocation, error) {
	if strings.TrimSpace(failureKind) == "" {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"k12storage: failed physical invocation requires failure_kind",
		)
	}
	return s.transitionModelPhysicalInvocation(
		ctx,
		agentName,
		physicalInvocationID,
		[]k12.ModelInvocationStatus{k12.ModelInvocationSent},
		k12.ModelInvocationFailed,
		"",
		"",
		failureKind,
	)
}

// MarkModelPhysicalInvocationNotSent closes a locally prepared operation after
// the shared provider transport proves http.Client.Do was never entered. It
// must never rewrite sent: that row may belong to a concurrent CAS winner whose
// physical request is already in flight.
func (s *Store) MarkModelPhysicalInvocationNotSent(
	ctx context.Context,
	agentName string,
	physicalInvocationID string,
) (k12.ModelPhysicalInvocation, error) {
	return s.transitionModelPhysicalInvocation(
		ctx,
		agentName,
		physicalInvocationID,
		[]k12.ModelInvocationStatus{k12.ModelInvocationPrepared},
		k12.ModelInvocationFailed,
		"",
		"",
		"provider_request_not_sent",
	)
}

func (s *Store) MarkModelPhysicalInvocationOutcomeUnknown(
	ctx context.Context,
	agentName string,
	physicalInvocationID string,
	failureKind string,
) (k12.ModelPhysicalInvocation, error) {
	if strings.TrimSpace(failureKind) == "" {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"k12storage: physical invocation outcome_unknown requires failure_kind",
		)
	}
	return s.transitionModelPhysicalInvocation(
		ctx,
		agentName,
		physicalInvocationID,
		[]k12.ModelInvocationStatus{k12.ModelInvocationSent},
		k12.ModelInvocationOutcomeUnknown,
		"",
		"",
		failureKind,
	)
}
