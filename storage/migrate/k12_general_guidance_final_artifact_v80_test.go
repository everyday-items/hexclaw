package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12GeneralGuidanceFinalArtifactV80PreservesRowsAndWidensOnlyCoverage(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := Run(ctx, db, All[:len(All)-1]); err != nil {
		t.Fatalf("run migration chain through V79: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('agent-v80')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_grading_jobs (
		record_id,agent_name,status,dedupe_key,created_at,updated_at
	) VALUES ('job-v80-old','agent-v80','projecting','dedupe-v80-old',100,100),
	         ('job-v80-new','agent-v80','projecting','dedupe-v80-new',100,100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_grading_final_artifacts (
		artifact_id,agent_name,job_id,structure_version,coverage_status,
		total_count,published_count,skipped_count,ordered_current_digests_json,
		canonical_markdown,artifact_digest,summary_invocation_id,created_at,updated_at,
		finalization_generation
	) VALUES ('artifact-v80-old','agent-v80','job-v80-old',1,'complete',
		1,1,0,'["receipt-v80-old"]','# existing',?,'summary-v80',100,100,3)`,
		strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("run V80 migration: %v", err)
	}
	var status string
	var generation int
	if err := db.QueryRow(`SELECT coverage_status,finalization_generation
		FROM k12_grading_final_artifacts WHERE artifact_id='artifact-v80-old'`).
		Scan(&status, &generation); err != nil || status != "complete" || generation != 3 {
		t.Fatalf("legacy artifact status=%q generation=%d err=%v", status, generation, err)
	}
	insert := `INSERT INTO k12_grading_final_artifacts (
		artifact_id,agent_name,job_id,structure_version,coverage_status,
		total_count,published_count,skipped_count,ordered_current_digests_json,
		canonical_markdown,artifact_digest,summary_invocation_id,created_at,updated_at,
		finalization_generation
	) VALUES (?,?,?,?,?,1,1,0,'["receipt-v80-new"]',?,?,?,100,100,0)`
	if _, err := db.Exec(insert,
		"artifact-v80-new", "agent-v80", "job-v80-new", 1, "general_guidance",
		"No verified textbook grounding is available.", strings.Repeat("b", 64), "",
	); err != nil {
		t.Fatalf("insert general guidance artifact: %v", err)
	}
	if _, err := db.Exec(`UPDATE k12_grading_final_artifacts
		SET skipped_count=1 WHERE artifact_id='artifact-v80-new'`); err == nil {
		t.Fatal("general guidance accepted an invalid skipped count")
	}
}
