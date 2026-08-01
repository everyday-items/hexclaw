package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12TextbookCatalogWorkerV67AddsDurableWorkerState(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{
		"ingest_job_id", "source_plan_digest", "extractor_contract",
		"next_attempt_at", "heartbeat_at", "failure_code",
	} {
		has, err := columnExists(context.Background(), db, "k12_textbook_catalog_jobs", column)
		if err != nil || !has {
			t.Fatalf("worker column %s missing: has=%v err=%v", column, has, err)
		}
	}
	var trigger int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type='trigger' AND name='k12_textbook_catalog_source_snapshot_immutable'`).Scan(
		&trigger,
	); err != nil || trigger != 1 {
		t.Fatalf("immutable source snapshot trigger=%d err=%v", trigger, err)
	}
}
