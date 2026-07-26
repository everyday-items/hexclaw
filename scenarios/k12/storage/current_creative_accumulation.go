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

var ErrCurrentCommandConflict = errors.New("current K12 command identity conflict")

// CreateAccumulationWithDerivedMetadata atomically writes canonical content,
// validated server-derived metadata and each provenance record. source_ref is
// left at its legacy empty default; current nullable source lives only in
// derived_source_ref.
func (s *Store) CreateAccumulationWithDerivedMetadata(
	ctx context.Context,
	rec *records.AgentRecord,
	metadata k12.AccumulationDerivedMetadata,
	commandKey, requestDigest string,
) (bool, error) {
	if rec == nil || rec.AgentName == "" ||
		rec.Collection != k12.CollectionAccumulation {
		return false, records.ErrInvalidRecord
	}
	if err := metadata.Validate(); err != nil {
		return false, fmt.Errorf("%w: %v", records.ErrInvalidFields, err)
	}
	commandKey = strings.TrimSpace(commandKey)
	requestDigest = strings.TrimSpace(requestDigest)
	if commandKey == "" || requestDigest == "" {
		return false, ErrCurrentCommandConflict
	}
	fields, err := k12.ParseAccumFields(rec.Fields)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(fields.Source) != "" ||
		fields.Subject != metadata.Subject || fields.EntryType != metadata.EntryType {
		return false, fmt.Errorf(
			"%w: current accumulation root must use derived metadata and empty legacy source",
			records.ErrInvalidFields,
		)
	}
	schema, err := s.registry.Get(k12.CollectionAccumulation)
	if err != nil {
		return false, err
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(rec.Fields); err != nil {
			return false, fmt.Errorf("%w: %v", records.ErrInvalidFields, err)
		}
	}
	mp, err := s.mapperFor(k12.CollectionAccumulation)
	if err != nil {
		return false, err
	}
	domainVals, err := mp.encode(rec.Fields)
	if err != nil {
		return false, err
	}
	subjectProvenance, err := json.Marshal(metadata.SubjectProvenance)
	if err != nil {
		return false, err
	}
	entryTypeProvenance, err := json.Marshal(metadata.EntryTypeProvenance)
	if err != nil {
		return false, err
	}
	sourceProvenance := ""
	var derivedSource any
	if source := strings.TrimSpace(metadata.Source); source != "" {
		derivedSource = source
		raw, err := json.Marshal(metadata.SourceProvenance)
		if err != nil {
			return false, err
		}
		sourceProvenance = string(raw)
	}
	if err := ensureAgentRegistered(ctx, s.db, rec.AgentName); err != nil {
		return false, err
	}
	rec.SchemaVersion = schema.Version
	rec.Status = k12.AccumStatusKept
	rec.Tags = strings.TrimSpace(rec.Tags)
	if rec.Tags == "" {
		rec.Tags = "[]"
	}
	if rec.RecordID == "" {
		rec.RecordID = idgen.NanoID()
	}
	now := nowUnix()
	rec.CreatedAt, rec.UpdatedAt, rec.Version = now, now, 0
	rec.DedupeKey = schema.DedupeKey(rec)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if receipt, err := getCurrentCreateReceiptVia(
		ctx, tx, rec.AgentName, "accumulation", commandKey,
	); err == nil {
		if receipt.RequestDigest != requestDigest {
			return false, ErrCurrentCommandConflict
		}
		rec.RecordID = receipt.ObjectID
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	} else if !errors.Is(err, records.ErrNotFound) {
		return false, err
	}
	cols := mp.domainCols()
	query := fmt.Sprintf(`INSERT INTO %s (%s, %s) VALUES (%s)
		ON CONFLICT(agent_name, dedupe_key) DO NOTHING`,
		mp.table(), baseCols, strings.Join(cols, ", "), placeholders(11+len(cols)))
	args := append([]any{
		rec.RecordID, rec.AgentName, rec.SchemaVersion, rec.Status, rec.DedupeKey,
		rec.Tags, rec.DueAt, rec.SourceSession, rec.Version, rec.CreatedAt, rec.UpdatedAt,
	}, domainVals...)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("k12storage: create current accumulation: %w", err)
	}
	affected, _ := result.RowsAffected()
	created := affected > 0
	if !created {
		if err := tx.QueryRowContext(ctx, `SELECT record_id
			FROM k12_accumulations
			WHERE agent_name=? AND dedupe_key=? AND deleted_at IS NULL`,
			rec.AgentName, rec.DedupeKey,
		).Scan(&rec.RecordID); err == sql.ErrNoRows {
			return false, records.ErrNotFound
		} else if err != nil {
			return false, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE k12_accumulations
			SET derived_source_ref=?, subject_provenance_json=?,
			    entry_type_provenance_json=?, source_provenance_json=?
			WHERE record_id=? AND agent_name=?`,
			derivedSource, string(subjectProvenance), string(entryTypeProvenance),
			sourceProvenance, rec.RecordID, rec.AgentName,
		); err != nil {
			return false, err
		}
	}
	receipt := k12.CurrentCreateReceipt{
		ObjectKind: "accumulation", ObjectID: rec.RecordID,
		CommandKey: commandKey, RequestDigest: requestDigest,
		Created: created, CreatedAt: now,
	}
	if err := insertCurrentCreateReceipt(ctx, tx, rec.AgentName, receipt); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return created, nil
}

func getCurrentCreateReceiptVia(
	ctx context.Context,
	q dbQueryer,
	agentName, objectKind, commandKey string,
) (k12.CurrentCreateReceipt, error) {
	var receipt k12.CurrentCreateReceipt
	var raw string
	err := q.QueryRowContext(ctx, `SELECT request_digest, object_id,
		receipt_json, created_at
		FROM k12_current_create_receipts
		WHERE agent_name=? AND object_kind=? AND command_key=?`,
		agentName, objectKind, commandKey,
	).Scan(&receipt.RequestDigest, &receipt.ObjectID, &raw, &receipt.CreatedAt)
	if err == sql.ErrNoRows {
		return receipt, records.ErrNotFound
	}
	if err != nil {
		return receipt, err
	}
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return receipt, err
	}
	receipt.ObjectKind = objectKind
	receipt.CommandKey = commandKey
	return receipt, nil
}

func (s *Store) GetCurrentCreateReceipt(
	ctx context.Context,
	agentName, objectKind, commandKey, requestDigest string,
) (k12.CurrentCreateReceipt, error) {
	receipt, err := getCurrentCreateReceiptVia(
		ctx, s.db, agentName, objectKind, strings.TrimSpace(commandKey),
	)
	if err != nil {
		return receipt, err
	}
	if receipt.RequestDigest != strings.TrimSpace(requestDigest) {
		return k12.CurrentCreateReceipt{}, ErrCurrentCommandConflict
	}
	return receipt, nil
}

func insertCurrentCreateReceipt(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
	receipt k12.CurrentCreateReceipt,
) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO k12_current_create_receipts (
		agent_name, object_kind, command_key, request_digest,
		object_id, receipt_json, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		agentName, receipt.ObjectKind, receipt.CommandKey,
		receipt.RequestDigest, receipt.ObjectID, string(raw), receipt.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("k12storage: insert current create receipt: %w", err)
	}
	return nil
}

// CreateCreativeWorkWithInitialGeneration writes the current CreativeWork root
// and its generation-1 checkpoint in one SQLite transaction. It intentionally
// skips creativeWorkMapper.syncChildren: current works never create legacy
// version/feedback rows.
func (s *Store) CreateCreativeWorkWithInitialGeneration(
	ctx context.Context,
	rec *records.AgentRecord,
	commandKey string,
	requestDigest string,
	source k12.CreativeWorkSourceSnapshot,
) (k12.WorkFeedbackGeneration, bool, error) {
	if rec == nil || rec.AgentName == "" ||
		rec.Collection != k12.CollectionCreativeWork {
		return k12.WorkFeedbackGeneration{}, false, records.ErrInvalidRecord
	}
	commandKey = strings.TrimSpace(commandKey)
	requestDigest = strings.TrimSpace(requestDigest)
	if commandKey == "" || requestDigest == "" {
		return k12.WorkFeedbackGeneration{}, false, fmt.Errorf(
			"%w: creative work command_key/request_digest required", ErrCurrentCommandConflict,
		)
	}
	if len(rec.Fields) == 0 {
		rec.Fields = "{}"
	}
	fields, err := k12.ParseCreativeWorkFields(rec.Fields)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	if len(fields.Versions) != 0 {
		return k12.WorkFeedbackGeneration{}, false, fmt.Errorf(
			"%w: current CreativeWork cannot contain legacy versions", records.ErrInvalidFields,
		)
	}
	source.WorkType = strings.TrimSpace(source.WorkType)
	if source.WorkType == "" {
		source.WorkType = fields.WorkType
	}
	source.DisplayName = strings.TrimSpace(source.DisplayName)
	if source.DisplayName == "" {
		source.DisplayName = fields.DisplayName
	}
	source.WorkTitle = strings.TrimSpace(source.WorkTitle)
	if source.WorkTitle == "" {
		source.WorkTitle = fields.WorkTitle
	}
	if source.WorkType != k12.WorkTypeWriting && source.WorkType != k12.WorkTypeArt {
		return k12.WorkFeedbackGeneration{}, false, fmt.Errorf(
			"%w: invalid current work type %q", records.ErrInvalidFields, source.WorkType,
		)
	}
	if source.WorkType == k12.WorkTypeWriting &&
		strings.TrimSpace(source.ContentMarkdown) == "" &&
		strings.TrimSpace(source.SourceAssetID) == "" {
		return k12.WorkFeedbackGeneration{}, false, fmt.Errorf(
			"%w: writing requires content or source asset", records.ErrInvalidFields,
		)
	}
	if source.WorkType == k12.WorkTypeArt && strings.TrimSpace(source.SourceAssetID) == "" {
		return k12.WorkFeedbackGeneration{}, false, fmt.Errorf(
			"%w: art requires source asset", records.ErrInvalidFields,
		)
	}

	schema, err := s.registry.Get(k12.CollectionCreativeWork)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(rec.Fields); err != nil {
			return k12.WorkFeedbackGeneration{}, false, fmt.Errorf(
				"%w: %v", records.ErrInvalidFields, err,
			)
		}
	}
	mp, err := s.mapperFor(k12.CollectionCreativeWork)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	domainVals, err := mp.encode(rec.Fields)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	if err := ensureAgentRegistered(ctx, s.db, rec.AgentName); err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}

	rec.SchemaVersion = schema.Version
	rec.Status = k12.WorkStatusDraft
	rec.Tags = strings.TrimSpace(rec.Tags)
	if rec.Tags == "" {
		rec.Tags = "[]"
	}
	if rec.RecordID == "" {
		rec.RecordID = idgen.NanoID()
	}
	now := nowUnix()
	rec.CreatedAt, rec.UpdatedAt, rec.Version = now, now, 0
	// A save command is the identity boundary. Two independent saves of byte-
	// identical content are two works; only an exact replay of the same command
	// may collapse. The durable receipt below is the primary replay source and
	// this command-derived dedupe key is the concurrent-insert backstop.
	dedupeSum := sha256.Sum256([]byte(
		rec.AgentName + "\x00creative_work\x00" + commandKey,
	))
	rec.DedupeKey = hex.EncodeToString(dedupeSum[:])

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	defer tx.Rollback()
	if receipt, err := getCurrentCreateReceiptVia(
		ctx, tx, rec.AgentName, "creative_work", commandKey,
	); err == nil {
		if receipt.RequestDigest != requestDigest {
			return k12.WorkFeedbackGeneration{}, false, ErrCurrentCommandConflict
		}
		rec.RecordID = receipt.ObjectID
		var initialID string
		if err := tx.QueryRowContext(ctx, `SELECT initial_feedback_generation_id
			FROM k12_creative_works
			WHERE record_id=? AND agent_name=? AND deleted_at IS NULL`,
			rec.RecordID, rec.AgentName,
		).Scan(&initialID); err == sql.ErrNoRows {
			return k12.WorkFeedbackGeneration{}, false, records.ErrNotFound
		} else if err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		generation, err := getWorkFeedbackGenerationVia(
			ctx, tx, rec.AgentName, initialID,
		)
		if err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		return generation, false, nil
	} else if !errors.Is(err, records.ErrNotFound) {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	cols := mp.domainCols()
	query := fmt.Sprintf(`INSERT INTO %s (%s, %s) VALUES (%s)
        ON CONFLICT(agent_name, dedupe_key) DO NOTHING`,
		mp.table(), baseCols, strings.Join(cols, ", "), placeholders(11+len(cols)))
	args := append([]any{
		rec.RecordID, rec.AgentName, rec.SchemaVersion, rec.Status, rec.DedupeKey,
		rec.Tags, rec.DueAt, rec.SourceSession, rec.Version, rec.CreatedAt, rec.UpdatedAt,
	}, domainVals...)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, false, fmt.Errorf(
			"k12storage: create current work root: %w", err,
		)
	}
	affected, _ := result.RowsAffected()
	created := affected > 0
	if !created {
		if err := tx.QueryRowContext(ctx, `SELECT record_id
			FROM k12_creative_works
			WHERE agent_name=? AND dedupe_key=? AND deleted_at IS NULL`,
			rec.AgentName, rec.DedupeKey,
		).Scan(&rec.RecordID); err == sql.ErrNoRows {
			return k12.WorkFeedbackGeneration{}, false, records.ErrNotFound
		} else if err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		var initialID string
		if err := tx.QueryRowContext(ctx, `SELECT initial_feedback_generation_id
			FROM k12_creative_works WHERE record_id=? AND agent_name=?`,
			rec.RecordID, rec.AgentName,
		).Scan(&initialID); err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		if initialID == "" {
			return k12.WorkFeedbackGeneration{}, false, fmt.Errorf(
				"%w: duplicate work has no initial generation", ErrCurrentCommandConflict,
			)
		}
		generation, err := getWorkFeedbackGenerationVia(
			ctx, tx, rec.AgentName, initialID,
		)
		if err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		if generation.RequestDigest != requestDigest {
			return k12.WorkFeedbackGeneration{}, false, fmt.Errorf(
				"%w: duplicate work request digest changed", ErrCurrentCommandConflict,
			)
		}
		receipt := k12.CurrentCreateReceipt{
			ObjectKind: "creative_work", ObjectID: rec.RecordID,
			CommandKey: commandKey, RequestDigest: requestDigest,
			Created: false, CreatedAt: now,
		}
		if err := insertCurrentCreateReceipt(
			ctx, tx, rec.AgentName, receipt,
		); err != nil {
			// A concurrent winner may already have committed the same receipt.
			// Re-read and validate instead of manufacturing a second identity.
			existing, readErr := getCurrentCreateReceiptVia(
				ctx, tx, rec.AgentName, "creative_work", commandKey,
			)
			if readErr != nil || existing.RequestDigest != requestDigest ||
				existing.ObjectID != rec.RecordID {
				return k12.WorkFeedbackGeneration{}, false, err
			}
		}
		if err := tx.Commit(); err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		return generation, false, nil
	}

	generation := k12.WorkFeedbackGeneration{
		GenerationID:  idgen.NanoID(),
		WorkID:        rec.RecordID,
		AgentName:     rec.AgentName,
		GenerationNo:  1,
		CommandKey:    "auto:" + rec.RecordID,
		RequestDigest: requestDigest,
		Status:        k12.WorkFeedbackQueued,
		FeedbackType:  source.WorkType,
		Source:        source,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := insertWorkFeedbackGeneration(ctx, tx, generation, string(sourceJSON)); err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_creative_works
		SET initial_feedback_generation_id=?, feedback_state='queued'
		WHERE record_id=? AND agent_name=? AND deleted_at IS NULL`,
		generation.GenerationID, rec.RecordID, rec.AgentName,
	); err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	receipt := k12.CurrentCreateReceipt{
		ObjectKind: "creative_work", ObjectID: rec.RecordID,
		CommandKey: commandKey, RequestDigest: requestDigest,
		Created: true, CreatedAt: now,
	}
	if err := insertCurrentCreateReceipt(
		ctx, tx, rec.AgentName, receipt,
	); err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	return generation, true, nil
}

