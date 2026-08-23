package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12LearningArchiveV82PreservesArtifactsAndLeavesLegacyWorkTermBlank(t *testing.T) {
	index := -1
	for i := range All {
		if All[i].Version == 82 {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatal("migration v82 is not registered")
	}
	if All[index].AtomicFunc == nil || All[index].Func != nil || All[index].SQL != "" {
		t.Fatalf("migration v82 must be atomic: %+v", All[index])
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := Run(ctx, db, All[:index]); err != nil {
		t.Fatalf("run migrations before v82: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('archive-v82')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_creative_works
		(record_id,agent_name,status,work_type,dedupe_key,created_at,updated_at)
		VALUES('legacy-work-v82','archive-v82','draft','writing','legacy-dedupe-v82',10,10)`); err != nil {
		t.Fatalf("seed legacy work: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO k12_print_artifacts
		(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
		VALUES('legacy-artifact-v82','archive-v82','practice_question','set-v82','Legacy','# legacy',?,10)`,
		strings.Repeat("a", 64)); err != nil {
		t.Fatalf("seed legacy artifact: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO k12_print_artifact_renders
		(artifact_id,format,render_contract_version,content_type,byte_digest,byte_size,payload,created_at)
		VALUES('legacy-artifact-v82','pdf','k12-pdf-v1','application/pdf',?,9,X'255044462D312E370A',10)`,
		strings.Repeat("c", 64)); err != nil {
		t.Fatalf("seed legacy render: %v", err)
	}

	if err := Run(ctx, db, All[:index+1]); err != nil {
		t.Fatalf("run v82: %v", err)
	}
	var gradeTerm, markdown string
	if err := db.QueryRow(`SELECT grade_term FROM k12_creative_works
		WHERE record_id='legacy-work-v82'`).Scan(&gradeTerm); err != nil {
		t.Fatal(err)
	}
	if gradeTerm != "" {
		t.Fatalf("legacy work grade_term=%q want blank", gradeTerm)
	}
	if err := db.QueryRow(`SELECT canonical_markdown FROM k12_print_artifacts
		WHERE artifact_id='legacy-artifact-v82'`).Scan(&markdown); err != nil {
		t.Fatal(err)
	}
	if markdown != "# legacy" {
		t.Fatalf("legacy artifact changed: %q", markdown)
	}
	var byteSize int
	var payloadHex string
	if err := db.QueryRow(`SELECT byte_size,hex(payload) FROM k12_print_artifact_renders
		WHERE artifact_id='legacy-artifact-v82'`).Scan(&byteSize, &payloadHex); err != nil {
		t.Fatalf("read preserved legacy render: %v", err)
	}
	if byteSize != 9 || payloadHex != "255044462D312E370A" {
		t.Fatalf("legacy render bytes changed: size=%d hex=%s", byteSize, payloadHex)
	}
	if _, err := db.Exec(`INSERT INTO k12_print_artifacts
		(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
		VALUES('archive-artifact-v82','archive-v82','learning_archive','五年级上','Archive','# archive',?,20)`,
		strings.Repeat("b", 64)); err != nil {
		t.Fatalf("learning archive source kind rejected: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO k12_print_artifacts
		(artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
		VALUES('old-kind-artifact-v82','archive-v82','practice_answer','set-v82-answer','Answer','# answer',?,20)`,
		strings.Repeat("d", 64)); err != nil {
		t.Fatalf("existing source kind rejected after v82: %v", err)
	}
	if _, err := db.Exec(`UPDATE k12_print_artifacts SET title='changed'
		WHERE artifact_id='archive-artifact-v82'`); err == nil {
		t.Fatal("v82 lost print artifact immutability")
	}
	var violations, versionCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=82`).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if violations != 0 || versionCount != 1 {
		t.Fatalf("v82 integrity/version=%d/%d want 0/1", violations, versionCount)
	}
}
