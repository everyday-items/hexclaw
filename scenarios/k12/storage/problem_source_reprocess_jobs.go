package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrProblemSourceReprocessNotFound             = errors.New("problem source reprocess job not found in owner scope")
	ErrProblemSourceReprocessFenced               = errors.New("problem source reprocess job lease fenced")
	ErrProblemSourceReprocessReconciliationFenced = errors.New("problem source reprocess reconciliation lease fenced")
	ErrProblemSourceReprocessConflict             = errors.New("problem source reprocess job state conflict")
	ErrProblemSourceReprocessInvalid              = errors.New("invalid problem source reprocess job operation")
)

type ProblemSourceReprocessStatus string

const (
	ProblemSourceReprocessPrepared          ProblemSourceReprocessStatus = "prepared"
	ProblemSourceReprocessQueued            ProblemSourceReprocessStatus = "queued"
	ProblemSourceReprocessRunning           ProblemSourceReprocessStatus = "running"
	ProblemSourceReprocessNeedsConfirmation ProblemSourceReprocessStatus = "needs_confirmation"
	ProblemSourceReprocessSucceeded         ProblemSourceReprocessStatus = "succeeded"
	ProblemSourceReprocessFailed            ProblemSourceReprocessStatus = "failed"
	ProblemSourceReprocessOutcomeUnknown    ProblemSourceReprocessStatus = "outcome_unknown"
	ProblemSourceReprocessCancelled         ProblemSourceReprocessStatus = "cancelled"
)

// ProblemSourceReprocessLease is the minimum fencing token accepted by worker
// mutations. Work identity is located by WorkID; authority is proven only by
// the current LeaseOwner + monotonically increasing LeaseEpoch pair.
type ProblemSourceReprocessLease struct {
	WorkID     string
	LeaseOwner string
	LeaseEpoch int64
}

// ProblemSourceReprocessReconciliationLease is intentionally independent from
// the provider-send worker lease. Holding it authorizes inspection and a
// terminal resolution only; it can never make outcome_unknown replayable by
// ClaimProblemSourceReprocessJob.
type ProblemSourceReprocessReconciliationLease struct {
	WorkID              string
	ReconciliationOwner string
	ReconciliationEpoch int64
}

type ProblemSourceReprocessOutcomeUnknownResolution string

const (
	ProblemSourceReprocessOutcomeUnknownResolutionSucceeded         ProblemSourceReprocessOutcomeUnknownResolution = "succeeded"
	ProblemSourceReprocessOutcomeUnknownResolutionNeedsConfirmation ProblemSourceReprocessOutcomeUnknownResolution = "needs_confirmation"
)

// ProblemSourceReprocessFailure is durable, bounded operator evidence. A
// retryable failure additionally requires RetryAt to be strictly after the
// mutation time. For outcome_unknown, a future RetryAt schedules only a
// provider reconciliation check and never an ordinary work retry.
type ProblemSourceReprocessFailure struct {
	Code    string
	Detail  string
	RetryAt time.Time
}

// ProblemSourceReprocessJob is the typed durable view of one immutable source
// action work item plus its mutable lease/state-machine envelope. Lease and
// retry deadlines are UTC Unix milliseconds; created/updated audit timestamps
// retain the repository-wide Unix-second convention used when V72 enqueues.
type ProblemSourceReprocessJob struct {
	WorkID                       string
	CommandReceiptID             string
	OwnerScope                   string
	AgentName                    string
	DispatchID                   string
	JobID                        string
	ProblemID                    string
	Action                       string
	StructureVersion             int
	InputRevision                int
	InputDigest                  string
	AffectedProblemIDs           []string
	RequestJSON                  json.RawMessage
	Status                       ProblemSourceReprocessStatus
	LeaseOwner                   string
	LeaseEpoch                   int64
	LeaseExpiresAtMilli          int64
	AttemptCount                 int
	NextAttemptAtMilli           int64
	ReconciliationOwner          string
	ReconciliationEpoch          int64
	ReconciliationExpiresAtMilli int64
	ReconciliationAttemptCount   int
	NextReconcileAtMilli         int64
	FailureCode                  string
	FailureDetail                string
	CreatedAt                    int64
	UpdatedAt                    int64
}

func (job ProblemSourceReprocessJob) Lease() ProblemSourceReprocessLease {
	return ProblemSourceReprocessLease{
		WorkID: job.WorkID, LeaseOwner: job.LeaseOwner, LeaseEpoch: job.LeaseEpoch,
	}
}

func (job ProblemSourceReprocessJob) ReconciliationLease() ProblemSourceReprocessReconciliationLease {
	return ProblemSourceReprocessReconciliationLease{
		WorkID:              job.WorkID,
		ReconciliationOwner: job.ReconciliationOwner,
		ReconciliationEpoch: job.ReconciliationEpoch,
	}
}

const problemSourceReprocessColumns = `
work_id,command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,problem_id,
action,structure_version,input_revision,input_digest,affected_problem_ids_json,
request_json,status,lease_owner,lease_epoch,lease_expires_at,attempt_count,
next_attempt_at,reconciliation_owner,reconciliation_epoch,reconciliation_expires_at,
reconciliation_attempt_count,next_reconcile_at,failure_code,failure_detail,created_at,updated_at`

