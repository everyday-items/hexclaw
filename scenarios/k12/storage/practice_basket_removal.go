package k12storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// RemovePracticeItemAndRetireGeneration 在一个 SQLite 事务内移除草稿篮题目，
// 同步退出逐题生成任务，或把已提交的积累默写推进为可再次加入状态。
func (s *Store) RemovePracticeItemAndRetireGeneration(
	ctx context.Context,
	agentName, setRecordID, itemID, generationJobID, fieldsJSON string,
	expectedVersion int,
) error {
	agentName = strings.TrimSpace(agentName)
	setRecordID = strings.TrimSpace(setRecordID)
	itemID = strings.TrimSpace(itemID)
	generationJobID = strings.TrimSpace(generationJobID)
	if agentName == "" || setRecordID == "" || itemID == "" {
		return fmt.Errorf("k12storage: 移除练习项缺少 owner/set/item")
	}
	cur, err := s.Get(ctx, setRecordID)
	if err != nil {
		return err
	}
	if cur.AgentName != agentName || cur.Collection != k12.CollectionPracticeSet {
		return records.ErrNotFound
	}
	if cur.Status != k12.PracticeStatusDraft {
		return fmt.Errorf("k12storage: 只有 draft 练习集可移除题目")
	}
	schema, err := s.registry.Get(cur.Collection)
	if err != nil {
		return err
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(fieldsJSON); err != nil {
			return fmt.Errorf(
				"%w: 记录集 %q: %v",
				records.ErrInvalidFields, cur.Collection, err,
			)
		}
	}
	mp := practiceSetMapper{}
	domainVals, err := mp.encode(fieldsJSON)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("k12storage: 开启练习项移除事务: %w", err)
	}
	defer tx.Rollback()

	cols := mp.domainCols()
	assigns := make([]string, 0, len(cols))
	for _, column := range cols {
		assigns = append(assigns, column+" = ?")
	}
	now := nowUnix()
	query := fmt.Sprintf(
		`UPDATE %s SET %s, version=version+1, updated_at=?
		 WHERE record_id=? AND agent_name=? AND status=? AND version=?`,
		mp.table(), strings.Join(assigns, ", "),
	)
	args := append([]any{}, domainVals...)
	args = append(
		args, now, setRecordID, agentName,
		k12.PracticeStatusDraft, expectedVersion,
	)
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("k12storage: 更新移除后的练习集: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return records.ErrVersionConflict
	}

	if generationJobID != "" {
		job, err := scanPracticeGenerationJob(tx.QueryRowContext(
			ctx,
			practiceGenerationJobSelect+
				` WHERE agent_name=? AND generation_job_id=?`,
			agentName, generationJobID,
		))
		if err == sql.ErrNoRows {
			return records.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("k12storage: 读取待退出逐题 generation: %w", err)
		}
		if job.Scope != "single" || job.ResultSetID != setRecordID ||
			len(job.ResultItemIDs) != 1 || job.ResultItemIDs[0] != itemID ||
			job.RetiredAt != 0 {
			return fmt.Errorf("k12storage: 练习项与逐题 generation 身份不一致或已退出")
		}
		status := job.Status
		switch status {
		case k12.PracticeGenerationQueued,
			k12.PracticeGenerationGenerating,
			k12.PracticeGenerationValidating:
			status = k12.PracticeGenerationCancelled
		}
		res, err = tx.ExecContext(ctx, `UPDATE k12_practice_generation_jobs
			SET status=?, retired_at=?, retired_reason='removed', updated_at=?
			WHERE agent_name=? AND generation_job_id=? AND scope='single'
			  AND retired_at=0`,
			status, now, now, agentName, generationJobID,
		)
		if err != nil {
			return fmt.Errorf("k12storage: 退出逐题 generation: %w", err)
		}
		if affected, _ := res.RowsAffected(); affected != 1 {
			return fmt.Errorf("k12storage: 逐题 generation 未被唯一退出")
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_accumulation_dictation_generations
		SET status='re_add', practice_item_id='', failure_reason='', updated_at=?
		WHERE agent_name=? AND practice_item_id=? AND status='committed'`,
		now, agentName, itemID,
	); err != nil {
		return fmt.Errorf("k12storage: transition accumulation dictation to re-add: %w", err)
	}
	if err := mp.syncChildren(ctx, tx, setRecordID, fieldsJSON); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("k12storage: 提交练习项移除事务: %w", err)
	}
	return nil
}
