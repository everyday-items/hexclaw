package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12GradingExecutionKindV93 区分本机确定性执行与真实 Provider 执行。
var K12GradingExecutionKindV93 = Migration{
	Version:     93,
	Description: "K12 grading item execution kind",
	AtomicFunc:  migrateK12GradingExecutionKindV93,
}

const k12GradingExecutionKindV93DDL = `
CREATE TABLE k12_grading_item_invocations_v93 (
    item_invocation_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    problem_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    operation TEXT NOT NULL CHECK(operation IN
        ('solve','solve_generate','solve_verify','grade','parent_guide')),
    execution_kind TEXT NOT NULL DEFAULT 'provider'
        CHECK(execution_kind IN ('provider','local_deterministic')),
    operation_attempt INTEGER NOT NULL CHECK(operation_attempt >= 1),
    request_digest TEXT NOT NULL CHECK(request_digest != ''),
    provider TEXT NOT NULL CHECK(provider != ''),
    model TEXT NOT NULL CHECK(model != ''),
    route_snapshot_json TEXT NOT NULL CHECK(route_snapshot_json != ''),
    status TEXT NOT NULL CHECK(status IN
        ('prepared','sent','succeeded','failed','outcome_unknown','reconciled')),
    cost_receipt_id TEXT NOT NULL DEFAULT '',
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
    CHECK(status != 'succeeded' OR (
        result_digest != '' AND result_json != '' AND
        ((execution_kind = 'provider' AND cost_receipt_id != '') OR
         (execution_kind = 'local_deterministic' AND cost_receipt_id = ''))
    )),
    CHECK(status NOT IN ('failed','outcome_unknown') OR
        (failure_class != '' AND failure_code != ''))
);`

func migrateK12GradingExecutionKindV93(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) (retErr error) {
	hasTable, err := tableExists(ctx, db, "k12_grading_item_invocations")
	if err != nil {
		return fmt.Errorf("check k12 grading item invocations: %w", err)
	}
	if !hasTable {
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
	hasExecutionKind, err := columnExists(ctx, db, "k12_grading_item_invocations", "execution_kind")
	if err != nil {
		return fmt.Errorf("check k12 grading execution kind: %w", err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get V93 migration connection: %w", err)
	}
	defer conn.Close()
	var foreignKeys int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("read V93 foreign_keys: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable V93 foreign_keys: %w", err)
	}
	defer func() {
		if foreignKeys != 0 {
			if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`); retErr == nil && err != nil {
				retErr = fmt.Errorf("restore V93 foreign_keys: %w", err)
			}
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin V93 migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS k12_grading_item_invocations_v93`); err != nil {
		return fmt.Errorf("drop stale V93 invocation table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, k12GradingExecutionKindV93DDL); err != nil {
		return fmt.Errorf("create V93 invocation table: %w", err)
	}
	executionKindExpr := `'provider'`
	if hasExecutionKind {
		executionKindExpr = "execution_kind"
	}
	copySQL := `INSERT INTO k12_grading_item_invocations_v93 (
        item_invocation_id,agent_name,job_id,problem_id,attempt_id,
        operation,execution_kind,operation_attempt,request_digest,provider,model,
        route_snapshot_json,status,cost_receipt_id,result_digest,result_json,
        failure_class,failure_code,created_at,updated_at
    )
    SELECT item_invocation_id,agent_name,job_id,problem_id,attempt_id,
        operation,` + executionKindExpr + `,operation_attempt,request_digest,provider,model,
        route_snapshot_json,status,cost_receipt_id,result_digest,result_json,
        failure_class,failure_code,created_at,updated_at
    FROM k12_grading_item_invocations`
	if _, err := tx.ExecContext(ctx, copySQL); err != nil {
		return fmt.Errorf("copy V93 grading item invocations: %w", err)
	}
	for _, statement := range []string{
		`DROP TABLE k12_grading_item_invocations`,
		`ALTER TABLE k12_grading_item_invocations_v93 RENAME TO k12_grading_item_invocations`,
		`CREATE INDEX idx_k12_grading_item_invocations_job
            ON k12_grading_item_invocations(agent_name,job_id,problem_id,operation,operation_attempt)`,
		`CREATE INDEX idx_k12_grading_item_invocations_status
            ON k12_grading_item_invocations(status,updated_at)`,
		`CREATE UNIQUE INDEX idx_k12_grading_item_invocations_cost_receipt
            ON k12_grading_item_invocations(cost_receipt_id) WHERE cost_receipt_id != ''`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("activate V93 grading item invocation schema: %w", err)
		}
	}
	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		return fmt.Errorf("check V93 foreign keys: %w", err)
	}
	if violations != 0 {
		return fmt.Errorf("V93 foreign-key check found %d conflicts", violations)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit V93 migration: %w", err)
	}
	return nil
}
