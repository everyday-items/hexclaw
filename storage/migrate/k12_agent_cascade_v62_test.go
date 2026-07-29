package migrate

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12AgentCascadeV62PreservesV37RowsAndCascadesAgentDelete(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/cascade.db?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY, description TEXT NOT NULL DEFAULT '', applied_at INTEGER NOT NULL
);
INSERT INTO schema_migrations(version,description,applied_at) VALUES(61,'pre-v62',1);
CREATE TABLE agents(name TEXT PRIMARY KEY);
CREATE TABLE k12_creative_works (
    record_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE
);
CREATE TABLE k12_accumulations (
    record_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE
);
CREATE TABLE k12_current_create_receipts (
    agent_name TEXT NOT NULL REFERENCES agents(name),
    object_kind TEXT NOT NULL CHECK(object_kind IN ('creative_work','accumulation')),
    command_key TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    object_id TEXT NOT NULL,
    receipt_json TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(agent_name, object_kind, command_key)
);
CREATE INDEX idx_k12_current_create_receipt_object
    ON k12_current_create_receipts(agent_name, object_kind, object_id);
CREATE TABLE k12_work_feedback_generations (
    generation_id TEXT PRIMARY KEY,
    work_id TEXT NOT NULL REFERENCES k12_creative_works(record_id),
    agent_name TEXT NOT NULL REFERENCES agents(name),
    generation_no INTEGER NOT NULL CHECK(generation_no > 0),
    command_key TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK(status IN ('queued','running','succeeded','failed')),
    feedback_type TEXT NOT NULL DEFAULT '',
    source_snapshot_json TEXT NOT NULL DEFAULT '{}',
    request_snapshot_json TEXT NOT NULL DEFAULT '{}',
    route_snapshot_json TEXT NOT NULL DEFAULT '{}',
    invocation_snapshot_json TEXT NOT NULL DEFAULT '{}',
    feedback_json TEXT NOT NULL DEFAULT '',
    projection_markdown TEXT NOT NULL DEFAULT '',
    failure_reason TEXT NOT NULL DEFAULT '',
    attempt INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(work_id, generation_no),
    UNIQUE(work_id, command_key)
);
CREATE UNIQUE INDEX idx_k12_work_feedback_one_active
    ON k12_work_feedback_generations(work_id)
    WHERE status IN ('queued','running');
CREATE INDEX idx_k12_work_feedback_owner_work
    ON k12_work_feedback_generations(agent_name, work_id, generation_no);
CREATE TRIGGER k12_work_feedback_generation_identity_immutable
BEFORE UPDATE OF generation_id, work_id, agent_name, generation_no, command_key,
                 request_digest, feedback_type, source_snapshot_json,
                 request_snapshot_json, route_snapshot_json
ON k12_work_feedback_generations
BEGIN
    SELECT RAISE(ABORT, 'work feedback generation identity is immutable');