func insertWorkFeedbackGeneration(
	ctx context.Context,
	tx *sql.Tx,
	generation k12.WorkFeedbackGeneration,
	sourceJSON string,
) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO k12_work_feedback_generations (
		generation_id, work_id, agent_name, generation_no, command_key,
		request_digest, status, feedback_type, source_snapshot_json,
		request_snapshot_json, route_snapshot_json, invocation_snapshot_json,
		feedback_json, projection_markdown, failure_reason, attempt,
		created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', '{}', '{}', '', '', '', ?, ?, ?)`,
		generation.GenerationID, generation.WorkID, generation.AgentName,
		generation.GenerationNo, generation.CommandKey, generation.RequestDigest,
		generation.Status, generation.FeedbackType, sourceJSON,
		generation.Attempt, generation.CreatedAt, generation.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("k12storage: insert work feedback generation: %w", err)
	}
	return nil
}

func scanWorkFeedbackGeneration(row rowScanner) (k12.WorkFeedbackGeneration, error) {
	var generation k12.WorkFeedbackGeneration
	var sourceJSON, feedbackJSON, projection string
	if err := row.Scan(
		&generation.GenerationID, &generation.WorkID, &generation.AgentName,
		&generation.GenerationNo, &generation.CommandKey, &generation.RequestDigest,
		&generation.Status, &generation.FeedbackType, &sourceJSON, &feedbackJSON,
		&projection, &generation.FailureReason, &generation.Attempt,
		&generation.CreatedAt, &generation.UpdatedAt,
	); err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	if sourceJSON != "" {
		if err := json.Unmarshal([]byte(sourceJSON), &generation.Source); err != nil {
			return k12.WorkFeedbackGeneration{}, fmt.Errorf(
				"k12storage: decode work feedback source snapshot: %w", err,
			)
		}
	}
	if feedbackJSON != "" {
		feedback, err := k12.ParseWorkFeedbackJSON([]byte(feedbackJSON))
		if err != nil {
			return k12.WorkFeedbackGeneration{}, err
		}
		generation.Feedback = &feedback
	} else if projection != "" {
		// Legacy V37 backfill may only have the historical Markdown projection.
		// Keep it display-only; do not synthesize evidence or actions.
		generation.Feedback = &k12.WorkFeedback{
			FeedbackID:         generation.GenerationID,
			VersionID:          "legacy-read-only",
			FeedbackType:       generation.FeedbackType,
			ProjectionMarkdown: projection,
		}
	}
	return generation, nil
}

