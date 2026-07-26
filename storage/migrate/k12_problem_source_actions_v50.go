package migrate

// K12ProblemSourceActionsV50 is the durable command ledger for progressive
// per-problem source actions. The command response is committed in the same
// transaction as the skip/resume state change, so an idempotent replay returns
// the exact persisted result instead of rebuilding mutable projection state.
var K12ProblemSourceActionsV50 = Migration{
	Version:     50,
	Description: "BUG-20260726-031 K12 分题来源动作命令收据",
	SQL:         K12ProblemSourceActionsV50DDL,
}

const K12ProblemSourceActionsV50DDL = `
CREATE TABLE IF NOT EXISTS k12_problem_source_action_receipts (
    command_receipt_id TEXT PRIMARY KEY,
    owner_scope TEXT NOT NULL CHECK(length(trim(owner_scope)) > 0),
    agent_name TEXT NOT NULL,
    dispatch_id TEXT NOT NULL,
    job_id TEXT NOT NULL,
    problem_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL CHECK(length(trim(idempotency_key)) > 0),
    request_digest TEXT NOT NULL CHECK(length(request_digest) = 64),
    action TEXT NOT NULL CHECK(action IN
        ('correct_text','select_region','retake','skip','resume')),
    structure_version INTEGER NOT NULL CHECK(structure_version >= 1),
    expected_input_revision INTEGER NOT NULL CHECK(expected_input_revision >= 1),
    result_input_revision INTEGER NOT NULL CHECK(result_input_revision >= 1),
    response_json TEXT NOT NULL CHECK(json_valid(response_json)),
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    UNIQUE(owner_scope,idempotency_key),
    FOREIGN KEY(dispatch_id)
        REFERENCES k12_image_task_dispatches(dispatch_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name,job_id)
        REFERENCES k12_grading_jobs(agent_name,record_id) ON DELETE CASCADE,
    FOREIGN KEY(agent_name,problem_id)
        REFERENCES k12_problems(agent_name,problem_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_k12_problem_source_action_scope
    ON k12_problem_source_action_receipts(
        agent_name,dispatch_id,job_id,problem_id,result_input_revision
    );`