END;
CREATE TABLE k12_accumulation_dictation_generations (
    generation_id TEXT PRIMARY KEY,
    accumulation_id TEXT NOT NULL REFERENCES k12_accumulations(record_id),
    agent_name TEXT NOT NULL REFERENCES agents(name),
    command_key TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK(status IN ('queued','generating','validating','committed','failed')),
    source_snapshot_json TEXT NOT NULL DEFAULT '{}',
    route_snapshot_json TEXT NOT NULL DEFAULT '{}',
    invocation_snapshot_json TEXT NOT NULL DEFAULT '{}',
    practice_item_id TEXT NOT NULL DEFAULT '',
    failure_reason TEXT NOT NULL DEFAULT '',
    attempt INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
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
INSERT INTO agents(name) VALUES('kid');
INSERT INTO k12_creative_works(record_id,agent_name) VALUES('work-1','kid');
INSERT INTO k12_accumulations(record_id,agent_name) VALUES('acc-1','kid');
INSERT INTO k12_current_create_receipts(
    agent_name,object_kind,command_key,request_digest,object_id,receipt_json,created_at
) VALUES('kid','creative_work','create-1','req-create','work-1','{"ok":true}',10);
INSERT INTO k12_work_feedback_generations(
    generation_id,work_id,agent_name,generation_no,command_key,request_digest,status,
    feedback_type,source_snapshot_json,request_snapshot_json,route_snapshot_json,
    invocation_snapshot_json,feedback_json,projection_markdown,failure_reason,attempt,
    created_at,updated_at
) VALUES(
    'feedback-1','work-1','kid',1,'feedback-command','req-feedback','failed',
    'art','{"source":1}','{"request":1}','{"route":1}',
    '{"invocation":1}','{"feedback":1}','projection','cancelled',1,20,30
);
INSERT INTO k12_accumulation_dictation_generations(
    generation_id,accumulation_id,agent_name,command_key,request_digest,status,
    source_snapshot_json,route_snapshot_json,invocation_snapshot_json,
    practice_item_id,failure_reason,attempt,created_at,updated_at
) VALUES(
    'dictation-1','acc-1','kid','dictation-command','req-dictation','committed',
    '{"source":2}','{"route":2}','{"invocation":2}',
    'practice-1','',1,40,50
);`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`DELETE FROM agents WHERE name='kid'`); err == nil {
		t.Fatal("unchanged V37 schema unexpectedly allowed Agent delete")
	}
	for _, table := range []string{
		"k12_current_create_receipts",
		"k12_work_feedback_generations",
		"k12_accumulation_dictation_generations",
	} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("V37 RED changed %s rows=%d err=%v", table, count, err)
		}
	}

	if err := Run(context.Background(), db, []Migration{K12AgentCascadeV62}); err != nil {
		t.Fatal(err)
	}
	for table, columns := range map[string][]string{
		"k12_work_feedback_generations": {
			"agent_name", "work_id",
		},
		"k12_accumulation_dictation_generations": {
			"agent_name", "accumulation_id",
		},
		"k12_current_create_receipts": {
			"agent_name",
		},
	} {
		for _, column := range columns {
			var action string
			if err := db.QueryRow(
				`SELECT on_delete FROM pragma_foreign_key_list(?) WHERE "from"=?`,
				table,
				column,
			).Scan(&action); err != nil {
				t.Fatalf("%s.%s foreign key: %v", table, column, err)
			}
			if action != "CASCADE" {
				t.Fatalf("%s.%s on_delete=%s want CASCADE", table, column, action)
			}
		}
	}
	for _, object := range []struct {
		kind string
		name string
	}{
		{"index", "idx_k12_current_create_receipt_object"},
		{"index", "idx_k12_work_feedback_one_active"},
		{"index", "idx_k12_work_feedback_owner_work"},
		{"index", "idx_k12_accum_dictation_practice_item"},
		{"index", "idx_k12_accum_dictation_one_active"},
		{"index", "idx_k12_accum_dictation_owner"},
		{"trigger", "k12_work_feedback_generation_identity_immutable"},
		{"trigger", "k12_accum_dictation_identity_immutable"},
	} {
		var count int
		if err := db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type=? AND name=?`,
			object.kind,
			object.name,
		).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s %s count=%d err=%v", object.kind, object.name, count, err)
		}
	}

	assertK12AgentCascadeRow(
		t,
		db,
		`SELECT agent_name,object_kind,command_key,request_digest,
		        object_id,receipt_json,created_at
		   FROM k12_current_create_receipts
		  WHERE agent_name='kid'`,
		"kid", "creative_work", "create-1", "req-create",
		"work-1", `{"ok":true}`, int64(10),
	)
	assertK12AgentCascadeRow(
		t,
		db,
		`SELECT generation_id,work_id,agent_name,generation_no,command_key,
		        request_digest,status,feedback_type,source_snapshot_json,
		        request_snapshot_json,route_snapshot_json,invocation_snapshot_json,
		        feedback_json,projection_markdown,failure_reason,attempt,
		        created_at,updated_at
		   FROM k12_work_feedback_generations
		  WHERE generation_id='feedback-1'`,
		"feedback-1", "work-1", "kid", int64(1), "feedback-command",
		"req-feedback", "failed", "art", `{"source":1}`,
		`{"request":1}`, `{"route":1}`, `{"invocation":1}`,
		`{"feedback":1}`, "projection", "cancelled", int64(1),
		int64(20), int64(30),
	)
	assertK12AgentCascadeRow(
		t,
		db,
		`SELECT generation_id,accumulation_id,agent_name,command_key,
		        request_digest,status,source_snapshot_json,route_snapshot_json,
		        invocation_snapshot_json,practice_item_id,failure_reason,attempt,
		        created_at,updated_at
		   FROM k12_accumulation_dictation_generations
		  WHERE generation_id='dictation-1'`,
		"dictation-1", "acc-1", "kid", "dictation-command",
		"req-dictation", "committed", `{"source":2}`, `{"route":2}`,
		`{"invocation":2}`, "practice-1", "", int64(1),
		int64(40), int64(50),
	)

	for name, fragments := range map[string][]string{
		"idx_k12_current_create_receipt_object": {
			"ON k12_current_create_receipts(agent_name, object_kind, object_id)",
		},
		"idx_k12_work_feedback_one_active": {
			"ON k12_work_feedback_generations(work_id)",
			"WHERE status IN ('queued','running')",
		},
		"idx_k12_work_feedback_owner_work": {
			"ON k12_work_feedback_generations(agent_name, work_id, generation_no)",
		},
		"idx_k12_accum_dictation_practice_item": {
			"ON k12_accumulation_dictation_generations(practice_item_id)",
			"WHERE practice_item_id != ''",
		},
		"idx_k12_accum_dictation_one_active": {
			"ON k12_accumulation_dictation_generations(accumulation_id)",
			"WHERE status IN ('queued','generating','validating')",
		},
		"idx_k12_accum_dictation_owner": {
			"ON k12_accumulation_dictation_generations(",
			"agent_name, accumulation_id, updated_at",
		},
		"k12_work_feedback_generation_identity_immutable": {
			"BEFORE UPDATE OF generation_id, work_id, agent_name, generation_no, command_key",
			"work feedback generation identity is immutable",
		},
		"k12_accum_dictation_identity_immutable": {
			"BEFORE UPDATE OF generation_id, accumulation_id, agent_name, command_key",
			"accumulation dictation generation identity is immutable",
		},
	} {
		var schemaSQL string
		if err := db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE name=?`,
			name,
		).Scan(&schemaSQL); err != nil {
			t.Fatalf("read %s SQL: %v", name, err)
		}
		for _, fragment := range fragments {
			if !strings.Contains(schemaSQL, fragment) {
				t.Fatalf("%s SQL lost %q: %s", name, fragment, schemaSQL)
			}
		}
	}
	if _, err := db.Exec(`
UPDATE k12_work_feedback_generations
   SET command_key='changed'
 WHERE generation_id='feedback-1'`); err == nil {
		t.Fatal("work feedback immutable trigger no longer rejects identity updates")
	}
	if _, err := db.Exec(`
UPDATE k12_accumulation_dictation_generations
   SET command_key='changed'
 WHERE generation_id='dictation-1'`); err == nil {
		t.Fatal("dictation immutable trigger no longer rejects identity updates")
	}

	var violations int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("foreign_key_check violations=%d", violations)
	}

	if _, err := db.Exec(`SAVEPOINT parent_cascade`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
DELETE FROM k12_creative_works WHERE record_id='work-1';
DELETE FROM k12_accumulations WHERE record_id='acc-1';`); err != nil {
		t.Fatalf("parent-object delete did not cascade generation rows: %v", err)
	}
	for _, table := range []string{
		"k12_work_feedback_generations",
		"k12_accumulation_dictation_generations",
	} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows=%d err=%v after parent delete", table, count, err)
		}
	}
	if _, err := db.Exec(`ROLLBACK TO parent_cascade; RELEASE parent_cascade`); err != nil {
		t.Fatalf("restore parent-cascade fixture: %v", err)
	}

	if _, err := db.Exec(`DELETE FROM agents WHERE name='kid'`); err != nil {
		t.Fatalf("Agent delete did not cascade V37 children: %v", err)
	}
	for _, table := range []string{
		"k12_current_create_receipts",
		"k12_work_feedback_generations",
		"k12_accumulation_dictation_generations",
	} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows=%d err=%v after Agent delete", table, count, err)
		}
	}
}

