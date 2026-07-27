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

const weeklyArithmeticBatchColumns = `batch_id,agent_name,plan_id,ordinal,state,
    item_count,content_digest,retryable,failure_message,generation_checkpoint_json,
    items_json,answer_keys_json,created_at,updated_at,completed_at`

var weeklyArithmeticMu sync.Mutex

type WeeklyArithmeticCommand struct {
	AgentName      string
	ScopeID        string
	CommandKind    string
	ItemID         string
	IdempotencyKey string
	RequestDigest  string
	Status         string
	ResultJSON     string
	ResultDigest   string
	ResponseJSON   string
	CreatedAt      int64
	UpdatedAt      int64
}

func weeklyArithmeticID(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + hex.EncodeToString(sum[:12])
}

func scanWeeklyArithmeticBatch(row rowScanner) (k12.WeeklyArithmeticBatch, error) {
	var batch k12.WeeklyArithmeticBatch
	var retryable int
	var itemsJSON, keysJSON string
	var completed sql.NullInt64
	err := row.Scan(&batch.BatchID, &batch.AgentName, &batch.PlanID, &batch.Ordinal,
		&batch.State, &batch.ItemCount, &batch.ContentDigest, &retryable,
		&batch.FailureMessage, &batch.GenerationCheckpoint, &itemsJSON, &keysJSON,
		&batch.CreatedAt, &batch.UpdatedAt, &completed)
	if err != nil {
		return k12.WeeklyArithmeticBatch{}, err
	}
	batch.Retryable = retryable != 0
	if completed.Valid {
		value := completed.Int64
		batch.CompletedAt = &value
	}
	if err := json.Unmarshal([]byte(itemsJSON), &batch.Items); err != nil {
		return k12.WeeklyArithmeticBatch{}, err
	}
	if err := json.Unmarshal([]byte(keysJSON), &batch.AnswerKeys); err != nil {
		return k12.WeeklyArithmeticBatch{}, err
	}
	return batch, nil
}

func getWeeklyArithmeticBatchVia(
	ctx context.Context,
	q dbQueryer,
	agentName, where string,
	args ...any,
) (k12.WeeklyArithmeticBatch, error) {
	all := append([]any{agentName}, args...)
	batch, err := scanWeeklyArithmeticBatch(q.QueryRowContext(ctx,
		`SELECT `+weeklyArithmeticBatchColumns+`
         FROM k12_weekly_arithmetic_batches WHERE agent_name=? AND `+where, all...))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.WeeklyArithmeticBatch{}, records.ErrNotFound
	}
	return batch, err
}

func (s *Store) GetWeeklyArithmeticBatch(
	ctx context.Context,
	agentName, batchID string,
) (k12.WeeklyArithmeticBatch, error) {
	return getWeeklyArithmeticBatchVia(
		ctx, s.db, strings.TrimSpace(agentName), `batch_id=?`, strings.TrimSpace(batchID))
}

func (s *Store) GetLatestWeeklyArithmeticBatch(
	ctx context.Context,
	agentName, planID string,
) (k12.WeeklyArithmeticBatch, error) {
	return getWeeklyArithmeticBatchVia(ctx, s.db, strings.TrimSpace(agentName),
		`plan_id=? ORDER BY ordinal DESC LIMIT 1`, strings.TrimSpace(planID))
}

func scanWeeklyArithmeticCommand(row rowScanner) (WeeklyArithmeticCommand, error) {
	var command WeeklyArithmeticCommand
	err := row.Scan(&command.AgentName, &command.ScopeID, &command.CommandKind,
		&command.ItemID, &command.IdempotencyKey, &command.RequestDigest,
		&command.Status, &command.ResultJSON, &command.ResultDigest,
		&command.ResponseJSON, &command.CreatedAt, &command.UpdatedAt)
	return command, err
}

