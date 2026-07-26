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

var ErrGradingItemInvocationConflict = errors.New("grading item invocation immutable identity conflict")

const gradingItemInvocationColumns = `item_invocation_id,agent_name,job_id,problem_id,attempt_id,
    operation,operation_attempt,request_digest,provider,model,route_snapshot_json,status,
    cost_receipt_id,result_digest,result_json,failure_class,failure_code,created_at,updated_at`

func scanGradingItemInvocation(row rowScanner) (k12.GradingItemInvocation, error) {
	var item k12.GradingItemInvocation
	var operation, status, routeJSON string
	err := row.Scan(&item.InvocationID, &item.AgentName, &item.JobID, &item.ProblemID, &item.AttemptID,
		&operation, &item.OperationAttempt, &item.RequestDigest,
		&item.RouteSnapshot.Provider, &item.RouteSnapshot.Model, &routeJSON, &status,
		&item.CostReceiptID, &item.ResultDigest, &item.ResultJSON, &item.FailureClass, &item.FailureCode,
		&item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return k12.GradingItemInvocation{}, err
	}
	if err := json.Unmarshal([]byte(routeJSON), &item.RouteSnapshot); err != nil {
		return k12.GradingItemInvocation{}, fmt.Errorf("k12storage: parse grading item route snapshot: %w", err)
	}
	item.RouteSnapshot = k12.NormalizeGradingModelSnapshot(item.RouteSnapshot)
	item.Operation = k12.GradingItemOperation(operation)
	item.Status = k12.ModelInvocationStatus(status)
	return item, nil
}

func sameGradingItemIdentity(a, b k12.GradingItemInvocation) bool {
	return a.AgentName == b.AgentName && a.JobID == b.JobID && a.ProblemID == b.ProblemID &&
		a.AttemptID == b.AttemptID && a.Operation == b.Operation &&
		a.OperationAttempt == b.OperationAttempt && a.RequestDigest == b.RequestDigest &&
		a.RouteSnapshot == b.RouteSnapshot
}

func ensureGradingItemScope(ctx context.Context, q dbQueryer, agentName, jobID, problemID, attemptID string) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1
        FROM k12_grading_jobs j
        JOIN k12_problems p ON p.agent_name=j.agent_name AND p.submission_id=j.submission_id AND p.problem_id=?
        JOIN k12_attempts a ON a.agent_name=p.agent_name AND a.submission_id=j.submission_id
            AND a.problem_id=p.problem_id AND a.attempt_id=?
        WHERE j.agent_name=? AND j.record_id=?`, problemID, attemptID, agentName, jobID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return records.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("k12storage: validate grading item scope: %w", err)
	}
	return nil
}

// PrepareGradingItemInvocation establishes the durable before-send point for
// one stable (job,problem,operation,attempt) key. Exact concurrent replay
// returns the winner; changed input/route/Attempt identity fails closed.
func (s *Store) PrepareGradingItemInvocation(ctx context.Context, item k12.GradingItemInvocation) (k12.GradingItemInvocation, bool, error) {
	if err := item.ValidateIdentity(); err != nil {
		return k12.GradingItemInvocation{}, false, fmt.Errorf("k12storage: %w", err)
	}
	if err := ensureAgentRegistered(ctx, s.db, item.AgentName); err != nil {
		return k12.GradingItemInvocation{}, false, err
	}
	if err := ensureGradingItemScope(ctx, s.db, item.AgentName, item.JobID, item.ProblemID, item.AttemptID); err != nil {
		return k12.GradingItemInvocation{}, false, err
	}
	if item.CreatedAt <= 0 {
		item.CreatedAt = nowUnix()
	}
	item.UpdatedAt = item.CreatedAt
	item.Status = k12.ModelInvocationPrepared
	routeJSON, err := json.Marshal(item.RouteSnapshot)
	if err != nil {
		return k12.GradingItemInvocation{}, false, fmt.Errorf("k12storage: marshal grading item route: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO k12_grading_item_invocations (`+gradingItemInvocationColumns+`)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(job_id,problem_id,operation,operation_attempt) DO NOTHING`,
		item.InvocationID, item.AgentName, item.JobID, item.ProblemID, item.AttemptID,
		item.Operation, item.OperationAttempt, item.RequestDigest,
		item.RouteSnapshot.Provider, item.RouteSnapshot.Model, string(routeJSON), item.Status,
		"", "", "", "", "", item.CreatedAt, item.UpdatedAt)
	if err != nil {
		return k12.GradingItemInvocation{}, false, fmt.Errorf("k12storage: prepare grading item invocation: %w", err)
	}
	created, _ := res.RowsAffected()
	stored, err := s.GetGradingItemInvocationByAttempt(ctx, item.AgentName, item.JobID,
		item.ProblemID, item.Operation, item.OperationAttempt)
	if err != nil {
		return k12.GradingItemInvocation{}, false, err
	}
	if !sameGradingItemIdentity(stored, item) {
		return k12.GradingItemInvocation{}, false, fmt.Errorf("%w: job=%s problem=%s operation=%s attempt=%d",
			ErrGradingItemInvocationConflict, item.JobID, item.ProblemID, item.Operation, item.OperationAttempt)
	}
	return stored, created > 0, nil
}

