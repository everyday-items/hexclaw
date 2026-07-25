package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// K12ParentGuideV33 separates the parent-facing teaching guide from solve and
// grade provider calls. Historical assessment receipts remain valid with a
// NULL parent_guide_invocation_id; new receipts can reference the independent
// immutable operation.
var K12ParentGuideV33 = Migration{
	Version:     33,
	Description: "v0.5.0 completed-homework 错题七项家长讲法独立调用账本",
	AtomicFunc:  migrateK12ParentGuideV33,
}

const k12ParentGuideInvocationV33DDL = `
CREATE TABLE k12_grading_item_invocations_v33 (
    item_invocation_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    problem_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    operation TEXT NOT NULL CHECK(operation IN ('solve','grade','parent_guide')),
    operation_attempt INTEGER NOT NULL CHECK(operation_attempt >= 1),
    request_digest TEXT NOT NULL CHECK(request_digest != ''),
    provider TEXT NOT NULL CHECK(provider != ''),
    model TEXT NOT NULL CHECK(model != ''),
    route_snapshot_json TEXT NOT NULL CHECK(route_snapshot_json != ''),
    status TEXT NOT NULL CHECK(status IN ('prepared','sent','succeeded','failed','outcome_unknown','reconciled')),
    result_digest TEXT NOT NULL DEFAULT '',
    result_json TEXT NOT NULL DEFAULT '',
    failure_class TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(job_id, problem_id, operation, operation_attempt),
    FOREIGN KEY(agent_name, job_id)
        REFERENCES k12_grading_jobs(agent_name, record_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name, problem_id)
        REFERENCES k12_problems(agent_name, problem_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name, attempt_id, problem_id)
        REFERENCES k12_attempts(agent_name, attempt_id, problem_id) ON DELETE CASCADE,
    CHECK(status != 'succeeded' OR (result_digest != '' AND result_json != '')),
    CHECK(status NOT IN ('failed','outcome_unknown') OR (failure_class != '' AND failure_code != ''))
);`

const k12ParentGuideAssessmentV33DDL = `
CREATE TABLE k12_grading_assessment_items_v33 (
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    problem_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    confirmed_version INTEGER NOT NULL CHECK(confirmed_version >= 1),
    input_digest TEXT NOT NULL CHECK(input_digest != ''),
    status TEXT NOT NULL CHECK(status IN
        ('correct','wrong','unanswered','answer_unclear','blank_solved','out_of_scope','untrusted')),
    result_json TEXT NOT NULL CHECK(result_json != ''),
    result_digest TEXT NOT NULL CHECK(result_digest != ''),
    solve_invocation_id TEXT,
    grade_invocation_id TEXT,
    parent_guide_invocation_id TEXT,
    projection_record_id TEXT NOT NULL DEFAULT '',
    projection_created INTEGER NOT NULL DEFAULT 0 CHECK(projection_created IN (0,1)),
    projection_status TEXT NOT NULL CHECK(projection_status = 'committed'),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(job_id, problem_id),
    FOREIGN KEY(agent_name, job_id)
        REFERENCES k12_grading_jobs(agent_name, record_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name, problem_id)
        REFERENCES k12_problems(agent_name, problem_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name, attempt_id, problem_id)
        REFERENCES k12_attempts(agent_name, attempt_id, problem_id) ON DELETE CASCADE,
    FOREIGN KEY(solve_invocation_id)
        REFERENCES k12_grading_item_invocations_v33(item_invocation_id),
    FOREIGN KEY(grade_invocation_id)
        REFERENCES k12_grading_item_invocations_v33(item_invocation_id),
    FOREIGN KEY(parent_guide_invocation_id)
        REFERENCES k12_grading_item_invocations_v33(item_invocation_id),
    CHECK(projection_created = 0 OR projection_record_id != ''),
    CHECK(parent_guide_invocation_id IS NULL OR status IN ('wrong','blank_solved')),
    CHECK(
        (status IN ('correct','wrong','untrusted') AND solve_invocation_id IS NOT NULL AND grade_invocation_id IS NOT NULL)
        OR (status = 'blank_solved' AND solve_invocation_id IS NOT NULL AND grade_invocation_id IS NULL)
        OR (status IN ('unanswered','answer_unclear') AND solve_invocation_id IS NULL AND grade_invocation_id IS NULL)
        OR (status = 'out_of_scope' AND grade_invocation_id IS NULL)
    )
);`

const k12ParentGuideV33PostDDL = `
ALTER TABLE k12_grading_item_invocations_v33 RENAME TO k12_grading_item_invocations;
ALTER TABLE k12_grading_assessment_items_v33 RENAME TO k12_grading_assessment_items;
CREATE INDEX idx_k12_grading_item_invocations_job
    ON k12_grading_item_invocations(agent_name, job_id, problem_id, operation, operation_attempt);
CREATE INDEX idx_k12_grading_item_invocations_status
    ON k12_grading_item_invocations(status, updated_at);
CREATE INDEX idx_k12_grading_assessment_items_job
    ON k12_grading_assessment_items(agent_name, job_id, problem_id);`