func getWeeklyArithmeticCommandVia(
	ctx context.Context,
	q dbQueryer,
	agentName, scopeID, kind, itemID, key string,
) (WeeklyArithmeticCommand, error) {
	command, err := scanWeeklyArithmeticCommand(q.QueryRowContext(ctx, `SELECT
        agent_name,scope_id,command_kind,item_id,idempotency_key,request_digest,
        status,result_json,result_digest,response_json,created_at,updated_at
        FROM k12_weekly_arithmetic_commands
        WHERE agent_name=? AND scope_id=? AND command_kind=? AND item_id=?
          AND idempotency_key=?`, agentName, scopeID, kind, itemID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return WeeklyArithmeticCommand{}, records.ErrNotFound
	}
	return command, err
}

func (s *Store) PrepareWeeklyArithmeticBatch(
	ctx context.Context,
	agentName, planID string,
	expectedRevision int,
	idempotencyKey, requestDigest, checkpointJSON string,
	at int64,
) (k12.WeeklyArithmeticBatch, bool, error) {
	weeklyArithmeticMu.Lock()
	defer weeklyArithmeticMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.WeeklyArithmeticBatch{}, false, err
	}
	defer tx.Rollback()
	command, commandErr := getWeeklyArithmeticCommandVia(
		ctx, tx, agentName, planID, "create", "", idempotencyKey)
	if commandErr == nil {
		if command.RequestDigest != requestDigest {
			return k12.WeeklyArithmeticBatch{}, false, records.ErrVersionConflict
		}
		var batch k12.WeeklyArithmeticBatch
		if err := json.Unmarshal([]byte(command.ResponseJSON), &batch); err != nil {
			return k12.WeeklyArithmeticBatch{}, false, err
		}
		return batch, true, tx.Commit()
	}
	if !errors.Is(commandErr, records.ErrNotFound) {
		return k12.WeeklyArithmeticBatch{}, false, commandErr
	}
	plan, err := getWeeklyPlanVia(ctx, tx, agentName, `plan_id=?`, planID)
	if err != nil {
		return k12.WeeklyArithmeticBatch{}, false, err
	}
	if plan.Status != k12.WeeklyPlanDraft || plan.Revision != expectedRevision {
		return k12.WeeklyArithmeticBatch{}, false, records.ErrVersionConflict
	}
	present := false
	for _, track := range plan.Tracks {
		if track.PlanSection == k12.WeeklySectionArithmeticWarmup {
			present = true
		}
	}
	if !present {
		return k12.WeeklyArithmeticBatch{}, false, records.ErrIllegalTransition
	}
	ordinal := 1
	latest, latestErr := getWeeklyArithmeticBatchVia(
		ctx, tx, agentName, `plan_id=? ORDER BY ordinal DESC LIMIT 1`, planID)
	if latestErr == nil {
		if latest.State != k12.WeeklyArithmeticCompleted {
			return k12.WeeklyArithmeticBatch{}, false, records.ErrIllegalTransition
		}
		ordinal = latest.Ordinal + 1
	} else if !errors.Is(latestErr, records.ErrNotFound) {
		return k12.WeeklyArithmeticBatch{}, false, latestErr
	}
	batch := k12.WeeklyArithmeticBatch{
		BatchID:   weeklyArithmeticID("warith-", planID, fmt.Sprint(ordinal)),
		AgentName: agentName, PlanID: planID, Ordinal: ordinal,
		State: k12.WeeklyArithmeticPreparing, FailureMessage: "",
		GenerationCheckpoint: checkpointJSON,
		Items:                []k12.WeeklyPracticeItem{}, AnswerKeys: map[string]string{},
		CreatedAt: at, UpdatedAt: at,
	}
	if !json.Valid([]byte(checkpointJSON)) {
		return k12.WeeklyArithmeticBatch{}, false, records.ErrInvalidFields
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_arithmetic_batches
        (`+weeklyArithmeticBatchColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		batch.BatchID, batch.AgentName, batch.PlanID, batch.Ordinal, batch.State,
		0, "", 0, "", batch.GenerationCheckpoint, "[]", "{}", at, at, nil); err != nil {
		return k12.WeeklyArithmeticBatch{}, false, err
	}
	response, _ := json.Marshal(batch)
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_arithmetic_commands
        (agent_name,scope_id,command_kind,item_id,idempotency_key,request_digest,
         status,response_json,created_at,updated_at)
        VALUES(?,?,?,?,?,?,'committed',?,?,?)`, agentName, planID, "create", "",
		idempotencyKey, requestDigest, string(response), at, at); err != nil {
		return k12.WeeklyArithmeticBatch{}, false, err
	}
	return batch, false, tx.Commit()
}

func (s *Store) FinishWeeklyArithmeticGeneration(
	ctx context.Context,
	agentName, batchID, state string,
	items []k12.WeeklyPracticeItem,
	answerKeys map[string]string,
	contentDigest, failureMessage string,
	at int64,
) error {
	if state != k12.WeeklyArithmeticReady &&
		state != k12.WeeklyArithmeticFailedRetryable &&
		state != k12.WeeklyArithmeticFailedTerminal {
		return records.ErrInvalidStatus
	}
	itemsJSON, _ := json.Marshal(items)
	keysJSON, _ := json.Marshal(answerKeys)
	itemCount := len(items)
	retryable := state == k12.WeeklyArithmeticFailedRetryable
	if state != k12.WeeklyArithmeticReady {
		itemsJSON, keysJSON = []byte("[]"), []byte("{}")
		itemCount, contentDigest = 0, ""
	}
	weeklyArithmeticMu.Lock()
	defer weeklyArithmeticMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE k12_weekly_arithmetic_batches
        SET state=?,item_count=?,content_digest=?,retryable=?,failure_message=?,
            items_json=?,answer_keys_json=?,updated_at=?
        WHERE agent_name=? AND batch_id=? AND state='preparing'`,
		state, itemCount, contentDigest, boolInt(retryable), failureMessage,
		string(itemsJSON), string(keysJSON), at, agentName, batchID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return records.ErrVersionConflict
	}
	if state == k12.WeeklyArithmeticReady {
		if err := upsertWeeklyManualPracticePreferenceTx(
			ctx, tx, agentName, k12.WeeklySectionArithmeticWarmup, itemCount, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) StartWeeklyArithmeticBatch(
	ctx context.Context,
	agentName, batchID, key, digest string,
	at int64,
) (k12.WeeklyArithmeticBatch, bool, error) {
	return s.transitionWeeklyArithmeticBatch(
		ctx, agentName, batchID, "start", key, digest,
		k12.WeeklyArithmeticReady, k12.WeeklyArithmeticInProgress, at)
}

func (s *Store) PrepareWeeklyArithmeticRetry(
	ctx context.Context,
	agentName, batchID, key, digest string,
	at int64,
) (k12.WeeklyArithmeticBatch, bool, error) {
	return s.transitionWeeklyArithmeticBatch(
		ctx, agentName, batchID, "retry", key, digest,
		k12.WeeklyArithmeticFailedRetryable, k12.WeeklyArithmeticPreparing, at)
}

func (s *Store) transitionWeeklyArithmeticBatch(
	ctx context.Context,
	agentName, batchID, kind, key, digest, from, to string,
	at int64,
) (k12.WeeklyArithmeticBatch, bool, error) {
	weeklyArithmeticMu.Lock()
	defer weeklyArithmeticMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.WeeklyArithmeticBatch{}, false, err
	}
	defer tx.Rollback()
	command, commandErr := getWeeklyArithmeticCommandVia(
		ctx, tx, agentName, batchID, kind, "", key)
	if commandErr == nil {
		if command.RequestDigest != digest {
			return k12.WeeklyArithmeticBatch{}, false, records.ErrVersionConflict
		}
		var batch k12.WeeklyArithmeticBatch
		if err := json.Unmarshal([]byte(command.ResponseJSON), &batch); err != nil {
			return k12.WeeklyArithmeticBatch{}, false, err
		}
		return batch, true, tx.Commit()
	}
	if !errors.Is(commandErr, records.ErrNotFound) {
		return k12.WeeklyArithmeticBatch{}, false, commandErr
	}
	batch, err := getWeeklyArithmeticBatchVia(ctx, tx, agentName, `batch_id=?`, batchID)
	if err != nil {
		return k12.WeeklyArithmeticBatch{}, false, err
	}
	if batch.State != from {
		return k12.WeeklyArithmeticBatch{}, false, records.ErrIllegalTransition
	}
	batch.State, batch.UpdatedAt = to, at
	batch.Retryable, batch.FailureMessage = false, ""
	if _, err := tx.ExecContext(ctx, `UPDATE k12_weekly_arithmetic_batches
        SET state=?,retryable=0,failure_message='',updated_at=?
        WHERE agent_name=? AND batch_id=? AND state=?`,
		to, at, agentName, batchID, from); err != nil {
		return k12.WeeklyArithmeticBatch{}, false, err
	}
	response, _ := json.Marshal(batch)
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_arithmetic_commands
        (agent_name,scope_id,command_kind,item_id,idempotency_key,request_digest,
         status,response_json,created_at,updated_at)
        VALUES(?,?,?,?,?,?,'committed',?,?,?)`, agentName, batchID, kind, "",
		key, digest, string(response), at, at); err != nil {
		return k12.WeeklyArithmeticBatch{}, false, err
	}
	return batch, false, tx.Commit()
}