func (s *Store) GetGradingItemInvocation(ctx context.Context, agentName, invocationID string) (k12.GradingItemInvocation, error) {
	item, err := scanGradingItemInvocation(s.db.QueryRowContext(ctx, `SELECT `+gradingItemInvocationColumns+`
        FROM k12_grading_item_invocations WHERE agent_name=? AND item_invocation_id=?`, agentName, invocationID))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.GradingItemInvocation{}, records.ErrNotFound
	}
	if err != nil {
		return k12.GradingItemInvocation{}, fmt.Errorf("k12storage: get grading item invocation: %w", err)
	}
	return item, nil
}

func (s *Store) GetGradingItemInvocationByAttempt(ctx context.Context, agentName, jobID, problemID string,
	operation k12.GradingItemOperation, operationAttempt int,
) (k12.GradingItemInvocation, error) {
	item, err := scanGradingItemInvocation(s.db.QueryRowContext(ctx, `SELECT `+gradingItemInvocationColumns+`
        FROM k12_grading_item_invocations
        WHERE agent_name=? AND job_id=? AND problem_id=? AND operation=? AND operation_attempt=?`,
		agentName, jobID, problemID, operation, operationAttempt))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.GradingItemInvocation{}, records.ErrNotFound
	}
	if err != nil {
		return k12.GradingItemInvocation{}, fmt.Errorf("k12storage: get grading item invocation by attempt: %w", err)
	}
	return item, nil
}