func getWorkFeedbackGenerationVia(
	ctx context.Context,
	q dbQueryer,
	agentName, generationID string,
) (k12.WorkFeedbackGeneration, error) {
	generation, err := scanWorkFeedbackGeneration(q.QueryRowContext(ctx, `
SELECT generation_id, work_id, agent_name, generation_no, command_key,
       request_digest, status, feedback_type, source_snapshot_json,
       feedback_json, projection_markdown, failure_reason, attempt,
       created_at, updated_at
FROM k12_work_feedback_generations
WHERE generation_id=? AND agent_name=?`, generationID, agentName))
	if err == sql.ErrNoRows {
		return k12.WorkFeedbackGeneration{}, records.ErrNotFound
	}
	if err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	return generation, nil
}

func (s *Store) GetWorkFeedbackGeneration(
	ctx context.Context,
	agentName, generationID string,
) (k12.WorkFeedbackGeneration, error) {
	return getWorkFeedbackGenerationVia(ctx, s.db, agentName, generationID)
}

// ListDirectWorkFeedbackGenerationsForRecovery returns only current works that
// are not owned by ImageTask. ImageTask-promoted works are recovered by the
// ImageTask coordinator so the two runners can never schedule the same
// generation.
func (s *Store) ListDirectWorkFeedbackGenerationsForRecovery(
	ctx context.Context,
	agentName string,
) ([]k12.WorkFeedbackGeneration, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT g.generation_id, g.work_id, g.agent_name, g.generation_no,
       g.command_key, g.request_digest, g.status, g.feedback_type,
       g.source_snapshot_json, g.feedback_json, g.projection_markdown,
       g.failure_reason, g.attempt, g.created_at, g.updated_at
FROM k12_work_feedback_generations g
JOIN k12_creative_works w
  ON w.record_id=g.work_id AND w.agent_name=g.agent_name
WHERE g.agent_name=?
  AND g.status IN ('queued','running')
  AND w.deleted_at IS NULL
  AND COALESCE(w.source_intake_id,'')=''
ORDER BY g.created_at,g.generation_id`, strings.TrimSpace(agentName))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]k12.WorkFeedbackGeneration, 0)
	for rows.Next() {
		generation, err := scanWorkFeedbackGeneration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, generation)
	}
	return out, rows.Err()
}

func (s *Store) MarkWorkFeedbackGenerationRunning(
	ctx context.Context,
	agentName, generationID string,
) (k12.WorkFeedbackGeneration, error) {
	now := nowUnix()
	if _, err := s.db.ExecContext(ctx, `UPDATE k12_work_feedback_generations
		SET status='running', updated_at=?
		WHERE generation_id=? AND agent_name=? AND status='queued'`,
		now, generationID, agentName,
	); err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	generation, err := getWorkFeedbackGenerationVia(
		ctx, s.db, agentName, generationID,
	)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	switch generation.Status {
	case k12.WorkFeedbackRunning, k12.WorkFeedbackSucceeded:
		return generation, nil
	default:
		return k12.WorkFeedbackGeneration{}, records.ErrVersionConflict
	}
}

// PrepareWorkFeedbackGeneration resumes generation 1 after an initial failure,
// or appends exactly one later generation keyed by the caller's command.
func (s *Store) PrepareWorkFeedbackGeneration(
	ctx context.Context,
	agentName, workID, commandKey, requestDigest string,
) (k12.WorkFeedbackGeneration, bool, error) {
	if agentName == "" || workID == "" || commandKey == "" || requestDigest == "" {
		return k12.WorkFeedbackGeneration{}, false, ErrCurrentCommandConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	defer tx.Rollback()
	var initialID, latestID string
	var deletedAt sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT initial_feedback_generation_id,
		latest_feedback_generation_id, deleted_at
		FROM k12_creative_works WHERE record_id=? AND agent_name=?`,
		workID, agentName,
	).Scan(&initialID, &latestID, &deletedAt); err == sql.ErrNoRows {
		return k12.WorkFeedbackGeneration{}, false, records.ErrNotFound
	} else if err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	if deletedAt.Valid {
		return k12.WorkFeedbackGeneration{}, false, records.ErrNotFound
	}
	if initialID == "" {
		source, err := legacyCreativeWorkSourceSnapshot(ctx, tx, agentName, workID)
		if err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		sourceJSON, err := json.Marshal(source)
		if err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		now := nowUnix()
		initial := k12.WorkFeedbackGeneration{
			GenerationID: idgen.NanoID(), WorkID: workID, AgentName: agentName,
			GenerationNo: 1, CommandKey: "auto:" + workID,
			RequestDigest: requestDigest, Status: k12.WorkFeedbackQueued,
			FeedbackType: source.WorkType, Source: source,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := insertWorkFeedbackGeneration(ctx, tx, initial, string(sourceJSON)); err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE k12_creative_works
			SET initial_feedback_generation_id=?, feedback_state='queued',
			    row_version=row_version+1
			WHERE record_id=? AND agent_name=? AND deleted_at IS NULL`,
			initial.GenerationID, workID, agentName,
		); err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		return initial, true, nil
	}

	initial, err := getWorkFeedbackGenerationVia(ctx, tx, agentName, initialID)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	if latestID == "" {
		switch initial.Status {
		case k12.WorkFeedbackFailed:
			now := nowUnix()
			if _, err := tx.ExecContext(ctx, `UPDATE k12_work_feedback_generations
				SET status='queued', failure_reason='', attempt=attempt+1, updated_at=?
				WHERE generation_id=? AND agent_name=? AND status='failed'`,
				now, initial.GenerationID, agentName,
			); err != nil {
				return k12.WorkFeedbackGeneration{}, false, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE k12_creative_works
				SET feedback_state='queued', row_version=row_version+1
				WHERE record_id=? AND agent_name=? AND deleted_at IS NULL`,
				workID, agentName,
			); err != nil {
				return k12.WorkFeedbackGeneration{}, false, err
			}
			initial.Status = k12.WorkFeedbackQueued
			initial.FailureReason = ""
			initial.Attempt++
			initial.UpdatedAt = now
		case k12.WorkFeedbackQueued, k12.WorkFeedbackRunning:
			// The automatic initial checkpoint itself is the work to resume.
		case k12.WorkFeedbackSucceeded:
			if _, err := tx.ExecContext(ctx, `UPDATE k12_creative_works
				SET latest_feedback_generation_id=?, feedback_state='succeeded'
				WHERE record_id=? AND agent_name=? AND deleted_at IS NULL`,
				initial.GenerationID, workID, agentName,
			); err != nil {
				return k12.WorkFeedbackGeneration{}, false, err
			}
		default:
			return k12.WorkFeedbackGeneration{}, false, ErrCurrentCommandConflict
		}
		if err := tx.Commit(); err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		return initial, false, nil
	}

	existing, err := scanWorkFeedbackGeneration(tx.QueryRowContext(ctx, `
SELECT generation_id, work_id, agent_name, generation_no, command_key,
       request_digest, status, feedback_type, source_snapshot_json,
       feedback_json, projection_markdown, failure_reason, attempt,
       created_at, updated_at
FROM k12_work_feedback_generations
WHERE work_id=? AND agent_name=? AND command_key=?`,
		workID, agentName, commandKey,
	))
	if err == nil {
		if existing.RequestDigest != requestDigest {
			return k12.WorkFeedbackGeneration{}, false, ErrCurrentCommandConflict
		}
		if err := tx.Commit(); err != nil {
			return k12.WorkFeedbackGeneration{}, false, err
		}
		return existing, false, nil
	}
	if err != sql.ErrNoRows {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	var activeCount int
	if err := tx.QueryRowContext(ctx, `SELECT count(*)
		FROM k12_work_feedback_generations
		WHERE work_id=? AND status IN ('queued','running')`, workID,
	).Scan(&activeCount); err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	if activeCount != 0 {
		return k12.WorkFeedbackGeneration{}, false, records.ErrVersionConflict
	}
	var generationNo int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(generation_no),0)+1
		FROM k12_work_feedback_generations WHERE work_id=?`, workID,
	).Scan(&generationNo); err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	source := initial.Source
	sourceJSON, err := json.Marshal(source)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	now := nowUnix()
	next := k12.WorkFeedbackGeneration{
		GenerationID: idgen.NanoID(), WorkID: workID, AgentName: agentName,
		GenerationNo: generationNo, CommandKey: commandKey,
		RequestDigest: requestDigest, Status: k12.WorkFeedbackQueued,
		FeedbackType: initial.FeedbackType, Source: source,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := insertWorkFeedbackGeneration(ctx, tx, next, string(sourceJSON)); err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return k12.WorkFeedbackGeneration{}, false, err
	}
	return next, true, nil
}

func legacyCreativeWorkSourceSnapshot(
	ctx context.Context,
	q dbQueryer,
	agentName, workID string,
) (k12.CreativeWorkSourceSnapshot, error) {
	var source k12.CreativeWorkSourceSnapshot
	if err := q.QueryRowContext(ctx, `SELECT work_type, display_name, work_title
		FROM k12_creative_works
		WHERE record_id=? AND agent_name=? AND deleted_at IS NULL`,
		workID, agentName,
	).Scan(&source.WorkType, &source.DisplayName, &source.WorkTitle); err == sql.ErrNoRows {
		return source, records.ErrNotFound
	} else if err != nil {
		return source, err
	}
	var ocrJobID string
	err := q.QueryRowContext(ctx, `SELECT source_asset_id, content_markdown,
		ocr_raw, ocr_version, ocr_confirmed_digest, content_confirmed_at, ocr_job_id
		FROM k12_creative_work_versions
		WHERE work_record_id=? ORDER BY version_index DESC LIMIT 1`, workID,
	).Scan(
		&source.SourceAssetID, &source.ContentMarkdown, &source.OCRRaw,
		&source.OCRVersion, &source.OCRDigest, &source.ContentConfirmedAt, &ocrJobID,
	)
	if err == sql.ErrNoRows {
		return source, nil
	}
	if err != nil {
		return source, err
	}
	return source, nil
}

func (s *Store) FailWorkFeedbackGeneration(
	ctx context.Context,
	agentName, generationID, reason string,
) (k12.WorkFeedbackGeneration, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	defer tx.Rollback()
	generation, err := getWorkFeedbackGenerationVia(ctx, tx, agentName, generationID)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	if generation.Status == k12.WorkFeedbackSucceeded {
		return k12.WorkFeedbackGeneration{}, records.ErrVersionConflict
	}
	if generation.Status != k12.WorkFeedbackFailed {
		now := nowUnix()
		if _, err := tx.ExecContext(ctx, `UPDATE k12_work_feedback_generations
			SET status='failed', failure_reason=?, updated_at=?
			WHERE generation_id=? AND agent_name=? AND status IN ('queued','running')`,
			strings.TrimSpace(reason), now, generationID, agentName,
		); err != nil {
			return k12.WorkFeedbackGeneration{}, err
		}
		if generation.GenerationNo == 1 {
			if _, err := tx.ExecContext(ctx, `UPDATE k12_creative_works
				SET feedback_state='failed', row_version=row_version+1
				WHERE record_id=? AND agent_name=? AND
				      initial_feedback_generation_id=? AND
				      latest_feedback_generation_id='' AND deleted_at IS NULL`,
				generation.WorkID, agentName, generationID,
			); err != nil {
				return k12.WorkFeedbackGeneration{}, err
			}
		}
		generation.Status = k12.WorkFeedbackFailed
		generation.FailureReason = strings.TrimSpace(reason)
		generation.UpdatedAt = now
	}
	if err := tx.Commit(); err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	return generation, nil
}

func (s *Store) CompleteWorkFeedbackGeneration(
	ctx context.Context,
	agentName, generationID string,
	feedback k12.WorkFeedback,
) (k12.WorkFeedbackGeneration, error) {
	if err := feedback.Validate(); err != nil {
		return k12.WorkFeedbackGeneration{}, fmt.Errorf("%w: %v", records.ErrInvalidFields, err)
	}
	feedbackJSON, err := json.Marshal(feedback)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	defer tx.Rollback()
	generation, err := getWorkFeedbackGenerationVia(ctx, tx, agentName, generationID)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	if generation.Status == k12.WorkFeedbackSucceeded {
		if err := tx.Commit(); err != nil {
			return k12.WorkFeedbackGeneration{}, err
		}
		return generation, nil
	}
	var deletedAt sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT deleted_at FROM k12_creative_works
		WHERE record_id=? AND agent_name=?`, generation.WorkID, agentName,
	).Scan(&deletedAt); err == sql.ErrNoRows || deletedAt.Valid {
		return k12.WorkFeedbackGeneration{}, records.ErrNotFound
	} else if err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	var laterCount int
	if err := tx.QueryRowContext(ctx, `SELECT count(*)
		FROM k12_work_feedback_generations
		WHERE work_id=? AND generation_no>?`, generation.WorkID, generation.GenerationNo,
	).Scan(&laterCount); err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	if laterCount != 0 {
		return k12.WorkFeedbackGeneration{}, records.ErrVersionConflict
	}
	now := nowUnix()
	result, err := tx.ExecContext(ctx, `UPDATE k12_work_feedback_generations
		SET status='succeeded', feedback_json=?, projection_markdown=?,
		    failure_reason='', updated_at=?
		WHERE generation_id=? AND agent_name=? AND status IN ('queued','running')`,
		string(feedbackJSON), feedback.ProjectionMarkdown, now, generationID, agentName,
	)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return k12.WorkFeedbackGeneration{}, records.ErrVersionConflict
	}
	stateSet := ""
	if generation.GenerationNo == 1 {
		stateSet = ", feedback_state='succeeded'"
	}
	result, err = tx.ExecContext(ctx, `UPDATE k12_creative_works
		SET latest_feedback_generation_id=?, row_version=row_version+1,
		    status='feedback_ready'`+stateSet+`
		WHERE record_id=? AND agent_name=? AND deleted_at IS NULL`,
		generationID, generation.WorkID, agentName,
	)
	if err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return k12.WorkFeedbackGeneration{}, records.ErrVersionConflict
	}
	if err := tx.Commit(); err != nil {
		return k12.WorkFeedbackGeneration{}, err
	}
	generation.Status = k12.WorkFeedbackSucceeded
	generation.Feedback = &feedback
	generation.FailureReason = ""
	generation.UpdatedAt = now
	return generation, nil
}

