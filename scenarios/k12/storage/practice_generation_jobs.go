package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type practiceGenerationJobScanner interface {
	Scan(dest ...any) error
}

func scanPracticeGenerationJob(row practiceGenerationJobScanner) (k12.PracticeGenerationJob, error) {
	var job k12.PracticeGenerationJob
	var resultJSON string
	if err := row.Scan(
		&job.GenerationJobID, &job.AgentName, &job.IdempotencyKey, &job.RequestDigest,
		&job.Scope, &job.SourceKind, &job.SourceID, &job.SourceVersion,
		&job.VariantsPerSource, &job.Difficulty, &job.Total, &job.Textbook,
		&job.Status, &job.ResultSetID, &resultJSON, &job.DeduplicatedCount,
		&job.FailureReason, &job.SourceMistakeID, &job.SourceSummary,
		&job.RequestSnapshot, &job.RouteSnapshot, &job.Attempt, &job.RetiredAt,
		&job.GenerationOutput, &job.OutputAttempt,
		&job.ValidationOutput, &job.ValidationAttempt, &job.RetiredReason,
		&job.CreatedAt, &job.UpdatedAt,
	); err != nil {
		return k12.PracticeGenerationJob{}, err
	}
	if err := json.Unmarshal([]byte(resultJSON), &job.ResultItemIDs); err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 解析组卷结果 item ids: %w", err)
	}
	return job, nil
}

const practiceGenerationJobSelect = `SELECT generation_job_id, agent_name, idempotency_key,
	request_digest, scope, source_kind, source_id, source_version,
	variants_per_source, difficulty, total, textbook, status,
	result_set_id, result_item_ids_json, deduplicated_count, failure_reason,
	source_mistake_id, source_mistake_summary, request_snapshot_json, route_snapshot_json,
	attempt, retired_at, generation_output_json, generation_output_attempt,
	validation_output_json, validation_output_attempt, retired_reason, created_at, updated_at
	FROM k12_practice_generation_jobs`

var ErrPracticeGenerationOutputConflict = errors.New(
	"single practice generation output immutable conflict",
)

// GetPracticeGenerationJob 按 owner + 幂等键读取正式组卷命令收据。
func (s *Store) GetPracticeGenerationJob(ctx context.Context, agentName, idempotencyKey string) (k12.PracticeGenerationJob, error) {
	job, err := scanPracticeGenerationJob(s.db.QueryRowContext(ctx,
		practiceGenerationJobSelect+` WHERE agent_name = ? AND idempotency_key = ?`,
		agentName, idempotencyKey))
	if err == sql.ErrNoRows {
		return k12.PracticeGenerationJob{}, records.ErrNotFound
	}
	if err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 读组卷任务: %w", err)
	}
	return job, nil
}

// GetPracticeGenerationJobByID reads an owner-scoped durable generation by its
// immutable job identity.
func (s *Store) GetPracticeGenerationJobByID(
	ctx context.Context,
	agentName, generationJobID string,
) (k12.PracticeGenerationJob, error) {
	job, err := scanPracticeGenerationJob(s.db.QueryRowContext(ctx,
		practiceGenerationJobSelect+` WHERE agent_name = ? AND generation_job_id = ?`,
		agentName, generationJobID))
	if err == sql.ErrNoRows {
		return k12.PracticeGenerationJob{}, records.ErrNotFound
	}
	if err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 按 ID 读组卷任务: %w", err)
	}
	return job, nil
}

// GetLatestSinglePracticeGeneration is the canonical source-list projection
// input. It includes terminal/retired history so callers can distinguish
// failed, removed and re-due states without reconstructing client state.
func (s *Store) GetLatestSinglePracticeGeneration(
	ctx context.Context,
	agentName, sourceMistakeID string,
) (k12.PracticeGenerationJob, error) {
	job, err := scanPracticeGenerationJob(s.db.QueryRowContext(ctx,
		practiceGenerationJobSelect+` WHERE agent_name=? AND scope='single'
			AND source_mistake_id=? ORDER BY created_at DESC, generation_job_id DESC LIMIT 1`,
		agentName, sourceMistakeID))
	if err == sql.ErrNoRows {
		return k12.PracticeGenerationJob{}, records.ErrNotFound
	}
	if err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 读来源题最新 generation: %w", err)
	}
	return job, nil
}

// GetLatestPracticeGenerationBySource 按冻结来源身份读取唯一共享任务。
func (s *Store) GetLatestPracticeGenerationBySource(
	ctx context.Context,
	agentName, sourceKind, sourceID string,
	sourceVersion int,
) (k12.PracticeGenerationJob, error) {
	job, err := scanPracticeGenerationJob(s.db.QueryRowContext(ctx,
		practiceGenerationJobSelect+` WHERE agent_name=? AND scope='single'
			AND source_kind=? AND source_id=? AND source_version=?
			ORDER BY created_at DESC, generation_job_id DESC LIMIT 1`,
		strings.TrimSpace(agentName), strings.TrimSpace(sourceKind),
		strings.TrimSpace(sourceID), sourceVersion,
	))
	if err == sql.ErrNoRows {
		return k12.PracticeGenerationJob{}, records.ErrNotFound
	}
	if err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf(
			"k12storage: read practice generation by source: %w", err,
		)
	}
	return job, nil
}

