package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12ModelInvocationRequestPolicyV64 adds the independently auditable,
// immutable per-invocation request policy snapshot approved by DD-036.
var K12ModelInvocationRequestPolicyV64 = Migration{
	Version:     64,
	Description: "DD-036 K12 recognizing model request policy snapshot",
	Func:        migrateK12ModelInvocationRequestPolicyV64,
}

func migrateK12ModelInvocationRequestPolicyV64(
	ctx context.Context,
	db *sql.DB,
) error {
	has, err := columnExists(
		ctx,
		db,
		"k12_model_invocations",
		"request_policy_snapshot_json",
	)
	if err != nil {
		return fmt.Errorf(
			"check k12_model_invocations.request_policy_snapshot_json: %w",
			err,
		)
	}
	if has {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE k12_model_invocations
        ADD COLUMN request_policy_snapshot_json TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf(
			"add k12_model_invocations.request_policy_snapshot_json: %w",
			err,
		)
	}
	return nil
}
