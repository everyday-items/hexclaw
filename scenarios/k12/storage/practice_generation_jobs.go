package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// GetPracticeGenerationJob 按 owner + 幂等键读取正式组卷命令收据。
func (s *Store) GetPracticeGenerationJob(ctx context.Context, agentName, idempotencyKey string) (k12.PracticeGenerationJob, error) {
	var job k12.PracticeGenerationJob
	var resultJSON string
	err := s.db.QueryRowContext(ctx, `SELECT generation_job_id, agent_name, idempotency_key,
        request_digest, scope, variants_per_source, difficulty, total, textbook, status,
        result_set_id, result_item_ids_json, deduplicated_count, failure_reason, created_at, updated_at
        FROM k12_practice_generation_jobs WHERE agent_name = ? AND idempotency_key = ?`,
		agentName, idempotencyKey).Scan(
		&job.GenerationJobID, &job.AgentName, &job.IdempotencyKey, &job.RequestDigest,
		&job.Scope, &job.VariantsPerSource, &job.Difficulty, &job.Total, &job.Textbook,
		&job.Status, &job.ResultSetID, &resultJSON, &job.DeduplicatedCount,
		&job.FailureReason, &job.CreatedAt, &job.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return k12.PracticeGenerationJob{}, records.ErrNotFound
	}
	if err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 读组卷任务: %w", err)
	}
	if err := json.Unmarshal([]byte(resultJSON), &job.ResultItemIDs); err != nil {
		return k12.PracticeGenerationJob{}, fmt.Errorf("k12storage: 解析组卷结果 item ids: %w", err)
	}
	return job, nil
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
         result_item_ids_json, deduplicated_count, failure_reason, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?)
        ON CONFLICT(agent_name, idempotency_key) DO UPDATE SET
          status=excluded.status, deduplicated_count=excluded.deduplicated_count,
          failure_reason=excluded.failure_reason, updated_at=excluded.updated_at
        WHERE k12_practice_generation_jobs.request_digest=excluded.request_digest
          AND k12_practice_generation_jobs.status!='committed'`,
		job.GenerationJobID, job.AgentName, job.IdempotencyKey, job.RequestDigest, job.Scope,
		job.VariantsPerSource, job.Difficulty, job.Total, job.Textbook, job.Status,
		string(resultJSON), job.DeduplicatedCount, job.FailureReason, job.CreatedAt, job.UpdatedAt)
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
         result_item_ids_json, deduplicated_count, failure_reason, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, 0, '', ?, ?)
        ON CONFLICT(agent_name, idempotency_key) DO NOTHING`,
		job.GenerationJobID, job.AgentName, job.IdempotencyKey, job.RequestDigest, job.Scope,
		job.VariantsPerSource, job.Difficulty, job.Total, job.Textbook,
		k12.PracticeGenerationValidating, queuedJSON, job.CreatedAt, job.UpdatedAt)
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
	if _, err := tx.ExecContext(ctx, `UPDATE k12_practice_generation_jobs SET
        status=?, result_set_id=?, result_item_ids_json=?, deduplicated_count=?, failure_reason='', updated_at=?
        WHERE agent_name=? AND idempotency_key=? AND request_digest=?`,
		k12.PracticeGenerationCommitted, rec.RecordID, string(resultJSON), job.DeduplicatedCount, job.UpdatedAt,
		job.AgentName, job.IdempotencyKey, job.RequestDigest); err != nil {
		return nil, false, fmt.Errorf("k12storage: 提交组卷收据: %w", err)
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
