package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12TextbookCatalogWorkerV67 adds the durable source snapshot and scheduling
// fields required by the bounded catalog worker. The source snapshot pins one
// completed Knowledge ingest attempt; retries never silently switch parsers or
// page artifacts after a job has been accepted.
var K12TextbookCatalogWorkerV67 = Migration{
	Version:     67,
	Description: "K12 教材目录 Worker 租约心跳、退避与 Knowledge 页证据快照",
	Func:        migrateK12TextbookCatalogWorkerV67,
}

func migrateK12TextbookCatalogWorkerV67(ctx context.Context, db *sql.DB) error {
	columns := []struct {
		name string
		ddl  string
	}{
		{"ingest_job_id", "TEXT NOT NULL DEFAULT ''"},
		{"source_plan_digest", "TEXT NOT NULL DEFAULT ''"},
		{"extractor_contract", "TEXT NOT NULL DEFAULT 'checkpoint-toc-footer-v1'"},
		{"next_attempt_at", "INTEGER NOT NULL DEFAULT 0 CHECK(next_attempt_at >= 0)"},
		{"heartbeat_at", "INTEGER NOT NULL DEFAULT 0 CHECK(heartbeat_at >= 0)"},
		{"failure_code", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, column := range columns {
		has, err := columnExists(ctx, db, "k12_textbook_catalog_jobs", column.name)
		if err != nil {
			return fmt.Errorf("检查 k12_textbook_catalog_jobs.%s: %w", column.name, err)
		}
		if has {
			continue
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE k12_textbook_catalog_jobs ADD COLUMN %s %s`,
			column.name, column.ddl,
		)); err != nil {
			return fmt.Errorf("新增 k12_textbook_catalog_jobs.%s: %w", column.name, err)
		}
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_k12_textbook_catalog_jobs_schedule
		ON k12_textbook_catalog_jobs(state,next_attempt_at,lease_expires_at,created_at,job_id)`); err != nil {
		return fmt.Errorf("创建教材目录任务调度索引: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER IF NOT EXISTS k12_textbook_catalog_source_snapshot_immutable
		BEFORE UPDATE OF ingest_job_id,source_plan_digest,extractor_contract,request_digest
		ON k12_textbook_catalog_jobs
		WHEN (length(OLD.ingest_job_id)>0 AND length(OLD.source_plan_digest)=64)
		 AND (NEW.ingest_job_id<>OLD.ingest_job_id
		      OR NEW.source_plan_digest<>OLD.source_plan_digest
		      OR NEW.extractor_contract<>OLD.extractor_contract
		      OR NEW.request_digest<>OLD.request_digest)
		BEGIN
		  SELECT RAISE(ABORT, 'textbook catalog source snapshot is immutable');
		END`); err != nil {
		return fmt.Errorf("创建教材目录来源快照不可变触发器: %w", err)
	}
	return nil
}
