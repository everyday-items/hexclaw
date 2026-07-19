package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12CreativeWorkOCRV20DDL is the release-governed DD-013 OCR job and
// confirmation-version ledger. Runtime code never creates these tables.
const K12CreativeWorkOCRV20DDL = `
CREATE TABLE IF NOT EXISTS k12_creative_work_ocr_jobs (
    job_id              TEXT PRIMARY KEY,
    agent_name          TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    request_id          TEXT NOT NULL,
    source_asset_id     TEXT NOT NULL,
    source_digest       TEXT NOT NULL,
    status              TEXT NOT NULL CHECK(status IN (
        'pending','processing','awaiting_confirmation','failed','confirmed'
    )),
    ocr_raw             TEXT NOT NULL DEFAULT '',
    error_message       TEXT NOT NULL DEFAULT '',
    attempt_count       INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
    confirmed_version   INTEGER NOT NULL DEFAULT 0 CHECK(confirmed_version >= 0),
    confirmed_digest    TEXT NOT NULL DEFAULT '',
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    UNIQUE(agent_name, request_id),
    CHECK((confirmed_version = 0 AND confirmed_digest = '') OR
          (confirmed_version > 0 AND confirmed_digest != ''))
);
CREATE INDEX IF NOT EXISTS idx_k12_creative_work_ocr_jobs_owner
    ON k12_creative_work_ocr_jobs(agent_name, created_at);
CREATE INDEX IF NOT EXISTS idx_k12_creative_work_ocr_jobs_status
    ON k12_creative_work_ocr_jobs(status, updated_at);

CREATE TABLE IF NOT EXISTS k12_creative_work_ocr_versions (
    job_id            TEXT NOT NULL REFERENCES k12_creative_work_ocr_jobs(job_id) ON DELETE CASCADE,
    version           INTEGER NOT NULL CHECK(version >= 1),
    content_markdown  TEXT NOT NULL,
    content_digest    TEXT NOT NULL,
    confirmed_at      INTEGER NOT NULL,
    PRIMARY KEY(job_id, version),
    UNIQUE(job_id, content_digest)
);

CREATE TRIGGER IF NOT EXISTS k12_creative_work_ocr_job_identity_immutable
BEFORE UPDATE OF agent_name, request_id, source_asset_id, source_digest
ON k12_creative_work_ocr_jobs
BEGIN
    SELECT RAISE(ABORT, 'creative work OCR job identity is immutable');
END;

CREATE TRIGGER IF NOT EXISTS k12_creative_work_ocr_raw_immutable
BEFORE UPDATE OF ocr_raw ON k12_creative_work_ocr_jobs
WHEN OLD.ocr_raw != '' AND NEW.ocr_raw != OLD.ocr_raw
BEGIN
    SELECT RAISE(ABORT, 'creative work OCR raw evidence is immutable');
END;

CREATE TRIGGER IF NOT EXISTS k12_creative_work_ocr_version_immutable
BEFORE UPDATE ON k12_creative_work_ocr_versions
BEGIN
    SELECT RAISE(ABORT, 'creative work OCR confirmation versions are append-only');
END;

CREATE TRIGGER IF NOT EXISTS k12_creative_work_ocr_version_no_direct_delete
BEFORE DELETE ON k12_creative_work_ocr_versions
WHEN EXISTS (SELECT 1 FROM k12_creative_work_ocr_jobs j WHERE j.job_id = OLD.job_id)
BEGIN
    SELECT RAISE(ABORT, 'creative work OCR confirmation versions are append-only');
END;`

func migrateK12CreativeWorkOCRV20(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, K12CreativeWorkOCRV20DDL); err != nil {
		return fmt.Errorf("创建作文 OCR Job/确认版本账本: %w", err)
	}
	for _, column := range []struct {
		name string
		def  string
	}{
		{"ocr_job_id", "TEXT NOT NULL DEFAULT ''"},
		{"ocr_raw", "TEXT NOT NULL DEFAULT ''"},
		{"ocr_version", "INTEGER NOT NULL DEFAULT 0"},
		{"ocr_confirmed_digest", "TEXT NOT NULL DEFAULT ''"},
		{"content_confirmed_at", "INTEGER NOT NULL DEFAULT 0"},
	} {
		has, err := columnExists(ctx, db, "k12_creative_work_versions", column.name)
		if err != nil {
			return fmt.Errorf("检查 k12_creative_work_versions.%s: %w", column.name, err)
		}
		if has {
			continue
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE k12_creative_work_versions ADD COLUMN %s %s`, column.name, column.def)); err != nil {
			return fmt.Errorf("新增 k12_creative_work_versions.%s: %w", column.name, err)
		}
	}
	return nil
}
