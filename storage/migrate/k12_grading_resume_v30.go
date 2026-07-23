package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12GradingResumeV30 is the additive ADR-K12-021 / DD-038 storage boundary. Rollback
// deliberately leaves these evidence tables in place: an older binary ignores
// them while a later upgrade can still reconcile sent/unknown operations.
var K12GradingResumeV30 = Migration{
	Version:     30,
	Description: "v0.5.0 DD-037/DD-038：结果待核实、冻结批改预算、分题调用账本与事务回执",
	Func:        migrateK12GradingResumeV30,
}

func migrateK12GradingResumeV30(ctx context.Context, db *sql.DB) error {
	hasGradingJobs, err := tableExists(ctx, db, "k12_grading_jobs")
	if err != nil {
		return fmt.Errorf("检查 k12_grading_jobs: %w", err)
	}
	// Selective migration fixtures (and installations that never installed the
	// K12 schema) still advance the global migration ledger. Match the existing
	// optional-subsystem convention: without the K12 root table there is nothing
	// to upgrade, and creating child ledgers with dangling FK/index targets would
	// be incorrect.
	if !hasGradingJobs {
		return nil
	}
	hasBudget, err := columnExists(ctx, db, "k12_grading_jobs", "budget_snapshot_json")
	if err != nil {
		return fmt.Errorf("检查 k12_grading_jobs.budget_snapshot_json: %w", err)
	}
	if !hasBudget {
		if _, err := db.ExecContext(ctx, `ALTER TABLE k12_grading_jobs
            ADD COLUMN budget_snapshot_json TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("新增 k12_grading_jobs.budget_snapshot_json: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, K12GradingResumeV30DDL); err != nil {
		return fmt.Errorf("创建 K12 分题批改账本: %w", err)
	}
	return nil
}

const K12GradingResumeV30DDL = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_grading_jobs_owner_id
    ON k12_grading_jobs(agent_name, record_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_attempts_owner_id_problem
    ON k12_attempts(agent_name, attempt_id, problem_id);

CREATE TABLE IF NOT EXISTS k12_grading_item_invocations (
    item_invocation_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    problem_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    operation TEXT NOT NULL CHECK(operation IN ('solve','grade')),
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
);
CREATE INDEX IF NOT EXISTS idx_k12_grading_item_invocations_job
    ON k12_grading_item_invocations(agent_name, job_id, problem_id, operation, operation_attempt);
CREATE INDEX IF NOT EXISTS idx_k12_grading_item_invocations_status
    ON k12_grading_item_invocations(status, updated_at);

CREATE TABLE IF NOT EXISTS k12_grading_assessment_items (
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
        REFERENCES k12_grading_item_invocations(item_invocation_id),
    FOREIGN KEY(grade_invocation_id)
        REFERENCES k12_grading_item_invocations(item_invocation_id),
    CHECK(projection_created = 0 OR projection_record_id != ''),
    CHECK(
        (status IN ('correct','wrong','untrusted') AND solve_invocation_id IS NOT NULL AND grade_invocation_id IS NOT NULL)
        OR (status = 'blank_solved' AND solve_invocation_id IS NOT NULL AND grade_invocation_id IS NULL)
        OR (status IN ('unanswered','answer_unclear') AND solve_invocation_id IS NULL AND grade_invocation_id IS NULL)
        OR (status = 'out_of_scope' AND grade_invocation_id IS NULL)
    )
);
CREATE INDEX IF NOT EXISTS idx_k12_grading_assessment_items_job
    ON k12_grading_assessment_items(agent_name, job_id, problem_id);`
