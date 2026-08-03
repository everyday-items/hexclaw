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

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

var ErrModelInvocationConflict = errors.New("model invocation immutable identity conflict")

const modelInvocationColumns = `invocation_id,agent_name,job_id,stage,request_digest,
    provider,model,route_snapshot_json,request_policy_snapshot_json,
    provider_idempotency_key,status,attempt,
    result_digest,result_json,external_request_id,failure_kind,created_at,updated_at`

func scanModelInvocation(row rowScanner) (k12.ModelInvocation, error) {
	var invocation k12.ModelInvocation
	var routeJSON, requestPolicyJSON, status string
	err := row.Scan(&invocation.InvocationID, &invocation.AgentName, &invocation.JobID,
		&invocation.Stage, &invocation.RequestDigest, &invocation.RouteSnapshot.Provider,
		&invocation.RouteSnapshot.Model, &routeJSON, &requestPolicyJSON,
		&invocation.ProviderIdempotencyKey, &status, &invocation.Attempt,
		&invocation.ResultDigest, &invocation.ResultJSON,
		&invocation.ExternalRequestID, &invocation.FailureKind,
		&invocation.CreatedAt, &invocation.UpdatedAt)
	if err != nil {
		return k12.ModelInvocation{}, err
	}
	if err := json.Unmarshal([]byte(routeJSON), &invocation.RouteSnapshot); err != nil {
		return k12.ModelInvocation{}, fmt.Errorf("k12storage: parse model invocation route snapshot: %w", err)
	}
	invocation.RouteSnapshot = k12.NormalizeGradingModelSnapshot(invocation.RouteSnapshot)
	if strings.TrimSpace(requestPolicyJSON) != "" {
		if err := json.Unmarshal(
			[]byte(requestPolicyJSON),
			&invocation.RequestPolicySnapshot,
		); err != nil {
			return k12.ModelInvocation{}, fmt.Errorf(
				"k12storage: parse model invocation request policy snapshot: %w",
				err,
			)
		}
	}
	invocation.RequestPolicySnapshot = k12.NormalizeModelRequestPolicySnapshot(
		invocation.RequestPolicySnapshot,
	)
	invocation.Status = k12.ModelInvocationStatus(status)
	return invocation, nil
}

func validateModelInvocation(invocation *k12.ModelInvocation) error {
	if invocation == nil {
		return fmt.Errorf("k12storage: model invocation is nil")
	}
	invocation.InvocationID = strings.TrimSpace(invocation.InvocationID)
	invocation.AgentName = strings.TrimSpace(invocation.AgentName)
	invocation.JobID = strings.TrimSpace(invocation.JobID)
	invocation.Stage = strings.TrimSpace(invocation.Stage)
	invocation.RequestDigest = strings.TrimSpace(invocation.RequestDigest)
	invocation.RouteSnapshot = k12.NormalizeGradingModelSnapshot(invocation.RouteSnapshot)
	invocation.RequestPolicySnapshot = k12.NormalizeModelRequestPolicySnapshot(
		invocation.RequestPolicySnapshot,
	)
	if invocation.InvocationID == "" || invocation.AgentName == "" || invocation.JobID == "" ||
		invocation.Stage == "" || invocation.RequestDigest == "" || invocation.Attempt < 1 ||
		invocation.RouteSnapshot.Provider == "" || invocation.RouteSnapshot.Model == "" || invocation.RouteSnapshot.Route == "" {
		return fmt.Errorf("k12storage: model invocation missing id/owner/job/stage/digest/route/attempt")
	}
	if err := k12.ValidateModelInvocationRequestPolicy(
		invocation.Stage,
		invocation.RouteSnapshot,
		invocation.RequestPolicySnapshot,
	); err != nil {
		return fmt.Errorf("k12storage: invalid model invocation request policy: %w", err)
	}
	return nil
}

