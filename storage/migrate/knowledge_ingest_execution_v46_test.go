package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestKnowledgeIngestExecutionV46IsRegisteredWithDurableContracts(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"kb_ingest_execution_snapshots",
		"kb_ingest_segments",
		"kb_job_failures",
	} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
			WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s table count=%d", table, count)
		}
	}

	v46Index := -1
	for i, migration := range All {
		if migration.Version == 46 {
			v46Index = i
			break
		}
	}
	if v46Index < 1 || All[v46Index-1].Version != 45 {
		t.Fatalf("V46 must be registered directly after V45, index=%d", v46Index)
	}

	for _, trigger := range []string{
		"trg_kb_ingest_execution_snapshots_immutable",
		"trg_kb_ingest_segments_identity_immutable",
		"trg_kb_job_failures_immutable",
	} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
			WHERE type='trigger' AND name=?`, trigger).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s trigger count=%d", trigger, count)
		}
	}
}
