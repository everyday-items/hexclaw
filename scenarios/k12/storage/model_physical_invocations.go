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
    failure_kind,created_at,updated_at`

func scanModelPhysicalInvocation(
	row rowScanner,
) (k12.ModelPhysicalInvocation, error) {
	var invocation k12.ModelPhysicalInvocation
	var routeJSON, requestPolicyJSON, status, physicalUnit string
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
	return nil
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

// prepareRecognizingInvocationWithInitialWholePageOnce owns one complete
// deferred SQLite transaction. A BUSY/BUSY_SNAPSHOT retry must restart this
// whole function so it never reuses a stale WAL read snapshot.
func (s *Store) prepareRecognizingInvocationWithInitialWholePageOnce(
	ctx context.Context,
	parent k12.ModelInvocation,
	child k12.ModelPhysicalInvocation,
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

	parentRouteJSON, err := json.Marshal(parent.RouteSnapshot)
	if err != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, err
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
	childRouteJSON, err := json.Marshal(child.RouteSnapshot)
	if err != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, err
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
			fmt.Errorf(
				"k12storage: begin atomic recognizing publication: %w",
				err,
			)
	}
	defer tx.Rollback()

	parentCreated := false
	storedParent, err := getModelInvocationByAttemptVia(
		ctx,
		tx,
		parent.JobID,
		parent.Stage,
		parent.Attempt,
	)
	if errors.Is(err, records.ErrNotFound) {
		res, insertErr := tx.ExecContext(
			ctx,
			`INSERT INTO k12_model_invocations (`+
				modelInvocationColumns+
				`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
		storedParent, err = getModelInvocationByAttemptVia(
			ctx,
			tx,
			parent.JobID,
			parent.Stage,
			parent.Attempt,
		)
	}
	if err != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, err
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

	storedChild, err := getModelPhysicalInvocationByUnitVia(
		ctx,
		tx,
		parent.InvocationID,
		k12.RecognitionPhysicalUnitWholePage,
	)
	childCreated := false
	if errors.Is(err, records.ErrNotFound) {
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
				`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
		storedChild, err = getModelPhysicalInvocationByUnitVia(
			ctx,
			tx,
			parent.InvocationID,
			k12.RecognitionPhysicalUnitWholePage,
		)
	}
	if err != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false, err
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
		storedParent, err = getModelInvocationByAttemptVia(
			ctx,
			tx,
			parent.JobID,
			parent.Stage,
			parent.Attempt,
		)
		if err != nil {
			return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
				err
		}
	}
	if err := tx.Commit(); err != nil {
		return k12.ModelInvocation{}, k12.ModelPhysicalInvocation{}, false,
			fmt.Errorf(
				"k12storage: commit atomic recognizing publication: %w",
				err,
			)
	}
	return storedParent, storedChild, parentCreated || childCreated, nil
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
		return s.prepareFallbackModelPhysicalInvocation(ctx, invocation)
	}
	if err := ensureAgentRegistered(ctx, s.db, invocation.AgentName); err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	parent, err := s.getModelInvocationByID(ctx, invocation.ParentInvocationID)
	if err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
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
		invocation.ParentInvocationID,
		k12.ModelInvocationSent,
	}
	insertArgs = append(insertArgs, fallbackGateArgs...)
	res, err := s.db.ExecContext(
		ctx,
		`INSERT INTO k12_model_physical_invocations (`+
			modelPhysicalInvocationColumns+
			`) SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?
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

// prepareFallbackModelPhysicalInvocationOnce owns the complete SQLite
// snapshot from private-content validation through exact replay/insert. A
// BUSY_SNAPSHOT retry therefore cannot reuse authorization facts observed
// before a concurrent writer committed.
func (s *Store) prepareFallbackModelPhysicalInvocationOnce(
	ctx context.Context,
	invocation k12.ModelPhysicalInvocation,
) (k12.ModelPhysicalInvocation, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: begin fallback physical prepare: %w",
			err,
		)
	}
	defer tx.Rollback()

	if err := ensureAgentRegistered(ctx, tx, invocation.AgentName); err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	parent, err := getModelInvocationByIDVia(
		ctx,
		tx,
		invocation.ParentInvocationID,
	)
	if err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
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

	existing, err := getModelPhysicalInvocationByUnitVia(
		ctx,
		tx,
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
		if err := tx.Commit(); err != nil {
			return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
				"k12storage: commit fallback physical prepare replay: %w",
				err,
			)
		}
		return existing, false, nil
	}
	if !errors.Is(err, records.ErrNotFound) {
		return k12.ModelPhysicalInvocation{}, false, err
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
		invocation.ParentInvocationID,
		k12.ModelInvocationSent,
	}
	insertArgs = append(insertArgs, fallbackGateArgs...)
	res, err := tx.ExecContext(
		ctx,
		`INSERT INTO k12_model_physical_invocations (`+
			modelPhysicalInvocationColumns+
			`) SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?
             FROM k12_model_invocations AS parent
             WHERE invocation_id=? AND status=?
               AND (`+fallbackGateSQL+`)
             ON CONFLICT DO NOTHING`,
		insertArgs...,
	)
	if err != nil {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: prepare fallback model physical invocation: %w",
			err,
		)
	}
	affected, _ := res.RowsAffected()
	stored, err := getModelPhysicalInvocationByUnitVia(
		ctx,
		tx,
		invocation.ParentInvocationID,
		invocation.PhysicalUnit,
	)
	if err != nil {
		if affected == 0 && errors.Is(err, records.ErrNotFound) {
			return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
				"%w: fallback unit %s lost its authorization gate",
				records.ErrIllegalTransition,
				invocation.PhysicalUnit,
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
	defer rows.Close()
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

// ClaimModelPhysicalInvocationSent is the one-winner CAS immediately before a
// provider POST. Only claimed=true authorizes the caller to send.
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

// claimModelPhysicalInvocationSentOnce keeps the fallback authorization,
// private-content checks, predecessor checks, and prepared→sent CAS in one
// SQLite snapshot. RetryOnBusy restarts this whole transaction after WAL
// snapshot invalidation.
func (s *Store) claimModelPhysicalInvocationSentOnce(
	ctx context.Context,
	agentName string,
	physicalInvocationID string,
) (k12.ModelPhysicalInvocation, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: begin physical invocation send claim: %w",
			err,
		)
	}
	defer tx.Rollback()

	before, err := getModelPhysicalInvocationByIDVia(
		ctx,
		tx,
		agentName,
		physicalInvocationID,
	)
	if err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
	}
	if before.PhysicalUnit != k12.RecognitionPhysicalUnitWholePage {
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
	fallbackGateSQL, fallbackGateArgs :=
		recognitionPhysicalFallbackGateSQL(
			"parent",
			before.PhysicalUnit,
		)
	claimArgs := []any{
		k12.ModelInvocationSent,
		nowUnix(),
		physicalInvocationID,
		agentName,
		k12.ModelInvocationPrepared,
		k12.ModelInvocationSent,
	}
	claimArgs = append(claimArgs, fallbackGateArgs...)
	invocation, err := scanModelPhysicalInvocation(tx.QueryRowContext(
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
	if err == nil {
		if err := tx.Commit(); err != nil {
			return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
				"k12storage: commit physical invocation send claim: %w",
				err,
			)
		}
		return invocation, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: claim model physical invocation sent: %w",
			err,
		)
	}
	current, err := getModelPhysicalInvocationByIDVia(
		ctx,
		tx,
		agentName,
		physicalInvocationID,
	)
	if err != nil {
		return k12.ModelPhysicalInvocation{}, false, err
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
	if err := tx.Commit(); err != nil {
		return k12.ModelPhysicalInvocation{}, false, fmt.Errorf(
			"k12storage: commit physical invocation send claim replay: %w",
			err,
		)
	}
	return current, false, nil
}

// MarkModelPhysicalInvocationSucceededWithContent is the only physical success
// transition. The Store, rather than its caller, computes the digest from the
// exact provider content and retains that content privately for restart-safe
// reconciliation. ModelPhysicalInvocation and its JSON representation expose
// only the digest.
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
	res, err := s.db.ExecContext(
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
	if err != nil {
		return k12.ModelPhysicalInvocation{}, fmt.Errorf(
			"k12storage: mark physical invocation succeeded with content: %w",
			err,
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
		if err := s.db.QueryRowContext(
			ctx,
			`SELECT result_content
             FROM k12_model_physical_invocations
             WHERE physical_invocation_id=? AND agent_name=?`,
			physicalInvocationID,
			agentName,
		).Scan(&storedContent); err != nil {
			return k12.ModelPhysicalInvocation{}, fmt.Errorf(
				"k12storage: read private physical result content: %w",
				err,
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

// ValidateModelPhysicalInvocationResultContent proves, without exposing the
// private content, that a succeeded physical receipt's stored digest is the
// ordinary SHA-256 of its exact persisted provider response.
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
	err := s.db.QueryRowContext(
		ctx,
		`SELECT status,result_digest,result_content
         FROM k12_model_physical_invocations
         WHERE physical_invocation_id=? AND agent_name=?`,
		physicalInvocationID,
		agentName,
	).Scan(&status, &resultDigest, &resultContent)
	if errors.Is(err, sql.ErrNoRows) {
		return records.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf(
			"k12storage: validate private physical result content: %w",
			err,
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