func (s *Store) PrepareWeeklyArithmeticAttempt(
	ctx context.Context,
	agentName, batchID, itemID, key, digest string,
	at int64,
) (WeeklyArithmeticCommand, *k12.WeeklyArithmeticAttempt, error) {
	weeklyArithmeticMu.Lock()
	defer weeklyArithmeticMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WeeklyArithmeticCommand{}, nil, err
	}
	defer tx.Rollback()
	command, commandErr := getWeeklyArithmeticCommandVia(
		ctx, tx, agentName, batchID, "attempt", itemID, key)
	if commandErr == nil {
		if command.RequestDigest != digest {
			return WeeklyArithmeticCommand{}, nil, records.ErrVersionConflict
		}
		if command.Status == "committed" {
			var attempt k12.WeeklyArithmeticAttempt
			if err := json.Unmarshal([]byte(command.ResponseJSON), &attempt); err != nil {
				return WeeklyArithmeticCommand{}, nil, err
			}
			return command, &attempt, tx.Commit()
		}
		return command, nil, tx.Commit()
	}
	if !errors.Is(commandErr, records.ErrNotFound) {
		return WeeklyArithmeticCommand{}, nil, commandErr
	}
	batch, err := getWeeklyArithmeticBatchVia(ctx, tx, agentName, `batch_id=?`, batchID)
	if err != nil {
		return WeeklyArithmeticCommand{}, nil, err
	}
	if batch.State != k12.WeeklyArithmeticInProgress {
		return WeeklyArithmeticCommand{}, nil, records.ErrIllegalTransition
	}
	found := false
	for _, item := range batch.Items {
		found = found || item.ItemID == itemID
	}
	if !found {
		return WeeklyArithmeticCommand{}, nil, records.ErrNotFound
	}
	command = WeeklyArithmeticCommand{
		AgentName: agentName, ScopeID: batchID, CommandKind: "attempt",
		ItemID: itemID, IdempotencyKey: key, RequestDigest: digest,
		Status: "prepared", ResultJSON: "{}", ResponseJSON: "{}",
		CreatedAt: at, UpdatedAt: at,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_arithmetic_commands
        (agent_name,scope_id,command_kind,item_id,idempotency_key,request_digest,
         status,result_json,response_json,created_at,updated_at)
        VALUES(?,?,?,?,?,?,'prepared','{}','{}',?,?)`,
		agentName, batchID, "attempt", itemID, key, digest, at, at); err != nil {
		return WeeklyArithmeticCommand{}, nil, err
	}
	return command, nil, tx.Commit()
}

func (s *Store) GetWeeklyArithmeticCommand(
	ctx context.Context,
	command WeeklyArithmeticCommand,
) (WeeklyArithmeticCommand, error) {
	stored, err := getWeeklyArithmeticCommandVia(ctx, s.db, command.AgentName,
		command.ScopeID, command.CommandKind, command.ItemID, command.IdempotencyKey)
	if err == nil && stored.RequestDigest != command.RequestDigest {
		return WeeklyArithmeticCommand{}, records.ErrVersionConflict
	}
	return stored, err
}

func (s *Store) ClaimWeeklyArithmeticAttempt(
	ctx context.Context,
	command WeeklyArithmeticCommand,
	at int64,
) (WeeklyArithmeticCommand, bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE k12_weekly_arithmetic_commands
        SET status='sent',updated_at=? WHERE agent_name=? AND scope_id=?
          AND command_kind='attempt' AND item_id=? AND idempotency_key=?
          AND request_digest=? AND status='prepared'`,
		at, command.AgentName, command.ScopeID, command.ItemID,
		command.IdempotencyKey, command.RequestDigest)
	if err != nil {
		return WeeklyArithmeticCommand{}, false, err
	}
	stored, err := s.GetWeeklyArithmeticCommand(ctx, command)
	n, _ := res.RowsAffected()
	return stored, n == 1, err
}

