package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12ProblemSourceRecognitionV73 adds the immutable typed recognition result
// ledger used by select_region/retake reprocessing and an independent lease
// for reconciling ambiguous provider outcomes without replaying source work.
// V72 work/input identities remain unchanged; a successful V73 commit appends
// a new input revision and binds its full recognition evidence to the
// originating work.
var K12ProblemSourceRecognitionV73 = Migration{
	Version:     73,
	Description: "K12 immutable source-recognition results and fenced result revisions",
	AtomicFunc:  migrateK12ProblemSourceRecognitionV73,
}

const k12ProblemSourceRecognitionV73DDL = `
CREATE INDEX IF NOT EXISTS idx_k12_problem_source_reprocess_reconciliation
    ON k12_problem_source_reprocess_jobs(
        status,next_reconcile_at,reconciliation_expires_at,updated_at,work_id
    )
    WHERE status='outcome_unknown';

CREATE TRIGGER IF NOT EXISTS k12_problem_source_reconciliation_state_guard
BEFORE UPDATE OF
    status,lease_owner,lease_expires_at,next_attempt_at,
    reconciliation_owner,reconciliation_epoch,reconciliation_expires_at,
    reconciliation_attempt_count,next_reconcile_at
ON k12_problem_source_reprocess_jobs
WHEN
    (
        NEW.status='outcome_unknown' AND (
            NEW.lease_owner != '' OR
            NEW.lease_expires_at != 0 OR
            NEW.next_attempt_at != 0 OR
            (
                NEW.reconciliation_owner = '' AND
                NEW.reconciliation_expires_at != 0
            ) OR
            (
                NEW.reconciliation_owner != '' AND (
                    length(trim(NEW.reconciliation_owner)) = 0 OR
                    NEW.reconciliation_epoch < 1 OR
                    NEW.reconciliation_attempt_count < 1 OR
                    NEW.reconciliation_expires_at <= 0 OR
                    NEW.next_reconcile_at != 0
                )
            )
        )
    ) OR (
        NEW.status != 'outcome_unknown' AND (
            NEW.reconciliation_owner != '' OR
            NEW.reconciliation_expires_at != 0 OR
            NEW.next_reconcile_at != 0
        )
    )
BEGIN
    SELECT RAISE(ABORT, 'invalid problem source reconciliation state');
END;

CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_model_physical_invocation_parent_identity
    ON k12_model_physical_invocations(physical_invocation_id,parent_invocation_id);

CREATE TABLE IF NOT EXISTS k12_problem_source_recognition_results (
    work_id TEXT PRIMARY KEY,
    command_receipt_id TEXT NOT NULL UNIQUE,
    owner_scope TEXT NOT NULL CHECK(length(trim(owner_scope)) > 0),
    agent_name TEXT NOT NULL,
    submission_id TEXT NOT NULL,
    dispatch_id TEXT NOT NULL,
    job_id TEXT NOT NULL,
    path_problem_id TEXT NOT NULL,
    parent_invocation_id TEXT NOT NULL CHECK(length(trim(parent_invocation_id)) > 0),
    parent_request_digest TEXT NOT NULL CHECK(length(trim(parent_request_digest)) > 0),
    parent_invocation_attempt INTEGER NOT NULL CHECK(parent_invocation_attempt >= 1),
    action TEXT NOT NULL CHECK(action IN ('select_region','retake')),
    structure_version INTEGER NOT NULL CHECK(structure_version >= 1),
    source_input_revision INTEGER NOT NULL CHECK(source_input_revision >= 1),
    result_input_revision INTEGER NOT NULL
        CHECK(result_input_revision = source_input_revision + 1),
    result_digest TEXT NOT NULL
        CHECK(
            length(result_digest) = 64 AND
            result_digest NOT GLOB '*[^0-9a-f]*'
        ),
    mapping_state TEXT NOT NULL CHECK(mapping_state = 'stable_exact_set'),
    structure_digest TEXT NOT NULL CHECK(length(trim(structure_digest)) > 0),
    affected_problem_ids_json TEXT NOT NULL
        CHECK(
            json_valid(affected_problem_ids_json) AND
            json_type(affected_problem_ids_json) = 'array' AND
            json_array_length(affected_problem_ids_json) > 0
        ),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    UNIQUE(work_id,parent_invocation_id),
    FOREIGN KEY(work_id)
        REFERENCES k12_problem_source_reprocess_jobs(work_id) ON DELETE CASCADE,
    FOREIGN KEY(command_receipt_id)
        REFERENCES k12_problem_source_action_receipts(command_receipt_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name,submission_id,structure_version)
        REFERENCES k12_problem_structure_snapshots(
            agent_name,submission_id,structure_version
        ) ON DELETE CASCADE,
    FOREIGN KEY(dispatch_id)
        REFERENCES k12_image_task_dispatches(dispatch_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name,job_id)
        REFERENCES k12_grading_jobs(agent_name,record_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name,path_problem_id)
        REFERENCES k12_problems(agent_name,problem_id) ON DELETE CASCADE,
    FOREIGN KEY(parent_invocation_id)
        REFERENCES k12_model_invocations(invocation_id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_k12_problem_source_recognition_scope
    ON k12_problem_source_recognition_results(
        owner_scope,agent_name,submission_id,structure_version,result_input_revision
    );

CREATE TABLE IF NOT EXISTS k12_problem_source_recognition_items (
    work_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    owner_scope TEXT NOT NULL CHECK(length(trim(owner_scope)) > 0),
    agent_name TEXT NOT NULL,
    submission_id TEXT NOT NULL,
    structure_version INTEGER NOT NULL CHECK(structure_version >= 1),
    problem_id TEXT NOT NULL,
    source_input_revision INTEGER NOT NULL CHECK(source_input_revision >= 1),
    result_input_revision INTEGER NOT NULL
        CHECK(result_input_revision = source_input_revision + 1),
    input_digest TEXT NOT NULL CHECK(length(trim(input_digest)) > 0),
    page_asset_id TEXT NOT NULL CHECK(length(trim(page_asset_id)) > 0),
    source_region_json TEXT
        CHECK(source_region_json IS NULL OR (
            json_valid(source_region_json) AND json_type(source_region_json) = 'object'
        )),
    source_content_digest TEXT NOT NULL
        CHECK(
            length(source_content_digest) = 64 AND
            source_content_digest NOT GLOB '*[^0-9a-f]*'
        ),
    source_media_type TEXT NOT NULL
        CHECK(source_media_type IN ('image/png','image/jpeg','image/gif','image/webp')),
    source_size_bytes INTEGER NOT NULL CHECK(source_size_bytes > 0),
    source_pixel_width INTEGER NOT NULL CHECK(source_pixel_width > 0),
    source_pixel_height INTEGER NOT NULL CHECK(source_pixel_height > 0),
    source_orientation_policy TEXT NOT NULL CHECK(source_orientation_policy = 'verified'),
    source_orientation_policy_version TEXT NOT NULL
        CHECK(length(trim(source_orientation_policy_version)) > 0),
    source_transform_chain_json TEXT NOT NULL
        CHECK(
            json_valid(source_transform_chain_json) AND
            json_type(source_transform_chain_json) = 'array'
        ),
    stem_raw TEXT NOT NULL CHECK(length(trim(stem_raw)) > 0),
    question_canonical_markdown TEXT NOT NULL
        CHECK(length(trim(question_canonical_markdown)) > 0),
    answer_state TEXT NOT NULL CHECK(answer_state IN ('blank','present','unclear')),
    answer_raw TEXT NOT NULL DEFAULT '',
    answer_canonical_markdown TEXT NOT NULL DEFAULT '',
    answer_bbox_json TEXT NOT NULL DEFAULT ''
        CHECK(answer_bbox_json = '' OR (
            json_valid(answer_bbox_json) AND json_type(answer_bbox_json) = 'object'
        )),
    subject TEXT NOT NULL DEFAULT '',
    knowledge_points_json TEXT NOT NULL DEFAULT '[]'
        CHECK(json_valid(knowledge_points_json) AND json_type(knowledge_points_json) = 'array'),
    recognition_confidence REAL
        CHECK(recognition_confidence IS NULL OR (
            recognition_confidence >= 0 AND recognition_confidence <= 1
        )),
    ocr_signals_json TEXT NOT NULL DEFAULT '[]'
        CHECK(json_valid(ocr_signals_json) AND json_type(ocr_signals_json) = 'array'),
    evidence_transcriptions_json TEXT NOT NULL DEFAULT '[]'
        CHECK(
            json_valid(evidence_transcriptions_json) AND
            json_type(evidence_transcriptions_json) = 'array'
        ),
    answer_evidence_transcriptions_json TEXT NOT NULL DEFAULT '[]'
        CHECK(
            json_valid(answer_evidence_transcriptions_json) AND
            json_type(answer_evidence_transcriptions_json) = 'array'
        ),
    confirmation_required INTEGER NOT NULL CHECK(confirmation_required IN (0,1)),
    confirmation_reasons_json TEXT NOT NULL DEFAULT '[]'
        CHECK(
            json_valid(confirmation_reasons_json) AND
            json_type(confirmation_reasons_json) = 'array'
        ),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(work_id,problem_id),
    UNIQUE(work_id,ordinal),
    FOREIGN KEY(work_id)
        REFERENCES k12_problem_source_recognition_results(work_id) ON DELETE CASCADE,
    FOREIGN KEY(owner_scope,page_asset_id)
        REFERENCES k12_page_assets(owner_scope,page_asset_id),
    FOREIGN KEY(agent_name,submission_id,structure_version,problem_id,result_input_revision)
        REFERENCES k12_problem_input_revisions(
            agent_name,submission_id,structure_version,problem_id,input_revision
        ) ON DELETE CASCADE,
    CHECK(answer_state != 'present' OR length(trim(answer_canonical_markdown)) > 0),
    CHECK(answer_state = 'present' OR answer_canonical_markdown = ''),
    CHECK(answer_state != 'blank' OR answer_bbox_json = ''),
    CHECK(
        (confirmation_required = 1 AND json_array_length(confirmation_reasons_json) > 0)
        OR
        (confirmation_required = 0 AND json_array_length(confirmation_reasons_json) = 0)
    )
);
CREATE INDEX IF NOT EXISTS idx_k12_problem_source_recognition_items_scope
    ON k12_problem_source_recognition_items(
        owner_scope,agent_name,submission_id,structure_version,problem_id,
        result_input_revision
    );

CREATE TABLE IF NOT EXISTS k12_problem_source_recognition_physical_results (
    work_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    parent_invocation_id TEXT NOT NULL CHECK(length(trim(parent_invocation_id)) > 0),
    physical_invocation_id TEXT NOT NULL CHECK(length(trim(physical_invocation_id)) > 0),
    physical_unit TEXT NOT NULL CHECK(physical_unit IN
        ('whole_page','segment_1','segment_2','segment_3','segment_4','segment_5','printed_inventory')),
    result_digest TEXT NOT NULL CHECK(length(trim(result_digest)) > 0),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(work_id,physical_invocation_id),
    UNIQUE(work_id,ordinal),
    FOREIGN KEY(work_id,parent_invocation_id)
        REFERENCES k12_problem_source_recognition_results(work_id,parent_invocation_id)
        ON DELETE CASCADE,
    FOREIGN KEY(physical_invocation_id,parent_invocation_id)
        REFERENCES k12_model_physical_invocations(
            physical_invocation_id,parent_invocation_id
        ) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_k12_problem_source_recognition_physical_parent
    ON k12_problem_source_recognition_physical_results(
        parent_invocation_id,physical_invocation_id,work_id
    );

CREATE TRIGGER IF NOT EXISTS k12_problem_source_recognition_result_immutable
BEFORE UPDATE ON k12_problem_source_recognition_results
BEGIN
    SELECT RAISE(ABORT, 'problem source recognition result is immutable');
END;

CREATE TRIGGER IF NOT EXISTS k12_problem_source_recognition_item_immutable
BEFORE UPDATE ON k12_problem_source_recognition_items
BEGIN
    SELECT RAISE(ABORT, 'problem source recognition item is immutable');
END;

CREATE TRIGGER IF NOT EXISTS k12_problem_source_recognition_physical_result_immutable
BEFORE UPDATE ON k12_problem_source_recognition_physical_results
BEGIN
    SELECT RAISE(ABORT, 'problem source recognition physical result is immutable');
END;
`

