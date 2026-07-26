package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12ProblemProgressiveV48 replaces the one-row-per-problem assessment lock
// with immutable input revisions and adds durable skip receipts. One partial
// unique index remains the database authority for the current projection.
var K12ProblemProgressiveV48 = Migration{
	Version:     48,
	Description: "BUG-20260726-031 K12 分题渐进回执、跳过证据与 current CAS",
	AtomicFunc:  migrateK12ProblemProgressiveV48,
}

const k12ProblemProgressiveAssessmentV48DDL = `
CREATE TABLE k12_grading_assessment_items_v48 (
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    problem_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    confirmed_version INTEGER NOT NULL CHECK(confirmed_version >= 1),
    input_revision INTEGER NOT NULL DEFAULT 1 CHECK(input_revision >= 1),
    published_revision INTEGER NOT NULL DEFAULT 1 CHECK(published_revision >= 1),
    current_disposition TEXT NOT NULL DEFAULT 'current'
        CHECK(current_disposition IN ('current','superseded')),
    structure_version INTEGER NOT NULL DEFAULT 1 CHECK(structure_version >= 1),
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
    PRIMARY KEY(job_id, problem_id, input_revision),
    UNIQUE(job_id, problem_id, published_revision),
    FOREIGN KEY(agent_name, job_id)
        REFERENCES k12_grading_jobs(agent_name, record_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name, problem_id)
        REFERENCES k12_problems(agent_name, problem_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name, attempt_id, problem_id)
        REFERENCES k12_attempts(agent_name, attempt_id, problem_id) ON DELETE CASCADE,
    FOREIGN KEY(solve_invocation_id)
        REFERENCES k12_grading_item_invocations(item_invocation_id),
    FOREIGN KEY(grade_invocation_id)
        REFERENCES k12_grading_item_invocations(item_invocation_id),
    FOREIGN KEY(parent_guide_invocation_id)
        REFERENCES k12_grading_item_invocations(item_invocation_id),
    CHECK(projection_created = 0 OR projection_record_id != ''),
    CHECK(parent_guide_invocation_id IS NULL OR status IN ('wrong','blank_solved')),
    CHECK(
        (status IN ('correct','wrong','untrusted') AND
            solve_invocation_id IS NOT NULL AND grade_invocation_id IS NOT NULL)
        OR (status = 'blank_solved' AND
            solve_invocation_id IS NOT NULL AND grade_invocation_id IS NULL)
        OR (status IN ('unanswered','answer_unclear') AND
            solve_invocation_id IS NULL AND grade_invocation_id IS NULL)
        OR (status = 'out_of_scope' AND grade_invocation_id IS NULL)
    )
);`

const k12ProblemSkipReceiptsV48DDL = `
CREATE TABLE k12_problem_skip_receipts (
    skip_receipt_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    problem_id TEXT NOT NULL,
    structure_version INTEGER NOT NULL CHECK(structure_version >= 1),
    input_revision INTEGER NOT NULL CHECK(input_revision >= 1),
    result_digest TEXT NOT NULL CHECK(result_digest != ''),
    current_disposition TEXT NOT NULL DEFAULT 'current'
        CHECK(current_disposition IN ('current','superseded')),
    published_revision INTEGER NOT NULL CHECK(published_revision >= 1),
    superseded_at INTEGER NOT NULL DEFAULT 0 CHECK(superseded_at >= 0),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(job_id, problem_id, input_revision),
    UNIQUE(job_id, problem_id, published_revision),
    FOREIGN KEY(agent_name, job_id)
        REFERENCES k12_grading_jobs(agent_name, record_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name, problem_id)
        REFERENCES k12_problems(agent_name, problem_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX idx_k12_problem_skip_receipts_current
    ON k12_problem_skip_receipts(agent_name,job_id,problem_id)
    WHERE current_disposition='current';
CREATE INDEX idx_k12_problem_skip_receipts_job
    ON k12_problem_skip_receipts(agent_name,job_id,problem_id,published_revision);`

func migrateK12ProblemProgressiveV48(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	hasAssessments, err := tableExists(ctx, db, "k12_grading_assessment_items")
	if err != nil {
		return fmt.Errorf("检查 V48 批改回执: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 V48 事务: %w", err)
	}
	defer tx.Rollback()
	if !hasAssessments {
		if err := recordVersion(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS k12_grading_assessment_items_v48`); err != nil {
		return fmt.Errorf("清理 V48 暂存回执: %w", err)
	}
	if _, err := tx.ExecContext(ctx, k12ProblemProgressiveAssessmentV48DDL); err != nil {
		return fmt.Errorf("创建 V48 渐进回执: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_grading_assessment_items_v48 (
        agent_name,job_id,problem_id,attempt_id,confirmed_version,
        input_revision,published_revision,current_disposition,structure_version,
        input_digest,status,result_json,result_digest,solve_invocation_id,grade_invocation_id,
        parent_guide_invocation_id,projection_record_id,projection_created,projection_status,
        created_at,updated_at
    )
    SELECT agent_name,job_id,problem_id,attempt_id,confirmed_version,
        confirmed_version,1,'current',1,
        input_digest,status,result_json,result_digest,solve_invocation_id,grade_invocation_id,
        parent_guide_invocation_id,projection_record_id,projection_created,projection_status,
        created_at,updated_at
    FROM k12_grading_assessment_items`); err != nil {
		return fmt.Errorf("复制 V48 历史回执: %w", err)
	}
	for _, statement := range []string{
		`DROP TABLE k12_grading_assessment_items`,
		`ALTER TABLE k12_grading_assessment_items_v48 RENAME TO k12_grading_assessment_items`,
		`CREATE INDEX idx_k12_grading_assessment_items_job
            ON k12_grading_assessment_items(agent_name,job_id,problem_id,published_revision)`,
		`CREATE UNIQUE INDEX idx_k12_grading_assessment_items_current
            ON k12_grading_assessment_items(agent_name,job_id,problem_id)
            WHERE current_disposition='current'`,
		k12ProblemSkipReceiptsV48DDL,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("切换 V48 渐进结构: %w", err)
		}
	}
	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).
		Scan(&violations); err != nil {
		return fmt.Errorf("检查 V48 外键: %w", err)
	}
	if violations != 0 {
		return fmt.Errorf("V48 外键检查发现 %d 个冲突", violations)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 V48 迁移: %w", err)
	}
	return nil
}
