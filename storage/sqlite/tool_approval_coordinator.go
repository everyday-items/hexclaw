package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
)

const toolApprovalColumns = `
approval_request_id, invocation_id, owner_id, resolved_session_id,
canonical_tool_name, arguments_digest, security_scope_digest,
scope_schema_version, arguments_envelope, deadline_at, state,
decision_id, decision, idempotency_key, decision_fingerprint,
terminal_result, ack_status, release_state, created_at, decided_at,
ack_committed_at, released_at, consumed_at`

const maxToolApprovalArgumentsEnvelopeBytes = 4 << 20

type toolApprovalRow struct {
	request             storage.ToolApprovalRequest
	state               string
	decisionID          string
	decision            string
	idempotencyKey      string
	decisionFingerprint string
	terminalResult      string
	ackStatus           string
	releaseState        string
	decidedAt           time.Time
	ackCommittedAt      time.Time
	releasedAt          time.Time
	consumedAt          time.Time
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanToolApprovalRow(scanner rowScanner) (*toolApprovalRow, error) {
	var row toolApprovalRow
	var deadlineAt, createdAt int64
	var decisionID, decision, idempotencyKey sql.NullString
	var decidedAt, ackCommittedAt, releasedAt, consumedAt sql.NullInt64
	if err := scanner.Scan(
		&row.request.RequestID, &row.request.InvocationID, &row.request.OwnerID,
		&row.request.ResolvedSessionID, &row.request.CanonicalToolName,
		&row.request.ArgumentsDigest, &row.request.SecurityScopeDigest,
		&row.request.ScopeSchemaVersion, &row.request.ArgumentsEnvelope,
		&deadlineAt, &row.state, &decisionID, &decision, &idempotencyKey,
		&row.decisionFingerprint, &row.terminalResult, &row.ackStatus,
		&row.releaseState, &createdAt, &decidedAt, &ackCommittedAt,
		&releasedAt, &consumedAt,
	); err != nil {
		return nil, err
	}
	row.request.DeadlineAt = time.Unix(0, deadlineAt).UTC()
	row.request.CreatedAt = time.UnixMilli(createdAt).UTC()
	row.decisionID = decisionID.String
	row.decision = decision.String
	row.idempotencyKey = idempotencyKey.String
	row.decidedAt = nullableUnixMilli(decidedAt)
	row.ackCommittedAt = nullableUnixMilli(ackCommittedAt)
	row.releasedAt = nullableUnixMilli(releasedAt)
	row.consumedAt = nullableUnixMilli(consumedAt)
	return &row, nil
}

func nullableUnixMilli(value sql.NullInt64) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return time.UnixMilli(value.Int64).UTC()
}

func nullableTimeMillis(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().UnixMilli()
}

func (r *toolApprovalRow) receipt(replayed bool) *storage.ToolApprovalReceipt {
	return &storage.ToolApprovalReceipt{
		RequestID: r.request.RequestID, InvocationID: r.request.InvocationID,
		OwnerID: r.request.OwnerID, ResolvedSessionID: r.request.ResolvedSessionID,
		ArgumentsDigest: r.request.ArgumentsDigest, SecurityScopeDigest: r.request.SecurityScopeDigest,
		ScopeSchemaVersion: r.request.ScopeSchemaVersion, DeadlineAt: r.request.DeadlineAt,
		State: r.state, DecisionID: r.decisionID, IdempotencyKey: r.idempotencyKey,
		Decision: r.decision, TerminalResult: r.terminalResult, ACKStatus: r.ackStatus,
		ReleaseState: r.releaseState, DecidedAt: r.decidedAt,
		ACKCommittedAt: r.ackCommittedAt, ReleasedAt: r.releasedAt,
		ConsumedAt: r.consumedAt, Replayed: replayed,
	}
}

