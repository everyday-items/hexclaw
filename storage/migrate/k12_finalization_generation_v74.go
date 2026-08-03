package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12FinalizationGenerationV74 adds one durable aggregate generation shared
// by source-changing commands and the immutable final-artifact commit. It
// closes the cross-process window in which a finalizer could publish receipt
// digests read before a concurrently committed source revision.
var K12FinalizationGenerationV74 = Migration{
	Version:     74,
	Description: "K12 source/final-artifact aggregate generation CAS",
	AtomicFunc:  migrateK12FinalizationGenerationV74,
}

func migrateK12FinalizationGenerationV74(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin K12 finalization generation V74 migration: %w", err)
	}
	defer tx.Rollback()

	// K12 can be omitted by selective migration fixtures. Record the version
	// without manufacturing a partial aggregate when either V19 or V49 is not
	// installed.
	for _, table := range []string{
		"k12_grading_jobs",
		"k12_grading_final_artifacts",
	} {
		exists, checkErr := txTableExists(ctx, tx, table)
		if checkErr != nil {
			return fmt.Errorf("check V74 parent table %s: %w", table, checkErr)
		}
		if !exists {
			if err := recordVersion(ctx, tx); err != nil {
				return err
			}
			return tx.Commit()
		}
	}

	for _, table := range []string{
		"k12_grading_jobs",
		"k12_grading_final_artifacts",
	} {
		hasColumn, checkErr := txColumnExists(
			ctx,
			tx,
			table,
			"finalization_generation",
		)
		if checkErr != nil {
			return fmt.Errorf(
				"check %s finalization generation column: %w",
				table,
				checkErr,
			)
		}
		if hasColumn {
			continue
		}
		if _, alterErr := tx.ExecContext(ctx, fmt.Sprintf(`
			ALTER TABLE %s
			ADD COLUMN finalization_generation INTEGER NOT NULL DEFAULT 0
			CHECK(finalization_generation >= 0)`, table)); alterErr != nil {
			return fmt.Errorf(
				"add %s finalization generation column: %w",
				table,
				alterErr,
			)
		}
	}

	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit K12 finalization generation V74 migration: %w", err)
	}
	return nil
}