// BeginPracticeGenerationJob 只持久化共享任务，不向公开 PracticeSet 写入占位项。
func (s *Store) BeginPracticeGenerationJob(
	ctx context.Context,
	job k12.PracticeGenerationJob,
) (k12.PracticeGenerationJob, bool, error) {
	job.AgentName = strings.TrimSpace(job.AgentName)
	job.IdempotencyKey = strings.TrimSpace(job.IdempotencyKey)
	job.SourceKind = strings.TrimSpace(job.SourceKind)
	job.SourceID = strings.TrimSpace(job.SourceID)
	job.SourceSummary = strings.TrimSpace(job.SourceSummary)
	job.RequestSnapshot = strings.TrimSpace(job.RequestSnapshot)
	job.RouteSnapshot = strings.TrimSpace(job.RouteSnapshot)
	if err := validatePracticeGenerationJob(job); err != nil {
		return k12.PracticeGenerationJob{}, false, err
	}
	if job.Scope != "single" || job.SourceID == "" || job.SourceVersion < 0 ||
		job.SourceSummary == "" || len(job.ResultItemIDs) != 1 ||
		strings.TrimSpace(job.ResultItemIDs[0]) == "" || job.RequestSnapshot == "" ||
		job.RouteSnapshot == "" {
		return k12.PracticeGenerationJob{}, false, fmt.Errorf(
			"k12storage: shared practice job missing source/snapshot/result identity",
		)
	}
	switch job.SourceKind {
	case k12.PracticeGenerationSourceMistake:
		if job.SourceMistakeID == "" {
			job.SourceMistakeID = job.SourceID
		}
		if job.SourceMistakeID != job.SourceID {
			return k12.PracticeGenerationJob{}, false, fmt.Errorf(
				"k12storage: mistake source identity mismatch",
			)
		}
	case k12.PracticeGenerationSourceAccumulation:
		if job.SourceMistakeID != "" {
			return k12.PracticeGenerationJob{}, false, fmt.Errorf(
				"k12storage: accumulation job cannot bind mistake source",
			)
		}
	default:
		return k12.PracticeGenerationJob{}, false, fmt.Errorf(
			"k12storage: unsupported practice source kind %q", job.SourceKind,
		)
	}
	if err := ensureAgentRegistered(ctx, s.db, job.AgentName); err != nil {
		return k12.PracticeGenerationJob{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.PracticeGenerationJob{}, false, fmt.Errorf(
			"k12storage: begin shared practice job transaction: %w", err,
		)
	}
	defer tx.Rollback()
	switch job.SourceKind {
	case k12.PracticeGenerationSourceMistake:
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT version FROM k12_mistakes
			WHERE record_id=? AND agent_name=?`, job.SourceID, job.AgentName).Scan(&version); err == sql.ErrNoRows {
			return k12.PracticeGenerationJob{}, false, records.ErrNotFound
		} else if err != nil {
			return k12.PracticeGenerationJob{}, false, err
		} else if version != job.SourceVersion {
			return k12.PracticeGenerationJob{}, false, records.ErrVersionConflict
		}
	case k12.PracticeGenerationSourceAccumulation:
		var version int
		var deletedAt sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT row_version,deleted_at
			FROM k12_accumulations WHERE record_id=? AND agent_name=?`,
			job.SourceID, job.AgentName,
		).Scan(&version, &deletedAt); err == sql.ErrNoRows || deletedAt.Valid {
			return k12.PracticeGenerationJob{}, false, records.ErrNotFound
		} else if err != nil {
			return k12.PracticeGenerationJob{}, false, err
		} else if version != job.SourceVersion {
			return k12.PracticeGenerationJob{}, false, records.ErrVersionConflict
		}
	}
	job.Status = k12.PracticeGenerationQueued
	job.ResultSetID = ""
	job.Attempt = 0
	job.FailureReason = ""
	job.RetiredAt = 0
	job.RetiredReason = ""
	if job.CreatedAt <= 0 {
		job.CreatedAt = nowUnix()
	}
	job.UpdatedAt = job.CreatedAt
	resultJSON, err := json.Marshal(job.ResultItemIDs)
	if err != nil {
		return k12.PracticeGenerationJob{}, false, err
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO k12_practice_generation_jobs(
		generation_job_id,agent_name,idempotency_key,request_digest,scope,
		source_kind,source_id,source_version,variants_per_source,difficulty,total,textbook,
		status,result_set_id,result_item_ids_json,deduplicated_count,failure_reason,
		source_mistake_id,source_mistake_summary,request_snapshot_json,route_snapshot_json,
		attempt,generation_output_json,generation_output_attempt,validation_output_json,
		validation_output_attempt,retired_at,retired_reason,created_at,updated_at
	) VALUES(
		?,?,?,?,?,?,?,?,?,?,?,?,?,'',?,0,'',?,?,?,?,0,'',0,'',0,0,'',?,?
	) ON CONFLICT DO NOTHING`,
		job.GenerationJobID, job.AgentName, job.IdempotencyKey, job.RequestDigest,
		job.Scope, job.SourceKind, job.SourceID, job.SourceVersion,
		job.VariantsPerSource, job.Difficulty, job.Total, job.Textbook, job.Status,
		string(resultJSON), job.SourceMistakeID, job.SourceSummary,
		job.RequestSnapshot, job.RouteSnapshot, job.CreatedAt, job.UpdatedAt,
	)
	if err != nil {
		return k12.PracticeGenerationJob{}, false, fmt.Errorf(
			"k12storage: persist shared practice job: %w", err,
		)
	}
	created, _ := inserted.RowsAffected()
	accepted, err := scanPracticeGenerationJob(tx.QueryRowContext(ctx,
		practiceGenerationJobSelect+` WHERE agent_name=? AND
			(idempotency_key=? OR (scope='single' AND source_kind=? AND
			 source_id=? AND source_version=?))
			ORDER BY CASE WHEN idempotency_key=? THEN 0 ELSE 1 END LIMIT 1`,
		job.AgentName, job.IdempotencyKey, job.SourceKind, job.SourceID,
		job.SourceVersion, job.IdempotencyKey,
	))
	if err != nil {
		return k12.PracticeGenerationJob{}, false, fmt.Errorf(
			"k12storage: read accepted shared practice job: %w", err,
		)
	}
	if accepted.SourceKind != job.SourceKind || accepted.SourceID != job.SourceID ||
		accepted.SourceVersion != job.SourceVersion ||
		accepted.RequestDigest != job.RequestDigest ||
		accepted.RequestSnapshot != job.RequestSnapshot ||
		accepted.RouteSnapshot != job.RouteSnapshot {
		return k12.PracticeGenerationJob{}, false, fmt.Errorf(
			"k12storage: shared practice source identity is bound to another request",
		)
	}
	if err := tx.Commit(); err != nil {
		return k12.PracticeGenerationJob{}, false, fmt.Errorf(
			"k12storage: commit shared practice job: %w", err,
		)
	}
	return accepted, created == 1, nil
}

// BeginCustomPaperGeneration durably accepts one custom-paper command before
// any external generation call. The immutable request/route snapshots are the
// only facts later retries may reuse; an existing exact command converges to
// the same receipt.
func (s *Store) BeginCustomPaperGeneration(
	ctx context.Context,
	job k12.PracticeGenerationJob,
) (k12.PracticeGenerationJob, bool, error) {
	if err := validatePracticeGenerationJob(job); err != nil {
		return k12.PracticeGenerationJob{}, false, err
	}
	if job.Scope != "week" && job.Scope != "unmastered" {
		return k12.PracticeGenerationJob{}, false,
			fmt.Errorf("k12storage: 自定义组卷 scope 非法 %q", job.Scope)
	}
	if strings.TrimSpace(job.RequestSnapshot) == "" ||
		strings.TrimSpace(job.RouteSnapshot) == "" ||
		strings.TrimSpace(job.RouteSnapshot) == "{}" {
		return k12.PracticeGenerationJob{}, false,
			fmt.Errorf("k12storage: 自定义组卷缺少冻结请求或路由快照")
	}
	if err := ensureAgentRegistered(ctx, s.db, job.AgentName); err != nil {
		return k12.PracticeGenerationJob{}, false, err
	}
	job.Status = k12.PracticeGenerationQueued
	if job.CreatedAt <= 0 {
		job.CreatedAt = nowUnix()
	}
	job.UpdatedAt = job.CreatedAt
	resultJSON := "[]"
	res, err := s.db.ExecContext(ctx, `INSERT INTO k12_practice_generation_jobs
		(generation_job_id, agent_name, idempotency_key, request_digest, scope,
		 variants_per_source, difficulty, total, textbook, status, result_set_id,
		 result_item_ids_json, deduplicated_count, failure_reason, source_mistake_id,
		 source_mistake_summary, request_snapshot_json, route_snapshot_json, attempt,
		 generation_output_json, generation_output_attempt, validation_output_json,
		 validation_output_attempt, retired_at, retired_reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, 0, '', '', '', ?, ?, 0,
		        '', 0, '', 0, 0, '', ?, ?)
		ON CONFLICT(agent_name,idempotency_key) DO NOTHING`,
		job.GenerationJobID, job.AgentName, job.IdempotencyKey,
		job.RequestDigest, job.Scope, job.VariantsPerSource,
		job.Difficulty, job.Total, job.Textbook, job.Status,
		resultJSON, job.RequestSnapshot, job.RouteSnapshot,
		job.CreatedAt, job.UpdatedAt,
	)
	if err != nil {
		return k12.PracticeGenerationJob{}, false,
			fmt.Errorf("k12storage: 接受自定义组卷任务: %w", err)
	}
	created, _ := res.RowsAffected()
	accepted, err := s.GetPracticeGenerationJob(
		ctx, job.AgentName, job.IdempotencyKey,
	)
	if err != nil {
		return k12.PracticeGenerationJob{}, false, err
	}
	if accepted.GenerationJobID != job.GenerationJobID ||
		accepted.RequestDigest != job.RequestDigest ||
		accepted.RequestSnapshot != job.RequestSnapshot ||
		accepted.RouteSnapshot != job.RouteSnapshot {
		return k12.PracticeGenerationJob{}, false,
			fmt.Errorf("k12storage: 自定义组卷幂等键绑定了不同请求或路由")
	}
	return accepted, created == 1, nil
}

// ListRecoverableSinglePracticeGenerations returns only durable active jobs.
// Startup coordinators replay this query; terminal jobs are never re-invoked.
func (s *Store) ListRecoverableSinglePracticeGenerations(
	ctx context.Context,
) ([]k12.PracticeGenerationJob, error) {
	rows, err := s.db.QueryContext(ctx, practiceGenerationJobSelect+`
		WHERE scope='single' AND retired_at=0
		  AND status IN ('queued','generating','validating')
		ORDER BY created_at, generation_job_id`)
	if err != nil {
		return nil, fmt.Errorf("k12storage: 列可恢复逐题 generation: %w", err)
	}
	defer rows.Close()
	var out []k12.PracticeGenerationJob
	for rows.Next() {
		job, scanErr := scanPracticeGenerationJob(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("k12storage: 扫描可恢复逐题 generation: %w", scanErr)
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("k12storage: 遍历可恢复逐题 generation: %w", err)
	}
	return out, nil
}

// BeginSinglePracticeGeneration atomically persists the single-source
// generation receipt and its draft placeholder. A duplicate click or a second
// command for the same active source converges to the existing pair.
func (s *Store) BeginSinglePracticeGeneration(
	ctx context.Context,
	rec *records.AgentRecord,
	expectedVersion int,
	job k12.PracticeGenerationJob,
) (
	stored *records.AgentRecord,
	accepted k12.PracticeGenerationJob,
	alreadyActive bool,
	err error,
) {
	if rec == nil {
		return nil, k12.PracticeGenerationJob{}, false, fmt.Errorf("k12storage: 逐题生成占位记录不可空")
	}
	if err := validatePracticeGenerationJob(job); err != nil {
		return nil, k12.PracticeGenerationJob{}, false, err
	}
	if job.Scope != "single" || strings.TrimSpace(job.SourceMistakeID) == "" ||
		strings.TrimSpace(job.SourceSummary) == "" || len(job.ResultItemIDs) != 1 {
		return nil, k12.PracticeGenerationJob{}, false,
			fmt.Errorf("k12storage: 逐题生成缺少 single/source/summary/result item 身份")
	}
	if rec.AgentName != job.AgentName || rec.Collection != k12.CollectionPracticeSet {
		return nil, k12.PracticeGenerationJob{}, false,
			fmt.Errorf("k12storage: 逐题生成 owner/collection 与占位记录不一致")
	}
	schema, err := s.registry.Get(k12.CollectionPracticeSet)
	if err != nil {
		return nil, k12.PracticeGenerationJob{}, false, err
	}
	mp, err := s.mapperFor(k12.CollectionPracticeSet)
	if err != nil {
		return nil, k12.PracticeGenerationJob{}, false, err
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(rec.Fields); err != nil {
			return nil, k12.PracticeGenerationJob{}, false,
				fmt.Errorf("%w: 记录集 %q: %v", records.ErrInvalidFields, rec.Collection, err)
		}
	}
	fields, err := k12.ParsePracticeSetFields(rec.Fields)
	if err != nil {
		return nil, k12.PracticeGenerationJob{}, false, err
	}
	matches := 0
	for _, item := range fields.Items {
		if item.GenerationJobID == job.GenerationJobID &&
			item.ItemID == job.ResultItemIDs[0] &&
			item.SourceMistakeSummary == job.SourceSummary &&
			item.GenerationStatus == k12.PracticeItemGenerationQueued {
			matches++
		}
	}
	if matches != 1 {
		return nil, k12.PracticeGenerationJob{}, false,
			fmt.Errorf("k12storage: 逐题 generation job 必须恰好对应一个 queued 占位项")
	}
	if err := ensureAgentRegistered(ctx, s.db, rec.AgentName); err != nil {
		return nil, k12.PracticeGenerationJob{}, false, err
	}

	created := expectedVersion < 0
	if created {
		rec.Status = schema.InitialStatus
		rec.SchemaVersion = schema.Version
		rec.DedupeKey = schema.DedupeKey(rec)
		if rec.RecordID == "" {
			rec.RecordID = idgen.NanoID()
		}
		now := nowUnix()
		rec.CreatedAt, rec.UpdatedAt, rec.Version = now, now, 0
		if rec.Tags == "" {
			rec.Tags = "[]"
		}
	} else if rec.RecordID == "" || rec.Status != k12.PracticeStatusDraft {
		return nil, k12.PracticeGenerationJob{}, false,
			fmt.Errorf("k12storage: 只能向既有 draft 练习集写逐题占位")
	}
	job.Status = k12.PracticeGenerationQueued
	job.ResultSetID = rec.RecordID
	resultJSON, err := json.Marshal(job.ResultItemIDs)
	if err != nil {
		return nil, k12.PracticeGenerationJob{}, false, err
	}
	domainVals, err := mp.encode(rec.Fields)
	if err != nil {
		return nil, k12.PracticeGenerationJob{}, false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, k12.PracticeGenerationJob{}, false,
			fmt.Errorf("k12storage: 开启逐题生成事务: %w", err)
	}
	defer tx.Rollback()

	inserted, err := tx.ExecContext(ctx, `INSERT INTO k12_practice_generation_jobs
		(generation_job_id, agent_name, idempotency_key, request_digest, scope,
		 variants_per_source, difficulty, total, textbook, status, result_set_id,
		 result_item_ids_json, deduplicated_count, failure_reason, source_mistake_id,
		 source_mistake_summary, request_snapshot_json, route_snapshot_json, attempt,
		 generation_output_json, generation_output_attempt, validation_output_json,
		 validation_output_attempt, retired_at, retired_reason, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?)
		ON CONFLICT DO NOTHING`,
		job.GenerationJobID, job.AgentName, job.IdempotencyKey, job.RequestDigest, job.Scope,
		job.VariantsPerSource, job.Difficulty, job.Total, job.Textbook, job.Status,
		job.ResultSetID, string(resultJSON), job.DeduplicatedCount, job.SourceMistakeID,
		job.SourceSummary, job.RequestSnapshot, job.RouteSnapshot, job.Attempt,
		job.GenerationOutput, job.OutputAttempt, job.ValidationOutput, job.ValidationAttempt,
		job.CreatedAt, job.UpdatedAt)
	if err != nil {
		return nil, k12.PracticeGenerationJob{}, false,
			fmt.Errorf("k12storage: 写逐题生成任务: %w", err)
	}
	if n, _ := inserted.RowsAffected(); n == 0 {
		existing, scanErr := scanPracticeGenerationJob(tx.QueryRowContext(ctx,
			practiceGenerationJobSelect+` WHERE agent_name = ? AND
				(idempotency_key = ? OR
				 (scope = 'single' AND source_mistake_id = ? AND retired_at = 0
				  AND status IN ('queued','generating','validating')))
			 ORDER BY CASE WHEN idempotency_key = ? THEN 0 ELSE 1 END LIMIT 1`,
			job.AgentName, job.IdempotencyKey, job.SourceMistakeID, job.IdempotencyKey))
		if scanErr != nil {
			return nil, k12.PracticeGenerationJob{}, false,
				fmt.Errorf("k12storage: 回查逐题 active generation: %w", scanErr)
		}
		if existing.IdempotencyKey == job.IdempotencyKey &&
			existing.RequestDigest != job.RequestDigest {
			return nil, k12.PracticeGenerationJob{}, false,
				fmt.Errorf("k12storage: 幂等键 %q 已绑定其他逐题请求", job.IdempotencyKey)
		}
		if existing.SourceMistakeID != job.SourceMistakeID {
			return nil, k12.PracticeGenerationJob{}, false,
				fmt.Errorf("k12storage: 逐题 active generation 唯一约束冲突")
		}
		if err := tx.Rollback(); err != nil {
			return nil, k12.PracticeGenerationJob{}, false, err
		}
		existingSet, getErr := s.Get(ctx, existing.ResultSetID)
		return existingSet, existing, true, getErr
	}

	if created {
		cols := mp.domainCols()
		q := fmt.Sprintf(`INSERT INTO %s (%s, %s) VALUES (%s)
			ON CONFLICT(agent_name, dedupe_key) DO NOTHING`, mp.table(), baseCols,
			strings.Join(cols, ", "), placeholders(11+len(cols)))
		args := append([]any{
			rec.RecordID, rec.AgentName, rec.SchemaVersion, rec.Status, rec.DedupeKey,
			rec.Tags, rec.DueAt, rec.SourceSession, rec.Version, rec.CreatedAt, rec.UpdatedAt,
		}, domainVals...)
		res, execErr := tx.ExecContext(ctx, q, args...)
		if execErr != nil {
			return nil, k12.PracticeGenerationJob{}, false,
				fmt.Errorf("k12storage: 创建逐题生成练习集: %w", execErr)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, k12.PracticeGenerationJob{}, false, records.ErrVersionConflict
		}
	} else {
		cols := mp.domainCols()
		assigns := make([]string, 0, len(cols))
		for _, col := range cols {
			assigns = append(assigns, col+" = ?")
		}
		q := fmt.Sprintf(`UPDATE %s SET %s, version=version+1, updated_at=?
			WHERE record_id=? AND agent_name=? AND status=? AND version=?`,
			mp.table(), strings.Join(assigns, ", "))
		args := append([]any{}, domainVals...)
		args = append(args, nowUnix(), rec.RecordID, rec.AgentName,
			k12.PracticeStatusDraft, expectedVersion)
		res, execErr := tx.ExecContext(ctx, q, args...)
		if execErr != nil {
			return nil, k12.PracticeGenerationJob{}, false,
				fmt.Errorf("k12storage: 写逐题生成占位: %w", execErr)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, k12.PracticeGenerationJob{}, false, records.ErrVersionConflict
		}
	}
	if err := mp.syncChildren(ctx, tx, rec.RecordID, rec.Fields); err != nil {
		return nil, k12.PracticeGenerationJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, k12.PracticeGenerationJob{}, false,
			fmt.Errorf("k12storage: 提交逐题 generation/placeholder: %w", err)
	}
	got, err := s.Get(ctx, rec.RecordID)
	if err != nil {
		return nil, k12.PracticeGenerationJob{}, false, err
	}
	return got, job, false, nil
}

func validSingleGenerationTransition(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case k12.PracticeGenerationQueued:
		return to == k12.PracticeGenerationGenerating ||
			to == k12.PracticeGenerationFailed
	case k12.PracticeGenerationGenerating:
		return to == k12.PracticeGenerationValidating ||
			to == k12.PracticeGenerationFailed
	case k12.PracticeGenerationValidating:
		return to == k12.PracticeGenerationCommitted ||
			to == k12.PracticeGenerationFailed ||
			to == k12.PracticeGenerationGenerating
	case k12.PracticeGenerationFailed:
		return to == k12.PracticeGenerationQueued
	default:
		return false
	}
}

// AdvancePracticeGenerationJob 推进 job-only 共享任务；正式题目只能由
// CommitPracticeGeneration 的同一事务写入。
func (s *Store) AdvancePracticeGenerationJob(
	ctx context.Context,
	agentName, generationJobID, status string,
	attempt int,
	failureReason string,
) (k12.PracticeGenerationJob, error) {
	agentName = strings.TrimSpace(agentName)
	generationJobID = strings.TrimSpace(generationJobID)
	failureReason = strings.TrimSpace(failureReason)
	if status == k12.PracticeGenerationCommitted {
		return k12.PracticeGenerationJob{}, fmt.Errorf(
			"k12storage: committed requires atomic practice item commit",
		)
	}
	current, err := s.GetPracticeGenerationJobByID(ctx, agentName, generationJobID)
	if err != nil {
		return k12.PracticeGenerationJob{}, err
	}
	if current.Scope != "single" || current.SourceKind == "" ||
		current.SourceID == "" || current.RetiredAt != 0 {
		return k12.PracticeGenerationJob{}, fmt.Errorf(
			"k12storage: shared practice job identity invalid or retired",
		)
	}
	if !validSingleGenerationTransition(current.Status, status) {
		return k12.PracticeGenerationJob{}, fmt.Errorf(
			"k12storage: invalid shared practice transition %s->%s",
			current.Status, status,
		)
	}
	if attempt < current.Attempt {
		return k12.PracticeGenerationJob{}, fmt.Errorf(
			"k12storage: shared practice attempt regressed %d->%d",
			current.Attempt, attempt,
		)
	}
	if status == k12.PracticeGenerationFailed && failureReason == "" {
		return k12.PracticeGenerationJob{}, fmt.Errorf(
			"k12storage: failed shared practice job requires a reason",
		)
	}
	if status != k12.PracticeGenerationFailed {
		failureReason = ""
	}
	res, err := s.db.ExecContext(ctx, `UPDATE k12_practice_generation_jobs
		SET status=?,attempt=?,failure_reason=?,updated_at=?
		WHERE agent_name=? AND generation_job_id=? AND scope='single'
		  AND source_kind=? AND source_id=? AND source_version=?
		  AND status=? AND attempt=? AND retired_at=0`,
		status, attempt, failureReason, nowUnix(), agentName, generationJobID,
		current.SourceKind, current.SourceID, current.SourceVersion,
		current.Status, current.Attempt,
	)
	if err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf(
			"k12storage: advance shared practice job: %w", err,
		)
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		latest, getErr := s.GetPracticeGenerationJobByID(ctx, agentName, generationJobID)
		if getErr == nil && latest.Status == status && latest.Attempt == attempt &&
			latest.FailureReason == failureReason {
			return latest, nil
		}
		return k12.PracticeGenerationJob{}, records.ErrVersionConflict
	}
	return s.GetPracticeGenerationJobByID(ctx, agentName, generationJobID)
}

// ReactivatePracticeGenerationJob 复用同一失败或已移除任务。已移除任务从
// validating 检查点恢复，因此不会重新取得外部调用发送权。
func (s *Store) ReactivatePracticeGenerationJob(
	ctx context.Context,
	agentName, generationJobID string,
) (k12.PracticeGenerationJob, error) {
	current, err := s.GetPracticeGenerationJobByID(
		ctx, strings.TrimSpace(agentName), strings.TrimSpace(generationJobID),
	)
	if err != nil {
		return k12.PracticeGenerationJob{}, err
	}
	status := ""
	switch {
	case current.Status == k12.PracticeGenerationFailed && current.RetiredAt == 0:
		status = k12.PracticeGenerationQueued
	case current.Status == k12.PracticeGenerationCommitted && current.RetiredAt != 0:
		status = k12.PracticeGenerationValidating
	case current.RetiredAt == 0:
		return current, nil
	default:
		return k12.PracticeGenerationJob{}, records.ErrIllegalTransition
	}
	res, err := s.db.ExecContext(ctx, `UPDATE k12_practice_generation_jobs
		SET status=?,failure_reason='',retired_at=0,retired_reason='',updated_at=?
		WHERE agent_name=? AND generation_job_id=? AND status=? AND retired_at=?`,
		status, nowUnix(), current.AgentName, current.GenerationJobID,
		current.Status, current.RetiredAt,
	)
	if err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf(
			"k12storage: reactivate shared practice job: %w", err,
		)
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return k12.PracticeGenerationJob{}, records.ErrVersionConflict
	}
	return s.GetPracticeGenerationJobByID(ctx, current.AgentName, current.GenerationJobID)
}

func itemGenerationStatusForJob(status string) (string, bool) {
	switch status {
	case k12.PracticeGenerationQueued:
		return k12.PracticeItemGenerationQueued, true
	case k12.PracticeGenerationGenerating:
		return k12.PracticeItemGenerationGenerating, true
	case k12.PracticeGenerationValidating:
		return k12.PracticeItemGenerationValidating, true
	case k12.PracticeGenerationCommitted:
		return k12.PracticeItemGenerationReady, true
	case k12.PracticeGenerationFailed:
		return k12.PracticeItemGenerationFailed, true
	default:
		return "", false
	}
}

// AdvanceSinglePracticeGeneration updates the durable job and its unique
// placeholder in one transaction. Committed is accepted only with a complete,
// independently verified item; retry resets the same failed job/item identity.
func (s *Store) AdvanceSinglePracticeGeneration(
	ctx context.Context,
	agentName, generationJobID, status string,
	attempt int,
	item k12.PracticeItem,
	failureReason string,
) (k12.PracticeGenerationJob, error) {
	itemStatus, ok := itemGenerationStatusForJob(status)
	if !ok {
		return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 逐题 generation 状态非法 %q", status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 开启逐题推进事务: %w", err)
	}
	defer tx.Rollback()
	current, err := scanPracticeGenerationJob(tx.QueryRowContext(ctx,
		practiceGenerationJobSelect+` WHERE agent_name=? AND generation_job_id=?`,
		agentName, generationJobID))
	if err == sql.ErrNoRows {
		return k12.PracticeGenerationJob{}, records.ErrNotFound
	}
	if err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 读取逐题 generation: %w", err)
	}
	if current.Scope != "single" || current.RetiredAt != 0 ||
		len(current.ResultItemIDs) != 1 || current.ResultSetID == "" {
		return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 逐题 generation 身份无效或已退出")
	}
	if !validSingleGenerationTransition(current.Status, status) {
		return k12.PracticeGenerationJob{}, fmt.Errorf(
			"k12storage: 逐题 generation 非法转移 %s→%s", current.Status, status,
		)
	}
	if attempt < current.Attempt {
		return k12.PracticeGenerationJob{}, fmt.Errorf(
			"k12storage: 逐题 generation attempt 倒退 %d→%d", current.Attempt, attempt,
		)
	}
	itemID := current.ResultItemIDs[0]
	if status == k12.PracticeGenerationCommitted {
		if item.ItemID != itemID || item.GenerationJobID != current.GenerationJobID ||
			item.SourceMistakeSummary != current.SourceSummary ||
			item.GenerationStatus != k12.PracticeItemGenerationReady ||
			!k12.PracticeItemPublishable(item) {
			return k12.PracticeGenerationJob{}, fmt.Errorf(
				"k12storage: committed 逐题 generation 缺少同一 identity 的 ready+verified 完整题答",
			)
		}
		if _, err := k12.NewPracticeSetRecord(agentName, "", k12.PracticeSetFields{
			SourceKind: k12.PracticeSourceSingleVariant,
			Title:      "逐题生成验证",
			Items:      []k12.PracticeItem{item},
		}); err != nil {
			return k12.PracticeGenerationJob{}, err
		}
		res, err := tx.ExecContext(ctx, `UPDATE k12_practice_set_items SET
			source_problem_id=?, source_mistake_summary=?, subject=?, added_via=?,
			generation_status=?, question_markdown=?, expected_answer_markdown=?,
			verification_status=?, verification_evidence=?, blocked_reason='',
			generation_job_id=?, variant_index=?, requested_difficulty=?, actual_difficulty=?
			WHERE set_record_id=? AND item_id=? AND generation_job_id=?`,
			item.SourceProblemID, item.SourceMistakeSummary, item.Subject, item.AddedVia,
			item.GenerationStatus, item.QuestionMarkdown, item.ExpectedAnswerMarkdown,
			item.VerificationStatus, item.VerificationEvidence, item.GenerationJobID,
			item.VariantIndex, item.RequestedDifficulty, item.ActualDifficulty,
			current.ResultSetID, itemID, current.GenerationJobID)
		if err != nil {
			return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 提交 ready 逐题项: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 逐题占位项不存在或不唯一")
		}
	} else {
		blockedReason := ""
		if status == k12.PracticeGenerationFailed {
			blockedReason = strings.TrimSpace(failureReason)
			if blockedReason == "" {
				return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: failed generation 必须记录原因")
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE k12_practice_set_items
			SET generation_status=?, blocked_reason=?
			WHERE set_record_id=? AND item_id=? AND generation_job_id=?`,
			itemStatus, blockedReason, current.ResultSetID, itemID, current.GenerationJobID)
		if err != nil {
			return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 推进逐题占位状态: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 逐题占位项不存在或不唯一")
		}
	}
	now := nowUnix()
	if _, err := tx.ExecContext(ctx, `UPDATE k12_practice_generation_jobs
		SET status=?, attempt=?, failure_reason=?, updated_at=?
		WHERE agent_name=? AND generation_job_id=?`,
		status, attempt, strings.TrimSpace(failureReason), now, agentName, generationJobID); err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 推进逐题 generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_practice_sets
		SET version=version+1, updated_at=?
		WHERE record_id=? AND agent_name=? AND status=?`,
		now, current.ResultSetID, agentName, k12.PracticeStatusDraft); err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 推进逐题练习集版本: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 提交逐题推进事务: %w", err)
	}
	return s.GetPracticeGenerationJobByID(ctx, agentName, generationJobID)
}

// SaveSinglePracticeGenerationOutput establishes the durable after-response
// checkpoint for a single model attempt. The response is immutable for that
// attempt: an exact replay converges, while changed bytes or an attempt/status
// mismatch fail closed.
func (s *Store) SaveSinglePracticeGenerationOutput(
	ctx context.Context,
	agentName, generationJobID string,
	attempt int,
	outputJSON string,
) (k12.PracticeGenerationJob, error) {
	return s.saveSinglePracticeStageOutput(
		ctx, agentName, generationJobID, attempt, outputJSON,
		k12.PracticeGenerationGenerating,
		"generation_output_json", "generation_output_attempt",
	)
}

// SaveSinglePracticeValidationOutput is the corresponding immutable
// after-response checkpoint for the independent validator invocation.
func (s *Store) SaveSinglePracticeValidationOutput(
	ctx context.Context,
	agentName, generationJobID string,
	attempt int,
	outputJSON string,
) (k12.PracticeGenerationJob, error) {
	return s.saveSinglePracticeStageOutput(
		ctx, agentName, generationJobID, attempt, outputJSON,
		k12.PracticeGenerationValidating,
		"validation_output_json", "validation_output_attempt",
	)
}

func (s *Store) saveSinglePracticeStageOutput(
	ctx context.Context,
	agentName, generationJobID string,
	attempt int,
	outputJSON, expectedStatus, outputColumn, attemptColumn string,
) (k12.PracticeGenerationJob, error) {
	agentName = strings.TrimSpace(agentName)
	generationJobID = strings.TrimSpace(generationJobID)
	outputJSON = strings.TrimSpace(outputJSON)
	if agentName == "" || generationJobID == "" || attempt < 1 || outputJSON == "" {
		return k12.PracticeGenerationJob{}, fmt.Errorf(
			"k12storage: 逐题 generation 输出检查点缺少 owner/job/attempt/output",
		)
	}
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE k12_practice_generation_jobs
		SET %s=?, %s=?, updated_at=?
		WHERE agent_name=? AND generation_job_id=? AND scope='single'
		  AND retired_at=0 AND status=? AND attempt=?
		  AND (%s='' OR %s<? OR (%s=? AND %s=?))`,
		outputColumn, attemptColumn, outputColumn, attemptColumn,
		attemptColumn, outputColumn),
		outputJSON, attempt, nowUnix(), agentName, generationJobID,
		expectedStatus, attempt, attempt, attempt, outputJSON,
	)
	if err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf(
			"k12storage: 保存逐题 generation 输出检查点: %w", err,
		)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return s.GetPracticeGenerationJobByID(ctx, agentName, generationJobID)
	}
	current, getErr := s.GetPracticeGenerationJobByID(ctx, agentName, generationJobID)
	if getErr != nil {
		return k12.PracticeGenerationJob{}, getErr
	}
	if current.Scope != "single" || current.RetiredAt != 0 ||
		current.Status != expectedStatus ||
		current.Attempt != attempt {
		return k12.PracticeGenerationJob{}, fmt.Errorf(
			"%w: 逐题 generation 输出 attempt/status 不匹配",
			records.ErrIllegalTransition,
		)
	}
	currentOutput, currentAttempt := current.GenerationOutput, current.OutputAttempt
	if expectedStatus == k12.PracticeGenerationValidating {
		currentOutput, currentAttempt = current.ValidationOutput, current.ValidationAttempt
	}
	if currentAttempt == attempt && currentOutput == outputJSON {
		return current, nil
	}
	return k12.PracticeGenerationJob{}, fmt.Errorf(
		"%w: job=%s attempt=%d",
		ErrPracticeGenerationOutputConflict, generationJobID, attempt,
	)
}