func (s *Store) MarkWeeklyArithmeticAttemptSucceeded(
	ctx context.Context,
	command WeeklyArithmeticCommand,
	resultJSON, resultDigest string,
	at int64,
) (WeeklyArithmeticCommand, error) {
	if !json.Valid([]byte(resultJSON)) || len(resultDigest) != 64 {
		return WeeklyArithmeticCommand{}, records.ErrInvalidFields
	}
	res, err := s.db.ExecContext(ctx, `UPDATE k12_weekly_arithmetic_commands
        SET status='succeeded',result_json=?,result_digest=?,updated_at=?
        WHERE agent_name=? AND scope_id=? AND command_kind='attempt' AND item_id=?
          AND idempotency_key=? AND request_digest=? AND status='sent'`,
		resultJSON, resultDigest, at, command.AgentName, command.ScopeID,
		command.ItemID, command.IdempotencyKey, command.RequestDigest)
	if err != nil {
		return WeeklyArithmeticCommand{}, err
	}
	stored, err := s.GetWeeklyArithmeticCommand(ctx, command)
	if err != nil {
		return WeeklyArithmeticCommand{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 &&
		(stored.Status != "succeeded" || stored.ResultDigest != resultDigest) {
		return WeeklyArithmeticCommand{}, records.ErrVersionConflict
	}
	return stored, nil
}

func (s *Store) MarkWeeklyArithmeticAttemptTerminal(
	ctx context.Context,
	command WeeklyArithmeticCommand,
	status string,
	at int64,
) error {
	if status != "failed" && status != "outcome_unknown" {
		return records.ErrInvalidStatus
	}
	_, err := s.db.ExecContext(ctx, `UPDATE k12_weekly_arithmetic_commands
        SET status=?,updated_at=? WHERE agent_name=? AND scope_id=?
          AND command_kind='attempt' AND item_id=? AND idempotency_key=?
          AND request_digest=? AND status='sent'`,
		status, at, command.AgentName, command.ScopeID, command.ItemID,
		command.IdempotencyKey, command.RequestDigest)
	return err
}

func (s *Store) CommitWeeklyArithmeticAttempt(
	ctx context.Context,
	command WeeklyArithmeticCommand,
	attempt k12.WeeklyArithmeticAttempt,
	effects GradingAssessmentEffects,
	at int64,
) (k12.WeeklyArithmeticAttempt, bool, error) {
	if err := validateGradingAssessmentEffects(effects); err != nil {
		return k12.WeeklyArithmeticAttempt{}, false, err
	}
	weeklyArithmeticMu.Lock()
	defer weeklyArithmeticMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.WeeklyArithmeticAttempt{}, false, err
	}
	defer tx.Rollback()
	stored, err := getWeeklyArithmeticCommandVia(ctx, tx, command.AgentName,
		command.ScopeID, "attempt", command.ItemID, command.IdempotencyKey)
	if err != nil {
		return k12.WeeklyArithmeticAttempt{}, false, err
	}
	if stored.RequestDigest != command.RequestDigest {
		return k12.WeeklyArithmeticAttempt{}, false, records.ErrVersionConflict
	}
	if stored.Status == "committed" {
		var replay k12.WeeklyArithmeticAttempt
		if err := json.Unmarshal([]byte(stored.ResponseJSON), &replay); err != nil {
			return k12.WeeklyArithmeticAttempt{}, false, err
		}
		return replay, true, tx.Commit()
	}
	if stored.Status != "succeeded" {
		return k12.WeeklyArithmeticAttempt{}, false, records.ErrIllegalTransition
	}
	batch, err := getWeeklyArithmeticBatchVia(
		ctx, tx, command.AgentName, `batch_id=?`, command.ScopeID)
	if err != nil {
		return k12.WeeklyArithmeticAttempt{}, false, err
	}
	if batch.State != k12.WeeklyArithmeticInProgress {
		return k12.WeeklyArithmeticAttempt{}, false, records.ErrIllegalTransition
	}
	emitted := false
	if effects.Mistake != nil {
		var created bool
		attempt.MistakeRecordID, created, emitted, err =
			s.commitAssessmentMistakeTx(ctx, tx, command.AgentName,
				*effects.Mistake, weeklyArithmeticID(
					"k12-weekly-arithmetic-", command.ScopeID, command.ItemID,
					command.IdempotencyKey, "mistake"))
		_ = created
		attempt.ReviewScheduled = true
	}
	if err != nil {
		return k12.WeeklyArithmeticAttempt{}, false, err
	}
	payload, _ := json.Marshal(attempt)
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_arithmetic_attempts
        (attempt_id,agent_name,batch_id,item_id,idempotency_key,request_digest,
         attempt_json,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		attempt.AttemptID, command.AgentName, command.ScopeID, command.ItemID,
		command.IdempotencyKey, command.RequestDigest, string(payload),
		attempt.CreatedAt); err != nil {
		return k12.WeeklyArithmeticAttempt{}, false, err
	}
	var completedItems int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT item_id)
        FROM k12_weekly_arithmetic_attempts WHERE batch_id=?`,
		command.ScopeID).Scan(&completedItems); err != nil {
		return k12.WeeklyArithmeticAttempt{}, false, err
	}
	if completedItems >= batch.ItemCount {
		if _, err := tx.ExecContext(ctx, `UPDATE k12_weekly_arithmetic_batches
            SET state='completed',retryable=0,failure_message='',
                updated_at=?,completed_at=?
            WHERE agent_name=? AND batch_id=? AND state='in_progress'`,
			at, at, command.AgentName, command.ScopeID); err != nil {
			return k12.WeeklyArithmeticAttempt{}, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_weekly_arithmetic_commands
        SET status='committed',response_json=?,updated_at=?
        WHERE agent_name=? AND scope_id=? AND command_kind='attempt' AND item_id=?
          AND idempotency_key=? AND request_digest=? AND status='succeeded'`,
		string(payload), at, command.AgentName, command.ScopeID, command.ItemID,
		command.IdempotencyKey, command.RequestDigest); err != nil {
		return k12.WeeklyArithmeticAttempt{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return k12.WeeklyArithmeticAttempt{}, false, err
	}
	if emitted && s.notifyOutbox != nil {
		s.notifyOutbox()
	}
	return attempt, false, nil
}

