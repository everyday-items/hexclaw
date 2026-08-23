package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12AccumulationDictationReAddV79PreservesRowsAndExtendsStateConstraint(t *testing.T) {
	var migration *Migration
	for index := range All {
		if All[index].Version == 79 {
			migration = &All[index]
			break
		}
	}
	if migration == nil {
		t.Fatal("migration v79 is not registered in migrate.All")
	}
	if migration.AtomicFunc == nil || migration.Func != nil || migration.SQL != "" {
		t.Fatalf("migration v79 must be one atomic table rebuild: %+v", migration)
	}

	db := openV79LegacyDictationDB(t)
	if err := applyMigration(context.Background(), db, *migration); err != nil {
		t.Fatalf("apply V79: %v", err)
	}
	var status, itemID, requestDigest, sourceSnapshot string
	var attempt int
	if err := db.QueryRow(`SELECT status,practice_item_id,request_digest,
		source_snapshot_json,attempt
		FROM k12_accumulation_dictation_generations
		WHERE generation_id='generation-1'`).
		Scan(&status, &itemID, &requestDigest, &sourceSnapshot, &attempt); err != nil {
		t.Fatal(err)
	}
	if status != "committed" || itemID != "practice-item-1" ||
		requestDigest != "sha256:request" || sourceSnapshot != `{"content":"桂花香"}` ||
		attempt != 1 {
		t.Fatalf("V79 changed historical generation: %s/%s/%s/%s/%d",
			status, itemID, requestDigest, sourceSnapshot, attempt)
	}
	if _, err := db.Exec(`UPDATE k12_accumulation_dictation_generations
		SET status='re_add',practice_item_id=''
		WHERE generation_id='generation-1'`); err != nil {
		t.Fatalf("V79 rejected re_add state: %v", err)
	}
	if _, err := db.Exec(`UPDATE k12_accumulation_dictation_generations
		SET status='removed_failure' WHERE generation_id='generation-1'`); err == nil {
		t.Fatal("V79 accepted an invalid dictation state")
	}
	if _, err := db.Exec(`UPDATE k12_accumulation_dictation_generations
		SET request_digest='changed' WHERE generation_id='generation-1'`); err == nil {
		t.Fatal("V79 lost immutable generation identity trigger")
	}
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type='table' AND name='k12_accumulation_dictation_generations'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "'re_add'") || !strings.Contains(ddl, "ON DELETE CASCADE") {
		t.Fatalf("V79 table contract incomplete: %s", ddl)
	}
	var violations, versionCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=79`).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if violations != 0 || versionCount != 1 {
		t.Fatalf("V79 integrity/version=%d/%d, want 0/1", violations, versionCount)
	}
}

func openV79LegacyDictationDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
PRAGMA foreign_keys=ON;
CREATE TABLE schema_migrations(
    version INTEGER PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    applied_at INTEGER NOT NULL
);
CREATE TABLE agents(name TEXT PRIMARY KEY);
CREATE TABLE k12_accumulations(
    record_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE
);
CREATE TABLE k12_accumulation_dictation_generations (
    generation_id            TEXT PRIMARY KEY,
    accumulation_id          TEXT NOT NULL
                                  REFERENCES k12_accumulations(record_id)
                                  ON DELETE CASCADE,
    agent_name               TEXT NOT NULL
                                  REFERENCES agents(name)
                                  ON DELETE CASCADE,
    command_key              TEXT NOT NULL,
    request_digest           TEXT NOT NULL,
    status                   TEXT NOT NULL DEFAULT 'queued'
                                  CHECK(status IN ('queued','generating','validating','committed','failed')),
    source_snapshot_json     TEXT NOT NULL DEFAULT '{}',
    route_snapshot_json      TEXT NOT NULL DEFAULT '{}',
    invocation_snapshot_json TEXT NOT NULL DEFAULT '{}',
    practice_item_id         TEXT NOT NULL DEFAULT '',
    failure_reason           TEXT NOT NULL DEFAULT '',
    attempt                  INTEGER NOT NULL DEFAULT 0,
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL,
    UNIQUE(accumulation_id, command_key)
);
CREATE UNIQUE INDEX idx_k12_accum_dictation_practice_item
    ON k12_accumulation_dictation_generations(practice_item_id)
    WHERE practice_item_id != '';
CREATE UNIQUE INDEX idx_k12_accum_dictation_one_active
    ON k12_accumulation_dictation_generations(accumulation_id)
    WHERE status IN ('queued','generating','validating');
CREATE INDEX idx_k12_accum_dictation_owner
    ON k12_accumulation_dictation_generations(agent_name, accumulation_id, updated_at);
CREATE TRIGGER k12_accum_dictation_identity_immutable
BEFORE UPDATE OF generation_id, accumulation_id, agent_name, command_key,
                 request_digest, source_snapshot_json, route_snapshot_json
ON k12_accumulation_dictation_generations
BEGIN
    SELECT RAISE(ABORT, 'accumulation dictation generation identity is immutable');
END;
INSERT INTO agents(name) VALUES('mingming');
INSERT INTO k12_accumulations(record_id,agent_name) VALUES('accumulation-1','mingming');
INSERT INTO k12_accumulation_dictation_generations(
    generation_id,accumulation_id,agent_name,command_key,request_digest,status,
    source_snapshot_json,practice_item_id,attempt,created_at,updated_at
) VALUES(
    'generation-1','accumulation-1','mingming','dictation:accumulation-1',
    'sha256:request','committed','{"content":"桂花香"}','practice-item-1',1,10,20
);`); err != nil {
		t.Fatalf("create V79 legacy schema: %v", err)
	}
	return db
}
