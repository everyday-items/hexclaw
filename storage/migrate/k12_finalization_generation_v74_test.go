package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12FinalizationGenerationV74IsRegisteredAndBackfillsZeroBaseline(t *testing.T) {
	var registered *Migration
	preV74 := make([]Migration, 0, len(All))
	for index := range All {
		migration := All[index]
		if migration.Version <= 73 {
			preV74 = append(preV74, migration)
		}
		if migration.Version == 74 {
			registered = &All[index]
		}
	}
	if registered == nil {
		t.Fatal("migration v74 is not registered in migrate.All")
	}
	if registered.AtomicFunc == nil || registered.Func != nil || registered.SQL != "" {
		t.Fatalf("migration v74 must be one additive AtomicFunc: %+v", *registered)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := Run(ctx, db, preV74); err != nil {
		t.Fatalf("run migration chain through V73: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('agent-v74')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO k12_grading_jobs (
			record_id,agent_name,status,dedupe_key,created_at,updated_at
		) VALUES ('job-v74','agent-v74','projecting','dedupe-v74',100,100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO k12_grading_final_artifacts (
			artifact_id,agent_name,job_id,structure_version,coverage_status,
			total_count,published_count,skipped_count,
			ordered_current_digests_json,canonical_markdown,artifact_digest,
			summary_invocation_id,created_at,updated_at
		) VALUES (
			'artifact-v74','agent-v74','job-v74',1,'complete',
			1,1,0,'["receipt-v74"]','# final',?,
			'summary-v74',100,100
		)`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}

	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("migrate legacy fixture through V74: %v", err)
	}
	for _, table := range []string{
		"k12_grading_jobs",
		"k12_grading_final_artifacts",
	} {
		has, columnErr := columnExists(ctx, db, table, "finalization_generation")
		if columnErr != nil || !has {
			t.Fatalf("V74 column %s.finalization_generation: has=%v err=%v", table, has, columnErr)
		}
	}
	var jobGeneration, artifactGeneration int64
	if err := db.QueryRow(`
		SELECT j.finalization_generation,a.finalization_generation
		FROM k12_grading_jobs j
		JOIN k12_grading_final_artifacts a
		  ON a.agent_name=j.agent_name AND a.job_id=j.record_id
		WHERE j.agent_name='agent-v74' AND j.record_id='job-v74'
	`).Scan(&jobGeneration, &artifactGeneration); err != nil {
		t.Fatal(err)
	}
	if jobGeneration != 0 || artifactGeneration != 0 {
		t.Fatalf("legacy generation baseline: job=%d artifact=%d, want 0/0",
			jobGeneration, artifactGeneration)
	}
	if _, err := db.Exec(`
		UPDATE k12_grading_jobs
		SET finalization_generation=-1
		WHERE agent_name='agent-v74' AND record_id='job-v74'`); err == nil {
		t.Fatal("negative finalization generation unexpectedly accepted")
	}
	var versionCount int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM schema_migrations WHERE version=74`).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if versionCount != 1 {
		t.Fatalf("V74 migration ledger count=%d, want 1", versionCount)
	}
}
