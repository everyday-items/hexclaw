package migrate

// K12CutoverV16DDL persists the DD-005/DD-006 chain cutover state and
// migration journal. It deliberately lives in the numbered migration layer;
// the release manager never auto-creates production tables.
const K12CutoverV16DDL = `
CREATE TABLE IF NOT EXISTS k12_cutover_chains (
    chain TEXT PRIMARY KEY,
    state TEXT NOT NULL CHECK(state IN ('old','switching','new','rolling_back')),
    active_run_id TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS k12_cutover_entrypoints (
    chain TEXT NOT NULL REFERENCES k12_cutover_chains(chain) ON DELETE CASCADE,
    entrypoint TEXT NOT NULL,
    implementation TEXT NOT NULL CHECK(implementation IN ('old','new')),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(chain, entrypoint)
);
CREATE TABLE IF NOT EXISTS k12_cutover_runs (
    run_id TEXT PRIMARY KEY,
    chain TEXT NOT NULL REFERENCES k12_cutover_chains(chain),
    status TEXT NOT NULL CHECK(status IN ('switching','completed','rolling_back','rolled_back')),
    backup_digest TEXT NOT NULL,
    failure_detail TEXT NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS k12_migration_journal (
    run_id TEXT NOT NULL REFERENCES k12_cutover_runs(run_id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    chain TEXT NOT NULL,
    entity_kind TEXT NOT NULL,
    entity_id TEXT NOT NULL,
    operation TEXT NOT NULL,
    before_json TEXT NOT NULL DEFAULT '',
    after_json TEXT NOT NULL DEFAULT '',
    external_effect TEXT NOT NULL DEFAULT '',
    compensation_status TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    PRIMARY KEY(run_id, ordinal)
);
CREATE INDEX IF NOT EXISTS idx_k12_migration_journal_chain
    ON k12_migration_journal(chain, run_id, ordinal);`