// PrepareModelInvocation establishes the durable before-send point. The unique
// (job,stage,attempt) key returns the original invocation on an exact replay and
// rejects a changed request or route snapshot.
func (s *Store) PrepareModelInvocation(ctx context.Context, invocation k12.ModelInvocation) (k12.ModelInvocation, bool, error) {
	if err := validateModelInvocation(&invocation); err != nil {
		return k12.ModelInvocation{}, false, err
	}
	if err := ensureAgentRegistered(ctx, s.db, invocation.AgentName); err != nil {
		return k12.ModelInvocation{}, false, err
	}
	if invocation.CreatedAt <= 0 {
		invocation.CreatedAt = nowUnix()
	}
	invocation.UpdatedAt = invocation.CreatedAt
	invocation.Status = k12.ModelInvocationPrepared
	routeJSON, err := json.Marshal(invocation.RouteSnapshot)
	if err != nil {
		return k12.ModelInvocation{}, false, err
	}
	requestPolicyJSON := ""
	if !invocation.RequestPolicySnapshot.IsZero() {
		raw, marshalErr := json.Marshal(invocation.RequestPolicySnapshot)
		if marshalErr != nil {
			return k12.ModelInvocation{}, false, marshalErr
		}
		requestPolicyJSON = string(raw)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO k12_model_invocations (`+modelInvocationColumns+`)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(job_id,stage,attempt) DO NOTHING`,
		invocation.InvocationID, invocation.AgentName, invocation.JobID, invocation.Stage,
		invocation.RequestDigest, invocation.RouteSnapshot.Provider, invocation.RouteSnapshot.Model,
		string(routeJSON), requestPolicyJSON, invocation.ProviderIdempotencyKey,
		invocation.Status, invocation.Attempt, "", "", "", "", invocation.CreatedAt,
		invocation.UpdatedAt)
	if err != nil {
		return k12.ModelInvocation{}, false, fmt.Errorf("k12storage: prepare model invocation: %w", err)
	}
	created, _ := res.RowsAffected()
	stored, err := s.getModelInvocationByAttempt(ctx, invocation.JobID, invocation.Stage, invocation.Attempt)
	if err != nil {
		return k12.ModelInvocation{}, false, err
	}
	if stored.AgentName != invocation.AgentName || stored.RequestDigest != invocation.RequestDigest ||
		stored.RouteSnapshot != invocation.RouteSnapshot ||
		stored.RequestPolicySnapshot != invocation.RequestPolicySnapshot {
		return k12.ModelInvocation{}, false, fmt.Errorf("%w: job=%s stage=%s attempt=%d", ErrModelInvocationConflict,
			invocation.JobID, invocation.Stage, invocation.Attempt)
	}
	return stored, created > 0, nil
}

func (s *Store) getModelInvocationByAttempt(ctx context.Context, jobID, stage string, attempt int) (k12.ModelInvocation, error) {
	invocation, err := scanModelInvocation(s.db.QueryRowContext(ctx, `SELECT `+modelInvocationColumns+`
        FROM k12_model_invocations WHERE job_id=? AND stage=? AND attempt=?`, jobID, stage, attempt))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.ModelInvocation{}, records.ErrNotFound
	}
	return invocation, err
}

// GetModelInvocationByAttempt is the read-only stable-operation lookup used by
// reconciliation paths. Unlike PrepareModelInvocation it can never create a
// ledger row or authorize an external send.
func (s *Store) GetModelInvocationByAttempt(
	ctx context.Context,
	agentName, jobID, stage string,
	attempt int,
) (k12.ModelInvocation, error) {
	agentName = strings.TrimSpace(agentName)
	jobID = strings.TrimSpace(jobID)
	stage = strings.TrimSpace(stage)
	if agentName == "" || jobID == "" || stage == "" || attempt < 1 {
		return k12.ModelInvocation{}, records.ErrNotFound
	}
	invocation, err := scanModelInvocation(s.db.QueryRowContext(ctx, `SELECT `+modelInvocationColumns+`
		FROM k12_model_invocations
		WHERE agent_name=? AND job_id=? AND stage=? AND attempt=?`,
		agentName,
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

func (s *Store) GetModelInvocation(ctx context.Context, agentName, invocationID string) (k12.ModelInvocation, error) {
	invocation, err := scanModelInvocation(s.db.QueryRowContext(ctx, `SELECT `+modelInvocationColumns+`
        FROM k12_model_invocations WHERE invocation_id=? AND agent_name=?`, invocationID, agentName))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.ModelInvocation{}, records.ErrNotFound
	}
	if err != nil {
		return k12.ModelInvocation{}, fmt.Errorf("k12storage: get model invocation: %w", err)
	}
	return invocation, nil
}

func (s *Store) ListModelInvocations(ctx context.Context, agentName, jobID string) ([]k12.ModelInvocation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+modelInvocationColumns+`
        FROM k12_model_invocations WHERE agent_name=? AND job_id=? ORDER BY stage,attempt`, agentName, jobID)
	if err != nil {
		return nil, fmt.Errorf("k12storage: list model invocations: %w", err)
	}
	defer rows.Close()
	out := make([]k12.ModelInvocation, 0)
	for rows.Next() {
		invocation, scanErr := scanModelInvocation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, invocation)
	}
	return out, rows.Err()
}

func (s *Store) transitionModelInvocation(ctx context.Context, agentName, invocationID string,
	from []k12.ModelInvocationStatus, to k12.ModelInvocationStatus, providerKey, resultDigest, externalRequestID, failureKind string,
) (k12.ModelInvocation, error) {
	placeholders := make([]string, len(from))
	for i := range from {
		placeholders[i] = "?"
	}
	args := []any{to, providerKey, providerKey, resultDigest, resultDigest,
		externalRequestID, externalRequestID, failureKind, failureKind,
		nowUnix(), invocationID, agentName}
	args = append(args, statusArgs(from)...)
	res, err := s.db.ExecContext(ctx, `UPDATE k12_model_invocations SET
        status=?,provider_idempotency_key=CASE WHEN ?='' THEN provider_idempotency_key ELSE ? END,
        result_digest=CASE WHEN ?='' THEN result_digest ELSE ? END,
        external_request_id=CASE WHEN ?='' THEN external_request_id ELSE ? END,
        failure_kind=CASE WHEN ?='' THEN failure_kind ELSE ? END,updated_at=?
        WHERE invocation_id=? AND agent_name=? AND status IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return k12.ModelInvocation{}, fmt.Errorf("k12storage: transition model invocation to %s: %w", to, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		current, getErr := s.GetModelInvocation(ctx, agentName, invocationID)
		if getErr != nil {
			return k12.ModelInvocation{}, getErr
		}
		if current.Status != to {
			return k12.ModelInvocation{}, fmt.Errorf("%w: invocation %s status %s -> %s", records.ErrIllegalTransition, invocationID, current.Status, to)
		}
		return current, nil
	}
	return s.GetModelInvocation(ctx, agentName, invocationID)
}

func statusArgs(statuses []k12.ModelInvocationStatus) []any {
	out := make([]any, len(statuses))
	for i, status := range statuses {
		out[i] = status
	}
	return out
}

func (s *Store) MarkModelInvocationSent(ctx context.Context, agentName, invocationID, providerKey string) (k12.ModelInvocation, error) {
	return s.transitionModelInvocation(ctx, agentName, invocationID,
		[]k12.ModelInvocationStatus{k12.ModelInvocationPrepared}, k12.ModelInvocationSent, providerKey, "", "", "")
}

func (s *Store) MarkModelInvocationSucceeded(ctx context.Context, agentName, invocationID, resultDigest, externalRequestID string) (k12.ModelInvocation, error) {
	if strings.TrimSpace(resultDigest) == "" {
		return k12.ModelInvocation{}, fmt.Errorf("k12storage: successful model invocation requires result_digest")
	}
	return s.transitionModelInvocation(ctx, agentName, invocationID,
		[]k12.ModelInvocationStatus{k12.ModelInvocationSent}, k12.ModelInvocationSucceeded, "", resultDigest, externalRequestID, "")
}

func modelInvocationResultPayloadDigest(resultJSON string) string {
	h := sha256.New()
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(resultJSON))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// MarkModelInvocationSucceededWithResult atomically binds the exact provider
// result bytes to the successful invocation transition. The payload is an
// immutable crash-recovery fact: only a byte-for-byte replay of all result
// identity fields is accepted after success.
func (s *Store) MarkModelInvocationSucceededWithResult(
	ctx context.Context,
	agentName, invocationID, resultDigest, resultJSON, externalRequestID string,
) (k12.ModelInvocation, error) {
	agentName = strings.TrimSpace(agentName)
	invocationID = strings.TrimSpace(invocationID)
	resultDigest = strings.TrimSpace(resultDigest)
	externalRequestID = strings.TrimSpace(externalRequestID)
	if agentName == "" || invocationID == "" {
		return k12.ModelInvocation{}, fmt.Errorf(
			"k12storage: successful model invocation requires agent and invocation id",
		)
	}
	if strings.TrimSpace(resultJSON) == "" || !json.Valid([]byte(resultJSON)) {
		return k12.ModelInvocation{}, fmt.Errorf(
			"k12storage: successful model invocation requires non-empty valid result_json",
		)
	}
	expectedDigest := modelInvocationResultPayloadDigest(resultJSON)
	if resultDigest == "" || resultDigest != expectedDigest {
		return k12.ModelInvocation{}, fmt.Errorf(
			"%w: invocation %s result digest does not match result_json",
			ErrModelInvocationConflict,
			invocationID,
		)
	}

	res, err := s.db.ExecContext(ctx, `UPDATE k12_model_invocations SET
        status=?,result_digest=?,result_json=?,external_request_id=?,
        failure_kind='',updated_at=?
        WHERE invocation_id=? AND agent_name=? AND status=?`,
		k12.ModelInvocationSucceeded,
		resultDigest,
		resultJSON,
		externalRequestID,
		nowUnix(),
		invocationID,
		agentName,
		k12.ModelInvocationSent,
	)
	if err != nil {
		return k12.ModelInvocation{}, fmt.Errorf(
			"k12storage: persist successful model invocation result: %w",
			err,
		)
	}
	updated, err := res.RowsAffected()
	if err != nil {
		return k12.ModelInvocation{}, err
	}
	stored, err := s.GetModelInvocation(ctx, agentName, invocationID)
	if err != nil {
		return k12.ModelInvocation{}, err
	}
	if updated == 0 && stored.Status != k12.ModelInvocationSucceeded {
		return k12.ModelInvocation{}, fmt.Errorf(
			"%w: invocation %s status %s -> %s",
			records.ErrIllegalTransition,
			invocationID,
			stored.Status,
			k12.ModelInvocationSucceeded,
		)
	}
	if stored.Status != k12.ModelInvocationSucceeded ||
		stored.ResultDigest != resultDigest ||
		stored.ResultJSON != resultJSON ||
		stored.ExternalRequestID != externalRequestID {
		return k12.ModelInvocation{}, fmt.Errorf(
			"%w: invocation %s successful result identity changed",
			ErrModelInvocationConflict,
			invocationID,
		)
	}
	return stored, nil
}

func (s *Store) MarkModelInvocationFailed(ctx context.Context, agentName, invocationID, failureKind string) (k12.ModelInvocation, error) {
	if strings.TrimSpace(failureKind) == "" {
		return k12.ModelInvocation{}, fmt.Errorf("k12storage: failed model invocation requires failure_kind")
	}
	return s.transitionModelInvocation(ctx, agentName, invocationID,
		[]k12.ModelInvocationStatus{k12.ModelInvocationSent}, k12.ModelInvocationFailed, "", "", "", failureKind)
}

func (s *Store) MarkModelInvocationOutcomeUnknown(ctx context.Context, agentName, invocationID, failureKind string) (k12.ModelInvocation, error) {
	if strings.TrimSpace(failureKind) == "" {
		return k12.ModelInvocation{}, fmt.Errorf("k12storage: outcome_unknown requires failure_kind")
	}
	return s.transitionModelInvocation(ctx, agentName, invocationID,
		[]k12.ModelInvocationStatus{k12.ModelInvocationSent}, k12.ModelInvocationOutcomeUnknown, "", "", "", failureKind)
}

// ReconcileModelInvocationNotExecuted records verified negative execution
// evidence. This is the only ledger outcome that can authorize a fresh attempt;
// it remains a separate explicit command from ordinary retry.
func (s *Store) ReconcileModelInvocationNotExecuted(ctx context.Context, agentName, invocationID string) (k12.ModelInvocation, error) {
	return s.transitionModelInvocation(ctx, agentName, invocationID,
		[]k12.ModelInvocationStatus{k12.ModelInvocationOutcomeUnknown}, k12.ModelInvocationReconciled,
		"", "", "", "reconciled_not_executed")
}

// ReconcileModelInvocationSucceeded records conclusive, already-durable result
// evidence for a request whose transport outcome was previously unknown. It is
// a ledger-only reconciliation: callers must validate the durable artifact
// against the frozen Job before invoking it, and must never use it to justify a
// second provider request.
func (s *Store) ReconcileModelInvocationSucceeded(
	ctx context.Context,
	agentName, invocationID, resultDigest, externalRequestID string,
) (k12.ModelInvocation, error) {
	resultDigest = strings.TrimSpace(resultDigest)
	externalRequestID = strings.TrimSpace(externalRequestID)
	if resultDigest == "" {
		return k12.ModelInvocation{}, fmt.Errorf("k12storage: reconciled model success requires result_digest")
	}
	stored, err := s.transitionModelInvocation(ctx, agentName, invocationID,
		[]k12.ModelInvocationStatus{k12.ModelInvocationOutcomeUnknown}, k12.ModelInvocationReconciled,
		"", resultDigest, externalRequestID, "reconciled_succeeded")
	if err != nil {
		return k12.ModelInvocation{}, err
	}
	if stored.Status != k12.ModelInvocationReconciled ||
		stored.FailureKind != "reconciled_succeeded" ||
		stored.ResultDigest != resultDigest ||
		(externalRequestID != "" && stored.ExternalRequestID != externalRequestID) {
		return k12.ModelInvocation{}, fmt.Errorf(
			"%w: invocation %s reconciled result identity changed",
			ErrModelInvocationConflict, invocationID,
		)
	}
	return stored, nil
}
