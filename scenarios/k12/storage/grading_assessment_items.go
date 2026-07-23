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

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

var ErrGradingAssessmentItemConflict = errors.New("grading assessment item immutable receipt conflict")

// GradingAssessmentEffects is deliberately typed and closed. It cannot run
// arbitrary SQL. A new receipt may either record one wrong-item Mistake (which
// also appends its typed Outbox event) or CAS one existing Mistake review, never
// both. An idempotent receipt replay executes neither effect again.
type GradingAssessmentEffects struct {
	Mistake *GradingMistakeEffect
	Review  *GradingReviewEffect
}

type GradingMistakeEffect struct {
	SourceSession string
	Fields        k12.MistakeFields
	DueAt         *int64
}

type GradingReviewEffect struct {
	RecordID        string
	ExpectedVersion int
	NewStatus       string
	Fields          k12.MistakeFields
	DueAt           *int64
}

const gradingAssessmentItemColumns = `agent_name,job_id,problem_id,attempt_id,confirmed_version,
    input_digest,status,result_json,result_digest,solve_invocation_id,grade_invocation_id,
    projection_record_id,projection_created,projection_status,created_at,updated_at`

func scanGradingAssessmentItem(row rowScanner) (k12.GradingAssessmentItem, error) {
	var item k12.GradingAssessmentItem
	var status string
	var solveID, gradeID sql.NullString
	var projectionCreated int64
	err := row.Scan(&item.AgentName, &item.JobID, &item.ProblemID, &item.AttemptID,
		&item.ConfirmedVersion, &item.InputDigest, &status, &item.ResultJSON, &item.ResultDigest,
		&solveID, &gradeID, &item.ProjectionRecordID, &projectionCreated,
		&item.ProjectionStatus, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return k12.GradingAssessmentItem{}, err
	}
	item.Status = k12.GradingAssessmentStatus(status)
	item.ProjectionCreated = projectionCreated != 0
	if solveID.Valid {
		item.SolveInvocationID = solveID.String
	}
	if gradeID.Valid {
		item.GradeInvocationID = gradeID.String
	}
	return item, nil
}

func sameGradingAssessmentReceipt(a, b k12.GradingAssessmentItem) bool {
	return a.AgentName == b.AgentName && a.JobID == b.JobID && a.ProblemID == b.ProblemID &&
		a.AttemptID == b.AttemptID && a.ConfirmedVersion == b.ConfirmedVersion &&
		a.InputDigest == b.InputDigest && a.Status == b.Status && a.ResultJSON == b.ResultJSON &&
		a.ResultDigest == b.ResultDigest && a.SolveInvocationID == b.SolveInvocationID &&
		a.GradeInvocationID == b.GradeInvocationID && a.ProjectionStatus == b.ProjectionStatus
}

func gradingAssessmentEventID(item k12.GradingAssessmentItem, effectKind string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s",
		item.JobID, item.ProblemID, item.ConfirmedVersion, effectKind)))
	return "k12-grading-" + hex.EncodeToString(sum[:])
}

func validateGradingAssessmentEffects(effects GradingAssessmentEffects) error {
	if effects.Mistake != nil && effects.Review != nil {
		return fmt.Errorf("k12storage: grading assessment Mistake and Review effects are mutually exclusive")
	}
	if effects.Review != nil {
		if strings.TrimSpace(effects.Review.RecordID) == "" || effects.Review.ExpectedVersion < 0 ||
			strings.TrimSpace(effects.Review.NewStatus) == "" {
			return fmt.Errorf("k12storage: grading review effect missing record/version/status")
		}
	}
	return nil
}

func (s *Store) validateAssessmentInvocationRef(ctx context.Context, item k12.GradingAssessmentItem,
	invocationID string, operation k12.GradingItemOperation,
) error {
	if invocationID == "" {
		return nil
	}
	invocation, err := s.GetGradingItemInvocation(ctx, item.AgentName, invocationID)
	if err != nil {
		return err
	}
	if invocation.JobID != item.JobID || invocation.ProblemID != item.ProblemID ||
		invocation.AttemptID != item.AttemptID || invocation.Operation != operation ||
		invocation.Status != k12.ModelInvocationSucceeded {
		return fmt.Errorf("%w: assessment source invocation %s does not match committed item",
			ErrGradingAssessmentItemConflict, invocationID)
	}
	return nil
}

