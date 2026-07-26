package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12InsightSnapshotV41AddsTermColumnsWithoutGuessingLegacyRows(t *testing.T) {
	db, err := sql.Open("sqlite", "file:k12-insight-v41?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE k12_mistakes (
			record_id TEXT PRIMARY KEY,
			agent_name TEXT NOT NULL,
			status TEXT NOT NULL
		);
		CREATE TABLE k12_accumulations (
			record_id TEXT PRIMARY KEY,
			agent_name TEXT NOT NULL,
			status TEXT NOT NULL
		);
		CREATE TABLE k12_practice_sets (
			record_id TEXT PRIMARY KEY,
			agent_name TEXT NOT NULL,
			status TEXT NOT NULL
		);
		INSERT INTO k12_mistakes(record_id, agent_name, status) VALUES('m1','mingming','new');
		INSERT INTO k12_accumulations(record_id, agent_name, status) VALUES('a1','mingming','待复习');
		INSERT INTO k12_practice_sets(record_id, agent_name, status) VALUES('p1','mingming','draft');
	`); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12InsightSnapshotV41}); err != nil {
		t.Fatal(err)
	}
	for table, recordID := range map[string]string{
		"k12_mistakes":      "m1",
		"k12_accumulations": "a1",
		"k12_practice_sets": "p1",
	} {
		has, err := columnExists(context.Background(), db, table, "grade_term")
		if err != nil || !has {
			t.Fatalf("%s.grade_term exists=%v err=%v", table, has, err)
		}
		var gradeTerm string
		if err := db.QueryRow(
			`SELECT grade_term FROM `+table+` WHERE record_id=?`,
			recordID,
		).Scan(&gradeTerm); err != nil {
			t.Fatal(err)
		}
		if gradeTerm != "" {
			t.Fatalf("%s legacy row was guessed into term %q", table, gradeTerm)
		}
	}
}

func TestK12InsightSnapshotV41IsRegisteredInMigrationOrder(t *testing.T) {
	found := false
	previous := 0
	for _, migration := range All {
		if migration.Version <= previous {
			t.Fatalf("migrations are not strictly ordered: %d after %d", migration.Version, previous)
		}
		if migration.Version == K12InsightSnapshotV41.Version {
			found = true
		}
		previous = migration.Version
	}
	if !found {
		t.Fatalf("migration V%d is not registered", K12InsightSnapshotV41.Version)
	}
}