func scanProblemSourceReprocessJob(row rowScanner) (ProblemSourceReprocessJob, error) {
	var (
		job                       ProblemSourceReprocessJob
		affectedJSON, requestJSON string
		status                    string
	)
	if err := row.Scan(
		&job.WorkID,
		&job.CommandReceiptID,
		&job.OwnerScope,
		&job.AgentName,
		&job.DispatchID,
		&job.JobID,
		&job.ProblemID,
		&job.Action,
		&job.StructureVersion,
		&job.InputRevision,
		&job.InputDigest,
		&affectedJSON,
		&requestJSON,
		&status,
		&job.LeaseOwner,
		&job.LeaseEpoch,
		&job.LeaseExpiresAtMilli,
		&job.AttemptCount,
		&job.NextAttemptAtMilli,
		&job.ReconciliationOwner,
		&job.ReconciliationEpoch,
		&job.ReconciliationExpiresAtMilli,
		&job.ReconciliationAttemptCount,
		&job.NextReconcileAtMilli,
		&job.FailureCode,
		&job.FailureDetail,
		&job.CreatedAt,
		&job.UpdatedAt,
	); err != nil {
		return ProblemSourceReprocessJob{}, err
	}
	if err := json.Unmarshal([]byte(affectedJSON), &job.AffectedProblemIDs); err != nil ||
		len(job.AffectedProblemIDs) == 0 {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"k12storage: decode source reprocess affected exact-set: %w",
			ErrProblemSourceReprocessInvalid,
		)
	}
	seenProblems := make(map[string]struct{}, len(job.AffectedProblemIDs))
	for index := range job.AffectedProblemIDs {
		problemID := strings.TrimSpace(job.AffectedProblemIDs[index])
		if problemID == "" {
			return ProblemSourceReprocessJob{}, fmt.Errorf(
				"k12storage: source reprocess affected exact-set contains an empty problem: %w",
				ErrProblemSourceReprocessInvalid,
			)
		}
		if _, duplicate := seenProblems[problemID]; duplicate {
			return ProblemSourceReprocessJob{}, fmt.Errorf(
				"k12storage: source reprocess affected exact-set contains duplicate %q: %w",
				problemID,
				ErrProblemSourceReprocessInvalid,
			)
		}
		seenProblems[problemID] = struct{}{}
		job.AffectedProblemIDs[index] = problemID
	}
	if !json.Valid([]byte(requestJSON)) {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"k12storage: source reprocess request is not JSON: %w",
			ErrProblemSourceReprocessInvalid,
		)
	}
	var requestObject map[string]json.RawMessage
	if err := json.Unmarshal([]byte(requestJSON), &requestObject); err != nil || requestObject == nil {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"k12storage: source reprocess request is not an object: %w",
			ErrProblemSourceReprocessInvalid,
		)
	}
	job.RequestJSON = append(json.RawMessage(nil), requestJSON...)
	job.Status = ProblemSourceReprocessStatus(status)
	return job, nil
}

