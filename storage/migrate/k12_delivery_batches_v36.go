package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12DeliveryBatchesV36 adds the immutable logical-command root above the V21
// provider receipts. Existing singleton receipts remain valid with batch_id="".
var K12DeliveryBatchesV36 = Migration{
	Version:     36,
	Description: "v0.5.0 K12 全 active direct binding 批次投递与子回执冻结",
	AtomicFunc:  migrateK12DeliveryBatchesV36,
}

func migrateK12DeliveryBatchesV36(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 K12 投递批次迁移事务: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS k12_delivery_batches (
    batch_id       TEXT PRIMARY KEY,
    agent_name     TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    object_kind    TEXT NOT NULL,
    object_id      TEXT NOT NULL,
    dedupe_key     TEXT NOT NULL,
    content_digest TEXT NOT NULL,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    UNIQUE(agent_name, dedupe_key),
    CHECK(length(trim(object_kind)) > 0),
    CHECK(length(trim(object_id)) > 0),
    CHECK(length(trim(dedupe_key)) > 0),
    CHECK(length(trim(content_digest)) > 0)
);
CREATE INDEX IF NOT EXISTS idx_k12_delivery_batches_owner_object
    ON k12_delivery_batches(agent_name, object_kind, object_id, created_at);`); err != nil {
		return fmt.Errorf("创建 K12 投递批次表: %w", err)
	}

	receiptsExist, err := txTableExists(ctx, tx, "k12_delivery_receipts")
	if err != nil {
		return fmt.Errorf("检查 k12_delivery_receipts: %w", err)
	}
	if receiptsExist {
		for _, column := range []struct {
			name string
			def  string
		}{
			{"batch_id", "TEXT NOT NULL DEFAULT ''"},
			{"batch_ordinal", "INTEGER NOT NULL DEFAULT 0"},
		} {
			has, checkErr := txColumnExists(ctx, tx, "k12_delivery_receipts", column.name)
			if checkErr != nil {
				return fmt.Errorf("检查 k12_delivery_receipts.%s: %w", column.name, checkErr)
			}
			if has {
				continue
			}
			if _, alterErr := tx.ExecContext(ctx, fmt.Sprintf(
				`ALTER TABLE k12_delivery_receipts ADD COLUMN %s %s`, column.name, column.def,
			)); alterErr != nil {
				return fmt.Errorf("新增 k12_delivery_receipts.%s: %w", column.name, alterErr)
			}
		}
		if _, err := tx.ExecContext(ctx, `
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_delivery_receipts_batch_ordinal
    ON k12_delivery_receipts(batch_id, batch_ordinal) WHERE batch_id != '';
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_delivery_receipts_batch_target
    ON k12_delivery_receipts(batch_id, platform, instance_id, chat_id) WHERE batch_id != '';
CREATE INDEX IF NOT EXISTS idx_k12_delivery_receipts_batch
    ON k12_delivery_receipts(batch_id, batch_ordinal) WHERE batch_id != '';`); err != nil {
			return fmt.Errorf("创建 K12 投递批次子回执索引: %w", err)
		}
	}
	practiceSetsExist, err := txTableExists(ctx, tx, "k12_practice_sets")
	if err != nil {
		return fmt.Errorf("检查 k12_practice_sets: %w", err)
	}
	if practiceSetsExist {
		has, checkErr := txColumnExists(ctx, tx, "k12_practice_sets", "delivery_batch_id")
		if checkErr != nil {
			return fmt.Errorf("检查 k12_practice_sets.delivery_batch_id: %w", checkErr)
		}
		if !has {
			if _, alterErr := tx.ExecContext(ctx, `ALTER TABLE k12_practice_sets
                ADD COLUMN delivery_batch_id TEXT NOT NULL DEFAULT ''`); alterErr != nil {
				return fmt.Errorf("新增 k12_practice_sets.delivery_batch_id: %w", alterErr)
			}
		}
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