func (s *Store) GetCreativeWorkGenerationState(
	ctx context.Context,
	agentName, workID string,
) (k12.CreativeWorkGenerationState, error) {
	var initialID, latestID string
	var state k12.CreativeWorkGenerationState
	if err := s.db.QueryRowContext(ctx, `SELECT initial_feedback_generation_id,
		latest_feedback_generation_id, row_version
		FROM k12_creative_works
		WHERE record_id=? AND agent_name=? AND deleted_at IS NULL`,
		workID, agentName,
	).Scan(&initialID, &latestID, &state.RowVersion); err == sql.ErrNoRows {
		return state, records.ErrNotFound
	} else if err != nil {
		return state, err
	}
	if initialID != "" {
		initial, err := s.GetWorkFeedbackGeneration(ctx, agentName, initialID)
		if err != nil {
			return state, err
		}
		state.Initial = &initial
	}
	if latestID != "" {
		latest, err := s.GetWorkFeedbackGeneration(ctx, agentName, latestID)
		if err != nil {
			return state, err
		}
		state.Latest = &latest
	}
	return state, nil
}

func scanAccumulationDictationGeneration(
	row rowScanner,
) (k12.AccumulationDictationGeneration, error) {
	var generation k12.AccumulationDictationGeneration
	if err := row.Scan(
		&generation.GenerationID, &generation.AccumulationID, &generation.AgentName,
		&generation.CommandKey, &generation.RequestDigest, &generation.Status,
		&generation.SourceSnapshot, &generation.PracticeItemID,
		&generation.FailureReason, &generation.Attempt,
		&generation.CreatedAt, &generation.UpdatedAt,
	); err != nil {
		return generation, err
	}
	return generation, nil
}

