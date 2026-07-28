package migrate

// K12ImageTaskSolveInvocationsV61 widens the shared image-task invocation
// ledger so deterministic homework solve preflight failures can be persisted
// before a grading job or model invocation exists.
var K12ImageTaskSolveInvocationsV61 = Migration{
	Version:     61,
	Description: "K12 image_task solve preflight terminal receipt",
	SQL:         K12ImageTaskSolveInvocationsV61DDL,
}

const K12ImageTaskSolveInvocationsV61DDL = `
CREATE TABLE k12_image_task_invocations_v61 (
    invocation_id           TEXT    PRIMARY KEY,
    agent_name              TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    dispatch_id             TEXT REFERENCES k12_image_task_dispatches(dispatch_id) ON DELETE CASCADE,
    intake_id               TEXT REFERENCES k12_creative_work_intakes(intake_id) ON DELETE CASCADE,
    work_record_id          TEXT REFERENCES k12_creative_works(record_id) ON DELETE CASCADE,
    operation               TEXT    NOT NULL CHECK(operation IN ('classification','writing_ocr','work_feedback','solve')),
    operation_key           TEXT    NOT NULL,
    request_digest          TEXT    NOT NULL,
    route_snapshot_json     TEXT    NOT NULL,
    status                  TEXT    NOT NULL DEFAULT 'prepared'
        CHECK(status IN ('prepared','sent','succeeded','failed','outcome_unknown','reconciled')),
    attempt                 INTEGER NOT NULL DEFAULT 1 CHECK(attempt >= 1),
    provider_request_key    TEXT    NOT NULL DEFAULT '',
    result_digest           TEXT    NOT NULL DEFAULT '',
    result_json             TEXT    NOT NULL DEFAULT '',
    error_kind              TEXT    NOT NULL DEFAULT '',
    retry_safe              INTEGER NOT NULL DEFAULT 0 CHECK(retry_safe IN (0,1)),
    started_at              INTEGER NOT NULL DEFAULT 0,
    finished_at             INTEGER NOT NULL DEFAULT 0,
    created_at              INTEGER NOT NULL,
    updated_at              INTEGER NOT NULL,
    deadline_at             INTEGER NOT NULL DEFAULT 0 CHECK(deadline_at >= 0),
    UNIQUE(agent_name, operation_key, attempt),
    CHECK(
        (operation = 'classification' AND dispatch_id IS NOT NULL AND intake_id IS NULL AND work_record_id IS NULL) OR
        (operation = 'writing_ocr' AND dispatch_id IS NULL AND intake_id IS NOT NULL AND work_record_id IS NULL) OR
        (operation = 'work_feedback' AND dispatch_id IS NULL AND intake_id IS NULL AND work_record_id IS NOT NULL) OR
        (operation = 'solve' AND dispatch_id IS NOT NULL AND intake_id IS NULL AND work_record_id IS NULL)
    )
);

INSERT INTO k12_image_task_invocations_v61 (
    invocation_id,agent_name,dispatch_id,intake_id,work_record_id,operation,
    operation_key,request_digest,route_snapshot_json,status,attempt,
    provider_request_key,result_digest,result_json,error_kind,retry_safe,
    started_at,finished_at,created_at,updated_at,deadline_at
)
SELECT invocation_id,agent_name,dispatch_id,intake_id,work_record_id,operation,
       operation_key,request_digest,route_snapshot_json,status,attempt,
       provider_request_key,result_digest,result_json,error_kind,retry_safe,
       started_at,finished_at,created_at,updated_at,deadline_at
FROM k12_image_task_invocations;

DROP TRIGGER IF EXISTS k12_image_invocation_identity_immutable;
DROP TABLE k12_image_task_invocations;
ALTER TABLE k12_image_task_invocations_v61 RENAME TO k12_image_task_invocations;

CREATE INDEX idx_k12_image_invocation_recovery
    ON k12_image_task_invocations(agent_name, status, updated_at);
CREATE INDEX idx_k12_image_invocation_recovery_deadline
    ON k12_image_task_invocations(agent_name, status, deadline_at)
    WHERE deadline_at > 0;

CREATE TRIGGER k12_image_invocation_identity_immutable
BEFORE UPDATE OF agent_name, dispatch_id, intake_id, work_record_id, operation,
    operation_key, request_digest, route_snapshot_json, attempt
ON k12_image_task_invocations
BEGIN
    SELECT RAISE(ABORT, 'image task invocation identity is immutable');
END;
`
