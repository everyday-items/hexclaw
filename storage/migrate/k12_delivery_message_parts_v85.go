package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

// K12DeliveryMessagePartsV85 将批次子回执从每目标一条扩展为每 target×part 一条，
// 同时保留旧回执为单一 Markdown part。
var K12DeliveryMessagePartsV85 = Migration{
	Version:     85,
	Description: "K12 target by message-part durable delivery receipts",
	AtomicFunc:  migrateK12DeliveryMessagePartsV85,
}

func migrateK12DeliveryMessagePartsV85(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin K12 delivery message parts V85 migration: %w", err)
	}
	defer tx.Rollback()

	for _, column := range []struct {
		name string
		def  string
	}{
		{name: "part_kind", def: "TEXT NOT NULL DEFAULT 'markdown'"},
		{name: "part_mime", def: "TEXT NOT NULL DEFAULT ''"},
		{name: "part_ordinal", def: "INTEGER NOT NULL DEFAULT 1"},
		{name: "part_digest", def: "TEXT NOT NULL DEFAULT ''"},
		{name: "prepared_resource_id", def: "TEXT NOT NULL DEFAULT ''"},
	} {
		has, checkErr := txColumnExists(ctx, tx, "k12_delivery_receipts", column.name)
		if checkErr != nil {
			return fmt.Errorf("inspect k12_delivery_receipts.%s: %w", column.name, checkErr)
		}
		if has {
			continue
		}
		if _, alterErr := tx.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE k12_delivery_receipts ADD COLUMN %s %s`, column.name, column.def,
		)); alterErr != nil {
			return fmt.Errorf("add k12_delivery_receipts.%s: %w", column.name, alterErr)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT delivery_id,payload_json
		FROM k12_delivery_receipts WHERE trim(part_digest)='' ORDER BY delivery_id`)
	if err != nil {
		return fmt.Errorf("read legacy delivery receipt payloads: %w", err)
	}
	defer rows.Close()
	type partBackfill struct {
		deliveryID string
		partDigest string
	}
	backfills := make([]partBackfill, 0)
	for rows.Next() {
		var deliveryID, payloadJSON string
		if err := rows.Scan(&deliveryID, &payloadJSON); err != nil {
			return fmt.Errorf("scan legacy delivery receipt payload: %w", err)
		}
		var legacyMessage channel.Message
		if err := json.Unmarshal([]byte(payloadJSON), &legacyMessage); err != nil {
			return fmt.Errorf("decode legacy delivery receipt %s: %w", deliveryID, err)
		}
		parts, err := legacyMessage.DeliveryParts()
		if err != nil {
			return fmt.Errorf("derive legacy delivery receipt %s canonical part: %w", deliveryID, err)
		}
		if len(parts) == 0 || parts[0].Kind != messagecontent.PartMarkdown || parts[0].Ordinal != 1 {
			return fmt.Errorf("legacy delivery receipt %s has no canonical Markdown part", deliveryID)
		}
		backfills = append(backfills, partBackfill{deliveryID: deliveryID, partDigest: parts[0].Digest})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy delivery receipt payloads: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy delivery receipt payloads: %w", err)
	}
	for _, backfill := range backfills {
		if _, err := tx.ExecContext(ctx, `UPDATE k12_delivery_receipts
			SET part_kind='markdown',part_mime='',part_ordinal=1,
				part_digest=?,prepared_resource_id=''
			WHERE delivery_id=? AND trim(part_digest)=''`, backfill.partDigest, backfill.deliveryID); err != nil {
			return fmt.Errorf("backfill legacy delivery receipt %s part identity: %w", backfill.deliveryID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
DROP INDEX IF EXISTS idx_k12_delivery_receipts_batch_target;
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_delivery_receipts_batch_target_part
	ON k12_delivery_receipts(batch_id,platform,instance_id,chat_id,part_ordinal)
	WHERE batch_id != '';`); err != nil {
		return fmt.Errorf("replace K12 delivery target-part indexes: %w", err)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit K12 delivery message parts V85 migration: %w", err)
	}
	return nil
}