func getAccumulationDictationGenerationVia(
	ctx context.Context,
	q dbQueryer,
	agentName, generationID string,
) (k12.AccumulationDictationGeneration, error) {
	generation, err := scanAccumulationDictationGeneration(q.QueryRowContext(ctx, `
SELECT generation_id, accumulation_id, agent_name, command_key,
       request_digest, status, source_snapshot_json, practice_item_id,
       failure_reason, attempt, created_at, updated_at
FROM k12_accumulation_dictation_generations
WHERE generation_id=? AND agent_name=?`, generationID, agentName))
	if err == sql.ErrNoRows {
		return generation, records.ErrNotFound
	}
	return generation, err
}

func (s *Store) GetLatestAccumulationDictationGeneration(
	ctx context.Context,
	agentName, accumulationID string,
) (*k12.AccumulationDictationGeneration, error) {
	generation, err := scanAccumulationDictationGeneration(s.db.QueryRowContext(ctx, `
SELECT generation_id, accumulation_id, agent_name, command_key,
       request_digest, status, source_snapshot_json, practice_item_id,
       failure_reason, attempt, created_at, updated_at
FROM k12_accumulation_dictation_generations
WHERE accumulation_id=? AND agent_name=?
ORDER BY created_at DESC, generation_id DESC LIMIT 1`, accumulationID, agentName))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &generation, nil
}

