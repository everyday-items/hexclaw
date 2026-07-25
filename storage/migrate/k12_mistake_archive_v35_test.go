package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12MistakeArchiveV35AddsAuditableRestoreSnapshot(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE k12_mistakes (
        record_id TEXT PRIMARY KEY,
        agent_name TEXT NOT NULL,
        status TEXT NOT NULL,
        due_at INTEGER,
        version INTEGER NOT NULL DEFAULT 0
    )`); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12MistakeArchiveV35}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{
		"archived_reason", "archived_at", "archive_command_id",
		"archived_from_status", "archived_from_due_at",
		"archived_from_spot_check_state", "last_archive_snapshot_json",
	} {
		has, err := columnExists(context.Background(), db, "k12_mistakes", column)
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Errorf("missing k12_mistakes.%s", column)
		}
	}
}

func TestK12MistakeArchiveV35IsRegisteredAtItsNumber(t *testing.T) {
	if len(All) < K12MistakeArchiveV35.Version ||
		All[K12MistakeArchiveV35.Version-1].Version != K12MistakeArchiveV35.Version {
		t.Fatalf("migration %d is not registered at its numbered position", K12MistakeArchiveV35.Version)
	}
}

func TestK12MistakeArchiveV35RejectsArchiveReasonOnActiveRecord(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE k12_mistakes (
        record_id TEXT PRIMARY KEY,
        agent_name TEXT NOT NULL,
        status TEXT NOT NULL,
        due_at INTEGER,
        version INTEGER NOT NULL DEFAULT 0
    )`); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12MistakeArchiveV35}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_mistakes(record_id,agent_name,status)
        VALUES('m1','mingming','new')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE k12_mistakes SET archived_reason='manual'
        WHERE record_id='m1'`); err == nil {
		t.Fatal("active record accepted archived_reason")
	}
	if _, err := db.Exec(`UPDATE k12_mistakes
        SET status='archived', archived_reason='manual' WHERE record_id='m1'`); err != nil {
		t.Fatalf("archived record rejected manual reason: %v", err)
	}
}

func TestK12MistakeArchiveV35DoesNotInventSnapshotForLegacyArchivedRow(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE k12_mistakes (
        record_id TEXT PRIMARY KEY,
        agent_name TEXT NOT NULL,
        status TEXT NOT NULL,
        due_at INTEGER,
        version INTEGER NOT NULL DEFAULT 0
    );
    INSERT INTO k12_mistakes(record_id,agent_name,status,due_at,version)
        VALUES('legacy-archived','mingming','archived',123,7)`); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12MistakeArchiveV35}); err != nil {
		t.Fatal(err)
	}
	var reason, fromStatus, snapshot string
	var archivedAt int64
	if err := db.QueryRow(`SELECT archived_reason,archived_at,archived_from_status,
        last_archive_snapshot_json FROM k12_mistakes WHERE record_id='legacy-archived'`).
		Scan(&reason, &archivedAt, &fromStatus, &snapshot); err != nil {
		t.Fatal(err)
	}
	if reason != "" || archivedAt != 0 || fromStatus != "" || snapshot != "" {
		t.Fatalf("migration invented legacy archive facts: reason=%q at=%d from=%q snapshot=%q",
			reason, archivedAt, fromStatus, snapshot)
	}
}
