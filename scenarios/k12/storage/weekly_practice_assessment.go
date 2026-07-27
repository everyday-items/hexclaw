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
	"sync"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

const weeklyAssessmentCommandColumns = `command_id,agent_name,snapshot_id,item_id,
    idempotency_key,request_digest,status,assessment_json,assessment_digest,
    failure_kind,attempt_id,created_at,updated_at`

var weeklyAssessmentCommitMu sync.Mutex

func scanWeeklyAssessmentCommand(row rowScanner) (k12.WeeklyPracticeAssessmentCommand, error) {
	var command k12.WeeklyPracticeAssessmentCommand
	var status string
	err := row.Scan(&command.CommandID, &command.AgentName, &command.SnapshotID,
		&command.ItemID, &command.IdempotencyKey, &command.RequestDigest, &status,
		&command.AssessmentJSON, &command.AssessmentDigest, &command.FailureKind,
		&command.AttemptID, &command.CreatedAt, &command.UpdatedAt)
	command.Status = k12.WeeklyPracticeAssessmentCommandStatus(status)
	return command, err
}

func (s *Store) PrepareWeeklyPracticeAssessmentCommand(
	ctx context.Context,
	command k12.WeeklyPracticeAssessmentCommand,
) (k12.WeeklyPracticeAssessmentCommand, bool, error) {
	if strings.TrimSpace(command.CommandID) == "" ||
		strings.TrimSpace(command.AgentName) == "" ||
		strings.TrimSpace(command.SnapshotID) == "" ||
		strings.TrimSpace(command.ItemID) == "" ||
		strings.TrimSpace(command.IdempotencyKey) == "" ||
		len(command.RequestDigest) != 64 ||
		command.Status != k12.WeeklyAssessmentPrepared ||
		command.CreatedAt <= 0 || command.UpdatedAt <= 0 {
		return k12.WeeklyPracticeAssessmentCommand{}, false,
			fmt.Errorf("%w: incomplete weekly assessment command", records.ErrInvalidFields)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO k12_weekly_assessment_commands
        (`+weeklyAssessmentCommandColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(agent_name,snapshot_id,item_id,idempotency_key) DO NOTHING`,
		command.CommandID, command.AgentName, command.SnapshotID, command.ItemID,
		command.IdempotencyKey, command.RequestDigest, command.Status,
		command.AssessmentJSON, command.AssessmentDigest, command.FailureKind,
		command.AttemptID, command.CreatedAt, command.UpdatedAt)
	if err != nil {
		return k12.WeeklyPracticeAssessmentCommand{}, false,
			fmt.Errorf("k12storage: prepare weekly assessment: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return command, true, nil
	}
	stored, err := s.GetWeeklyPracticeAssessmentCommand(ctx, command.AgentName,
		command.SnapshotID, command.ItemID, command.IdempotencyKey, command.RequestDigest)
	if err != nil {
		return k12.WeeklyPracticeAssessmentCommand{}, false, err
	}
	if stored.CommandID != command.CommandID {
		return k12.WeeklyPracticeAssessmentCommand{}, false, records.ErrVersionConflict
	}
	return stored, false, nil
}

func (s *Store) GetWeeklyPracticeAssessmentCommand(
	ctx context.Context,
	agentName, snapshotID, itemID, idempotencyKey, requestDigest string,
) (k12.WeeklyPracticeAssessmentCommand, error) {
	command, err := scanWeeklyAssessmentCommand(s.db.QueryRowContext(ctx,
		`SELECT `+weeklyAssessmentCommandColumns+`
         FROM k12_weekly_assessment_commands
         WHERE agent_name=? AND snapshot_id=? AND item_id=? AND idempotency_key=?`,
		agentName, snapshotID, itemID, idempotencyKey))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.WeeklyPracticeAssessmentCommand{}, records.ErrNotFound
	}
	if err != nil {
		return k12.WeeklyPracticeAssessmentCommand{}, err
	}
	if command.RequestDigest != requestDigest {
		return k12.WeeklyPracticeAssessmentCommand{}, records.ErrVersionConflict
	}
	return command, nil
}

func (s *Store) ClaimWeeklyPracticeAssessment(
	ctx context.Context,
	command k12.WeeklyPracticeAssessmentCommand,
	at int64,
) (k12.WeeklyPracticeAssessmentCommand, bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE k12_weekly_assessment_commands
        SET status='sent',updated_at=?
        WHERE command_id=? AND agent_name=? AND request_digest=? AND status='prepared'`,
		at, command.CommandID, command.AgentName, command.RequestDigest)
	if err != nil {
		return k12.WeeklyPracticeAssessmentCommand{}, false, err
	}
	stored, getErr := s.GetWeeklyPracticeAssessmentCommand(ctx, command.AgentName,
		command.SnapshotID, command.ItemID, command.IdempotencyKey, command.RequestDigest)
	if getErr != nil {
		return k12.WeeklyPracticeAssessmentCommand{}, false, getErr
	}
	n, _ := res.RowsAffected()
	return stored, n == 1, nil
}

func (s *Store) MarkWeeklyPracticeAssessmentSucceeded(
	ctx context.Context,
	command k12.WeeklyPracticeAssessmentCommand,
	assessmentJSON, assessmentDigest string,
	at int64,
) (k12.WeeklyPracticeAssessmentCommand, error) {
	if !json.Valid([]byte(assessmentJSON)) || len(assessmentDigest) != 64 {
		return k12.WeeklyPracticeAssessmentCommand{},
			fmt.Errorf("%w: invalid weekly assessment receipt", records.ErrInvalidFields)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE k12_weekly_assessment_commands
        SET status='succeeded',assessment_json=?,assessment_digest=?,
            failure_kind='',updated_at=?
        WHERE command_id=? AND agent_name=? AND request_digest=? AND status='sent'`,
		assessmentJSON, assessmentDigest, at, command.CommandID,
		command.AgentName, command.RequestDigest)
	if err != nil {
		return k12.WeeklyPracticeAssessmentCommand{}, err
	}
	stored, getErr := s.GetWeeklyPracticeAssessmentCommand(ctx, command.AgentName,
		command.SnapshotID, command.ItemID, command.IdempotencyKey, command.RequestDigest)
	if getErr != nil {
		return k12.WeeklyPracticeAssessmentCommand{}, getErr
	}
	if n, _ := res.RowsAffected(); n == 0 &&
		(stored.Status != k12.WeeklyAssessmentSucceeded ||
			stored.AssessmentDigest != assessmentDigest ||
			stored.AssessmentJSON != assessmentJSON) {
		return k12.WeeklyPracticeAssessmentCommand{}, records.ErrVersionConflict
	}
	return stored, nil
}

