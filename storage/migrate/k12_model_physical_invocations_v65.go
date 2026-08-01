package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// K12ModelPhysicalInvocationsV65 adds one immutable child receipt per actual
// DD-036 structured-recognition provider request.
var K12ModelPhysicalInvocationsV65 = Migration{
	Version:     65,
	Description: "DD-036 K12 recognizing physical model invocation ledger",
	Func:        migrateK12ModelPhysicalInvocationsV65,
}

const K12ModelPhysicalInvocationsV65DDL = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_model_invocations_child_identity
    ON k12_model_invocations(invocation_id,agent_name,job_id,stage);

CREATE TABLE IF NOT EXISTS k12_model_physical_invocations (
    physical_invocation_id TEXT PRIMARY KEY,
    parent_invocation_id TEXT NOT NULL,
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    stage TEXT NOT NULL CHECK(stage = 'recognizing'),
    physical_unit TEXT NOT NULL CHECK(physical_unit IN
        ('whole_page','segment_1','segment_2','segment_3','segment_4','segment_5','printed_inventory')),
    request_digest TEXT NOT NULL CHECK(request_digest != ''),
    route_snapshot_json TEXT NOT NULL CHECK(route_snapshot_json != ''),
    request_policy_snapshot_json TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK(status IN
        ('prepared','sent','succeeded','failed','outcome_unknown','reconciled')),
    attempt INTEGER NOT NULL CHECK(attempt = 1),
    result_digest TEXT NOT NULL DEFAULT '',
    result_content TEXT,
    external_request_id TEXT NOT NULL DEFAULT '',
    failure_kind TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(parent_invocation_id,physical_unit),
    FOREIGN KEY(parent_invocation_id,agent_name,job_id,stage)
        REFERENCES k12_model_invocations(invocation_id,agent_name,job_id,stage)
        ON DELETE CASCADE,
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE,
    FOREIGN KEY(job_id) REFERENCES k12_grading_jobs(record_id) ON DELETE CASCADE,
    CHECK(status != 'succeeded' OR
        (result_digest != '' AND result_content IS NOT NULL)),
    CHECK(status = 'succeeded' OR result_content IS NULL),
    CHECK(status NOT IN ('failed','outcome_unknown') OR failure_kind != '')
);
CREATE INDEX IF NOT EXISTS idx_k12_model_physical_invocations_job
    ON k12_model_physical_invocations(agent_name,job_id,parent_invocation_id,physical_unit);
CREATE INDEX IF NOT EXISTS idx_k12_model_physical_invocations_status
    ON k12_model_physical_invocations(status,updated_at);

CREATE TABLE IF NOT EXISTS k12_recognition_fallback_authorizations (
    parent_invocation_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    whole_physical_invocation_id TEXT NOT NULL UNIQUE,
    whole_result_digest TEXT NOT NULL CHECK(whole_result_digest != ''),
    whole_result_content TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY(parent_invocation_id)
        REFERENCES k12_model_invocations(invocation_id) ON DELETE CASCADE,
    FOREIGN KEY(whole_physical_invocation_id)
        REFERENCES k12_model_physical_invocations(physical_invocation_id)
        ON DELETE CASCADE,
    FOREIGN KEY(agent_name) REFERENCES agents(name) ON DELETE CASCADE,
    FOREIGN KEY(job_id) REFERENCES k12_grading_jobs(record_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_k12_recognition_fallback_authorizations_owner
    ON k12_recognition_fallback_authorizations(
        agent_name,job_id,parent_invocation_id
    );`

func migrateK12ModelPhysicalInvocationsV65(
	ctx context.Context,
	db *sql.DB,
) error {
	requiredColumns := []struct {
		table  string
		column string
	}{
		{table: "agents", column: "name"},
		{table: "k12_grading_jobs", column: "record_id"},
		{table: "k12_model_invocations", column: "invocation_id"},
		{table: "k12_model_invocations", column: "agent_name"},
		{table: "k12_model_invocations", column: "job_id"},
		{table: "k12_model_invocations", column: "stage"},
	}
	for _, required := range requiredColumns {
		has, err := columnExists(
			ctx,
			db,
			required.table,
			required.column,
		)
		if err != nil {
			return fmt.Errorf(
				"check %s.%s for v65: %w",
				required.table,
				required.column,
				err,
			)
		}
		if !has {
			// Optional/selective legacy fixtures may contain only a subset of
			// the K12 ledger. Do not create a child table with an unusable
			// composite foreign key against that incomplete parent.
			return nil
		}
	}
	if _, err := db.ExecContext(ctx, K12ModelPhysicalInvocationsV65DDL); err != nil {
		return fmt.Errorf("create K12 model physical invocation ledger: %w", err)
	}
	return nil
}
