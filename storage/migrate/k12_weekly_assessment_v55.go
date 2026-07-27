package migrate

// K12WeeklyAssessmentV55 adds the durable physical-call ledger used by weekly
// answer assessment. The succeeded result is retained until its attempt and
// learning effects are committed in one local transaction.
var K12WeeklyAssessmentV55 = Migration{
	Version:     55,
	Description: "K12 本周该练：批改物理调用与单事务学习副作用",
	SQL:         K12WeeklyAssessmentV55DDL,
}

const K12WeeklyAssessmentV55DDL = `
CREATE TABLE IF NOT EXISTS k12_weekly_assessment_commands (
    command_id          TEXT    PRIMARY KEY,
    agent_name          TEXT    NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    snapshot_id         TEXT    NOT NULL REFERENCES k12_weekly_practice_snapshots(snapshot_id) ON DELETE CASCADE,
    item_id             TEXT    NOT NULL CHECK(length(trim(item_id)) > 0),
    idempotency_key     TEXT    NOT NULL CHECK(length(trim(idempotency_key)) > 0),
    request_digest      TEXT    NOT NULL CHECK(length(request_digest) = 64),
    status              TEXT    NOT NULL CHECK(status IN
        ('prepared','sent','succeeded','failed','outcome_unknown','committed')),
    assessment_json     TEXT    NOT NULL DEFAULT '',
    assessment_digest   TEXT    NOT NULL DEFAULT '',
    failure_kind        TEXT    NOT NULL DEFAULT '',
    attempt_id          TEXT    NOT NULL DEFAULT '',
    created_at          INTEGER NOT NULL CHECK(created_at > 0),
    updated_at          INTEGER NOT NULL CHECK(updated_at > 0),
    UNIQUE(agent_name,snapshot_id,item_id,idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_k12_weekly_assessment_recovery
    ON k12_weekly_assessment_commands(status,updated_at);
`
