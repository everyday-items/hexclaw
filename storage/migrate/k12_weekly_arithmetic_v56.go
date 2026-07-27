package migrate

var K12WeeklyArithmeticV56 = Migration{
	Version:     56,
	Description: "BUG-20260726-034-A06: 周练口算 append-only batch、命令、作答与同步 checkpoint",
	SQL:         K12WeeklyArithmeticV56DDL,
}

const K12WeeklyArithmeticV56DDL = `
CREATE TABLE IF NOT EXISTS k12_weekly_arithmetic_batches (
    batch_id                   TEXT PRIMARY KEY,
    agent_name                 TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    plan_id                    TEXT NOT NULL REFERENCES k12_weekly_practice_plans(plan_id) ON DELETE CASCADE,
    ordinal                    INTEGER NOT NULL CHECK(ordinal > 0),
    state                      TEXT NOT NULL CHECK(state IN
        ('preparing','ready','in_progress','completed','failed_retryable','failed_terminal')),
    item_count                 INTEGER NOT NULL DEFAULT 0 CHECK(item_count >= 0),
    content_digest             TEXT NOT NULL DEFAULT '',
    retryable                  INTEGER NOT NULL DEFAULT 0 CHECK(retryable IN (0,1)),
    failure_message            TEXT NOT NULL DEFAULT '',
    generation_checkpoint_json TEXT NOT NULL CHECK(json_valid(generation_checkpoint_json)),
    items_json                 TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(items_json)),
    answer_keys_json           TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(answer_keys_json)),
    created_at                 INTEGER NOT NULL CHECK(created_at > 0),
    updated_at                 INTEGER NOT NULL CHECK(updated_at >= created_at),
    completed_at               INTEGER,
    UNIQUE(plan_id,ordinal)
);
CREATE INDEX IF NOT EXISTS idx_k12_weekly_arithmetic_latest
    ON k12_weekly_arithmetic_batches(agent_name,plan_id,ordinal DESC);

CREATE TABLE IF NOT EXISTS k12_weekly_arithmetic_commands (
    agent_name       TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    scope_id         TEXT NOT NULL,
    command_kind     TEXT NOT NULL CHECK(command_kind IN ('create','start','retry','attempt')),
    item_id          TEXT NOT NULL DEFAULT '',
    idempotency_key  TEXT NOT NULL,
    request_digest   TEXT NOT NULL CHECK(length(request_digest)=64),
    status           TEXT NOT NULL CHECK(status IN
        ('prepared','sent','succeeded','failed','outcome_unknown','committed')),
    result_json      TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(result_json)),
    result_digest    TEXT NOT NULL DEFAULT '',
    response_json    TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(response_json)),
    created_at       INTEGER NOT NULL CHECK(created_at > 0),
    updated_at       INTEGER NOT NULL CHECK(updated_at >= created_at),
    PRIMARY KEY(agent_name,scope_id,command_kind,item_id,idempotency_key)
);

CREATE TABLE IF NOT EXISTS k12_weekly_arithmetic_attempts (
    attempt_id       TEXT PRIMARY KEY,
    agent_name       TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    batch_id         TEXT NOT NULL REFERENCES k12_weekly_arithmetic_batches(batch_id) ON DELETE CASCADE,
    item_id          TEXT NOT NULL,
    idempotency_key  TEXT NOT NULL,
    request_digest   TEXT NOT NULL CHECK(length(request_digest)=64),
    attempt_json     TEXT NOT NULL CHECK(json_valid(attempt_json)),
    created_at       INTEGER NOT NULL CHECK(created_at > 0),
    UNIQUE(batch_id,item_id,idempotency_key)
);

CREATE TABLE IF NOT EXISTS k12_weekly_track_checkpoints (
    agent_name       TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    plan_id          TEXT NOT NULL REFERENCES k12_weekly_practice_plans(plan_id) ON DELETE CASCADE,
    plan_revision    INTEGER NOT NULL CHECK(plan_revision > 0),
    plan_section     TEXT NOT NULL CHECK(plan_section='textbook_consolidation'),
    checkpoint_json  TEXT NOT NULL CHECK(json_valid(checkpoint_json)),
    created_at       INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(agent_name,plan_id,plan_revision,plan_section)
);

CREATE TABLE IF NOT EXISTS k12_weekly_track_refresh_commands (
    agent_name       TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    plan_id          TEXT NOT NULL REFERENCES k12_weekly_practice_plans(plan_id) ON DELETE CASCADE,
    idempotency_key  TEXT NOT NULL,
    request_digest   TEXT NOT NULL CHECK(length(request_digest)=64),
    response_json    TEXT NOT NULL CHECK(json_valid(response_json)),
    created_revision INTEGER NOT NULL CHECK(created_revision IN (0,1)),
    created_at       INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(agent_name,plan_id,idempotency_key)
);
`
