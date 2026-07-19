package migrate

// K12RestoreAsV17DDL persists DD-025 restore-as evidence. Original archives,
// pre-restore snapshots and journal rows are immutable at the database boundary;
// only the migration receipt may advance from completed to rolled_back.
const K12RestoreAsV17DDL = `
CREATE TABLE IF NOT EXISTS k12_restore_archives (
    archive_digest  TEXT PRIMARY KEY,
    archive_version INTEGER NOT NULL,
    source_agent    TEXT NOT NULL,
    checksum        TEXT NOT NULL,
    archive_json    TEXT NOT NULL,
    created_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS k12_restore_snapshots (
    snapshot_digest TEXT PRIMARY KEY,
    target_agent    TEXT NOT NULL,
    checksum        TEXT NOT NULL,
    snapshot_json   TEXT NOT NULL,
    created_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS k12_restore_migrations (
    migration_id            TEXT PRIMARY KEY,
    source_agent            TEXT NOT NULL,
    target_agent            TEXT NOT NULL,
    idempotency_key         TEXT NOT NULL,
    original_archive_digest TEXT NOT NULL REFERENCES k12_restore_archives(archive_digest),
    migrated_checksum       TEXT NOT NULL,
    snapshot_digest         TEXT NOT NULL REFERENCES k12_restore_snapshots(snapshot_digest),
    status                  TEXT NOT NULL CHECK(status IN ('completed','rolled_back')),
    restored_count          INTEGER NOT NULL,
    created_at              INTEGER NOT NULL,
    completed_at            INTEGER NOT NULL,
    rolled_back_at          INTEGER NOT NULL DEFAULT 0,
    UNIQUE(target_agent, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_k12_restore_migrations_target
    ON k12_restore_migrations(target_agent, created_at);

CREATE TABLE IF NOT EXISTS k12_restore_asset_migrations (
    migration_id    TEXT NOT NULL REFERENCES k12_restore_migrations(migration_id) ON DELETE RESTRICT,
    ordinal         INTEGER NOT NULL,
    source_asset_id TEXT NOT NULL,
    target_asset_id TEXT NOT NULL,
    sha256          TEXT NOT NULL,
    mime            TEXT NOT NULL,
    created_new     INTEGER NOT NULL CHECK(created_new IN (0,1)),
    PRIMARY KEY(migration_id, ordinal),
    UNIQUE(migration_id, source_asset_id),
    UNIQUE(migration_id, target_asset_id)
);

CREATE TABLE IF NOT EXISTS k12_restore_journal (
    migration_id TEXT NOT NULL REFERENCES k12_restore_migrations(migration_id) ON DELETE RESTRICT,
    ordinal      INTEGER NOT NULL,
    operation    TEXT NOT NULL CHECK(operation IN ('preserve_archive','snapshot_target','rewrite_owner','migrate_asset','replace_profile','seal_migrated','rollback_snapshot','rollback_asset_cleanup')),
    entity_kind  TEXT NOT NULL,
    entity_id    TEXT NOT NULL,
    before_json  TEXT NOT NULL DEFAULT '',
    after_json   TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    PRIMARY KEY(migration_id, ordinal)
);
CREATE INDEX IF NOT EXISTS idx_k12_restore_journal_migration
    ON k12_restore_journal(migration_id, ordinal);

CREATE TRIGGER IF NOT EXISTS k12_restore_archives_no_update
BEFORE UPDATE ON k12_restore_archives BEGIN
    SELECT RAISE(ABORT, 'k12 restore archive is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_restore_archives_no_delete
BEFORE DELETE ON k12_restore_archives BEGIN
    SELECT RAISE(ABORT, 'k12 restore archive is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_restore_snapshots_no_update
BEFORE UPDATE ON k12_restore_snapshots BEGIN
    SELECT RAISE(ABORT, 'k12 restore snapshot is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_restore_snapshots_no_delete
BEFORE DELETE ON k12_restore_snapshots BEGIN
    SELECT RAISE(ABORT, 'k12 restore snapshot is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_restore_journal_no_update
BEFORE UPDATE ON k12_restore_journal BEGIN
    SELECT RAISE(ABORT, 'k12 restore journal is append-only');
END;
CREATE TRIGGER IF NOT EXISTS k12_restore_journal_no_delete
BEFORE DELETE ON k12_restore_journal BEGIN
    SELECT RAISE(ABORT, 'k12 restore journal is append-only');
END;
CREATE TRIGGER IF NOT EXISTS k12_restore_asset_migrations_no_update
BEFORE UPDATE ON k12_restore_asset_migrations BEGIN
    SELECT RAISE(ABORT, 'k12 restore asset migration is immutable');
END;
CREATE TRIGGER IF NOT EXISTS k12_restore_asset_migrations_no_delete
BEFORE DELETE ON k12_restore_asset_migrations BEGIN
    SELECT RAISE(ABORT, 'k12 restore asset migration is immutable');
END;`