func (s *Store) ListGradingItemInvocations(ctx context.Context, agentName, jobID string) ([]k12.GradingItemInvocation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+gradingItemInvocationColumns+`
        FROM k12_grading_item_invocations WHERE agent_name=? AND job_id=?
        ORDER BY problem_id,operation,operation_attempt,item_invocation_id`, agentName, jobID)
	if err != nil {
		return nil, fmt.Errorf("k12storage: list grading item invocations: %w", err)
	}
	defer rows.Close()
	out := make([]k12.GradingItemInvocation, 0)
	for rows.Next() {
		item, scanErr := scanGradingItemInvocation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) transitionGradingItemInvocation(ctx context.Context, agentName, invocationID string,
	from k12.ModelInvocationStatus, to k12.ModelInvocationStatus,
	costReceiptID, resultDigest, resultJSON, failureClass, failureCode string,
) (k12.GradingItemInvocation, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE k12_grading_item_invocations SET
        status=?,cost_receipt_id=?,result_digest=?,result_json=?,failure_class=?,failure_code=?,updated_at=?
        WHERE agent_name=? AND item_invocation_id=? AND status=?`,
		to, costReceiptID, resultDigest, resultJSON, failureClass, failureCode, nowUnix(), agentName, invocationID, from)
	if err != nil {
		return k12.GradingItemInvocation{}, fmt.Errorf("k12storage: transition grading item invocation to %s: %w", to, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		current, getErr := s.GetGradingItemInvocation(ctx, agentName, invocationID)
		if getErr != nil {
			return k12.GradingItemInvocation{}, getErr
		}
		if current.Status != to {
			return k12.GradingItemInvocation{}, fmt.Errorf("%w: item invocation %s status %s -> %s",
				records.ErrIllegalTransition, invocationID, current.Status, to)
		}
		if current.CostReceiptID != costReceiptID ||
			current.ResultDigest != resultDigest || current.ResultJSON != resultJSON ||
			current.FailureClass != failureClass || current.FailureCode != failureCode {
			return k12.GradingItemInvocation{}, fmt.Errorf("%w: item invocation %s terminal payload changed",
				ErrGradingItemInvocationConflict, invocationID)
		}
		return current, nil
	}
	return s.GetGradingItemInvocation(ctx, agentName, invocationID)
}

func (s *Store) MarkGradingItemInvocationSent(ctx context.Context, agentName, invocationID string) (k12.GradingItemInvocation, error) {
	return s.transitionGradingItemInvocation(ctx, agentName, invocationID,
		k12.ModelInvocationPrepared, k12.ModelInvocationSent, "", "", "", "", "")
}

// ClaimGradingItemInvocationSent is the cross-worker prepared -> sent CAS. A
// false claim means another worker owns (or already completed) the provider
// request; callers must not POST.
func (s *Store) ClaimGradingItemInvocationSent(
	ctx context.Context,
	agentName, invocationID string,
) (k12.GradingItemInvocation, bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE k12_grading_item_invocations SET
        status=?,updated_at=? WHERE agent_name=? AND item_invocation_id=? AND status=?`,
		k12.ModelInvocationSent, nowUnix(), agentName, invocationID, k12.ModelInvocationPrepared)
	if err != nil {
		return k12.GradingItemInvocation{}, false, fmt.Errorf("k12storage: claim grading item invocation sent: %w", err)
	}
	claimed, _ := res.RowsAffected()
	current, err := s.GetGradingItemInvocation(ctx, agentName, invocationID)
	if err != nil {
		return k12.GradingItemInvocation{}, false, err
	}
	return current, claimed == 1, nil
}

func (s *Store) MarkGradingItemInvocationSucceeded(ctx context.Context, agentName, invocationID, resultDigest, resultJSON string) (k12.GradingItemInvocation, error) {
	resultDigest = strings.TrimSpace(resultDigest)
	resultJSON = strings.TrimSpace(resultJSON)
	if resultDigest == "" || resultJSON == "" || !json.Valid([]byte(resultJSON)) {
		return k12.GradingItemInvocation{}, fmt.Errorf("k12storage: successful grading item invocation requires digest and valid result JSON")
	}
	return s.transitionGradingItemInvocation(ctx, agentName, invocationID,
		k12.ModelInvocationSent, k12.ModelInvocationSucceeded,
		"cost-"+invocationID, resultDigest, resultJSON, "", "")
}

func (s *Store) MarkGradingItemInvocationFailed(ctx context.Context, agentName, invocationID, failureClass, failureCode string) (k12.GradingItemInvocation, error) {
	failureClass, failureCode = strings.TrimSpace(failureClass), strings.TrimSpace(failureCode)
	if failureClass == "" || failureCode == "" {
		return k12.GradingItemInvocation{}, fmt.Errorf("k12storage: failed grading item invocation requires failure class and code")
	}
	return s.transitionGradingItemInvocation(ctx, agentName, invocationID,
		k12.ModelInvocationSent, k12.ModelInvocationFailed, "", "", "", failureClass, failureCode)
}

func (s *Store) MarkGradingItemInvocationOutcomeUnknown(ctx context.Context, agentName, invocationID, failureClass, failureCode string) (k12.GradingItemInvocation, error) {
	failureClass, failureCode = strings.TrimSpace(failureClass), strings.TrimSpace(failureCode)
	if failureClass == "" || failureCode == "" {
		return k12.GradingItemInvocation{}, fmt.Errorf("k12storage: outcome_unknown grading item invocation requires failure class and code")
	}
	return s.transitionGradingItemInvocation(ctx, agentName, invocationID,
		k12.ModelInvocationSent, k12.ModelInvocationOutcomeUnknown, "", "", "", failureClass, failureCode)
}
