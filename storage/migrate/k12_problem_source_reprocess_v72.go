package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12ProblemSourceReprocessV72 adds the immutable input/source evidence ledger
// and the lease-ready source reprocess queue. It is deliberately additive:
// the V19 Problem/Attempt facts and V50/V51 command/structure ledgers remain
// readable, while future source actions can move their current head without
// overwriting the OCR evidence that produced an earlier revision.
var K12ProblemSourceReprocessV72 = Migration{
	Version:     72,
	Description: "K12 immutable problem inputs, page assets and durable source reprocess work",
	AtomicFunc:  migrateK12ProblemSourceReprocessV72,
}

const k12ProblemSourceReprocessV72DDL = `
CREATE TABLE IF NOT EXISTS k12_page_assets (
    owner_scope TEXT NOT NULL CHECK(length(trim(owner_scope)) > 0),
    page_asset_id TEXT NOT NULL CHECK(length(trim(page_asset_id)) > 0),
    agent_name TEXT NOT NULL,
    content_digest TEXT NOT NULL
        CHECK(
            length(content_digest) = 64 AND
            content_digest NOT GLOB '*[^0-9a-f]*'
        ),
    media_type TEXT NOT NULL
        CHECK(media_type IN ('image/png','image/jpeg','image/gif','image/webp')),
    size_bytes INTEGER NOT NULL CHECK(size_bytes > 0),
    pixel_width INTEGER NOT NULL CHECK(pixel_width > 0),
    pixel_height INTEGER NOT NULL CHECK(pixel_height > 0),
    orientation_policy TEXT NOT NULL DEFAULT 'unverified'
        CHECK(orientation_policy IN ('unverified','verified')),
    orientation_policy_version TEXT NOT NULL DEFAULT 'unverified-v1'
        CHECK(length(trim(orientation_policy_version)) > 0)
        CHECK(
            orientation_policy != 'verified' OR
            orientation_policy_version NOT LIKE 'unverified-%'
        ),
    transform_chain_json TEXT NOT NULL DEFAULT '[]'
        CHECK(json_valid(transform_chain_json) AND json_type(transform_chain_json) = 'array'),
    storage_state TEXT NOT NULL DEFAULT 'staging'
        CHECK(storage_state IN ('staging','ready','failed','corrupt')),
    ready_at INTEGER NOT NULL DEFAULT 0 CHECK(ready_at >= 0),
    last_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    PRIMARY KEY(owner_scope,page_asset_id),
    UNIQUE(agent_name,page_asset_id),
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE,
    CHECK(
        page_asset_id = 'asset://' || agent_name || '/' || content_digest ||
            CASE media_type
                WHEN 'image/png' THEN '.png'
                WHEN 'image/jpeg' THEN '.jpg'
                WHEN 'image/gif' THEN '.gif'
                WHEN 'image/webp' THEN '.webp'
            END
    ),
    CHECK(storage_state != 'ready' OR ready_at > 0),
    CHECK(storage_state != 'ready' OR last_error = ''),
    CHECK(storage_state NOT IN ('failed','corrupt') OR length(trim(last_error)) > 0)
);
CREATE INDEX IF NOT EXISTS idx_k12_page_assets_scope_agent
    ON k12_page_assets(owner_scope,agent_name,storage_state,created_at,page_asset_id);

CREATE TRIGGER IF NOT EXISTS k12_page_asset_identity_immutable
BEFORE UPDATE OF
    owner_scope,page_asset_id,agent_name,content_digest,media_type,size_bytes,
    pixel_width,pixel_height,orientation_policy,orientation_policy_version,
    transform_chain_json,created_at
ON k12_page_assets
BEGIN
    SELECT RAISE(ABORT, 'page asset identity metadata is immutable');
END;

CREATE TABLE IF NOT EXISTS k12_image_task_owner_scopes (
    dispatch_id TEXT PRIMARY KEY,
    owner_scope TEXT NOT NULL CHECK(length(trim(owner_scope)) > 0),
    agent_name TEXT NOT NULL,
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    FOREIGN KEY(dispatch_id)
        REFERENCES k12_image_task_dispatches(dispatch_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_k12_image_task_owner_scopes_owner_agent
    ON k12_image_task_owner_scopes(owner_scope,agent_name,created_at,dispatch_id);

CREATE TRIGGER IF NOT EXISTS k12_image_task_owner_scope_immutable
BEFORE UPDATE ON k12_image_task_owner_scopes
BEGIN
    SELECT RAISE(ABORT, 'image task owner scope is immutable');
END;

CREATE TABLE IF NOT EXISTS k12_problem_input_revisions (
    agent_name TEXT NOT NULL,
    submission_id TEXT NOT NULL,
    structure_version INTEGER NOT NULL CHECK(structure_version >= 1),
    problem_id TEXT NOT NULL,
    input_revision INTEGER NOT NULL CHECK(input_revision >= 1),
    page_asset_id TEXT NOT NULL CHECK(length(trim(page_asset_id)) > 0),
    source_region_json TEXT
        CHECK(source_region_json IS NULL OR (
            json_valid(source_region_json) AND json_type(source_region_json) = 'object'
        )),
    stem_raw TEXT NOT NULL DEFAULT '',
    answer_raw TEXT NOT NULL DEFAULT '',
    answer_bbox_json TEXT NOT NULL DEFAULT ''
        CHECK(answer_bbox_json = '' OR json_valid(answer_bbox_json)),
    question_canonical_markdown TEXT NOT NULL DEFAULT '',
    answer_canonical_markdown TEXT NOT NULL DEFAULT '',
    input_digest TEXT NOT NULL DEFAULT '',
    current_disposition TEXT NOT NULL
        CHECK(current_disposition IN ('current','superseded')),
    origin_command_receipt_id TEXT,
    origin_kind TEXT NOT NULL DEFAULT 'command'
        CHECK(origin_kind IN ('command','legacy_unverified')),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    PRIMARY KEY(
        agent_name,submission_id,structure_version,problem_id,input_revision
    ),
    FOREIGN KEY(agent_name,submission_id,structure_version,problem_id)
        REFERENCES k12_problem_structure_members(
            agent_name,submission_id,structure_version,problem_id
        ) ON DELETE CASCADE,
    FOREIGN KEY(origin_command_receipt_id)
        REFERENCES k12_problem_source_action_receipts(command_receipt_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_problem_input_revisions_current
    ON k12_problem_input_revisions(
        agent_name,submission_id,structure_version,problem_id
    ) WHERE current_disposition='current';
CREATE INDEX IF NOT EXISTS idx_k12_problem_input_revisions_asset
    ON k12_problem_input_revisions(agent_name,page_asset_id,problem_id,input_revision);

CREATE TRIGGER IF NOT EXISTS k12_problem_input_revision_evidence_immutable
BEFORE UPDATE OF
    agent_name,submission_id,structure_version,problem_id,input_revision,
    page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
    question_canonical_markdown,answer_canonical_markdown,input_digest,
    origin_command_receipt_id,origin_kind
ON k12_problem_input_revisions
BEGIN
    SELECT RAISE(ABORT, 'problem input revision evidence is immutable');
END;

CREATE TABLE IF NOT EXISTS k12_problem_source_reprocess_jobs (
    work_id TEXT PRIMARY KEY CHECK(length(trim(work_id)) > 0),
    command_receipt_id TEXT NOT NULL UNIQUE,
    owner_scope TEXT NOT NULL CHECK(length(trim(owner_scope)) > 0),
    agent_name TEXT NOT NULL,
    dispatch_id TEXT NOT NULL,
    job_id TEXT NOT NULL,
    problem_id TEXT NOT NULL,
    action TEXT NOT NULL
        CHECK(action IN ('correct_text','select_region','retake','resume')),
    structure_version INTEGER NOT NULL CHECK(structure_version >= 1),
    input_revision INTEGER NOT NULL CHECK(input_revision >= 1),
    input_digest TEXT NOT NULL CHECK(length(trim(input_digest)) > 0),
    affected_problem_ids_json TEXT NOT NULL
        CHECK(
            json_valid(affected_problem_ids_json) AND
            json_type(affected_problem_ids_json) = 'array' AND
            json_array_length(affected_problem_ids_json) > 0
        ),
    request_json TEXT NOT NULL
        CHECK(json_valid(request_json) AND json_type(request_json) = 'object'),
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK(status IN (
            'prepared','queued','running','needs_confirmation','succeeded',
            'failed','outcome_unknown','cancelled'
        )),
    lease_owner TEXT NOT NULL DEFAULT '',
    lease_epoch INTEGER NOT NULL DEFAULT 0 CHECK(lease_epoch >= 0),
    lease_expires_at INTEGER NOT NULL DEFAULT 0 CHECK(lease_expires_at >= 0),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
    next_attempt_at INTEGER NOT NULL DEFAULT 0 CHECK(next_attempt_at >= 0),
    failure_code TEXT NOT NULL DEFAULT '',
    failure_detail TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    FOREIGN KEY(command_receipt_id)
        REFERENCES k12_problem_source_action_receipts(command_receipt_id) ON DELETE CASCADE,
    FOREIGN KEY(dispatch_id)
        REFERENCES k12_image_task_dispatches(dispatch_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name,job_id)
        REFERENCES k12_grading_jobs(agent_name,record_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name,problem_id)
        REFERENCES k12_problems(agent_name,problem_id) ON DELETE CASCADE,
    CHECK(
        status != 'running' OR
        (length(trim(lease_owner)) > 0 AND lease_epoch >= 1 AND lease_expires_at > 0)
    )
);
CREATE INDEX IF NOT EXISTS idx_k12_problem_source_reprocess_recovery
    ON k12_problem_source_reprocess_jobs(
        status,next_attempt_at,lease_expires_at,updated_at,work_id
    );
CREATE INDEX IF NOT EXISTS idx_k12_problem_source_reprocess_scope
    ON k12_problem_source_reprocess_jobs(
        owner_scope,agent_name,dispatch_id,structure_version,input_revision
    );

CREATE TRIGGER IF NOT EXISTS k12_problem_source_reprocess_identity_immutable
BEFORE UPDATE OF
    work_id,command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,
    problem_id,action,structure_version,input_revision,input_digest,
    affected_problem_ids_json,request_json,created_at
ON k12_problem_source_reprocess_jobs
BEGIN
    SELECT RAISE(ABORT, 'problem source reprocess identity is immutable');
END;
`

