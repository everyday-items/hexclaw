package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// K12CorrectWithProcessIssueV77 扩展终态批改状态，
// 同时保持 V48 的修订、处置、索引与外键契约不变。
var K12CorrectWithProcessIssueV77 = Migration{
	Version:     77,
	Description: "REG-K12-CORRECT-WITH-PROCESS-ISSUE-20260809-001 durable assessment status",
	AtomicFunc:  migrateK12CorrectWithProcessIssueV77,
}

const k12CorrectWithProcessIssueAssessmentV77DDL = `
CREATE TABLE k12_grading_assessment_items_v77 (
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
        ('correct','correct_with_process_issue','wrong','unanswered','answer_unclear',
         'blank_solved','out_of_scope','untrusted')),
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
    CHECK(parent_guide_invocation_id IS NULL OR
        status IN ('wrong','blank_solved','correct_with_process_issue')),
    CHECK(
        (status IN ('correct','wrong','untrusted') AND
            solve_invocation_id IS NOT NULL AND grade_invocation_id IS NOT NULL)
        OR (status = 'correct_with_process_issue' AND
            solve_invocation_id IS NOT NULL AND grade_invocation_id IS NOT NULL AND
            parent_guide_invocation_id IS NOT NULL)
        OR (status = 'blank_solved' AND
            solve_invocation_id IS NOT NULL AND grade_invocation_id IS NULL)
        OR (status IN ('unanswered','answer_unclear') AND
            solve_invocation_id IS NULL AND grade_invocation_id IS NULL)
        OR (status = 'out_of_scope' AND grade_invocation_id IS NULL)
    )
);`

func migrateK12CorrectWithProcessIssueV77(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) (retErr error) {
	hasAssessments, err := tableExists(ctx, db, "k12_grading_assessment_items")
	if err != nil {
		return fmt.Errorf("check V77 grading assessments: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin V77 migration transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			retErr = errors.Join(retErr, fmt.Errorf("roll back V77 migration transaction: %w", rollbackErr))
		}
	}()
	if !hasAssessments {
		if err := recordVersion(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS k12_grading_assessment_items_v77`); err != nil {
		return fmt.Errorf("drop stale V77 assessment table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, k12CorrectWithProcessIssueAssessmentV77DDL); err != nil {
		return fmt.Errorf("create V77 grading assessment table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_grading_assessment_items_v77 (
        agent_name,job_id,problem_id,attempt_id,confirmed_version,input_revision,
        published_revision,current_disposition,structure_version,input_digest,status,
        result_json,result_digest,solve_invocation_id,grade_invocation_id,
        parent_guide_invocation_id,projection_record_id,projection_created,projection_status,
        created_at,updated_at
    )
    SELECT agent_name,job_id,problem_id,attempt_id,confirmed_version,input_revision,
        published_revision,current_disposition,structure_version,input_digest,status,
        result_json,result_digest,solve_invocation_id,grade_invocation_id,
        parent_guide_invocation_id,projection_record_id,projection_created,projection_status,
        created_at,updated_at
    FROM k12_grading_assessment_items`); err != nil {
		return fmt.Errorf("copy V77 historical grading assessments: %w", err)
	}
	for _, statement := range []string{
		`DROP TABLE k12_grading_assessment_items`,
		`ALTER TABLE k12_grading_assessment_items_v77 RENAME TO k12_grading_assessment_items`,
		`CREATE INDEX idx_k12_grading_assessment_items_job
            ON k12_grading_assessment_items(agent_name,job_id,problem_id,published_revision)`,
		`CREATE UNIQUE INDEX idx_k12_grading_assessment_items_current
            ON k12_grading_assessment_items(agent_name,job_id,problem_id)
            WHERE current_disposition='current'`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("activate V77 grading assessment schema: %w", err)
		}
	}
	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		return fmt.Errorf("check V77 foreign keys: %w", err)
	}
	if violations != 0 {
		return fmt.Errorf("V77 foreign-key check found %d conflicts", violations)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit V77 migration: %w", err)
	}
	return nil
}