func TestK12AgentCascadeV62RollsBackSchemaAndVersionOnLateDDLFailure(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/rollback.db?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY, description TEXT NOT NULL DEFAULT '', applied_at INTEGER NOT NULL
);
INSERT INTO schema_migrations(version,description,applied_at) VALUES(61,'pre-v62',1);
CREATE TABLE agents(name TEXT PRIMARY KEY);
CREATE TABLE k12_current_create_receipts (
    agent_name TEXT NOT NULL REFERENCES agents(name),
    object_kind TEXT NOT NULL CHECK(object_kind IN ('creative_work','accumulation')),
    command_key TEXT NOT NULL,
    request_digest TEXT NOT NULL,
    object_id TEXT NOT NULL,
    receipt_json TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(agent_name, object_kind, command_key)
);
CREATE INDEX idx_k12_current_create_receipt_object
    ON k12_current_create_receipts(agent_name, object_kind, object_id);
CREATE TABLE k12_work_feedback_generations(generation_id TEXT PRIMARY KEY);
CREATE TABLE k12_work_feedback_generations_v62(generation_id TEXT PRIMARY KEY);
INSERT INTO agents(name) VALUES('kid');
INSERT INTO k12_current_create_receipts(
    agent_name,object_kind,command_key,request_digest,object_id,receipt_json,created_at
) VALUES('kid','creative_work','create-1','req-create','work-1','{"ok":true}',10);
`); err != nil {
		t.Fatal(err)
	}

	if err := Run(
		context.Background(),
		db,
		[]Migration{K12AgentCascadeV62},
	); err == nil {
		t.Fatal("forced late V62 DDL collision unexpectedly committed")
	}
	var maxVersion int
	if err := db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&maxVersion); err != nil {
		t.Fatal(err)
	}
	if maxVersion != 61 {
		t.Fatalf("failed V62 recorded schema version %d", maxVersion)
	}
	assertK12AgentCascadeRow(
		t,
		db,
		`SELECT agent_name,object_kind,command_key,request_digest,
		        object_id,receipt_json,created_at
		   FROM k12_current_create_receipts`,
		"kid", "creative_work", "create-1", "req-create",
		"work-1", `{"ok":true}`, int64(10),
	)
	var action string
	if err := db.QueryRow(`
SELECT on_delete
  FROM pragma_foreign_key_list('k12_current_create_receipts')
 WHERE "from"='agent_name'`).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if action != "NO ACTION" {
		t.Fatalf("failed V62 leaked rebuilt FK action %q", action)
	}
	for _, table := range []string{
		"k12_current_create_receipts",
		"k12_work_feedback_generations",
		"k12_work_feedback_generations_v62",
	} {
		var count int
		if err := db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`,
			table,
		).Scan(&count); err != nil || count != 1 {
			t.Fatalf("failed V62 table %s count=%d err=%v", table, count, err)
		}
	}
}

func assertK12AgentCascadeRow(
	t *testing.T,
	db *sql.DB,
	query string,
	want ...any,
) {
	t.Helper()
	got := make([]any, len(want))
	destinations := make([]any, len(want))
	for index := range got {
		destinations[index] = &got[index]
	}
	if err := db.QueryRow(query).Scan(destinations...); err != nil {
		t.Fatal(err)
	}
	for index := range got {
		if raw, ok := got[index].([]byte); ok {
			got[index] = string(raw)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("row drift:\n got: %#v\nwant: %#v", got, want)
	}
}
