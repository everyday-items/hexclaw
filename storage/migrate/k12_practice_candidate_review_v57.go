package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

var K12PracticeCandidateReviewV57 = Migration{
	Version:     57,
	Description: "BUG-20260725-011/013/017: 候选题 Hash 原子提交与复习命令状态机",
	Func:        migrateK12PracticeCandidateReviewV57,
}

func migrateK12PracticeCandidateReviewV57(ctx context.Context, db *sql.DB) error {
	hasHash, err := columnExists(ctx, db, "k12_practice_set_items", "normalized_content_hash")
	if err != nil {
		return fmt.Errorf("检查 k12_practice_set_items.normalized_content_hash: %w", err)
	}
	if !hasHash {
		if _, err := db.ExecContext(ctx, `ALTER TABLE k12_practice_set_items
			ADD COLUMN normalized_content_hash TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("新增 k12_practice_set_items.normalized_content_hash: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, K12PracticeCandidateReviewV57DDL); err != nil {
		return fmt.Errorf("创建 K12 候选题/复习状态表: %w", err)
	}
	return nil
}

const K12PracticeCandidateReviewV57DDL = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_practice_items_content_hash
    ON k12_practice_set_items(set_record_id, normalized_content_hash)
    WHERE normalized_content_hash != '';

CREATE TABLE IF NOT EXISTS k12_practice_candidate_selections (
    selection_id          TEXT PRIMARY KEY,
    agent_name            TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    source_mistake_id     TEXT NOT NULL REFERENCES k12_mistakes(record_id) ON DELETE CASCADE,
    target_set_record_id  TEXT NOT NULL REFERENCES k12_practice_sets(record_id) ON DELETE CASCADE,
    state                 TEXT NOT NULL CHECK(state IN ('open','committed')),
    next_batch_ordinal    INTEGER NOT NULL DEFAULT 1 CHECK(next_batch_ordinal >= 1),
    revision              INTEGER NOT NULL DEFAULT 1 CHECK(revision >= 1),
    idempotency_key       TEXT NOT NULL,
    request_digest        TEXT NOT NULL CHECK(length(request_digest)=64),
    grade                 TEXT NOT NULL DEFAULT '',
    textbook              TEXT NOT NULL DEFAULT '',
    route_snapshot_json   TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(route_snapshot_json)),
    source_session_id     TEXT NOT NULL DEFAULT '',
    created_at            INTEGER NOT NULL CHECK(created_at > 0),
    updated_at            INTEGER NOT NULL CHECK(updated_at >= created_at),
    UNIQUE(agent_name,idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_k12_candidate_selection_source
    ON k12_practice_candidate_selections(agent_name,source_mistake_id,state,updated_at DESC);

CREATE TABLE IF NOT EXISTS k12_practice_candidates (
    candidate_id             TEXT PRIMARY KEY,
    selection_id             TEXT NOT NULL REFERENCES k12_practice_candidate_selections(selection_id) ON DELETE CASCADE,
    candidate_kind           TEXT NOT NULL CHECK(candidate_kind IN ('original','variant')),
    batch_ordinal            INTEGER NOT NULL CHECK(batch_ordinal >= 0),
    candidate_ordinal        INTEGER NOT NULL CHECK(candidate_ordinal >= 0),
    normalized_content_hash  TEXT NOT NULL DEFAULT '',
    state                    TEXT NOT NULL CHECK(state IN ('generating','ready','failed','already_in_set')),
    problem_json             TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(problem_json)),
    failure_message          TEXT NOT NULL DEFAULT '',
    batch_idempotency_key    TEXT NOT NULL DEFAULT '',
    created_at               INTEGER NOT NULL CHECK(created_at > 0),
    updated_at               INTEGER NOT NULL CHECK(updated_at >= created_at),
    UNIQUE(selection_id,batch_ordinal,candidate_ordinal),
    UNIQUE(selection_id,batch_idempotency_key,candidate_ordinal)
);
CREATE INDEX IF NOT EXISTS idx_k12_candidates_selection
    ON k12_practice_candidates(selection_id,batch_ordinal,candidate_ordinal);

CREATE TABLE IF NOT EXISTS k12_practice_candidate_commits (
    commit_id               TEXT PRIMARY KEY,
    agent_name              TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    selection_id            TEXT NOT NULL REFERENCES k12_practice_candidate_selections(selection_id) ON DELETE CASCADE,
    target_set_record_id    TEXT NOT NULL REFERENCES k12_practice_sets(record_id) ON DELETE CASCADE,
    selected_hashes_digest  TEXT NOT NULL CHECK(length(selected_hashes_digest)=64),
    added_count             INTEGER NOT NULL DEFAULT 0 CHECK(added_count >= 0),
    result_json             TEXT NOT NULL CHECK(json_valid(result_json)),
    request_digest          TEXT NOT NULL CHECK(length(request_digest)=64),
    idempotency_key         TEXT NOT NULL,
    created_at              INTEGER NOT NULL CHECK(created_at > 0)
    ,UNIQUE(agent_name,idempotency_key)
);

CREATE TABLE IF NOT EXISTS k12_mistake_review_states (
    agent_name           TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    mistake_record_id    TEXT NOT NULL REFERENCES k12_mistakes(record_id) ON DELETE CASCADE,
    state                TEXT NOT NULL CHECK(state IN
        ('scheduled','deferred_this_week','suppressed','mastered')),
    deferred_iso_year    INTEGER NOT NULL DEFAULT 0,
    deferred_iso_week    INTEGER NOT NULL DEFAULT 0 CHECK(deferred_iso_week BETWEEN 0 AND 53),
    prior_schedule_json  TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(prior_schedule_json)),
    revision             INTEGER NOT NULL DEFAULT 1 CHECK(revision >= 1),
    updated_at           INTEGER NOT NULL CHECK(updated_at > 0),
    PRIMARY KEY(agent_name,mistake_record_id)
);
CREATE INDEX IF NOT EXISTS idx_k12_mistake_review_state
    ON k12_mistake_review_states(agent_name,state,updated_at);

CREATE TABLE IF NOT EXISTS k12_mistake_review_commands (
    agent_name           TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    mistake_record_id    TEXT NOT NULL REFERENCES k12_mistakes(record_id) ON DELETE CASCADE,
    idempotency_key      TEXT NOT NULL,
    command_type         TEXT NOT NULL CHECK(command_type IN
        ('defer_this_week','suppress','restore')),
    from_state           TEXT NOT NULL CHECK(from_state IN
        ('scheduled','deferred_this_week','suppressed','mastered')),
    to_state             TEXT NOT NULL CHECK(to_state IN
        ('scheduled','deferred_this_week','suppressed','mastered')),
    prior_schedule_json  TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(prior_schedule_json)),
    request_digest       TEXT NOT NULL CHECK(length(request_digest)=64),
    result_json          TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(result_json)),
    created_at           INTEGER NOT NULL CHECK(created_at > 0),
    PRIMARY KEY(agent_name,idempotency_key)
);

INSERT OR IGNORE INTO k12_mistake_review_states
    (agent_name,mistake_record_id,state,deferred_iso_year,deferred_iso_week,
     prior_schedule_json,revision,updated_at)
SELECT agent_name,record_id,
       CASE status WHEN 'mastered' THEN 'mastered'
                   WHEN 'archived' THEN 'suppressed'
                   ELSE 'scheduled' END,
       0,0,
       CASE WHEN status='archived' THEN json_object(
           'state',CASE archived_from_status WHEN 'mastered' THEN 'mastered' ELSE 'scheduled' END,
           'due_at',archived_from_due_at)
       ELSE '{}' END,
       1,MAX(1,updated_at)
FROM k12_mistakes;

CREATE TRIGGER IF NOT EXISTS trg_k12_mistake_review_insert
AFTER INSERT ON k12_mistakes
BEGIN
    INSERT OR IGNORE INTO k12_mistake_review_states
        (agent_name,mistake_record_id,state,deferred_iso_year,deferred_iso_week,
         prior_schedule_json,revision,updated_at)
    VALUES(NEW.agent_name,NEW.record_id,
        CASE NEW.status WHEN 'mastered' THEN 'mastered'
                        WHEN 'archived' THEN 'suppressed'
                        ELSE 'scheduled' END,
        0,0,'{}',1,MAX(1,NEW.updated_at));
END;

CREATE TRIGGER IF NOT EXISTS trg_k12_mistake_review_mastery
AFTER UPDATE OF status ON k12_mistakes
WHEN NEW.status='mastered'
BEGIN
    INSERT INTO k12_mistake_review_states
        (agent_name,mistake_record_id,state,deferred_iso_year,deferred_iso_week,
         prior_schedule_json,revision,updated_at)
    VALUES(NEW.agent_name,NEW.record_id,'mastered',0,0,'{}',1,MAX(1,NEW.updated_at))
    ON CONFLICT(agent_name,mistake_record_id) DO UPDATE SET
        state='mastered',deferred_iso_year=0,deferred_iso_week=0,
        prior_schedule_json='{}',revision=k12_mistake_review_states.revision+1,
        updated_at=excluded.updated_at;
END;
`