// RecordPracticeGenerationFailure 只记录命令失败终态，不创建/修改练习篮。
// committed 收据不可被失败重试覆盖；同 key 不同摘要不可篡改原请求快照。
func (s *Store) RecordPracticeGenerationFailure(ctx context.Context, job k12.PracticeGenerationJob) error {
	if err := validatePracticeGenerationJob(job); err != nil {
		return err
	}
	if err := ensureAgentRegistered(ctx, s.db, job.AgentName); err != nil {
		return err
	}
	job.Status = k12.PracticeGenerationFailed
	resultJSON, _ := json.Marshal([]string{})
	res, err := s.db.ExecContext(ctx, `INSERT INTO k12_practice_generation_jobs
        (generation_job_id, agent_name, idempotency_key, request_digest, scope,
         variants_per_source, difficulty, total, textbook, status, result_set_id,
         result_item_ids_json, deduplicated_count, failure_reason, source_mistake_id,
         source_mistake_summary, request_snapshot_json, route_snapshot_json, attempt,
         generation_output_json, generation_output_attempt, validation_output_json,
         validation_output_attempt, retired_at, retired_reason, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(agent_name, idempotency_key) DO UPDATE SET
          status=excluded.status, deduplicated_count=excluded.deduplicated_count,
          failure_reason=excluded.failure_reason, attempt=excluded.attempt,
          updated_at=excluded.updated_at
        WHERE k12_practice_generation_jobs.request_digest=excluded.request_digest
          AND k12_practice_generation_jobs.status!='committed'`,
		job.GenerationJobID, job.AgentName, job.IdempotencyKey, job.RequestDigest, job.Scope,
		job.VariantsPerSource, job.Difficulty, job.Total, job.Textbook, job.Status,
		string(resultJSON), job.DeduplicatedCount, job.FailureReason, job.SourceMistakeID,
		job.SourceSummary, job.RequestSnapshot, job.RouteSnapshot, job.Attempt,
		job.GenerationOutput, job.OutputAttempt, job.ValidationOutput, job.ValidationAttempt,
		job.RetiredAt, job.RetiredReason, job.CreatedAt, job.UpdatedAt)
	if err != nil {
		return fmt.Errorf("k12storage: 记录组卷失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		existing, getErr := s.GetPracticeGenerationJob(ctx, job.AgentName, job.IdempotencyKey)
		if getErr != nil {
			return getErr
		}
		if existing.RequestDigest != job.RequestDigest {
			return fmt.Errorf("k12storage: 幂等键 %q 已绑定其他组卷请求", job.IdempotencyKey)
		}
		if existing.Status == k12.PracticeGenerationCommitted {
			return nil
		}
	}
	return nil
}

// CommitPracticeGeneration 把完整生成结果与 committed job 收据放在同一 SQLite 事务。
// expectedVersion < 0 表示新建篮；否则对既有 draft 篮做 CAS 整批替换。任一步失败均零半篮。
// 返回 alreadyCommitted=true 表示并发/重放命中了既有 committed 结果。
func (s *Store) CommitPracticeGeneration(ctx context.Context, rec *records.AgentRecord, expectedVersion int,
	job k12.PracticeGenerationJob) (stored *records.AgentRecord, alreadyCommitted bool, err error) {
	if rec == nil {
		return nil, false, fmt.Errorf("k12storage: 组卷提交记录不可空")
	}
	if err := validatePracticeGenerationJob(job); err != nil {
		return nil, false, err
	}
	if rec.AgentName != job.AgentName || rec.Collection != k12.CollectionPracticeSet {
		return nil, false, fmt.Errorf("k12storage: 组卷结果 owner/collection 与任务不一致")
	}
	if job.Scope == "single" && job.SourceKind != "" {
		if len(job.ResultItemIDs) != 1 {
			return nil, false, fmt.Errorf(
				"k12storage: shared source commit requires exactly one result item",
			)
		}
		fields, parseErr := k12.ParsePracticeSetFields(rec.Fields)
		if parseErr != nil {
			return nil, false, parseErr
		}
		matches := 0
		for _, item := range fields.Items {
			if item.ItemID == job.ResultItemIDs[0] &&
				item.GenerationJobID == job.GenerationJobID &&
				item.GenerationStatus == k12.PracticeItemGenerationReady &&
				strings.TrimSpace(item.NormalizedContentHash) != "" &&
				k12.PracticeItemPublishable(item) {
				matches++
			}
		}
		if matches != 1 {
			return nil, false, fmt.Errorf(
				"k12storage: shared source commit requires one ready verified hashed item",
			)
		}
	}
	schema, err := s.registry.Get(k12.CollectionPracticeSet)
	if err != nil {
		return nil, false, err
	}
	mp, err := s.mapperFor(k12.CollectionPracticeSet)
	if err != nil {
		return nil, false, err
	}
	if err := ensureAgentRegistered(ctx, s.db, rec.AgentName); err != nil {
		return nil, false, err
	}
	resultJSON, err := json.Marshal(job.ResultItemIDs)
	if err != nil {
		return nil, false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("k12storage: 开启原子组卷事务: %w", err)
	}
	defer tx.Rollback()

	// 第一条事务语句即写：既抢占 SQLite 写锁，又以唯一键保留幂等串行点。
	queuedJSON := "[]"
	res, err := tx.ExecContext(ctx, `INSERT INTO k12_practice_generation_jobs
        (generation_job_id, agent_name, idempotency_key, request_digest, scope,
         variants_per_source, difficulty, total, textbook, status, result_set_id,
         result_item_ids_json, deduplicated_count, failure_reason, source_mistake_id,
         source_mistake_summary, request_snapshot_json, route_snapshot_json, attempt,
         generation_output_json, generation_output_attempt, validation_output_json,
         validation_output_attempt, retired_at, retired_reason, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, 0, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(agent_name, idempotency_key) DO NOTHING`,
		job.GenerationJobID, job.AgentName, job.IdempotencyKey, job.RequestDigest, job.Scope,
		job.VariantsPerSource, job.Difficulty, job.Total, job.Textbook,
		k12.PracticeGenerationValidating, queuedJSON, job.SourceMistakeID, job.SourceSummary,
		job.RequestSnapshot, job.RouteSnapshot, job.Attempt,
		job.GenerationOutput, job.OutputAttempt, job.ValidationOutput, job.ValidationAttempt,
		job.RetiredAt, job.RetiredReason,
		job.CreatedAt, job.UpdatedAt)
	if err != nil {
		return nil, false, fmt.Errorf("k12storage: 锁定组卷幂等键: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var digest, status, resultSetID string
		if err := tx.QueryRowContext(ctx, `SELECT request_digest, status, result_set_id
            FROM k12_practice_generation_jobs WHERE agent_name=? AND idempotency_key=?`,
			job.AgentName, job.IdempotencyKey).Scan(&digest, &status, &resultSetID); err != nil {
			return nil, false, fmt.Errorf("k12storage: 回查组卷幂等键: %w", err)
		}
		if digest != job.RequestDigest {
			return nil, false, fmt.Errorf("k12storage: 幂等键 %q 已绑定其他组卷请求", job.IdempotencyKey)
		}
		if status == k12.PracticeGenerationCommitted {
			if err := tx.Rollback(); err != nil {
				return nil, false, err
			}
			got, getErr := s.Get(ctx, resultSetID)
			return got, true, getErr
		}
		if _, err := tx.ExecContext(ctx, `UPDATE k12_practice_generation_jobs
            SET status=?, failure_reason='', updated_at=? WHERE agent_name=? AND idempotency_key=?`,
			k12.PracticeGenerationValidating, job.UpdatedAt, job.AgentName, job.IdempotencyKey); err != nil {
			return nil, false, fmt.Errorf("k12storage: 推进组卷重试: %w", err)
		}
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(rec.Fields); err != nil {
			return nil, false, fmt.Errorf("%w: 记录集 %q: %v", records.ErrInvalidFields, rec.Collection, err)
		}
	}
	domainVals, err := mp.encode(rec.Fields)
	if err != nil {
		return nil, false, err
	}

	created := expectedVersion < 0
	syncBasketChildren := true
	if created {
		if len(job.ResultItemIDs) == 0 {
			return nil, false, fmt.Errorf("k12storage: 不得为零新增组卷任务创建空练习篮")
		}
		rec.Status = schema.InitialStatus
		rec.SchemaVersion = schema.Version
		rec.DedupeKey = schema.DedupeKey(rec)
		if rec.RecordID == "" {
			rec.RecordID = idgen.NanoID()
		}
		now := nowUnix()
		rec.CreatedAt, rec.UpdatedAt, rec.Version = now, now, 0
		if rec.Tags == "" {
			rec.Tags = "[]"
		}
		cols := mp.domainCols()
		q := fmt.Sprintf(`INSERT INTO %s (%s, %s) VALUES (%s)
            ON CONFLICT(agent_name, dedupe_key) DO NOTHING`, mp.table(), baseCols,
			strings.Join(cols, ", "), placeholders(11+len(cols)))
		args := append([]any{rec.RecordID, rec.AgentName, rec.SchemaVersion, rec.Status, rec.DedupeKey,
			rec.Tags, rec.DueAt, rec.SourceSession, rec.Version, rec.CreatedAt, rec.UpdatedAt}, domainVals...)
		insert, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return nil, false, fmt.Errorf("k12storage: 创建组卷练习篮: %w", err)
		}
		if n, _ := insert.RowsAffected(); n == 0 {
			return nil, false, records.ErrVersionConflict
		}
	} else {
		if rec.RecordID == "" || rec.Status != k12.PracticeStatusDraft {
			return nil, false, fmt.Errorf("k12storage: 只能原子追加到既有 draft 练习篮")
		}
		if len(job.ResultItemIDs) == 0 {
			// 全部候选均被去重时，仍需在同一写事务提交 committed 收据，但不得仅为
			// “记录零新增”推进集合 version/updated_at。首条 job 写已持有 SQLite 写锁，
			// 此处按原 version 校验即可防止基于陈旧集合提交去重结论。
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_practice_sets
                    WHERE record_id=? AND agent_name=? AND status=? AND version=?`,
				rec.RecordID, rec.AgentName, k12.PracticeStatusDraft, expectedVersion).Scan(&exists); err != nil {
				return nil, false, fmt.Errorf("k12storage: 校验零新增组卷集合: %w", err)
			}
			if exists != 1 {
				return nil, false, records.ErrVersionConflict
			}
			syncBasketChildren = false
		} else {
			cols := mp.domainCols()
			assigns := make([]string, 0, len(cols))
			for _, col := range cols {
				assigns = append(assigns, col+" = ?")
			}
			q := fmt.Sprintf(`UPDATE %s SET %s, version=version+1, updated_at=?
            WHERE record_id=? AND agent_name=? AND status=? AND version=?`,
				mp.table(), strings.Join(assigns, ", "))
			args := append([]any{}, domainVals...)
			args = append(args, nowUnix(), rec.RecordID, rec.AgentName, k12.PracticeStatusDraft, expectedVersion)
			updated, err := tx.ExecContext(ctx, q, args...)
			if err != nil {
				return nil, false, fmt.Errorf("k12storage: 更新组卷练习篮: %w", err)
			}
			if n, _ := updated.RowsAffected(); n == 0 {
				return nil, false, records.ErrVersionConflict
			}
		}
	}
	if syncBasketChildren {
		if err := mp.syncChildren(ctx, tx, rec.RecordID, rec.Fields); err != nil {
			return nil, false, err
		}
	}
	updatedJob, err := tx.ExecContext(ctx, `UPDATE k12_practice_generation_jobs SET
        status=?, result_set_id=?, result_item_ids_json=?, deduplicated_count=?, failure_reason='', updated_at=?
        WHERE agent_name=? AND idempotency_key=? AND request_digest=?`,
		k12.PracticeGenerationCommitted, rec.RecordID, string(resultJSON), job.DeduplicatedCount, job.UpdatedAt,
		job.AgentName, job.IdempotencyKey, job.RequestDigest)
	if err != nil {
		return nil, false, fmt.Errorf("k12storage: 提交组卷收据: %w", err)
	}
	if changed, _ := updatedJob.RowsAffected(); changed != 1 {
		return nil, false, fmt.Errorf("k12storage: shared generation receipt was not uniquely committed")
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("k12storage: 提交原子组卷事务: %w", err)
	}
	got, err := s.Get(ctx, rec.RecordID)
	return got, false, err
}

func validatePracticeGenerationJob(job k12.PracticeGenerationJob) error {
	if strings.TrimSpace(job.GenerationJobID) == "" || strings.TrimSpace(job.AgentName) == "" ||
		strings.TrimSpace(job.IdempotencyKey) == "" || strings.TrimSpace(job.RequestDigest) == "" {
		return fmt.Errorf("k12storage: 组卷任务缺少 job_id / agent / idempotency_key / request_digest")
	}
	return nil
}