func migrateK12ParentGuideV33(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) (retErr error) {
	hasInvocations, err := tableExists(ctx, db, "k12_grading_item_invocations")
	if err != nil {
		return fmt.Errorf("检查 k12_grading_item_invocations: %w", err)
	}
	hasAssessments, err := tableExists(ctx, db, "k12_grading_assessment_items")
	if err != nil {
		return fmt.Errorf("检查 k12_grading_assessment_items: %w", err)
	}
	if !hasInvocations {
		if hasAssessments {
			return fmt.Errorf("k12_grading_assessment_items 存在但 k12_grading_item_invocations 缺失")
		}
		tx, txErr := db.BeginTx(ctx, nil)
		if txErr != nil {
			return txErr
		}
		defer tx.Rollback()
		if err := recordVersion(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	if !hasAssessments {
		return fmt.Errorf("k12_grading_item_invocations 存在但 k12_grading_assessment_items 缺失")
	}
	hasParentGuideColumn, err := columnExists(
		ctx,
		db,
		"k12_grading_assessment_items",
		"parent_guide_invocation_id",
	)
	if err != nil {
		return fmt.Errorf("检查 parent_guide_invocation_id: %w", err)
	}
	var invocationDDL string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master
		WHERE type='table' AND name='k12_grading_item_invocations'`).Scan(&invocationDDL); err != nil {
		return fmt.Errorf("读取 k12_grading_item_invocations DDL: %w", err)
	}
	supportsParentGuideOperation := strings.Contains(invocationDDL, "'parent_guide'")
	if hasParentGuideColumn && supportsParentGuideOperation {
		tx, txErr := db.BeginTx(ctx, nil)
		if txErr != nil {
			return txErr
		}
		defer tx.Rollback()
		if err := recordVersion(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("获取 V33 迁移连接: %w", err)
	}
	defer conn.Close()
	var foreignKeys int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("读取 V33 foreign_keys: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("关闭 V33 foreign_keys: %w", err)
	}
	defer func() {
		if foreignKeys != 0 {
			if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`); retErr == nil && err != nil {
				retErr = fmt.Errorf("恢复 V33 foreign_keys: %w", err)
			}
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 V33 事务: %w", err)
	}
	defer tx.Rollback()
	for _, table := range []string{
		"k12_grading_assessment_items_v33",
		"k12_grading_item_invocations_v33",
	} {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
			return fmt.Errorf("清理 V33 暂存表 %s: %w", table, err)
		}
	}
	if _, err := tx.ExecContext(ctx, k12ParentGuideInvocationV33DDL); err != nil {
		return fmt.Errorf("创建 V33 分题调用账本: %w", err)
	}
	if _, err := tx.ExecContext(ctx, k12ParentGuideAssessmentV33DDL); err != nil {
		return fmt.Errorf("创建 V33 批改回执: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_grading_item_invocations_v33 (
        item_invocation_id,agent_name,job_id,problem_id,attempt_id,
        operation,operation_attempt,request_digest,provider,model,route_snapshot_json,status,
        result_digest,result_json,failure_class,failure_code,created_at,updated_at
    )
    SELECT item_invocation_id,agent_name,job_id,problem_id,attempt_id,
        operation,operation_attempt,request_digest,provider,model,route_snapshot_json,status,
        result_digest,result_json,failure_class,failure_code,created_at,updated_at
    FROM k12_grading_item_invocations`); err != nil {
		return fmt.Errorf("复制 V30 分题调用证据: %w", err)
	}
	parentGuideSelect := "NULL"
	if hasParentGuideColumn {
		parentGuideSelect = "parent_guide_invocation_id"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_grading_assessment_items_v33 (
        agent_name,job_id,problem_id,attempt_id,confirmed_version,input_digest,status,
        result_json,result_digest,solve_invocation_id,grade_invocation_id,parent_guide_invocation_id,
        projection_record_id,projection_created,projection_status,created_at,updated_at
    )
    SELECT agent_name,job_id,problem_id,attempt_id,confirmed_version,input_digest,status,
        result_json,result_digest,solve_invocation_id,grade_invocation_id,`+parentGuideSelect+`,
        projection_record_id,projection_created,projection_status,created_at,updated_at
    FROM k12_grading_assessment_items`); err != nil {
		return fmt.Errorf("复制 V30 批改回执证据: %w", err)
	}
	for _, statement := range []string{
		`DROP TABLE k12_grading_assessment_items`,
		`DROP TABLE k12_grading_item_invocations`,
		k12ParentGuideV33PostDDL,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("切换 V33 账本结构: %w", err)
		}
	}
	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		return fmt.Errorf("检查 V33 外键: %w", err)
	}
	if violations != 0 {
		return fmt.Errorf("V33 外键检查发现 %d 个冲突", violations)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 V33 迁移: %w", err)
	}
	return nil
}