func validateToolApprovalRequest(req *storage.ToolApprovalRequest) error {
	if req == nil {
		return errors.New("tool approval request is nil")
	}
	for name, value := range map[string]string{
		"approval_request_id": req.RequestID, "invocation_id": req.InvocationID,
		"owner_id": req.OwnerID, "resolved_session_id": req.ResolvedSessionID,
		"canonical_tool_name": req.CanonicalToolName, "arguments_digest": req.ArgumentsDigest,
		"security_scope_digest": req.SecurityScopeDigest,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if req.ScopeSchemaVersion != storage.CurrentToolApprovalScopeSchemaVersion {
		return fmt.Errorf("unsupported tool approval scope schema version %d", req.ScopeSchemaVersion)
	}
	if req.DeadlineAt.IsZero() {
		return errors.New("tool approval deadline is required")
	}
	if len(req.ArgumentsEnvelope) > maxToolApprovalArgumentsEnvelopeBytes {
		return errors.New("tool approval arguments envelope is too large")
	}
	return nil
}

func sameToolApprovalRequestIdentity(a, b *storage.ToolApprovalRequest) bool {
	if a == nil || b == nil {
		return false
	}
	return a.RequestID == b.RequestID && a.InvocationID == b.InvocationID &&
		a.OwnerID == b.OwnerID && a.ResolvedSessionID == b.ResolvedSessionID &&
		a.CanonicalToolName == b.CanonicalToolName && a.ArgumentsDigest == b.ArgumentsDigest &&
		a.SecurityScopeDigest == b.SecurityScopeDigest && a.ScopeSchemaVersion == b.ScopeSchemaVersion &&
		a.DeadlineAt.Equal(b.DeadlineAt)
}

// CreateToolApprovalRequest persists the backend-frozen identity before any
// transport send. Repeating the exact request is a no-op; any identity drift
// fails closed.
func (s *Store) CreateToolApprovalRequest(ctx context.Context, req *storage.ToolApprovalRequest) (bool, error) {
	if err := validateToolApprovalRequest(req); err != nil {
		return false, err
	}
	now := time.Now().UTC()
	if !req.CreatedAt.IsZero() {
		now = req.CreatedAt.UTC()
	}
	result, err := s.db.ExecContext(ctx, `
INSERT INTO tool_approval_requests (
    approval_request_id, invocation_id, owner_id, resolved_session_id,
    canonical_tool_name, arguments_digest, security_scope_digest,
    scope_schema_version, arguments_envelope, deadline_at, state,
    terminal_result, ack_status, release_state, created_at, updated_at
)
SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', '', 'pending', 'held', ?, ?
WHERE EXISTS (
    SELECT 1 FROM sessions WHERE id = ? AND user_id = ? AND status >= 0
)
ON CONFLICT(approval_request_id) DO NOTHING`,
		req.RequestID, req.InvocationID, req.OwnerID, req.ResolvedSessionID,
		req.CanonicalToolName, req.ArgumentsDigest, req.SecurityScopeDigest,
		req.ScopeSchemaVersion, req.ArgumentsEnvelope, req.DeadlineAt.UTC().UnixNano(),
		now.UnixMilli(), now.UnixMilli(), req.ResolvedSessionID, req.OwnerID,
	)
	if err != nil {
		return false, fmt.Errorf("create durable tool approval request: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 1 {
		return true, nil
	}
	existing, err := s.getToolApprovalRow(ctx, req.RequestID)
	if err == nil {
		if sameToolApprovalRequestIdentity(&existing.request, req) {
			return false, nil
		}
		return false, storage.ErrToolApprovalIdentityMismatch
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	var owned int
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ? AND user_id = ? AND status >= 0)`,
		req.ResolvedSessionID, req.OwnerID,
	).Scan(&owned); err != nil {
		return false, err
	}
	if owned != 1 {
		return false, storage.ErrToolApprovalIdentityMismatch
	}
	return false, storage.ErrToolApprovalIdentityMismatch
}

func (s *Store) getToolApprovalRow(ctx context.Context, requestID string) (*toolApprovalRow, error) {
	row, err := scanToolApprovalRow(s.db.QueryRowContext(ctx,
		`SELECT `+toolApprovalColumns+` FROM tool_approval_requests WHERE approval_request_id = ?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return row, err
}

func getToolApprovalRowTx(ctx context.Context, tx *sql.Tx, requestID string) (*toolApprovalRow, error) {
	row, err := scanToolApprovalRow(tx.QueryRowContext(ctx,
		`SELECT `+toolApprovalColumns+` FROM tool_approval_requests WHERE approval_request_id = ?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storage.ErrNotFound
	}
	return row, err
}

func validateToolApprovalDecision(decision *storage.ToolApprovalDecision) error {
	if decision == nil {
		return errors.New("tool approval decision is nil")
	}
	for name, value := range map[string]string{
		"approval_request_id": decision.RequestID, "invocation_id": decision.InvocationID,
		"owner_id": decision.OwnerID, "resolved_session_id": decision.ResolvedSessionID,
		"arguments_digest": decision.ArgumentsDigest, "security_scope_digest": decision.SecurityScopeDigest,
		"decision_id": decision.DecisionID, "idempotency_key": decision.IdempotencyKey,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if decision.ScopeSchemaVersion != storage.CurrentToolApprovalScopeSchemaVersion {
		return storage.ErrToolApprovalIdentityMismatch
	}
	switch decision.Decision {
	case storage.ToolApprovalDecisionApprovedOnce,
		storage.ToolApprovalDecisionApprovedRemember,
		storage.ToolApprovalDecisionDenied:
		return nil
	default:
		return errors.New("invalid tool approval decision")
	}
}

func toolApprovalDecisionFingerprint(decision *storage.ToolApprovalDecision) string {
	canonical := strings.Join([]string{
		decision.RequestID, decision.InvocationID, decision.OwnerID,
		decision.ResolvedSessionID, decision.ArgumentsDigest,
		decision.SecurityScopeDigest, fmt.Sprintf("%d", decision.ScopeSchemaVersion),
		decision.Decision,
	}, "\x00")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

func toolApprovalDecisionMatchesRequest(decision *storage.ToolApprovalDecision, req *storage.ToolApprovalRequest) bool {
	return decision.RequestID == req.RequestID && decision.InvocationID == req.InvocationID &&
		decision.OwnerID == req.OwnerID && decision.ResolvedSessionID == req.ResolvedSessionID &&
		decision.ArgumentsDigest == req.ArgumentsDigest && decision.SecurityScopeDigest == req.SecurityScopeDigest &&
		decision.ScopeSchemaVersion == req.ScopeSchemaVersion
}

// DecideToolApproval commits the terminal decision, optional remembered grant,
// one-time release intent, and ACK receipt in one SQLite transaction.
func (s *Store) DecideToolApproval(ctx context.Context, decision *storage.ToolApprovalDecision) (*storage.ToolApprovalReceipt, error) {
	if err := validateToolApprovalDecision(decision); err != nil {
		return nil, err
	}
	decidedAt := time.Now().UTC()
	if !decision.DecidedAt.IsZero() {
		decidedAt = decision.DecidedAt.UTC()
	}
	fingerprint := toolApprovalDecisionFingerprint(decision)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := getToolApprovalRowTx(ctx, tx, decision.RequestID)
	if err != nil {
		return nil, err
	}
	if !toolApprovalDecisionMatchesRequest(decision, &row.request) {
		return nil, storage.ErrToolApprovalIdentityMismatch
	}
	if row.state != storage.ToolApprovalStatePending {
		if (row.state == storage.ToolApprovalTerminalExpired || row.state == storage.ToolApprovalTerminalFenced) &&
			row.idempotencyKey == "" {
			if _, err := tx.ExecContext(ctx, `
UPDATE tool_approval_requests
SET decision_id = ?, decision = ?, idempotency_key = ?, decision_fingerprint = ?,
    decided_at = COALESCE(decided_at, ?), ack_committed_at = COALESCE(ack_committed_at, ?), updated_at = ?
WHERE approval_request_id = ? AND state IN ('expired','fenced') AND idempotency_key IS NULL`,
				decision.DecisionID, decision.Decision, decision.IdempotencyKey, fingerprint,
				decidedAt.UnixMilli(), decidedAt.UnixMilli(), decidedAt.UnixMilli(), decision.RequestID,
			); err != nil {
				return nil, mapToolApprovalConstraintError(err)
			}
			row, err = getToolApprovalRowTx(ctx, tx, decision.RequestID)
			if err != nil {
				return nil, err
			}
			if row.idempotencyKey != decision.IdempotencyKey || row.decisionFingerprint != fingerprint {
				return nil, storage.ErrToolApprovalConflict
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return row.receipt(false), nil
		}
		if row.idempotencyKey == decision.IdempotencyKey && row.decisionFingerprint == fingerprint {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return row.receipt(true), nil
		}
		return nil, storage.ErrToolApprovalConflict
	}

	var otherRequestID string
	err = tx.QueryRowContext(ctx,
		`SELECT approval_request_id FROM tool_approval_requests WHERE idempotency_key = ? LIMIT 1`,
		decision.IdempotencyKey,
	).Scan(&otherRequestID)
	if err == nil && otherRequestID != decision.RequestID {
		return nil, storage.ErrToolApprovalConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	state := decision.Decision
	terminalResult := decision.Decision
	ackStatus := storage.ToolApprovalACKAccepted
	releaseState := storage.ToolApprovalReleaseAuthorized
	var releasedAt any = decidedAt.UnixMilli()
	if !decidedAt.Before(row.request.DeadlineAt) {
		state = storage.ToolApprovalTerminalExpired
		terminalResult = storage.ToolApprovalTerminalExpired
		ackStatus = storage.ToolApprovalACKExpired
		releaseState = storage.ToolApprovalReleaseFenced
		releasedAt = nil
	} else if decision.Decision == storage.ToolApprovalDecisionDenied {
		releaseState = storage.ToolApprovalReleaseFenced
		releasedAt = nil
	}

	if state == storage.ToolApprovalDecisionApprovedRemember {
		grantID := "grant:" + row.request.RequestID
		if _, err := tx.ExecContext(ctx, `
INSERT INTO remembered_permission_grants (
    owner_id, resolved_session_id, canonical_tool_name, security_scope_digest,
    grant_id, created_request_id, created_decision_id, active,
    revoked_at, revoked_reason, schema_version
) VALUES (?, ?, ?, ?, ?, ?, ?, 1, NULL, '', ?)
ON CONFLICT(owner_id, resolved_session_id, canonical_tool_name, security_scope_digest)
DO UPDATE SET grant_id = excluded.grant_id,
              created_request_id = excluded.created_request_id,
              created_decision_id = excluded.created_decision_id,
              active = 1, revoked_at = NULL, revoked_reason = '',
              schema_version = excluded.schema_version,
              created_at = CURRENT_TIMESTAMP`,
			row.request.OwnerID, row.request.ResolvedSessionID, row.request.CanonicalToolName,
			row.request.SecurityScopeDigest, grantID, row.request.RequestID,
			decision.DecisionID, row.request.ScopeSchemaVersion,
		); err != nil {
			return nil, mapToolApprovalConstraintError(err)
		}
	}

	result, err := tx.ExecContext(ctx, `
UPDATE tool_approval_requests
SET state = ?, decision_id = ?, decision = ?, idempotency_key = ?,
    decision_fingerprint = ?, terminal_result = ?, ack_status = ?, release_state = ?,
    decided_at = ?, ack_committed_at = ?, released_at = ?, updated_at = ?
WHERE approval_request_id = ? AND state = 'pending'`,
		state, decision.DecisionID, decision.Decision, decision.IdempotencyKey,
		fingerprint, terminalResult, ackStatus, releaseState, decidedAt.UnixMilli(),
		decidedAt.UnixMilli(), releasedAt, decidedAt.UnixMilli(), decision.RequestID,
	)
	if err != nil {
		return nil, mapToolApprovalConstraintError(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, storage.ErrToolApprovalConflict
	}
	row, err = getToolApprovalRowTx(ctx, tx, decision.RequestID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return row.receipt(false), nil
}

func mapToolApprovalConstraintError(err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unique constraint") || strings.Contains(message, "constraint failed") {
		return fmt.Errorf("%w: %v", storage.ErrToolApprovalConflict, err)
	}
	return err
}

// ExpireToolApproval atomically wins only while the request is still pending.
// A concurrently committed decision is returned unchanged to the caller.
func (s *Store) ExpireToolApproval(ctx context.Context, requestID string, now time.Time) (*storage.ToolApprovalReceipt, error) {
	if strings.TrimSpace(requestID) == "" {
		return nil, errors.New("tool approval request id is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := getToolApprovalRowTx(ctx, tx, requestID)
	if err != nil {
		return nil, err
	}
	if row.state == storage.ToolApprovalStatePending && !now.Before(row.request.DeadlineAt) {
		if _, err := tx.ExecContext(ctx, `
UPDATE tool_approval_requests
SET state = 'expired', terminal_result = 'expired', ack_status = 'expired',
    release_state = 'fenced', decided_at = ?, ack_committed_at = ?, updated_at = ?
WHERE approval_request_id = ? AND state = 'pending'`,
			now.UnixMilli(), now.UnixMilli(), now.UnixMilli(), requestID,
		); err != nil {
			return nil, err
		}
		row, err = getToolApprovalRowTx(ctx, tx, requestID)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return row.receipt(false), nil
}

// FenceToolApprovalRequest terminally denies an invocation whose transport or
// parent operation was abandoned before a user decision. It never rewrites an
// already committed terminal.
func (s *Store) FenceToolApprovalRequest(
	ctx context.Context, requestID, reason string, now time.Time,
) (*storage.ToolApprovalReceipt, error) {
	if strings.TrimSpace(requestID) == "" {
		return nil, errors.New("tool approval request id is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := getToolApprovalRowTx(ctx, tx, requestID)
	if err != nil {
		return nil, err
	}
	if row.state == storage.ToolApprovalStatePending {
		if _, err := tx.ExecContext(ctx, `
UPDATE tool_approval_requests
SET state = 'fenced', terminal_result = 'fenced', ack_status = 'rejected',
    release_state = 'fenced', decided_at = ?, ack_committed_at = ?, updated_at = ?
WHERE approval_request_id = ? AND state = 'pending'`,
			now.UnixMilli(), now.UnixMilli(), now.UnixMilli(), requestID,
		); err != nil {
			return nil, err
		}
		row, err = getToolApprovalRowTx(ctx, tx, requestID)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	_ = reason // Terminal wire/ACK intentionally carries no free-form reason.
	return row.receipt(false), nil
}

// ConsumeToolApprovalRelease grants exactly one executor the right to proceed.
func (s *Store) ConsumeToolApprovalRelease(ctx context.Context, identity *storage.ToolApprovalExecutionIdentity) (bool, error) {
	if identity == nil || strings.TrimSpace(identity.RequestID) == "" {
		return false, errors.New("tool approval execution identity is required")
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE tool_approval_requests
SET release_state = 'consumed', consumed_at = ?, updated_at = ?
WHERE approval_request_id = ? AND invocation_id = ? AND owner_id = ?
  AND resolved_session_id = ? AND arguments_digest = ? AND security_scope_digest = ?
  AND scope_schema_version = ? AND state IN ('approved_once','approved_remember')
  AND release_state = 'authorized'`,
		now.UnixMilli(), now.UnixMilli(), identity.RequestID, identity.InvocationID,
		identity.OwnerID, identity.ResolvedSessionID, identity.ArgumentsDigest,
		identity.SecurityScopeDigest, identity.ScopeSchemaVersion,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected == 1 {
		return true, nil
	}
	row, err := s.getToolApprovalRow(ctx, identity.RequestID)
	if err != nil {
		return false, err
	}
	if row.request.InvocationID != identity.InvocationID || row.request.OwnerID != identity.OwnerID ||
		row.request.ResolvedSessionID != identity.ResolvedSessionID || row.request.ArgumentsDigest != identity.ArgumentsDigest ||
		row.request.SecurityScopeDigest != identity.SecurityScopeDigest || row.request.ScopeSchemaVersion != identity.ScopeSchemaVersion {
		return false, storage.ErrToolApprovalIdentityMismatch
	}
	return false, nil
}

func (s *Store) GetToolApprovalReceipt(ctx context.Context, requestID string) (*storage.ToolApprovalReceipt, error) {
	row, err := s.getToolApprovalRow(ctx, requestID)
	if err != nil {
		return nil, err
	}
	return row.receipt(false), nil
}

func (s *Store) ListPendingToolApprovals(
	ctx context.Context, ownerID, resolvedSessionID string, now time.Time,
) ([]*storage.ToolApprovalRequest, error) {
	if strings.TrimSpace(ownerID) == "" || strings.TrimSpace(resolvedSessionID) == "" {
		return nil, storage.ErrToolApprovalIdentityMismatch
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE tool_approval_requests
SET state = 'expired', terminal_result = 'expired', ack_status = 'expired',
    release_state = 'fenced', decided_at = ?, ack_committed_at = ?, updated_at = ?
WHERE owner_id = ? AND resolved_session_id = ? AND state = 'pending' AND deadline_at <= ?`,
		now.UnixMilli(), now.UnixMilli(), now.UnixMilli(), ownerID, resolvedSessionID, now.UnixNano(),
	); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT `+toolApprovalColumns+` FROM tool_approval_requests
WHERE owner_id = ? AND resolved_session_id = ? AND state = 'pending' AND deadline_at > ?
	ORDER BY created_at, approval_request_id`, ownerID, resolvedSessionID, now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var requests []*storage.ToolApprovalRequest
	for rows.Next() {
		row, err := scanToolApprovalRow(rows)
		if err != nil {
			return nil, err
		}
		request := row.request
		requests = append(requests, &request)
	}
	return requests, rows.Err()
}

// FenceOrphanedToolApprovals is called once during coordinator construction.
// A request from an earlier process has no live execution closure and must not
// be approved into an unconsumable/double-execution state.
func (s *Store) FenceOrphanedToolApprovals(ctx context.Context, startedAt time.Time) (int64, error) {
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE tool_approval_requests
SET state = 'fenced', terminal_result = 'fenced', ack_status = 'rejected',
    release_state = 'fenced', decided_at = ?, ack_committed_at = ?, updated_at = ?
WHERE state = 'pending' AND created_at < ?`,
		startedAt.UnixMilli(), startedAt.UnixMilli(), startedAt.UnixMilli(), startedAt.UnixMilli(),
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func fenceAndRevokeSessionToolAuthority(ctx context.Context, exec contextExecer, sessionID, reason string, now time.Time) error {
	if _, err := exec.ExecContext(ctx, `
UPDATE tool_approval_requests
SET state = CASE WHEN state = 'pending' THEN 'fenced' ELSE state END,
    terminal_result = CASE WHEN state = 'pending' THEN 'fenced' ELSE terminal_result END,
    ack_status = CASE WHEN state = 'pending' THEN 'rejected' ELSE ack_status END,
    release_state = CASE WHEN release_state IN ('held','authorized') THEN 'fenced' ELSE release_state END,
    decided_at = CASE WHEN state = 'pending' THEN ? ELSE decided_at END,
    ack_committed_at = CASE WHEN state = 'pending' THEN ? ELSE ack_committed_at END,
    updated_at = ?
WHERE resolved_session_id = ?`, now.UnixMilli(), now.UnixMilli(), now.UnixMilli(), sessionID); err != nil {
		return err
	}
	_, err := exec.ExecContext(ctx, `
UPDATE remembered_permission_grants
SET active = 0,
    revoked_at = COALESCE(revoked_at, ?),
    revoked_reason = CASE WHEN revoked_reason = '' THEN ? ELSE revoked_reason END
WHERE resolved_session_id = ?`, now.UnixMilli(), reason, sessionID)
	return err
}

func (s *Store) RevokeSessionToolApprovals(ctx context.Context, sessionID, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fenceAndRevokeSessionToolAuthority(ctx, tx, sessionID, reason, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}