func (s *Store) GetAccumulationCurrentProjection(
	ctx context.Context,
	agentName, accumulationID string,
) (derivedSource *string, rowVersion int, err error) {
	var source sql.NullString
	var deletedAt sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT derived_source_ref, row_version, deleted_at
		FROM k12_accumulations WHERE record_id=? AND agent_name=?`,
		accumulationID, agentName,
	).Scan(&source, &rowVersion, &deletedAt)
	if err == sql.ErrNoRows || deletedAt.Valid {
		return nil, 0, records.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	if source.Valid {
		value := source.String
		derivedSource = &value
	}
	return derivedSource, rowVersion, nil
}

func (s *Store) PrepareAccumulationDictationGeneration(
	ctx context.Context,
	agentName, accumulationID, commandKey, requestDigest, sourceSnapshot string,
) (k12.AccumulationDictationGeneration, bool, error) {
	if agentName == "" || accumulationID == "" || commandKey == "" ||
		requestDigest == "" || strings.TrimSpace(sourceSnapshot) == "" {
		return k12.AccumulationDictationGeneration{}, false, ErrCurrentCommandConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.AccumulationDictationGeneration{}, false, err
	}
	defer tx.Rollback()
	var deletedAt sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT deleted_at FROM k12_accumulations
		WHERE record_id=? AND agent_name=?`, accumulationID, agentName,
	).Scan(&deletedAt); err == sql.ErrNoRows || deletedAt.Valid {
		return k12.AccumulationDictationGeneration{}, false, records.ErrNotFound
	} else if err != nil {
		return k12.AccumulationDictationGeneration{}, false, err
	}
	existing, err := scanAccumulationDictationGeneration(tx.QueryRowContext(ctx, `
SELECT generation_id, accumulation_id, agent_name, command_key,
       request_digest, status, source_snapshot_json, practice_item_id,
       failure_reason, attempt, created_at, updated_at
FROM k12_accumulation_dictation_generations
WHERE accumulation_id=? AND agent_name=?
ORDER BY created_at DESC, generation_id DESC LIMIT 1`, accumulationID, agentName))
	if err == nil {
		if existing.RequestDigest != requestDigest ||
			existing.SourceSnapshot != sourceSnapshot {
			return k12.AccumulationDictationGeneration{}, false, ErrCurrentCommandConflict
		}
		if existing.Status == k12.DictationFailed {
			now := nowUnix()
			if _, err := tx.ExecContext(ctx, `UPDATE k12_accumulation_dictation_generations
				SET status='queued', failure_reason='', attempt=attempt+1, updated_at=?
				WHERE generation_id=? AND agent_name=? AND status='failed'`,
				now, existing.GenerationID, agentName,
			); err != nil {
				return k12.AccumulationDictationGeneration{}, false, err
			}
			existing.Status = k12.DictationQueued
			existing.FailureReason = ""
			existing.Attempt++
			existing.UpdatedAt = now
		}
		if err := tx.Commit(); err != nil {
			return k12.AccumulationDictationGeneration{}, false, err
		}
		return existing, false, nil
	}
	if err != sql.ErrNoRows {
		return k12.AccumulationDictationGeneration{}, false, err
	}
	now := nowUnix()
	generation := k12.AccumulationDictationGeneration{
		GenerationID: idgen.NanoID(), AccumulationID: accumulationID,
		AgentName: agentName, CommandKey: commandKey, RequestDigest: requestDigest,
		Status: k12.DictationQueued, SourceSnapshot: sourceSnapshot,
		Attempt: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO k12_accumulation_dictation_generations (
    generation_id, accumulation_id, agent_name, command_key, request_digest,
    status, source_snapshot_json, route_snapshot_json, invocation_snapshot_json,
    practice_item_id, failure_reason, attempt, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, 'queued', ?, '{}', '{}', '', '', ?, ?, ?)`,
		generation.GenerationID, accumulationID, agentName, commandKey,
		requestDigest, sourceSnapshot, generation.Attempt, now, now,
	); err != nil {
		return k12.AccumulationDictationGeneration{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return k12.AccumulationDictationGeneration{}, false, err
	}
	return generation, true, nil
}

func (s *Store) CommitAccumulationDictationGeneration(
	ctx context.Context,
	agentName, generationID, practiceItemID string,
) (k12.AccumulationDictationGeneration, error) {
	if practiceItemID == "" {
		return k12.AccumulationDictationGeneration{}, records.ErrInvalidFields
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.AccumulationDictationGeneration{}, err
	}
	defer tx.Rollback()
	generation, err := getAccumulationDictationGenerationVia(ctx, tx, agentName, generationID)
	if err != nil {
		return generation, err
	}
	if generation.Status == k12.DictationCommitted {
		if generation.PracticeItemID != practiceItemID {
			return generation, ErrCurrentCommandConflict
		}
		if err := tx.Commit(); err != nil {
			return generation, err
		}
		return generation, nil
	}
	var deletedAt sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT deleted_at FROM k12_accumulations
		WHERE record_id=? AND agent_name=?`,
		generation.AccumulationID, agentName,
	).Scan(&deletedAt); err == sql.ErrNoRows || deletedAt.Valid {
		return generation, records.ErrNotFound
	} else if err != nil {
		return generation, err
	}
	now := nowUnix()
	result, err := tx.ExecContext(ctx, `UPDATE k12_accumulation_dictation_generations
		SET status='committed', practice_item_id=?, failure_reason='', updated_at=?
		WHERE generation_id=? AND agent_name=?
		  AND status IN ('queued','generating','validating')`,
		practiceItemID, now, generationID, agentName,
	)
	if err != nil {
		return generation, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return generation, records.ErrVersionConflict
	}
	if err := tx.Commit(); err != nil {
		return generation, err
	}
	generation.Status = k12.DictationCommitted
	generation.PracticeItemID = practiceItemID
	generation.FailureReason = ""
	generation.UpdatedAt = now
	return generation, nil
}

