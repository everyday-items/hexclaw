package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

var ErrModelInvocationConflict = errors.New("model invocation immutable identity conflict")

const modelInvocationColumns = `invocation_id,agent_name,job_id,stage,request_digest,
    provider,model,route_snapshot_json,provider_idempotency_key,status,attempt,
    result_digest,external_request_id,failure_kind,created_at,updated_at`

func scanModelInvocation(row rowScanner) (k12.ModelInvocation, error) {
	var invocation k12.ModelInvocation
	var routeJSON, status string
	err := row.Scan(&invocation.InvocationID, &invocation.AgentName, &invocation.JobID,
		&invocation.Stage, &invocation.RequestDigest, &invocation.RouteSnapshot.Provider,
		&invocation.RouteSnapshot.Model, &routeJSON, &invocation.ProviderIdempotencyKey,
		&status, &invocation.Attempt, &invocation.ResultDigest, &invocation.ExternalRequestID,
		&invocation.FailureKind, &invocation.CreatedAt, &invocation.UpdatedAt)
	if err != nil {
		return k12.ModelInvocation{}, err
	}
	if err := json.Unmarshal([]byte(routeJSON), &invocation.RouteSnapshot); err != nil {
		return k12.ModelInvocation{}, fmt.Errorf("k12storage: parse model invocation route snapshot: %w", err)
	}
	invocation.RouteSnapshot = k12.NormalizeGradingModelSnapshot(invocation.RouteSnapshot)
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
	if invocation.InvocationID == "" || invocation.AgentName == "" || invocation.JobID == "" ||
		invocation.Stage == "" || invocation.RequestDigest == "" || invocation.Attempt < 1 ||
		invocation.RouteSnapshot.Provider == "" || invocation.RouteSnapshot.Model == "" || invocation.RouteSnapshot.Route == "" {
		return fmt.Errorf("k12storage: model invocation missing id/owner/job/stage/digest/route/attempt")
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
	res, err := s.db.ExecContext(ctx, `INSERT INTO k12_model_invocations (`+modelInvocationColumns+`)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(job_id,stage,attempt) DO NOTHING`,
		invocation.InvocationID, invocation.AgentName, invocation.JobID, invocation.Stage,
		invocation.RequestDigest, invocation.RouteSnapshot.Provider, invocation.RouteSnapshot.Model,
		string(routeJSON), invocation.ProviderIdempotencyKey, invocation.Status, invocation.Attempt,
		"", "", "", invocation.CreatedAt, invocation.UpdatedAt)
	if err != nil {
		return k12.ModelInvocation{}, false, fmt.Errorf("k12storage: prepare model invocation: %w", err)
	}
	created, _ := res.RowsAffected()
	stored, err := s.getModelInvocationByAttempt(ctx, invocation.JobID, invocation.Stage, invocation.Attempt)
	if err != nil {
		return k12.ModelInvocation{}, false, err
	}
	if stored.AgentName != invocation.AgentName || stored.RequestDigest != invocation.RequestDigest ||
		stored.RouteSnapshot.Provider != invocation.RouteSnapshot.Provider ||
		stored.RouteSnapshot.Model != invocation.RouteSnapshot.Model ||
		stored.RouteSnapshot.Route != invocation.RouteSnapshot.Route {
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