// CommitGradingAssessmentItem writes the receipt and its one allowed local
// effect in a single short SQLite transaction. External model calls must have
// completed before this method; they never execute while the transaction is
// open.
func (s *Store) CommitGradingAssessmentItem(ctx context.Context, item k12.GradingAssessmentItem,
	effects GradingAssessmentEffects,
) (k12.GradingAssessmentItem, bool, error) {
	if err := item.Validate(); err != nil {
		return k12.GradingAssessmentItem{}, false, fmt.Errorf("k12storage: %w", err)
	}
	if item.ProjectionRecordID != "" || item.ProjectionCreated {
		return k12.GradingAssessmentItem{}, false,
			fmt.Errorf("k12storage: grading assessment projection facts are storage-owned")
	}
	if err := validateGradingAssessmentEffects(effects); err != nil {
		return k12.GradingAssessmentItem{}, false, err
	}
	if err := ensureAgentRegistered(ctx, s.db, item.AgentName); err != nil {
		return k12.GradingAssessmentItem{}, false, err
	}
	if err := ensureGradingItemScope(ctx, s.db, item.AgentName, item.JobID, item.ProblemID, item.AttemptID); err != nil {
		return k12.GradingAssessmentItem{}, false, err
	}
	if err := s.validateAssessmentInvocationRef(ctx, item, item.SolveInvocationID, k12.GradingItemOperationSolve); err != nil {
		return k12.GradingAssessmentItem{}, false, err
	}
	if err := s.validateAssessmentInvocationRef(ctx, item, item.GradeInvocationID, k12.GradingItemOperationGrade); err != nil {
		return k12.GradingAssessmentItem{}, false, err
	}
	if item.CreatedAt <= 0 {
		item.CreatedAt = nowUnix()
	}
	item.UpdatedAt = item.CreatedAt

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.GradingAssessmentItem{}, false, fmt.Errorf("k12storage: begin grading assessment transaction: %w", err)
	}
	defer tx.Rollback()
	var solveID, gradeID any
	if item.SolveInvocationID != "" {
		solveID = item.SolveInvocationID
	}
	if item.GradeInvocationID != "" {
		gradeID = item.GradeInvocationID
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO k12_grading_assessment_items (`+gradingAssessmentItemColumns+`)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(job_id,problem_id) DO NOTHING`,
		item.AgentName, item.JobID, item.ProblemID, item.AttemptID, item.ConfirmedVersion,
		item.InputDigest, item.Status, item.ResultJSON, item.ResultDigest, solveID, gradeID,
		item.ProjectionRecordID, boolInt(item.ProjectionCreated), item.ProjectionStatus,
		item.CreatedAt, item.UpdatedAt)
	if err != nil {
		return k12.GradingAssessmentItem{}, false, fmt.Errorf("k12storage: insert grading assessment receipt: %w", err)
	}
	inserted, _ := res.RowsAffected()
	if inserted == 0 {
		stored, err := getGradingAssessmentItemVia(ctx, tx, item.AgentName, item.JobID, item.ProblemID)
		if err != nil {
			return k12.GradingAssessmentItem{}, false, err
		}
		if !sameGradingAssessmentReceipt(stored, item) {
			return k12.GradingAssessmentItem{}, false, fmt.Errorf("%w: job=%s problem=%s",
				ErrGradingAssessmentItemConflict, item.JobID, item.ProblemID)
		}
		if err := tx.Commit(); err != nil {
			return k12.GradingAssessmentItem{}, false, fmt.Errorf("k12storage: commit receipt replay: %w", err)
		}
		return stored, false, nil
	}
	if err := validateGradingAssessmentInputBinding(ctx, tx, item); err != nil {
		return k12.GradingAssessmentItem{}, false, err
	}

	emitted := false
	if effects.Mistake != nil {
		item.ProjectionRecordID, item.ProjectionCreated, emitted, err =
			s.commitAssessmentMistakeTx(ctx, tx, item.AgentName, *effects.Mistake,
				gradingAssessmentEventID(item, "mistake_recorded"))
		if err == nil {
			var projectionResult sql.Result
			projectionResult, err = tx.ExecContext(ctx, `UPDATE k12_grading_assessment_items
				SET projection_record_id=?,projection_created=?
				WHERE agent_name=? AND job_id=? AND problem_id=?`, item.ProjectionRecordID,
				boolInt(item.ProjectionCreated), item.AgentName, item.JobID, item.ProblemID)
			if err != nil {
				err = fmt.Errorf("k12storage: persist grading assessment projection: %w", err)
			} else if updated, _ := projectionResult.RowsAffected(); updated != 1 {
				err = fmt.Errorf("k12storage: persist grading assessment projection updated %d rows", updated)
			}
		}
	} else if effects.Review != nil {
		err = s.commitAssessmentReviewTx(ctx, tx, item.AgentName, *effects.Review)
	}
	if err != nil {
		return k12.GradingAssessmentItem{}, false, err
	}
	assessmentEmitted, err := appendGradingAssessmentCommittedEvent(ctx, tx, item)
	if err != nil {
		return k12.GradingAssessmentItem{}, false, err
	}
	emitted = emitted || assessmentEmitted
	if err := tx.Commit(); err != nil {
		return k12.GradingAssessmentItem{}, false, fmt.Errorf("k12storage: commit grading assessment transaction: %w", err)
	}
	if emitted && s.notifyOutbox != nil {
		s.notifyOutbox()
	}
	return item, true, nil
}

func validateGradingAssessmentInputBinding(ctx context.Context, q dbQueryer,
	item k12.GradingAssessmentItem,
) error {
	var confirmedVersion int
	var inputDigest string
	err := q.QueryRowContext(ctx, `SELECT confirmed_version,input_digest FROM k12_attempts
		WHERE agent_name=? AND attempt_id=? AND problem_id=?`, item.AgentName, item.AttemptID,
		item.ProblemID).Scan(&confirmedVersion, &inputDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return records.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("k12storage: validate assessment input binding: %w", err)
	}
	if confirmedVersion != item.ConfirmedVersion || inputDigest != item.InputDigest {
		return fmt.Errorf("%w: attempt=%s frozen input changed",
			ErrGradingAssessmentItemConflict, item.AttemptID)
	}
	return nil
}

func appendGradingAssessmentCommittedEvent(ctx context.Context, ex dbHandle,
	item k12.GradingAssessmentItem,
) (bool, error) {
	payload, err := json.Marshal(GradingAssessmentCommittedPayload{
		AgentName: item.AgentName, JobID: item.JobID, ProblemID: item.ProblemID,
		AttemptID: item.AttemptID, Status: string(item.Status), ResultDigest: item.ResultDigest,
	})
	if err != nil {
		return false, fmt.Errorf("k12storage: marshal grading assessment event: %w", err)
	}
	return appendOutboxEvent(ctx, ex, OutboxEvent{
		EventID:   gradingAssessmentEventID(item, "assessment_committed"),
		AgentName: item.AgentName, AggregateID: item.JobID + ":" + item.ProblemID,
		EventType: EventGradingAssessmentCommitted, PayloadVersion: 1, Payload: string(payload),
	})
}

func (s *Store) commitAssessmentMistakeTx(ctx context.Context, tx *sql.Tx, agentName string,
	effect GradingMistakeEffect, eventID string,
) (string, bool, bool, error) {
	record, err := k12.NewMistakeRecord(agentName, effect.SourceSession, effect.Fields)
	if err != nil {
		return "", false, false, err
	}
	record.DueAt = effect.DueAt
	schema, err := s.registry.Get(record.Collection)
	if err != nil {
		return "", false, false, err
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(record.Fields); err != nil {
			return "", false, false, fmt.Errorf("%w: 记录集 %q: %v", records.ErrInvalidFields, record.Collection, err)
		}
	}
	record.Status = schema.InitialStatus
	record.DedupeKey = schema.DedupeKey(record)
	record.SchemaVersion = schema.Version
	record.RecordID = idgen.NanoID()
	record.Tags = "[]"
	now := nowUnix()
	record.CreatedAt, record.UpdatedAt, record.Version = now, now, 0
	mp := mistakeMapper{}
	domainVals, err := mp.encode(record.Fields)
	if err != nil {
		return "", false, false, err
	}
	q := fmt.Sprintf(`INSERT INTO %s (%s, %s) VALUES (%s)
        ON CONFLICT(agent_name,dedupe_key) DO NOTHING`, mp.table(), baseCols,
		strings.Join(mp.domainCols(), ", "), placeholders(11+len(mp.domainCols())))
	args := append([]any{record.RecordID, record.AgentName, record.SchemaVersion, record.Status,
		record.DedupeKey, record.Tags, record.DueAt, record.SourceSession, record.Version,
		record.CreatedAt, record.UpdatedAt}, domainVals...)
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return "", false, false, fmt.Errorf("k12storage: grading assessment mistake insert: %w", err)
	}
	created, _ := res.RowsAffected()
	if created == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT record_id FROM k12_mistakes
            WHERE agent_name=? AND dedupe_key=?`, record.AgentName, record.DedupeKey).Scan(&record.RecordID); err != nil {
			return "", false, false, fmt.Errorf("k12storage: grading assessment mistake dedupe lookup: %w", err)
		}
	}
	emitted, err := appendMistakeRecordedEvent(ctx, tx, record, created > 0, eventID)
	if err != nil {
		return "", false, false, err
	}
	return record.RecordID, created > 0, emitted, nil
}

