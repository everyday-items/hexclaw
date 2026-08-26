package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12IMInboundRoutingSnapshotV92 为候选快照补充独立的路由请求摘要，
// 使候选集合与触发该次分流的原始命令可以分别校验。
var K12IMInboundRoutingSnapshotV92 = Migration{
	Version:     92,
	Description: "K12 inbound photo routing request digest",
	Func: func(ctx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin K12 inbound routing snapshot V92 migration: %w", err)
		}
		defer tx.Rollback()
		exists, err := txColumnExists(ctx, tx, "k12_im_inbound_routing_snapshots", "request_digest")
		if err != nil {
			return fmt.Errorf("inspect routing snapshot request digest: %w", err)
		}
		if !exists {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE k12_im_inbound_routing_snapshots
				ADD COLUMN request_digest TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("add routing snapshot request digest: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS
			idx_k12_im_inbound_routing_snapshots_request
			ON k12_im_inbound_routing_snapshots(receipt_id,request_digest)`); err != nil {
			return fmt.Errorf("index routing snapshot request digest: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit K12 inbound routing snapshot V92 migration: %w", err)
		}
		return nil
	},
}
