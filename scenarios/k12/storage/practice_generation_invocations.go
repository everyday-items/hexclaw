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

const practiceGenerationInvocationColumns = `invocation_id,agent_name,job_id,stage,
	request_digest,provider,model,route_snapshot_json,provider_idempotency_key,status,
	attempt,result_digest,external_request_id,failure_kind,created_at,updated_at`

func scanPracticeGenerationInvocation(row rowScanner) (k12.ModelInvocation, error) {
	var invocation k12.ModelInvocation
	var routeJSON, status string
	if err := row.Scan(
		&invocation.InvocationID, &invocation.AgentName, &invocation.JobID,
		&invocation.Stage, &invocation.RequestDigest, &invocation.RouteSnapshot.Provider,
		&invocation.RouteSnapshot.Model, &routeJSON, &invocation.ProviderIdempotencyKey,
		&status, &invocation.Attempt, &invocation.ResultDigest,
		&invocation.ExternalRequestID, &invocation.FailureKind,
		&invocation.CreatedAt, &invocation.UpdatedAt,
	); err != nil {
		return k12.ModelInvocation{}, err
	}
	if err := json.Unmarshal([]byte(routeJSON), &invocation.RouteSnapshot); err != nil {
		return k12.ModelInvocation{}, fmt.Errorf(
			"k12storage: 解析逐题模型调用路由快照: %w", err,
		)
	}
	invocation.RouteSnapshot = k12.NormalizeGradingModelSnapshot(invocation.RouteSnapshot)
	invocation.Status = k12.ModelInvocationStatus(status)
	return invocation, nil
}

func validPracticeGenerationInvocationStage(stage string) bool {
	return stage == k12.PracticeGenerationStageGenerate ||
		stage == k12.PracticeGenerationStageValidate
}