func (s *Store) commitAssessmentReviewTx(ctx context.Context, tx *sql.Tx, agentName string,
	effect GradingReviewEffect,
) error {
	rows, err := s.queryRecordsVia(ctx, tx, mistakeMapper{},
		`WHERE agent_name=? AND record_id=?`, agentName, effect.RecordID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return records.ErrNotFound
	}
	current := rows[0]
	if current.Version != effect.ExpectedVersion {
		return records.ErrVersionConflict
	}
	schema, err := s.registry.Get(k12.CollectionMistakes)
	if err != nil {
		return err
	}
	if !schemaHasStatus(schema, effect.NewStatus) {
		return records.ErrInvalidStatus
	}
	if !schemaCanTransition(schema, current.Status, effect.NewStatus) {
		return records.ErrIllegalTransition
	}
	raw, err := json.Marshal(effect.Fields)
	if err != nil {
		return err
	}
	if err := schema.ValidateFields(string(raw)); err != nil {
		return fmt.Errorf("%w: 记录集 %q: %v", records.ErrInvalidFields, k12.CollectionMistakes, err)
	}
	values, err := (mistakeMapper{}).encode(string(raw))
	if err != nil {
		return err
	}
	assignments := make([]string, 0, len((mistakeMapper{}).domainCols()))
	for _, col := range (mistakeMapper{}).domainCols() {
		assignments = append(assignments, col+"=?")
	}
	args := append([]any{effect.NewStatus, effect.DueAt}, values...)
	args = append(args, nowUnix(), effect.RecordID, agentName, effect.ExpectedVersion)
	res, err := tx.ExecContext(ctx, `UPDATE k12_mistakes SET status=?,due_at=?,`+
		strings.Join(assignments, ",")+`,version=version+1,updated_at=?
        WHERE record_id=? AND agent_name=? AND version=?`, args...)
	if err != nil {
		return fmt.Errorf("k12storage: grading assessment review update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return records.ErrVersionConflict
	}
	return nil
}

func getGradingAssessmentItemVia(ctx context.Context, q dbQueryer, agentName, jobID, problemID string) (k12.GradingAssessmentItem, error) {
	item, err := scanGradingAssessmentItem(q.QueryRowContext(ctx, `SELECT `+gradingAssessmentItemColumns+`
        FROM k12_grading_assessment_items WHERE agent_name=? AND job_id=? AND problem_id=?`,
		agentName, jobID, problemID))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.GradingAssessmentItem{}, records.ErrNotFound
	}
	if err != nil {
		return k12.GradingAssessmentItem{}, fmt.Errorf("k12storage: get grading assessment item: %w", err)
	}
	return item, nil
}

func (s *Store) GetGradingAssessmentItem(ctx context.Context, agentName, jobID, problemID string) (k12.GradingAssessmentItem, error) {
	return getGradingAssessmentItemVia(ctx, s.db, agentName, jobID, problemID)
}

func (s *Store) ListGradingAssessmentItems(ctx context.Context, agentName, jobID string) ([]k12.GradingAssessmentItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+gradingAssessmentItemColumns+`
        FROM k12_grading_assessment_items WHERE agent_name=? AND job_id=? ORDER BY problem_id`, agentName, jobID)
	if err != nil {
		return nil, fmt.Errorf("k12storage: list grading assessment items: %w", err)
	}
	defer rows.Close()
	out := make([]k12.GradingAssessmentItem, 0)
	for rows.Next() {
		item, scanErr := scanGradingAssessmentItem(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
