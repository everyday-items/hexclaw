package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12GenericPrintJobsV25InstallsImmutableArtifactsAndDurableJobs(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE agents(name TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12GenericPrintJobsV25}); err != nil {
		t.Fatalf("apply V25: %v", err)
	}
	for _, table := range []string{"k12_print_artifacts", "k12_generic_print_jobs"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table %s missing: count=%d err=%v", table, count, err)
		}
	}
	for _, column := range []string{"source_kind", "source_ref", "title", "canonical_markdown", "source_digest"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('k12_print_artifacts') WHERE name=?`, column).Scan(&count); err != nil || count != 1 {
			t.Fatalf("immutable artifact column %s missing: count=%d err=%v", column, count, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('ming')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_print_artifacts
		(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
		VALUES('part-1','ming','prep_card','submission:s1','辅导要点','# 原稿','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE k12_print_artifacts SET canonical_markdown='# 被篡改' WHERE artifact_id='part-1'`); err == nil {
		t.Fatal("immutable printable artifact must reject UPDATE")
	}
}

func TestK12GenericPrintJobsV25IsRegisteredAfterKnowledgeV24(t *testing.T) {
	if K12GenericPrintJobsV25.Version != 25 {
		t.Fatalf("V25 version=%d", K12GenericPrintJobsV25.Version)
	}
	for i, migration := range All {
		if migration.Version == 25 {
			if i == 0 || All[i-1].Version != 24 {
				t.Fatalf("V25 predecessor is not V24")
			}
			return
		}
	}
	t.Fatal("V25 is not registered")
}

func TestK12GenericPrintJobsV25AgentCascadeClearsArtifactAndJobTogether(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA foreign_keys=ON; CREATE TABLE agents(name TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12GenericPrintJobsV25}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('ming');
		INSERT INTO k12_print_artifacts
		(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
		VALUES('part-1','ming','prep_card','submission:s1','辅导要点','# 原稿','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',1);
		INSERT INTO k12_generic_print_jobs
		(print_job_id,agent_name,idempotency_key,request_digest,artifact_id,status,attempt_count,
		 prepared_at,created_at,updated_at)
		VALUES('gprint-1','ming','click-1','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
		 'part-1','preparing',1,1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM agents WHERE name='ming'`); err != nil {
		t.Fatalf("Tutor deletion must cascade through job and artifact: %v", err)
	}
	for _, table := range []string{"k12_generic_print_jobs", "k12_print_artifacts"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows after owner deletion=%d err=%v", table, count, err)
		}
	}
}