func (s *Store) PutWeeklyTrackCheckpoint(
	ctx context.Context,
	agentName, planID string,
	revision int,
	checkpointJSON string,
	at int64,
) error {
	if !json.Valid([]byte(checkpointJSON)) {
		return records.ErrInvalidFields
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO k12_weekly_track_checkpoints
        (agent_name,plan_id,plan_revision,plan_section,checkpoint_json,created_at)
        VALUES(?,?,?,'textbook_consolidation',?,?)
        ON CONFLICT DO NOTHING`, agentName, planID, revision, checkpointJSON, at)
	return err
}

func (s *Store) GetWeeklyTrackCheckpoint(
	ctx context.Context,
	agentName, planID string,
	revision int,
) (string, error) {
	var checkpoint string
	err := s.db.QueryRowContext(ctx, `SELECT checkpoint_json
        FROM k12_weekly_track_checkpoints
        WHERE agent_name=? AND plan_id=? AND plan_revision=?
          AND plan_section='textbook_consolidation'`,
		agentName, planID, revision).Scan(&checkpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return "", records.ErrNotFound
	}
	return checkpoint, err
}

func (s *Store) CommitWeeklyTextbookRefresh(
	ctx context.Context,
	agentName, planID string,
	expectedRevision int,
	key, digest string,
	next k12.WeeklyPracticePlan,
	createdRevision bool,
	manualItemCount int,
	checkpointJSON string,
	at int64,
) (k12.WeeklyPracticePlan, bool, bool, error) {
	weeklyArithmeticMu.Lock()
	defer weeklyArithmeticMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.WeeklyPracticePlan{}, false, false, err
	}
	defer tx.Rollback()
	var storedDigest, responseJSON string
	var storedCreated int
	err = tx.QueryRowContext(ctx, `SELECT request_digest,response_json,created_revision
        FROM k12_weekly_track_refresh_commands
        WHERE agent_name=? AND plan_id=? AND idempotency_key=?`,
		agentName, planID, key).Scan(&storedDigest, &responseJSON, &storedCreated)
	if err == nil {
		if storedDigest != digest {
			return k12.WeeklyPracticePlan{}, false, false, records.ErrVersionConflict
		}
		var plan k12.WeeklyPracticePlan
		if err := json.Unmarshal([]byte(responseJSON), &plan); err != nil {
			return k12.WeeklyPracticePlan{}, false, false, err
		}
		return plan, true, storedCreated != 0, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return k12.WeeklyPracticePlan{}, false, false, err
	}
	current, err := getWeeklyPlanVia(ctx, tx, agentName, `plan_id=?`, planID)
	if err != nil {
		return k12.WeeklyPracticePlan{}, false, false, err
	}
	if current.Status != k12.WeeklyPlanDraft || current.Revision != expectedRevision {
		return k12.WeeklyPracticePlan{}, false, false, records.ErrVersionConflict
	}
	if err := updateWeeklyPlanTx(ctx, tx, next, next.SourceDigest); err != nil {
		return k12.WeeklyPracticePlan{}, false, false, err
	}
	if checkpointJSON != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_track_checkpoints
            (agent_name,plan_id,plan_revision,plan_section,checkpoint_json,created_at)
            VALUES(?,?,?,'textbook_consolidation',?,?)
            ON CONFLICT DO NOTHING`, agentName, planID, next.Revision,
			checkpointJSON, at); err != nil {
			return k12.WeeklyPracticePlan{}, false, false, err
		}
	}
	if createdRevision && manualItemCount > 0 {
		if err := upsertWeeklyManualPracticePreferenceTx(
			ctx, tx, agentName, k12.WeeklySectionTextbookConsolidation,
			manualItemCount, at); err != nil {
			return k12.WeeklyPracticePlan{}, false, false, err
		}
	}
	response, _ := json.Marshal(next)
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_weekly_track_refresh_commands
        (agent_name,plan_id,idempotency_key,request_digest,response_json,
         created_revision,created_at) VALUES(?,?,?,?,?,?,?)`,
		agentName, planID, key, digest, string(response),
		boolInt(createdRevision), at); err != nil {
		return k12.WeeklyPracticePlan{}, false, false, err
	}
	return next, false, createdRevision, tx.Commit()
}