func migrateK12ProblemSourceRecognitionV73(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin K12 source recognition V73 migration: %w", err)
	}
	defer tx.Rollback()

	for _, table := range []string{
		"k12_problem_source_reprocess_jobs",
		"k12_problem_input_revisions",
		"k12_page_assets",
		"k12_problem_source_action_receipts",
		"k12_problem_structure_snapshots",
		"k12_image_task_dispatches",
		"k12_grading_jobs",
		"k12_problems",
		"k12_model_invocations",
		"k12_model_physical_invocations",
	} {
		exists, checkErr := txTableExists(ctx, tx, table)
		if checkErr != nil {
			return fmt.Errorf("check V73 parent table %s: %w", table, checkErr)
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
		{name: "reconciliation_owner", def: "TEXT NOT NULL DEFAULT ''"},
		{
			name: "reconciliation_epoch",
			def:  "INTEGER NOT NULL DEFAULT 0 CHECK(reconciliation_epoch >= 0)",
		},
		{
			name: "reconciliation_expires_at",
			def:  "INTEGER NOT NULL DEFAULT 0 CHECK(reconciliation_expires_at >= 0)",
		},
		{
			name: "reconciliation_attempt_count",
			def:  "INTEGER NOT NULL DEFAULT 0 CHECK(reconciliation_attempt_count >= 0)",
		},
		{
			name: "next_reconcile_at",
			def:  "INTEGER NOT NULL DEFAULT 0 CHECK(next_reconcile_at >= 0)",
		},
	} {
		hasColumn, checkErr := txColumnExists(
			ctx,
			tx,
			"k12_problem_source_reprocess_jobs",
			column.name,
		)
		if checkErr != nil {
			return fmt.Errorf("check source reconciliation column %s: %w", column.name, checkErr)
		}
		if hasColumn {
			continue
		}
		if _, alterErr := tx.ExecContext(
			ctx,
			fmt.Sprintf(
				"ALTER TABLE k12_problem_source_reprocess_jobs ADD COLUMN %s %s",
				column.name,
				column.def,
			),
		); alterErr != nil {
			return fmt.Errorf("add source reconciliation column %s: %w", column.name, alterErr)
		}
	}

	if _, err := tx.ExecContext(ctx, k12ProblemSourceRecognitionV73DDL); err != nil {
		return fmt.Errorf("create K12 source recognition V73 ledgers: %w", err)
	}
	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).
		Scan(&violations); err != nil {
		return fmt.Errorf("check K12 source recognition V73 foreign keys: %w", err)
	}
	if violations != 0 {
		return fmt.Errorf("K12 source recognition V73 found %d foreign-key conflicts", violations)
	}
	if err := recordVersion(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit K12 source recognition V73 migration: %w", err)
	}
	return nil
}