func normalizeProblemSourceReprocessLookup(ownerScope, workID string) (string, string, error) {
	ownerScope = strings.TrimSpace(ownerScope)
	workID = strings.TrimSpace(workID)
	if ownerScope == "" || workID == "" {
		return "", "", fmt.Errorf(
			"%w: owner_scope and work_id are required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	return ownerScope, workID, nil
}

func normalizeProblemSourceReprocessLease(
	lease ProblemSourceReprocessLease,
) (ProblemSourceReprocessLease, error) {
	lease.WorkID = strings.TrimSpace(lease.WorkID)
	lease.LeaseOwner = strings.TrimSpace(lease.LeaseOwner)
	if lease.WorkID == "" || lease.LeaseOwner == "" || lease.LeaseEpoch < 1 {
		return ProblemSourceReprocessLease{}, fmt.Errorf(
			"%w: work_id, lease_owner and a positive lease_epoch are required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	return lease, nil
}

func normalizeProblemSourceReprocessReconciliationLease(
	lease ProblemSourceReprocessReconciliationLease,
) (ProblemSourceReprocessReconciliationLease, error) {
	lease.WorkID = strings.TrimSpace(lease.WorkID)
	lease.ReconciliationOwner = strings.TrimSpace(lease.ReconciliationOwner)
	if lease.WorkID == "" || lease.ReconciliationOwner == "" ||
		lease.ReconciliationEpoch < 1 {
		return ProblemSourceReprocessReconciliationLease{}, fmt.Errorf(
			"%w: work_id, reconciliation_owner and a positive reconciliation_epoch are required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	return lease, nil
}

func problemSourceReprocessLeaseWindow(
	now time.Time,
	duration time.Duration,
) (int64, int64, error) {
	nowMilli := now.UTC().UnixMilli()
	durationMilli := duration.Milliseconds()
	const maxInt64 = int64(^uint64(0) >> 1)
	if nowMilli <= 0 || durationMilli <= 0 || nowMilli > maxInt64-durationMilli {
		return 0, 0, fmt.Errorf(
			"%w: positive now and lease duration are required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	return nowMilli, nowMilli + durationMilli, nil
}

func normalizeProblemSourceReprocessFailure(
	failure ProblemSourceReprocessFailure,
) (ProblemSourceReprocessFailure, error) {
	failure.Code = strings.TrimSpace(failure.Code)
	failure.Detail = strings.TrimSpace(failure.Detail)
	if failure.Code == "" || failure.Detail == "" ||
		len(failure.Code) > 128 || len(failure.Detail) > 4096 {
		return ProblemSourceReprocessFailure{}, fmt.Errorf(
			"%w: bounded failure code and detail are required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	return failure, nil
}

// GetProblemSourceReprocessJob is the owner-scoped inspection path. Owner
// mismatches deliberately collapse to not-found.
func (s *Store) GetProblemSourceReprocessJob(
	ctx context.Context,
	ownerScope string,
	workID string,
) (ProblemSourceReprocessJob, error) {
	if s == nil || s.db == nil {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"%w: store unavailable",
			ErrProblemSourceReprocessInvalid,
		)
	}
	ownerScope, workID, err := normalizeProblemSourceReprocessLookup(ownerScope, workID)
	if err != nil {
		return ProblemSourceReprocessJob{}, err
	}
	job, err := scanProblemSourceReprocessJob(s.db.QueryRowContext(ctx, `SELECT `+
		problemSourceReprocessColumns+`
		FROM k12_problem_source_reprocess_jobs
		WHERE owner_scope=? AND work_id=?`, ownerScope, workID))
	if errors.Is(err, sql.ErrNoRows) {
		return ProblemSourceReprocessJob{}, ErrProblemSourceReprocessNotFound
	}
	if err != nil {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"k12storage: inspect problem source reprocess job: %w",
			err,
		)
	}
	return job, nil
}

// ListRecoverableProblemSourceReprocessJobs is a bounded inspection snapshot;
// callers must still Claim to obtain authority. outcome_unknown,
// needs_confirmation and terminal states are intentionally absent.
func (s *Store) ListRecoverableProblemSourceReprocessJobs(
	ctx context.Context,
	now time.Time,
	limit int,
) ([]ProblemSourceReprocessJob, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("%w: store unavailable", ErrProblemSourceReprocessInvalid)
	}
	nowMilli := now.UTC().UnixMilli()
	if nowMilli <= 0 {
		return nil, fmt.Errorf("%w: positive inspection time is required", ErrProblemSourceReprocessInvalid)
	}
	if limit <= 0 || limit > 256 {
		limit = 32
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+problemSourceReprocessColumns+`
		FROM k12_problem_source_reprocess_jobs
		WHERE status IN ('prepared','queued')
		   OR (status='failed' AND next_attempt_at>0 AND next_attempt_at<=?)
		   OR (status='running' AND lease_expires_at<=?)
		ORDER BY
		  CASE status
		    WHEN 'prepared' THEN 0 WHEN 'queued' THEN 1
		    WHEN 'failed' THEN 2 ELSE 3 END,
		  CASE status
		    WHEN 'failed' THEN next_attempt_at
		    WHEN 'running' THEN lease_expires_at
		    ELSE created_at
		  END,
		  created_at,work_id
		LIMIT ?`, nowMilli, nowMilli, limit)
	if err != nil {
		return nil, fmt.Errorf("k12storage: list recoverable source reprocess jobs: %w", err)
	}
	defer rows.Close()
	jobs := make([]ProblemSourceReprocessJob, 0, limit)
	for rows.Next() {
		job, scanErr := scanProblemSourceReprocessJob(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("k12storage: scan recoverable source reprocess job: %w", scanErr)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("k12storage: iterate recoverable source reprocess jobs: %w", err)
	}
	return jobs, nil
}

// ListProblemSourceReprocessOutcomeUnknownDue returns a bounded inspection
// snapshot of ambiguous outcomes whose reconciliation schedule is due and
// whose dedicated reconciliation lease is absent or expired. A zero schedule
// is the legacy-compatible representation of "reconcile now". Returned rows
// confer no authority; callers must Claim the dedicated lease before acting.
func (s *Store) ListProblemSourceReprocessOutcomeUnknownDue(
	ctx context.Context,
	now time.Time,
	limit int,
) ([]ProblemSourceReprocessJob, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("%w: store unavailable", ErrProblemSourceReprocessInvalid)
	}
	nowMilli := now.UTC().UnixMilli()
	if nowMilli <= 0 {
		return nil, fmt.Errorf(
			"%w: positive reconciliation inspection time is required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	if limit <= 0 || limit > 256 {
		limit = 32
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+problemSourceReprocessColumns+`
		FROM k12_problem_source_reprocess_jobs
		WHERE status='outcome_unknown'
		  AND (next_reconcile_at=0 OR next_reconcile_at<=?)
		  AND (reconciliation_owner='' OR reconciliation_expires_at<=?)
		ORDER BY
		  CASE WHEN next_reconcile_at=0 THEN 0 ELSE 1 END,
		  CASE WHEN next_reconcile_at=0 THEN created_at ELSE next_reconcile_at END,
		  created_at,work_id
		LIMIT ?`, nowMilli, nowMilli, limit)
	if err != nil {
		return nil, fmt.Errorf(
			"k12storage: list due problem source reconciliations: %w",
			err,
		)
	}
	defer rows.Close()
	jobs := make([]ProblemSourceReprocessJob, 0, limit)
	for rows.Next() {
		job, scanErr := scanProblemSourceReprocessJob(rows)
		if scanErr != nil {
			return nil, fmt.Errorf(
				"k12storage: scan due problem source reconciliation: %w",
				scanErr,
			)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"k12storage: iterate due problem source reconciliations: %w",
			err,
		)
	}
	return jobs, nil
}

// ClaimProblemSourceReprocessOutcomeUnknownForReconciliation atomically takes
// one due ambiguous outcome without changing its status to running. This path
// grants authority to inspect an already-sent provider request; it never
// grants authority to send the source request again.
func (s *Store) ClaimProblemSourceReprocessOutcomeUnknownForReconciliation(
	ctx context.Context,
	reconcilerID string,
	now time.Time,
	leaseDuration time.Duration,
) (ProblemSourceReprocessJob, bool, error) {
	reconcilerID = strings.TrimSpace(reconcilerID)
	if s == nil || s.db == nil || reconcilerID == "" || len(reconcilerID) > 255 {
		return ProblemSourceReprocessJob{}, false, fmt.Errorf(
			"%w: store and bounded reconciler_id are required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	nowMilli, leaseExpiresAt, err := problemSourceReprocessLeaseWindow(now, leaseDuration)
	if err != nil {
		return ProblemSourceReprocessJob{}, false, err
	}
	nowUnix := now.UTC().Unix()
	if nowUnix <= 0 {
		return ProblemSourceReprocessJob{}, false, fmt.Errorf(
			"%w: positive reconciliation audit time is required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	job, err := scanProblemSourceReprocessJob(s.db.QueryRowContext(ctx, `
		UPDATE k12_problem_source_reprocess_jobs
		SET reconciliation_owner=?,
		    reconciliation_epoch=reconciliation_epoch+1,
		    reconciliation_expires_at=?,
		    reconciliation_attempt_count=reconciliation_attempt_count+1,
		    next_reconcile_at=0,
		    updated_at=?
		WHERE work_id=(
		  SELECT work_id
		  FROM k12_problem_source_reprocess_jobs
		  WHERE status='outcome_unknown'
		    AND (next_reconcile_at=0 OR next_reconcile_at<=?)
		    AND (reconciliation_owner='' OR reconciliation_expires_at<=?)
		  ORDER BY
		    CASE WHEN next_reconcile_at=0 THEN 0 ELSE 1 END,
		    CASE WHEN next_reconcile_at=0 THEN created_at ELSE next_reconcile_at END,
		    created_at,work_id
		  LIMIT 1
		)
		AND status='outcome_unknown'
		AND (next_reconcile_at=0 OR next_reconcile_at<=?)
		AND (reconciliation_owner='' OR reconciliation_expires_at<=?)
		RETURNING `+problemSourceReprocessColumns,
		reconcilerID,
		leaseExpiresAt,
		nowUnix,
		nowMilli,
		nowMilli,
		nowMilli,
		nowMilli,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return ProblemSourceReprocessJob{}, false, nil
	}
	if err != nil {
		return ProblemSourceReprocessJob{}, false, fmt.Errorf(
			"k12storage: claim problem source reconciliation: %w",
			err,
		)
	}
	return job, true, nil
}

// ClaimProblemSourceReprocessJob atomically takes one prepared, queued, due
// retryable-failed, or expired-running row. A takeover increments both
// attempt_count and lease_epoch, fencing every prior worker.
func (s *Store) ClaimProblemSourceReprocessJob(
	ctx context.Context,
	workerID string,
	now time.Time,
	leaseDuration time.Duration,
) (ProblemSourceReprocessJob, bool, error) {
	workerID = strings.TrimSpace(workerID)
	if s == nil || s.db == nil || workerID == "" || len(workerID) > 255 {
		return ProblemSourceReprocessJob{}, false, fmt.Errorf(
			"%w: store and bounded worker_id are required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	nowMilli, leaseExpiresAt, err := problemSourceReprocessLeaseWindow(now, leaseDuration)
	if err != nil {
		return ProblemSourceReprocessJob{}, false, err
	}
	nowUnix := now.UTC().Unix()
	if nowUnix <= 0 {
		return ProblemSourceReprocessJob{}, false, fmt.Errorf(
			"%w: positive audit time is required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	job, err := scanProblemSourceReprocessJob(s.db.QueryRowContext(ctx, `
		UPDATE k12_problem_source_reprocess_jobs
		SET status='running',lease_owner=?,lease_epoch=lease_epoch+1,
		    lease_expires_at=?,attempt_count=attempt_count+1,next_attempt_at=0,
		    failure_code='',failure_detail='',updated_at=?
		WHERE work_id=(
		  SELECT work_id
		  FROM k12_problem_source_reprocess_jobs
		  WHERE status IN ('prepared','queued')
		     OR (status='failed' AND next_attempt_at>0 AND next_attempt_at<=?)
		     OR (status='running' AND lease_expires_at<=?)
		  ORDER BY
		    CASE status
		      WHEN 'prepared' THEN 0 WHEN 'queued' THEN 1
		      WHEN 'failed' THEN 2 ELSE 3 END,
		    CASE status
		      WHEN 'failed' THEN next_attempt_at
		      WHEN 'running' THEN lease_expires_at
		      ELSE created_at
		    END,
		    created_at,work_id
		  LIMIT 1
		)
		AND (
		  status IN ('prepared','queued')
		  OR (status='failed' AND next_attempt_at>0 AND next_attempt_at<=?)
		  OR (status='running' AND lease_expires_at<=?)
		)
		RETURNING `+problemSourceReprocessColumns,
		workerID,
		leaseExpiresAt,
		nowUnix,
		nowMilli,
		nowMilli,
		nowMilli,
		nowMilli,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return ProblemSourceReprocessJob{}, false, nil
	}
	if err != nil {
		return ProblemSourceReprocessJob{}, false, fmt.Errorf(
			"k12storage: claim problem source reprocess job: %w",
			err,
		)
	}
	return job, true, nil
}

// HeartbeatProblemSourceReprocessJob renews only the current, unexpired
// owner+epoch lease and never shortens its existing deadline.
func (s *Store) HeartbeatProblemSourceReprocessJob(
	ctx context.Context,
	lease ProblemSourceReprocessLease,
	now time.Time,
	leaseDuration time.Duration,
) (ProblemSourceReprocessJob, error) {
	if s == nil || s.db == nil {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"%w: store unavailable",
			ErrProblemSourceReprocessInvalid,
		)
	}
	lease, err := normalizeProblemSourceReprocessLease(lease)
	if err != nil {
		return ProblemSourceReprocessJob{}, err
	}
	nowMilli, leaseExpiresAt, err := problemSourceReprocessLeaseWindow(now, leaseDuration)
	if err != nil {
		return ProblemSourceReprocessJob{}, err
	}
	nowUnix := now.UTC().Unix()
	if nowUnix <= 0 {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"%w: positive audit time is required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	job, err := scanProblemSourceReprocessJob(s.db.QueryRowContext(ctx, `
		UPDATE k12_problem_source_reprocess_jobs
		SET lease_expires_at=CASE
		      WHEN lease_expires_at>? THEN lease_expires_at ELSE ? END,
		    updated_at=?
		WHERE work_id=? AND status='running' AND lease_owner=? AND lease_epoch=?
		  AND lease_expires_at>?
		RETURNING `+problemSourceReprocessColumns,
		leaseExpiresAt,
		leaseExpiresAt,
		nowUnix,
		lease.WorkID,
		lease.LeaseOwner,
		lease.LeaseEpoch,
		nowMilli,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return ProblemSourceReprocessJob{}, ErrProblemSourceReprocessFenced
	}
	if err != nil {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"k12storage: heartbeat problem source reprocess job: %w",
			err,
		)
	}
	return job, nil
}

// HeartbeatProblemSourceReprocessOutcomeUnknownReconciliation renews only the
// current, unexpired dedicated reconciliation lease and never shortens its
// existing deadline. It does not touch the ordinary provider-send lease.
func (s *Store) HeartbeatProblemSourceReprocessOutcomeUnknownReconciliation(
	ctx context.Context,
	lease ProblemSourceReprocessReconciliationLease,
	now time.Time,
	leaseDuration time.Duration,
) (ProblemSourceReprocessJob, error) {
	if s == nil || s.db == nil {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"%w: store unavailable",
			ErrProblemSourceReprocessInvalid,
		)
	}
	lease, err := normalizeProblemSourceReprocessReconciliationLease(lease)
	if err != nil {
		return ProblemSourceReprocessJob{}, err
	}
	nowMilli, leaseExpiresAt, err := problemSourceReprocessLeaseWindow(now, leaseDuration)
	if err != nil {
		return ProblemSourceReprocessJob{}, err
	}
	nowUnix := now.UTC().Unix()
	if nowUnix <= 0 {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"%w: positive reconciliation audit time is required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	job, err := scanProblemSourceReprocessJob(s.db.QueryRowContext(ctx, `
		UPDATE k12_problem_source_reprocess_jobs
		SET reconciliation_expires_at=CASE
		      WHEN reconciliation_expires_at>? THEN reconciliation_expires_at ELSE ? END,
		    updated_at=?
		WHERE work_id=? AND status='outcome_unknown'
		  AND reconciliation_owner=? AND reconciliation_epoch=?
		  AND reconciliation_expires_at>?
		RETURNING `+problemSourceReprocessColumns,
		leaseExpiresAt,
		leaseExpiresAt,
		nowUnix,
		lease.WorkID,
		lease.ReconciliationOwner,
		lease.ReconciliationEpoch,
		nowMilli,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return ProblemSourceReprocessJob{}, ErrProblemSourceReprocessReconciliationFenced
	}
	if err != nil {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"k12storage: heartbeat problem source reconciliation: %w",
			err,
		)
	}
	return job, nil
}

// ReleaseProblemSourceReprocessJob returns a lifecycle-interrupted claim to
// the ready queue without spending a business processing attempt. LeaseEpoch
// remains monotonic so every previously issued token stays fenced.
func (s *Store) ReleaseProblemSourceReprocessJob(
	ctx context.Context,
	lease ProblemSourceReprocessLease,
	now time.Time,
) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: store unavailable", ErrProblemSourceReprocessInvalid)
	}
	lease, err := normalizeProblemSourceReprocessLease(lease)
	if err != nil {
		return err
	}
	nowMilli := now.UTC().UnixMilli()
	nowUnix := now.UTC().Unix()
	if nowMilli <= 0 || nowUnix <= 0 {
		return fmt.Errorf(
			"%w: positive lifecycle release time is required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE k12_problem_source_reprocess_jobs
		SET status='queued',lease_owner='',lease_expires_at=0,next_attempt_at=0,
		    attempt_count=CASE WHEN attempt_count>0 THEN attempt_count-1 ELSE 0 END,
		    failure_code='',failure_detail='',updated_at=?
		WHERE work_id=? AND status='running' AND lease_owner=? AND lease_epoch=?
		  AND lease_expires_at>?`,
		nowUnix,
		lease.WorkID,
		lease.LeaseOwner,
		lease.LeaseEpoch,
		nowMilli,
	)
	if err != nil {
		return fmt.Errorf("k12storage: release lifecycle-interrupted source work: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("k12storage: inspect lifecycle source release rows: %w", err)
	}
	if affected != 1 {
		return ErrProblemSourceReprocessFenced
	}
	return nil
}

// ReleaseProblemSourceReprocessOutcomeUnknownReconciliation abandons only the
// dedicated inspection lease after a process lifecycle interruption. The
// ambiguous provider fact and its failure evidence remain outcome_unknown;
// no ordinary provider-send claim becomes eligible.
func (s *Store) ReleaseProblemSourceReprocessOutcomeUnknownReconciliation(
	ctx context.Context,
	lease ProblemSourceReprocessReconciliationLease,
	now time.Time,
) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: store unavailable", ErrProblemSourceReprocessInvalid)
	}
	lease, err := normalizeProblemSourceReprocessReconciliationLease(lease)
	if err != nil {
		return err
	}
	nowMilli := now.UTC().UnixMilli()
	nowUnix := now.UTC().Unix()
	if nowMilli <= 0 || nowUnix <= 0 {
		return fmt.Errorf(
			"%w: positive reconciliation release time is required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE k12_problem_source_reprocess_jobs
		SET reconciliation_owner='',reconciliation_expires_at=0,
		    reconciliation_attempt_count=CASE
		      WHEN reconciliation_attempt_count>0 THEN reconciliation_attempt_count-1
		      ELSE 0 END,
		    next_reconcile_at=?,updated_at=?
		WHERE work_id=? AND status='outcome_unknown'
		  AND reconciliation_owner=? AND reconciliation_epoch=?
		  AND reconciliation_expires_at>?`,
		nowMilli,
		nowUnix,
		lease.WorkID,
		lease.ReconciliationOwner,
		lease.ReconciliationEpoch,
		nowMilli,
	)
	if err != nil {
		return fmt.Errorf("k12storage: release lifecycle-interrupted source reconciliation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("k12storage: inspect source reconciliation release rows: %w", err)
	}
	if affected != 1 {
		return ErrProblemSourceReprocessReconciliationFenced
	}
	return nil
}

func (s *Store) transitionProblemSourceReprocessLease(
	ctx context.Context,
	lease ProblemSourceReprocessLease,
	now time.Time,
	status ProblemSourceReprocessStatus,
	failure ProblemSourceReprocessFailure,
	nextAttemptAt int64,
) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: store unavailable", ErrProblemSourceReprocessInvalid)
	}
	lease, err := normalizeProblemSourceReprocessLease(lease)
	if err != nil {
		return err
	}
	nowMilli := now.UTC().UnixMilli()
	nowUnix := now.UTC().Unix()
	if nowMilli <= 0 || nowUnix <= 0 {
		return fmt.Errorf("%w: positive transition time is required", ErrProblemSourceReprocessInvalid)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE k12_problem_source_reprocess_jobs
		SET status=?,lease_owner='',lease_expires_at=0,next_attempt_at=?,
		    failure_code=?,failure_detail=?,updated_at=?
		WHERE work_id=? AND status='running' AND lease_owner=? AND lease_epoch=?
		  AND lease_expires_at>?`,
		string(status),
		nextAttemptAt,
		failure.Code,
		failure.Detail,
		nowUnix,
		lease.WorkID,
		lease.LeaseOwner,
		lease.LeaseEpoch,
		nowMilli,
	)
	if err != nil {
		return fmt.Errorf("k12storage: transition problem source reprocess job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("k12storage: transition source reprocess rows: %w", err)
	}
	if affected != 1 {
		return ErrProblemSourceReprocessFenced
	}
	return nil
}

func (s *Store) CompleteProblemSourceReprocessSucceeded(
	ctx context.Context,
	lease ProblemSourceReprocessLease,
	now time.Time,
) error {
	return s.transitionProblemSourceReprocessLease(
		ctx,
		lease,
		now,
		ProblemSourceReprocessSucceeded,
		ProblemSourceReprocessFailure{},
		0,
	)
}

func (s *Store) CompleteProblemSourceReprocessNeedsConfirmation(
	ctx context.Context,
	lease ProblemSourceReprocessLease,
	reason ProblemSourceReprocessFailure,
	now time.Time,
) error {
	reason, err := normalizeProblemSourceReprocessFailure(reason)
	if err != nil {
		return err
	}
	return s.transitionProblemSourceReprocessLease(
		ctx,
		lease,
		now,
		ProblemSourceReprocessNeedsConfirmation,
		reason,
		0,
	)
}

func (s *Store) FailProblemSourceReprocessRetryable(
	ctx context.Context,
	lease ProblemSourceReprocessLease,
	failure ProblemSourceReprocessFailure,
	now time.Time,
) error {
	failure, err := normalizeProblemSourceReprocessFailure(failure)
	if err != nil {
		return err
	}
	nowMilli := now.UTC().UnixMilli()
	retryAtMilli := failure.RetryAt.UTC().UnixMilli()
	if nowMilli <= 0 || retryAtMilli <= nowMilli {
		return fmt.Errorf(
			"%w: retry_at must be strictly after the failure time",
			ErrProblemSourceReprocessInvalid,
		)
	}
	return s.transitionProblemSourceReprocessLease(
		ctx,
		lease,
		now,
		ProblemSourceReprocessFailed,
		failure,
		retryAtMilli,
	)
}

// MarkProblemSourceReprocessOutcomeUnknown parks ambiguous provider work in a
// non-recoverable state. A future RetryAt schedules only reconciliation; it is
// never copied into the ordinary retry schedule. A missing or non-future
// RetryAt remains legacy-compatible and is immediately eligible to reconcile.
func (s *Store) MarkProblemSourceReprocessOutcomeUnknown(
	ctx context.Context,
	lease ProblemSourceReprocessLease,
	failure ProblemSourceReprocessFailure,
	now time.Time,
) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: store unavailable", ErrProblemSourceReprocessInvalid)
	}
	lease, err := normalizeProblemSourceReprocessLease(lease)
	if err != nil {
		return err
	}
	failure, err = normalizeProblemSourceReprocessFailure(failure)
	if err != nil {
		return err
	}
	nowMilli := now.UTC().UnixMilli()
	nowUnix := now.UTC().Unix()
	if nowMilli <= 0 || nowUnix <= 0 {
		return fmt.Errorf(
			"%w: positive outcome-unknown transition time is required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	nextReconcileAt := int64(0)
	if failure.RetryAt.After(now) {
		nextReconcileAt = failure.RetryAt.UTC().UnixMilli()
		if nextReconcileAt <= 0 {
			return fmt.Errorf(
				"%w: future reconciliation time is outside the supported range",
				ErrProblemSourceReprocessInvalid,
			)
		}
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE k12_problem_source_reprocess_jobs
		SET status='outcome_unknown',
		    lease_owner='',lease_expires_at=0,next_attempt_at=0,
		    reconciliation_owner='',reconciliation_expires_at=0,
		    next_reconcile_at=?,failure_code=?,failure_detail=?,updated_at=?
		WHERE work_id=? AND status='running' AND lease_owner=? AND lease_epoch=?
		  AND lease_expires_at>?`,
		nextReconcileAt,
		failure.Code,
		failure.Detail,
		nowUnix,
		lease.WorkID,
		lease.LeaseOwner,
		lease.LeaseEpoch,
		nowMilli,
	)
	if err != nil {
		return fmt.Errorf("k12storage: mark source reprocess outcome unknown: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("k12storage: mark source outcome-unknown rows: %w", err)
	}
	if affected != 1 {
		return ErrProblemSourceReprocessFenced
	}
	return nil
}

// ResolveProblemSourceReprocessOutcomeUnknown consumes only a current,
// unexpired dedicated reconciliation lease. Reconciliation is terminal by
// construction: provider evidence may prove success or require a human
// confirmation, but it can never move work back into the send queue.
func (s *Store) ResolveProblemSourceReprocessOutcomeUnknown(
	ctx context.Context,
	lease ProblemSourceReprocessReconciliationLease,
	resolution ProblemSourceReprocessOutcomeUnknownResolution,
	failure ProblemSourceReprocessFailure,
	now time.Time,
) (ProblemSourceReprocessJob, error) {
	if s == nil || s.db == nil {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"%w: store unavailable",
			ErrProblemSourceReprocessInvalid,
		)
	}
	lease, err := normalizeProblemSourceReprocessReconciliationLease(lease)
	if err != nil {
		return ProblemSourceReprocessJob{}, err
	}

	var status ProblemSourceReprocessStatus
	switch resolution {
	case ProblemSourceReprocessOutcomeUnknownResolutionSucceeded:
		if strings.TrimSpace(failure.Code) != "" ||
			strings.TrimSpace(failure.Detail) != "" || !failure.RetryAt.IsZero() {
			return ProblemSourceReprocessJob{}, fmt.Errorf(
				"%w: succeeded reconciliation cannot retain failure evidence",
				ErrProblemSourceReprocessInvalid,
			)
		}
		failure = ProblemSourceReprocessFailure{}
		status = ProblemSourceReprocessSucceeded
	case ProblemSourceReprocessOutcomeUnknownResolutionNeedsConfirmation:
		if !failure.RetryAt.IsZero() {
			return ProblemSourceReprocessJob{}, fmt.Errorf(
				"%w: terminal reconciliation cannot schedule another attempt",
				ErrProblemSourceReprocessInvalid,
			)
		}
		failure, err = normalizeProblemSourceReprocessFailure(failure)
		if err != nil {
			return ProblemSourceReprocessJob{}, err
		}
		status = ProblemSourceReprocessNeedsConfirmation
	default:
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"%w: unsupported outcome-unknown reconciliation resolution %q",
			ErrProblemSourceReprocessInvalid,
			resolution,
		)
	}

	nowMilli := now.UTC().UnixMilli()
	nowUnix := now.UTC().Unix()
	if nowMilli <= 0 || nowUnix <= 0 {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"%w: positive reconciliation resolution time is required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	job, err := scanProblemSourceReprocessJob(s.db.QueryRowContext(ctx, `
		UPDATE k12_problem_source_reprocess_jobs
		SET status=?,
		    lease_owner='',lease_expires_at=0,next_attempt_at=0,
		    reconciliation_owner='',reconciliation_expires_at=0,next_reconcile_at=0,
		    failure_code=?,failure_detail=?,updated_at=?
		WHERE work_id=? AND status='outcome_unknown'
		  AND reconciliation_owner=? AND reconciliation_epoch=?
		  AND reconciliation_expires_at>?
		RETURNING `+problemSourceReprocessColumns,
		string(status),
		failure.Code,
		failure.Detail,
		nowUnix,
		lease.WorkID,
		lease.ReconciliationOwner,
		lease.ReconciliationEpoch,
		nowMilli,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return ProblemSourceReprocessJob{}, ErrProblemSourceReprocessReconciliationFenced
	}
	if err != nil {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"k12storage: resolve problem source reconciliation: %w",
			err,
		)
	}
	return job, nil
}

// CancelProblemSourceReprocessJob is owner-scoped and may fence prepared,
// queued, running, retryable-failed, or needs-confirmation work. It will not
// erase a succeeded or outcome_unknown fact. Exact cancellation replay is
// idempotent.
func (s *Store) CancelProblemSourceReprocessJob(
	ctx context.Context,
	ownerScope string,
	workID string,
	reason string,
	now time.Time,
) (ProblemSourceReprocessJob, error) {
	if s == nil || s.db == nil {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"%w: store unavailable",
			ErrProblemSourceReprocessInvalid,
		)
	}
	ownerScope, workID, err := normalizeProblemSourceReprocessLookup(ownerScope, workID)
	if err != nil {
		return ProblemSourceReprocessJob{}, err
	}
	reason = strings.TrimSpace(reason)
	nowMilli := now.UTC().UnixMilli()
	nowUnix := now.UTC().Unix()
	if reason == "" || len(reason) > 4096 || nowMilli <= 0 || nowUnix <= 0 {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"%w: bounded cancellation reason and positive time are required",
			ErrProblemSourceReprocessInvalid,
		)
	}
	job, err := scanProblemSourceReprocessJob(s.db.QueryRowContext(ctx, `
		UPDATE k12_problem_source_reprocess_jobs
		SET status='cancelled',lease_owner='',lease_expires_at=0,next_attempt_at=0,
		    failure_code='cancelled',failure_detail=?,updated_at=?
		WHERE owner_scope=? AND work_id=?
		  AND status IN ('prepared','queued','running','failed','needs_confirmation')
		RETURNING `+problemSourceReprocessColumns,
		reason,
		nowUnix,
		ownerScope,
		workID,
	))
	if err == nil {
		return job, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ProblemSourceReprocessJob{}, fmt.Errorf(
			"k12storage: cancel problem source reprocess job: %w",
			err,
		)
	}
	current, getErr := s.GetProblemSourceReprocessJob(ctx, ownerScope, workID)
	if getErr != nil {
		return ProblemSourceReprocessJob{}, getErr
	}
	if current.Status == ProblemSourceReprocessCancelled &&
		current.FailureCode == "cancelled" && current.FailureDetail == reason {
		return current, nil
	}
	return ProblemSourceReprocessJob{}, fmt.Errorf(
		"%w: cannot cancel source reprocess work in status %q",
		ErrProblemSourceReprocessConflict,
		current.Status,
	)
}
