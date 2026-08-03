package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12ModelInvocationResultV75 adds the immutable JSON result payload used to
// resume final projection after a provider success was durably recorded but
// the canonical final artifact was not yet committed.
var K12ModelInvocationResultV75 = Migration{
	Version:     75,
	Description: "K12 durable model invocation result payload",
	AtomicFunc:  migrateK12ModelInvocationResultV75,
}

func migrateK12ModelInvocationResultV75(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin K12 model invocation result V75 migration: %w", err)
	}
	defer tx.Rollback()

	exists, err := txTableExists(ctx, tx, "k12_model_invocations")
	if err != nil {
		return fmt.Errorf("check V75 model invocation table: %w", err)
	}
	if exists {
		hasColumn, checkErr := txColumnExists(
			ctx,
			tx,
			"k12_model_invocations",
			"result_json",
		)
		if checkErr != nil {
			return fmt.Errorf("check V75 model invocation result column: %w", checkErr)
		}
		if !hasColumn {
			if _, alterErr := tx.ExecContext(ctx, `
				ALTER TABLE k12_model_invocations
				ADD COLUMN result_json TEXT NOT NULL DEFAULT ''
				CHECK(result_json='' OR json_valid(result_json))`); alterErr != nil {
				return fmt.Errorf("add model invocation result_json column: %w", alterErr)
			}
		}
	}

	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit K12 model invocation result V75 migration: %w", err)
	}
	return nil
}