// PreparePracticeGenerationInvocation is the durable before-send point for a
// single-practice generator or validator call. Exact replays converge on the
// immutable (job, stage, attempt) receipt.
func (s *Store) PreparePracticeGenerationInvocation(
	ctx context.Context,
	invocation k12.ModelInvocation,
) (k12.ModelInvocation, bool, error) {
	if err := validateModelInvocation(&invocation); err != nil {
		return k12.ModelInvocation{}, false, err
	}
	if !validPracticeGenerationInvocationStage(invocation.Stage) {
		return k12.ModelInvocation{}, false, fmt.Errorf(
			"k12storage: 非法逐题模型调用阶段 %q", invocation.Stage,
		)
	}
	var jobOwner, scope string
	if err := s.db.QueryRowContext(ctx, `SELECT agent_name, scope
		FROM k12_practice_generation_jobs WHERE generation_job_id=?`,
		invocation.JobID,
	).Scan(&jobOwner, &scope); errors.Is(err, sql.ErrNoRows) {
		return k12.ModelInvocation{}, false, records.ErrNotFound
	} else if err != nil {
		return k12.ModelInvocation{}, false, fmt.Errorf(
			"k12storage: 校验逐题模型调用 job: %w", err,
		)
	}
	if jobOwner != invocation.AgentName || scope != "single" {
		return k12.ModelInvocation{}, false, fmt.Errorf(
			"%w: 逐题模型调用 owner/job 不匹配",
			ErrModelInvocationConflict,
		)
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
	res, err := s.db.ExecContext(ctx, `INSERT INTO k12_practice_generation_invocations (`+
		practiceGenerationInvocationColumns+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(job_id,stage,attempt) DO NOTHING`,
		invocation.InvocationID, invocation.AgentName, invocation.JobID, invocation.Stage,
		invocation.RequestDigest, invocation.RouteSnapshot.Provider,
		invocation.RouteSnapshot.Model, string(routeJSON),
		invocation.ProviderIdempotencyKey, invocation.Status, invocation.Attempt,
		"", "", "", invocation.CreatedAt, invocation.UpdatedAt,
	)
	if err != nil {
		return k12.ModelInvocation{}, false, fmt.Errorf(
			"k12storage: 准备逐题模型调用: %w", err,
		)
	}
	created, _ := res.RowsAffected()
	stored, err := s.getPracticeGenerationInvocationByAttempt(
		ctx, invocation.JobID, invocation.Stage, invocation.Attempt,
	)
	if err != nil {
		return k12.ModelInvocation{}, false, err
	}
	if stored.AgentName != invocation.AgentName ||
		stored.RequestDigest != invocation.RequestDigest ||
		stored.RouteSnapshot.Provider != invocation.RouteSnapshot.Provider ||
		stored.RouteSnapshot.Model != invocation.RouteSnapshot.Model ||
		stored.RouteSnapshot.Route != invocation.RouteSnapshot.Route {
		return k12.ModelInvocation{}, false, fmt.Errorf(
			"%w: job=%s stage=%s attempt=%d",
			ErrModelInvocationConflict, invocation.JobID,
			invocation.Stage, invocation.Attempt,
		)
	}
	return stored, created > 0, nil
}

func (s *Store) getPracticeGenerationInvocationByAttempt(
	ctx context.Context,
	jobID, stage string,
	attempt int,
) (k12.ModelInvocation, error) {
	invocation, err := scanPracticeGenerationInvocation(s.db.QueryRowContext(
		ctx,
		`SELECT `+practiceGenerationInvocationColumns+`
		 FROM k12_practice_generation_invocations
		 WHERE job_id=? AND stage=? AND attempt=?`,
		jobID, stage, attempt,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.ModelInvocation{}, records.ErrNotFound
	}
	if err != nil {
		return k12.ModelInvocation{}, fmt.Errorf(
			"k12storage: 按 attempt 读取逐题模型调用: %w", err,
		)
	}
	return invocation, nil
}

func (s *Store) GetPracticeGenerationInvocation(
	ctx context.Context,
	agentName, invocationID string,
) (k12.ModelInvocation, error) {
	invocation, err := scanPracticeGenerationInvocation(s.db.QueryRowContext(
		ctx,
		`SELECT `+practiceGenerationInvocationColumns+`
		 FROM k12_practice_generation_invocations
		 WHERE invocation_id=? AND agent_name=?`,
		invocationID, agentName,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.ModelInvocation{}, records.ErrNotFound
	}
	if err != nil {
		return k12.ModelInvocation{}, fmt.Errorf(
			"k12storage: 读取逐题模型调用: %w", err,
		)
	}
	return invocation, nil
}

func (s *Store) ListPracticeGenerationInvocations(
	ctx context.Context,
	agentName, jobID string,
) ([]k12.ModelInvocation, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+practiceGenerationInvocationColumns+`
		 FROM k12_practice_generation_invocations
		 WHERE agent_name=? AND job_id=? ORDER BY stage,attempt`,
		agentName, jobID,
	)
	if err != nil {
		return nil, fmt.Errorf("k12storage: 列逐题模型调用: %w", err)
	}
	defer rows.Close()
	out := make([]k12.ModelInvocation, 0)
	for rows.Next() {
		invocation, scanErr := scanPracticeGenerationInvocation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, invocation)
	}
	return out, rows.Err()
}

func (s *Store) transitionPracticeGenerationInvocation(
	ctx context.Context,
	agentName, invocationID string,
	from []k12.ModelInvocationStatus,
	to k12.ModelInvocationStatus,
	providerKey, resultDigest, externalRequestID, failureKind string,
) (k12.ModelInvocation, error) {
	holders := make([]string, len(from))
	for i := range from {
		holders[i] = "?"
	}
	args := []any{
		to, providerKey, providerKey, resultDigest, resultDigest,
		externalRequestID, externalRequestID, failureKind, failureKind,
		nowUnix(), invocationID, agentName,
	}
	args = append(args, statusArgs(from)...)
	res, err := s.db.ExecContext(ctx, `UPDATE k12_practice_generation_invocations SET
		status=?,
		provider_idempotency_key=CASE WHEN ?='' THEN provider_idempotency_key ELSE ? END,
		result_digest=CASE WHEN ?='' THEN result_digest ELSE ? END,
		external_request_id=CASE WHEN ?='' THEN external_request_id ELSE ? END,
		failure_kind=CASE WHEN ?='' THEN failure_kind ELSE ? END,
		updated_at=?
		WHERE invocation_id=? AND agent_name=? AND status IN (`+
		strings.Join(holders, ",")+`)`,
		args...,
	)
	if err != nil {
		return k12.ModelInvocation{}, fmt.Errorf(
			"k12storage: 推进逐题模型调用到 %s: %w", to, err,
		)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		current, getErr := s.GetPracticeGenerationInvocation(
			ctx, agentName, invocationID,
		)
		if getErr != nil {
			return k12.ModelInvocation{}, getErr
		}
		if current.Status != to {
			return k12.ModelInvocation{}, fmt.Errorf(
				"%w: 逐题 invocation %s status %s -> %s",
				records.ErrIllegalTransition, invocationID, current.Status, to,
			)
		}
		return current, nil
	}
	return s.GetPracticeGenerationInvocation(ctx, agentName, invocationID)
}

func (s *Store) MarkPracticeGenerationInvocationSent(
	ctx context.Context,
	agentName, invocationID, providerKey string,
) (k12.ModelInvocation, error) {
	invocation, _, err := s.ClaimPracticeGenerationInvocationSend(
		ctx, agentName, invocationID, providerKey,
	)
	return invocation, err
}

// ClaimPracticeGenerationInvocationSend atomically grants exactly one local
// worker authority to cross the before-send point.
func (s *Store) ClaimPracticeGenerationInvocationSend(
	ctx context.Context,
	agentName, invocationID, providerKey string,
) (k12.ModelInvocation, bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE k12_practice_generation_invocations SET
		status=?, provider_idempotency_key=CASE WHEN ?='' THEN provider_idempotency_key ELSE ? END,
		updated_at=? WHERE invocation_id=? AND agent_name=? AND status=?`,
		k12.ModelInvocationSent, providerKey, providerKey, nowUnix(),
		invocationID, agentName, k12.ModelInvocationPrepared,
	)
	if err != nil {
		return k12.ModelInvocation{}, false, fmt.Errorf(
			"k12storage: 领取逐题模型调用发送权: %w", err,
		)
	}
	claimed, _ := res.RowsAffected()
	invocation, getErr := s.GetPracticeGenerationInvocation(
		ctx, agentName, invocationID,
	)
	if getErr != nil {
		return k12.ModelInvocation{}, false, getErr
	}
	if claimed == 0 && invocation.Status == k12.ModelInvocationPrepared {
		return k12.ModelInvocation{}, false, fmt.Errorf(
			"%w: 逐题 invocation %s 未能领取发送权",
			records.ErrIllegalTransition, invocationID,
		)
	}
	return invocation, claimed == 1, nil
}

func (s *Store) MarkPracticeGenerationInvocationSucceeded(
	ctx context.Context,
	agentName, invocationID, resultDigest, externalRequestID string,
) (k12.ModelInvocation, error) {
	if strings.TrimSpace(resultDigest) == "" {
		return k12.ModelInvocation{}, fmt.Errorf(
			"k12storage: 成功的逐题模型调用必须有结果摘要",
		)
	}
	return s.transitionPracticeGenerationInvocation(
		ctx, agentName, invocationID,
		[]k12.ModelInvocationStatus{k12.ModelInvocationSent},
		k12.ModelInvocationSucceeded, "", resultDigest, externalRequestID, "",
	)
}

func (s *Store) MarkPracticeGenerationInvocationFailed(
	ctx context.Context,
	agentName, invocationID, failureKind string,
) (k12.ModelInvocation, error) {
	if strings.TrimSpace(failureKind) == "" {
		return k12.ModelInvocation{}, fmt.Errorf(
			"k12storage: 失败的逐题模型调用必须有失败类型",
		)
	}
	return s.transitionPracticeGenerationInvocation(
		ctx, agentName, invocationID,
		[]k12.ModelInvocationStatus{k12.ModelInvocationSent},
		k12.ModelInvocationFailed, "", "", "", failureKind,
	)
}

func (s *Store) MarkPracticeGenerationInvocationOutcomeUnknown(
	ctx context.Context,
	agentName, invocationID, failureKind string,
) (k12.ModelInvocation, error) {
	if strings.TrimSpace(failureKind) == "" {
		return k12.ModelInvocation{}, fmt.Errorf(
			"k12storage: 结果未知的逐题模型调用必须有失败类型",
		)
	}
	return s.transitionPracticeGenerationInvocation(
		ctx, agentName, invocationID,
		[]k12.ModelInvocationStatus{k12.ModelInvocationSent},
		k12.ModelInvocationOutcomeUnknown, "", "", "", failureKind,
	)
}

func (s *Store) ReconcilePracticeGenerationInvocationSucceeded(
	ctx context.Context,
	agentName, invocationID, resultDigest, externalRequestID string,
) (k12.ModelInvocation, error) {
	if strings.TrimSpace(resultDigest) == "" {
		return k12.ModelInvocation{}, fmt.Errorf(
			"k12storage: 核实成功的逐题模型调用必须有结果摘要",
		)
	}
	return s.transitionPracticeGenerationInvocation(
		ctx, agentName, invocationID,
		[]k12.ModelInvocationStatus{k12.ModelInvocationOutcomeUnknown},
		k12.ModelInvocationReconciled, "", resultDigest,
		externalRequestID, "reconciled_succeeded",
	)
}
