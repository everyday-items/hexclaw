package migrate

// K12ProblemAttemptsDDL is intentionally unnumbered. The migration owner registers it in the
// release sequence (planned V19); runtime stores never auto-create release-governed schema.
const K12ProblemAttemptsDDL = `
CREATE TABLE IF NOT EXISTS k12_problems (
    problem_id TEXT NOT NULL,
    agent_name TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    submission_id TEXT NOT NULL,
    page_asset_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    problem_kind TEXT NOT NULL CHECK(problem_kind IN ('standalone','compound_parent','subproblem')),
    parent_problem_id TEXT,
    subproblem_no TEXT NOT NULL DEFAULT '',
    subject TEXT NOT NULL DEFAULT '',
    stem_raw TEXT NOT NULL,
    stem_markdown TEXT NOT NULL,
    concept_ids_json TEXT NOT NULL DEFAULT '[]',
    transcription_confidence REAL,
    confirmation_required INTEGER NOT NULL DEFAULT 0 CHECK(confirmation_required IN (0,1)),
    confirmation_reasons_json TEXT NOT NULL DEFAULT '[]',
    canonical_version INTEGER NOT NULL DEFAULT 1 CHECK(canonical_version >= 1),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(agent_name, problem_id),
    FOREIGN KEY(agent_name, parent_problem_id)
        REFERENCES k12_problems(agent_name, problem_id),
    CHECK((problem_kind='subproblem' AND parent_problem_id IS NOT NULL AND subproblem_no!='') OR
          (problem_kind!='subproblem' AND parent_problem_id IS NULL AND subproblem_no=''))
);
CREATE INDEX IF NOT EXISTS idx_k12_problems_submission
    ON k12_problems(agent_name, submission_id, page_asset_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_problem_ordinal
    ON k12_problems(agent_name, submission_id, ordinal);
CREATE UNIQUE INDEX IF NOT EXISTS idx_k12_subproblem_no
    ON k12_problems(agent_name, submission_id, parent_problem_id, subproblem_no)
    WHERE problem_kind='subproblem';

CREATE TABLE IF NOT EXISTS k12_attempts (
    attempt_id TEXT NOT NULL,
    agent_name TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
    submission_id TEXT NOT NULL,
    problem_id TEXT NOT NULL,
    answer_state TEXT NOT NULL CHECK(answer_state IN ('blank','present','unclear')),
    answer_raw TEXT NOT NULL DEFAULT '',
    answer_markdown TEXT NOT NULL DEFAULT '',
    confirmed_version INTEGER NOT NULL DEFAULT 0 CHECK(confirmed_version >= 0),
    input_digest TEXT NOT NULL DEFAULT '',
    bbox_json TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(agent_name, attempt_id),
    FOREIGN KEY(agent_name, problem_id)
        REFERENCES k12_problems(agent_name, problem_id) ON DELETE CASCADE,
    UNIQUE(agent_name, submission_id, problem_id),
    CHECK((answer_state='present' AND answer_markdown!='') OR answer_state!='present'),
    CHECK((confirmed_version=0 AND input_digest='') OR (confirmed_version>0 AND input_digest!=''))
);
CREATE INDEX IF NOT EXISTS idx_k12_attempts_submission
    ON k12_attempts(agent_name, submission_id, problem_id);`