func migrateK12ProblemSourceReprocessV72(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin K12 source reprocess V72 migration: %w", err)
	}
	defer tx.Rollback()

	// K12 remains an optional subsystem for selective migration fixtures. Do
	// not manufacture dangling ledgers if an installation never had the V19,
	// V32, V50 and V51 parents.
	for _, table := range []string{
		"agents",
		"k12_problems",
		"k12_attempts",
		"k12_grading_jobs",
		"k12_image_task_dispatches",
		"k12_problem_source_action_receipts",
		"k12_problem_structure_snapshots",
		"k12_problem_structure_members",
	} {
		exists, checkErr := txTableExists(ctx, tx, table)
		if checkErr != nil {
			return fmt.Errorf("check V72 parent table %s: %w", table, checkErr)
		}
		if !exists {
			if err := recordVersion(ctx, tx); err != nil {
				return err
			}
			return tx.Commit()
		}
	}

	for _, column := range []struct {
		name string
		def  string
	}{
		{
			name: "request_json",
			def: `TEXT NOT NULL DEFAULT '{}'
                CHECK(json_valid(request_json) AND json_type(request_json) = 'object')`,
		},
		{
			name: "affected_problem_ids_json",
			def: `TEXT NOT NULL DEFAULT '[]'
                CHECK(
                    json_valid(affected_problem_ids_json) AND
                    json_type(affected_problem_ids_json) = 'array'
                )`,
		},
	} {
		hasColumn, checkErr := txColumnExists(
			ctx,
			tx,
			"k12_problem_source_action_receipts",
			column.name,
		)
		if checkErr != nil {
			return fmt.Errorf("check source action receipt column %s: %w", column.name, checkErr)
		}
		if hasColumn {
			continue
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`ALTER TABLE k12_problem_source_action_receipts ADD COLUMN %s %s`,
			column.name,
			column.def,
		)); err != nil {
			return fmt.Errorf("add source action receipt column %s: %w", column.name, err)
		}
	}

	if _, err := tx.ExecContext(ctx, k12ProblemSourceReprocessV72DDL); err != nil {
		return fmt.Errorf("create K12 source reprocess V72 ledgers: %w", err)
	}

	// Only confirmed current answerable members can be reconstructed without
	// guessing. A v0/empty-digest Attempt is recognition awaiting confirmation,
	// not immutable confirmed evidence, and must not acquire a synthetic V72
	// head. The row freezes confirmed V19/V51 facts exactly and explicitly marks
	// their legacy provenance unverified; source_region is left NULL because an
	// AttemptBBox is an answer anchor, not a source-pixel crop.
	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO k12_problem_input_revisions (
    agent_name,submission_id,structure_version,problem_id,input_revision,
    page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
    question_canonical_markdown,answer_canonical_markdown,input_digest,
    current_disposition,origin_command_receipt_id,origin_kind,created_at,updated_at
)
SELECT sm.agent_name,sm.submission_id,sm.structure_version,sm.problem_id,
       sm.input_revision,p.page_asset_id,NULL,p.stem_raw,a.answer_raw,a.bbox_json,
       p.stem_markdown,a.answer_markdown,a.input_digest,
       'current',NULL,'legacy_unverified',
       MAX(1,MIN(p.created_at,a.created_at)),
       MAX(1,MAX(p.updated_at,a.updated_at))
FROM k12_problem_structure_snapshots ss
JOIN k12_problem_structure_members sm
  ON sm.agent_name=ss.agent_name
 AND sm.submission_id=ss.submission_id
 AND sm.structure_version=ss.structure_version
JOIN k12_problems p
  ON p.agent_name=sm.agent_name
 AND p.submission_id=sm.submission_id
 AND p.problem_id=sm.problem_id
JOIN k12_attempts a
  ON a.agent_name=sm.agent_name
 AND a.submission_id=sm.submission_id
 AND a.problem_id=sm.problem_id
WHERE ss.current_disposition='current'
  AND a.confirmed_version>=1
  AND length(trim(a.input_digest))>0
`); err != nil {
		return fmt.Errorf("backfill immutable problem input revisions: %w", err)
	}

	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).
		Scan(&violations); err != nil {
		return fmt.Errorf("check K12 source reprocess V72 foreign keys: %w", err)
	}
	if violations != 0 {
		return fmt.Errorf("K12 source reprocess V72 found %d foreign-key conflicts", violations)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit K12 source reprocess V72 migration: %w", err)
	}
	return nil
}
