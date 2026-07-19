package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12CreativeWorkOCRV20InstallsImmutableJobAndVersionLedger(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{"k12_creative_work_ocr_jobs", "k12_creative_work_ocr_versions"} {
		has, err := tableExists(context.Background(), db, table)
		if err != nil || !has {
			t.Fatalf("V20 table %s missing: has=%v err=%v", table, has, err)
		}
	}
	for _, column := range []string{
		"ocr_job_id", "ocr_raw", "ocr_version", "ocr_confirmed_digest", "content_confirmed_at",
	} {
		has, err := columnExists(context.Background(), db, "k12_creative_work_versions", column)
		if err != nil || !has {
			t.Fatalf("V20 work-version column %s missing: has=%v err=%v", column, has, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('kid')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_creative_work_ocr_jobs
		(job_id,agent_name,request_id,source_asset_id,source_digest,status,ocr_raw,error_message,
		 attempt_count,confirmed_version,confirmed_digest,created_at,updated_at)
		VALUES ('ocr-1','kid','request-1','asset://kid/a.png','asset-digest','awaiting_confirmation',
		        '逐字原稿','',1,0,'',100,100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE k12_creative_work_ocr_jobs SET ocr_raw='被覆盖' WHERE job_id='ocr-1'`); err == nil {
		t.Fatal("non-empty OCR raw evidence must be immutable")
	}
	if _, err := db.Exec(`INSERT INTO k12_creative_work_ocr_versions
		(job_id,version,content_markdown,content_digest,confirmed_at)
		VALUES ('ocr-1',1,'家长确认稿','digest-v1',101)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE k12_creative_work_ocr_versions SET content_markdown='改写' WHERE job_id='ocr-1' AND version=1`); err == nil {
		t.Fatal("confirmed canonical versions must be append-only")
	}
	if _, err := db.Exec(`DELETE FROM k12_creative_work_ocr_versions WHERE job_id='ocr-1' AND version=1`); err == nil {
		t.Fatal("confirmed canonical versions must not be deleted")
	}
}

func TestK12CreativeWorkOCRV20IsTheSingleNumberedMigration(t *testing.T) {
	found := 0
	for _, migration := range All {
		if migration.Version == 20 {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one V20 migration, got %d", found)
	}
}