func (s *Store) FailAccumulationDictationGeneration(
	ctx context.Context,
	agentName, generationID, reason string,
) (k12.AccumulationDictationGeneration, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE k12_accumulation_dictation_generations
		SET status='failed', failure_reason=?, updated_at=?
		WHERE generation_id=? AND agent_name=?
		  AND status IN ('queued','generating','validating')`,
		strings.TrimSpace(reason), nowUnix(), generationID, agentName,
	)
	if err != nil {
		return k12.AccumulationDictationGeneration{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		current, getErr := getAccumulationDictationGenerationVia(
			ctx, s.db, agentName, generationID,
		)
		if getErr != nil {
			return current, getErr
		}
		if current.Status != k12.DictationFailed {
			return current, records.ErrVersionConflict
		}
		return current, nil
	}
	return getAccumulationDictationGenerationVia(ctx, s.db, agentName, generationID)
}

// TombstoneCurrentObject is the shared owner-scoped CAS/idempotent delete
// command for the two current record surfaces.
func (s *Store) TombstoneCurrentObject(
	ctx context.Context,
	agentName, objectKind, objectID string,
	expectedVersion int,
	commandKey string,
) (k12.CurrentDeleteReceipt, error) {
	table := ""
	switch objectKind {
	case "creative_work":
		table = "k12_creative_works"
	case "accumulation":
		table = "k12_accumulations"
	default:
		return k12.CurrentDeleteReceipt{}, records.ErrNotFound
	}
	if agentName == "" || objectID == "" || expectedVersion <= 0 ||
		strings.TrimSpace(commandKey) == "" {
		return k12.CurrentDeleteReceipt{}, records.ErrInvalidRecord
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.CurrentDeleteReceipt{}, err
	}
	defer tx.Rollback()
	var rowVersion int
	var deletedAt sql.NullInt64
	var storedCommand, receiptJSON string
	query := fmt.Sprintf(`SELECT row_version, deleted_at, delete_command_key,
		delete_receipt_json FROM %s WHERE record_id=? AND agent_name=?`, table)
	if err := tx.QueryRowContext(ctx, query, objectID, agentName).Scan(
		&rowVersion, &deletedAt, &storedCommand, &receiptJSON,
	); err == sql.ErrNoRows {
		return k12.CurrentDeleteReceipt{}, records.ErrNotFound
	} else if err != nil {
		return k12.CurrentDeleteReceipt{}, err
	}
	if deletedAt.Valid {
		if storedCommand != commandKey || receiptJSON == "" {
			return k12.CurrentDeleteReceipt{}, records.ErrVersionConflict
		}
		var receipt k12.CurrentDeleteReceipt
		if err := json.Unmarshal([]byte(receiptJSON), &receipt); err != nil {
			return receipt, err
		}
		if err := tx.Commit(); err != nil {
			return receipt, err
		}
		return receipt, nil
	}
	if rowVersion != expectedVersion {
		return k12.CurrentDeleteReceipt{}, records.ErrVersionConflict
	}
	receipt := k12.CurrentDeleteReceipt{
		ObjectKind: objectKind, ObjectID: objectID, Deleted: true,
		RowVersion: rowVersion + 1, DeletedAt: nowUnix(),
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return receipt, err
	}
	update := fmt.Sprintf(`UPDATE %s
		SET deleted_at=?, deleted_by=?, delete_command_key=?,
		    delete_receipt_json=?, row_version=row_version+1,
		    dedupe_key=dedupe_key||?
		WHERE record_id=? AND agent_name=? AND row_version=? AND deleted_at IS NULL`, table)
	result, err := tx.ExecContext(ctx, update,
		receipt.DeletedAt, agentName, commandKey, string(raw),
		dedupeTombstoneSep+objectID, objectID, agentName, expectedVersion,
	)
	if err != nil {
		return receipt, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return receipt, records.ErrVersionConflict
	}
	if err := tx.Commit(); err != nil {
		return receipt, err
	}
	return receipt, nil
}
