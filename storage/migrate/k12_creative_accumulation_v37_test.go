package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12CreativeAccumulationV37IsAdditiveAndInstallsGenerationRoots(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
CREATE TABLE agents (name TEXT PRIMARY KEY);
CREATE TABLE k12_creative_works (
    record_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE k12_accumulations (
    record_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    source_ref TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE k12_work_feedback (
    work_record_id TEXT NOT NULL,
    version_index INTEGER NOT NULL,
    feedback_markdown TEXT NOT NULL DEFAULT '',
    feedback_source TEXT NOT NULL DEFAULT '',
    feedback_skill TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(work_record_id, version_index)
);`); err != nil {
		t.Fatal(err)
	}

	if err := Run(context.Background(), db, []Migration{K12CreativeAccumulationV37}); err != nil {
		t.Fatal(err)
	}

	for table, columns := range map[string][]string{
		"k12_creative_works": {
			"initial_feedback_generation_id", "latest_feedback_generation_id",
			"feedback_state", "row_version", "deleted_at", "deleted_by",
			"delete_command_key", "delete_receipt_json",
		},
		"k12_accumulations": {
			"derived_source_ref", "subject_provenance_json",
			"entry_type_provenance_json", "source_provenance_json",
			"row_version", "deleted_at", "deleted_by", "delete_command_key",
			"delete_receipt_json",
		},
	} {
		for _, column := range columns {
			has, err := columnExists(context.Background(), db, table, column)
			if err != nil {
				t.Fatal(err)
			}
			if !has {
				t.Fatalf("%s.%s missing", table, column)
			}
		}
	}
	for _, table := range []string{
		"k12_work_feedback_generations",
		"k12_accumulation_dictation_generations",
		"k12_current_create_receipts",
	} {
		var got string
		if err := db.QueryRow(`SELECT name FROM sqlite_master
			WHERE type='table' AND name=?`, table).Scan(&got); err != nil {
			t.Fatalf("%s missing: %v", table, err)
		}
	}

	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type='index' AND name='idx_k12_work_feedback_one_active'`).Scan(&ddl); err != nil {
		t.Fatal("work active-generation uniqueness missing:", err)
	}
	if !strings.Contains(strings.ToLower(ddl), "where") {
		t.Fatalf("work active-generation index must be partial: %s", ddl)
	}
	if err := db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type='index' AND name='idx_k12_accum_dictation_one_active'`).Scan(&ddl); err != nil {
		t.Fatal("accumulation active-generation uniqueness missing:", err)
	}
	if !strings.Contains(strings.ToLower(ddl), "where") {
		t.Fatalf("accumulation active-generation index must be partial: %s", ddl)
	}
}

func TestK12CreativeAccumulationV37BackfillsLegacyFeedbackWithoutDeletingIt(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
CREATE TABLE agents (name TEXT PRIMARY KEY);
INSERT INTO agents(name) VALUES ('mingming');
CREATE TABLE k12_creative_works (
    record_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    work_type TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE k12_accumulations (
    record_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    source_ref TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE k12_work_feedback (
    work_record_id TEXT NOT NULL,
    version_index INTEGER NOT NULL,
    feedback_markdown TEXT NOT NULL DEFAULT '',
    feedback_source TEXT NOT NULL DEFAULT '',
    feedback_skill TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(work_record_id, version_index)
);
INSERT INTO k12_creative_works(record_id,agent_name,work_type,created_at,updated_at)
VALUES ('work-1','mingming','writing',10,20);
INSERT INTO k12_work_feedback(work_record_id,version_index,feedback_markdown,feedback_source,feedback_skill)
VALUES
 ('work-1',0,'first','ai','writing-feedback@legacy'),
 ('work-1',2,'latest','ai','writing-feedback@legacy');`); err != nil {
		t.Fatal(err)
	}

	if err := Run(context.Background(), db, []Migration{K12CreativeAccumulationV37}); err != nil {
		t.Fatal(err)
	}

	var legacyCount, generationCount int
	if err := db.QueryRow(`SELECT count(*) FROM k12_work_feedback`).Scan(&legacyCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM k12_work_feedback_generations
		WHERE work_id='work-1'`).Scan(&generationCount); err != nil {
		t.Fatal(err)
	}
	if legacyCount != 2 || generationCount != 2 {
		t.Fatalf("legacy/generation counts=%d/%d want 2/2", legacyCount, generationCount)
	}
	var initialID, latestID, state, projection string
	if err := db.QueryRow(`SELECT initial_feedback_generation_id,
		latest_feedback_generation_id, feedback_state
		FROM k12_creative_works WHERE record_id='work-1'`).
		Scan(&initialID, &latestID, &state); err != nil {
		t.Fatal(err)
	}
	if initialID == "" || latestID == "" || initialID == latestID || state != "succeeded" {
		t.Fatalf("legacy pointers initial=%q latest=%q state=%q", initialID, latestID, state)
	}
	if err := db.QueryRow(`SELECT projection_markdown
		FROM k12_work_feedback_generations WHERE generation_id=?`, latestID).
		Scan(&projection); err != nil {
		t.Fatal(err)
	}
	if projection != "latest" {
		t.Fatalf("latest projection=%q", projection)
	}
}

func TestK12CreativeAccumulationV37IsLatestNumberedMigration(t *testing.T) {
	if len(All) == 0 || All[len(All)-1].Version != K12CreativeAccumulationV37.Version {
		t.Fatalf("latest migration=%d want %d", All[len(All)-1].Version, K12CreativeAccumulationV37.Version)
	}
}