func (s *Store) MarkWeeklyPracticeAssessmentTerminal(
	ctx context.Context,
	command k12.WeeklyPracticeAssessmentCommand,
	status k12.WeeklyPracticeAssessmentCommandStatus,
	failureKind string,
	at int64,
) error {
	if status != k12.WeeklyAssessmentFailed &&
		status != k12.WeeklyAssessmentOutcomeUnknown {
		return records.ErrInvalidStatus
	}
	_, err := s.db.ExecContext(ctx, `UPDATE k12_weekly_assessment_commands
        SET status=?,failure_kind=?,updated_at=?
        WHERE command_id=? AND agent_name=? AND request_digest=? AND status='sent'`,
		status, failureKind, at, command.CommandID, command.AgentName, command.RequestDigest)
	return err
}

func (s *Store) GetWeeklyPracticeAttempt(
	ctx context.Context,
	agentName, attemptID string,
) (k12.WeeklyPracticeAttempt, error) {
	return getWeeklyPracticeAttemptVia(ctx, s.db, agentName, attemptID)
}

func getWeeklyPracticeAttemptVia(
	ctx context.Context,
	q dbQueryer,
	agentName, attemptID string,
) (k12.WeeklyPracticeAttempt, error) {
	var payload string
	err := q.QueryRowContext(ctx, `SELECT attempt_json
        FROM k12_weekly_practice_attempts
        WHERE agent_name=? AND attempt_id=?`, agentName, attemptID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return k12.WeeklyPracticeAttempt{}, records.ErrNotFound
	}
	if err != nil {
		return k12.WeeklyPracticeAttempt{}, err
	}
	var attempt k12.WeeklyPracticeAttempt
	if err := json.Unmarshal([]byte(payload), &attempt); err != nil {
		return k12.WeeklyPracticeAttempt{}, err
	}
	return attempt, nil
}

