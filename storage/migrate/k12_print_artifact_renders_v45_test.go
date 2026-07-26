package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12PrintArtifactRendersV45IsRegisteredAndImmutable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatal(err)
	}
	var tableCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name='k12_print_artifact_renders'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 1 {
		t.Fatalf("k12_print_artifact_renders table count=%d", tableCount)
	}
	var v45Index = -1
	for i, migration := range All {
		if migration.Version == 45 {
			v45Index = i
			break
		}
	}
	if v45Index < 1 || All[v45Index-1].Version != 44 {
		t.Fatalf("V45 must be registered directly after V44, index=%d", v45Index)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('v45-agent');
		INSERT INTO k12_print_artifacts
		(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
		VALUES('part-v45','v45-agent','tutoring_tips','submission:v45','辅导要点','# 内容',
		'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',1);
		INSERT INTO k12_print_artifact_renders
		(artifact_id,format,render_contract_version,content_type,byte_digest,byte_size,payload,created_at)
		VALUES('part-v45','pdf','k12-pdf-v1','application/pdf',
		'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
		9,X'255044462D312E370A',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE k12_print_artifact_renders SET byte_size=10 WHERE artifact_id='part-v45'`); err == nil {
		t.Fatal("frozen PDF render must reject UPDATE")
	}
}