func weeklyAssessmentEventID(command k12.WeeklyPracticeAssessmentCommand) string {
	sum := sha256.Sum256([]byte(command.CommandID + "\x00mistake_recorded"))
	return "k12-weekly-" + hex.EncodeToString(sum[:])
}

func (s *Store) CommitWeeklyPracticeAssessment(
	ctx context.Context,
	command k12.WeeklyPracticeAssessmentCommand,
	attempt k12.WeeklyPracticeAttempt,
	effects GradingAssessmentEffects,
	at int64,
) (k12.WeeklyPracticeAttempt, bool, error) {
	if err := validateGradingAssessmentEffects(effects); err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	weeklyAssessmentCommitMu.Lock()
	defer weeklyAssessmentCommitMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	defer tx.Rollback()
	storedCommand, err := scanWeeklyAssessmentCommand(tx.QueryRowContext(ctx,
		`SELECT `+weeklyAssessmentCommandColumns+`
         FROM k12_weekly_assessment_commands WHERE command_id=? AND agent_name=?`,
		command.CommandID, command.AgentName))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.WeeklyPracticeAttempt{}, false, records.ErrNotFound
	}
	if err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	if storedCommand.RequestDigest != command.RequestDigest {
		return k12.WeeklyPracticeAttempt{}, false, records.ErrVersionConflict
	}
	if storedCommand.Status == k12.WeeklyAssessmentCommitted {
		stored, err := getWeeklyPracticeAttemptVia(
			ctx, tx, command.AgentName, storedCommand.AttemptID)
		return stored, true, err
	}
	if storedCommand.Status != k12.WeeklyAssessmentSucceeded {
		return k12.WeeklyPracticeAttempt{}, false, records.ErrIllegalTransition
	}
	var receipt struct {
		AssessmentID         string `json:"assessment_id"`
		Result               string `json:"result"`
		VerificationEvidence string `json:"verification_evidence"`
	}
	if err := json.Unmarshal([]byte(storedCommand.AssessmentJSON), &receipt); err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	if receipt.AssessmentID != attempt.AssessmentID ||
		receipt.Result != attempt.Result ||
		receipt.VerificationEvidence != attempt.VerificationEvidence ||
		attempt.SnapshotID != storedCommand.SnapshotID ||
		attempt.ItemID != storedCommand.ItemID {
		return k12.WeeklyPracticeAttempt{}, false, records.ErrVersionConflict
	}
	emitted := false
	if effects.Mistake != nil {
		var created bool
		attempt.MistakeRecordID, created, emitted, err =
			s.commitAssessmentMistakeTx(ctx, tx, command.AgentName,
				*effects.Mistake, weeklyAssessmentEventID(command))
		_ = created
		attempt.ReviewScheduled = true
	} else if effects.Review != nil {
		err = s.commitAssessmentReviewTx(
			ctx, tx, command.AgentName, *effects.Review)
		attempt.ReviewScheduled = true
	}
	if err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	payload, err := json.Marshal(attempt)
	if err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_practice_attempts
        (attempt_id,agent_name,snapshot_id,item_id,idempotency_key,request_digest,
         attempt_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		attempt.AttemptID, command.AgentName, attempt.SnapshotID, attempt.ItemID,
		command.IdempotencyKey, command.RequestDigest, string(payload),
		attempt.CreatedAt, at); err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE k12_weekly_assessment_commands
        SET status='committed',attempt_id=?,updated_at=?
        WHERE command_id=? AND agent_name=? AND request_digest=? AND status='succeeded'`,
		attempt.AttemptID, at, command.CommandID, command.AgentName, command.RequestDigest)
	if err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return k12.WeeklyPracticeAttempt{}, false, records.ErrVersionConflict
	}
	if err := tx.Commit(); err != nil {
		return k12.WeeklyPracticeAttempt{}, false, err
	}
	if emitted && s.notifyOutbox != nil {
		s.notifyOutbox()
	}
	return attempt, false, nil
}
