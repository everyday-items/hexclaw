package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func migrationsBeforePracticeSourcesV86() []Migration {
	baseline := make([]Migration, 0, len(All))
	for _, migration := range All {
		if migration.Version < K12PracticeGenerationSourcesV86.Version {
			baseline = append(baseline, migration)
		}
	}
	return baseline
}

func TestK12PracticeGenerationSourcesV86MigratesLegacyAccumulationIntoSharedJob(t *testing.T) {
	db, err := sql.Open("sqlite", "file:k12-practice-sources-v86?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := Run(ctx, db, migrationsBeforePracticeSourcesV86()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO agents(name) VALUES('mingming');
		INSERT INTO k12_accumulations(
			record_id,agent_name,status,subject,entry_type,content,dedupe_key,
			created_at,updated_at,row_version
		) VALUES(
			'accumulation-1','mingming','active','语文','好词好句','桂花香',
			'accumulation-dedupe-1',10,11,3
		);
		INSERT INTO k12_practice_sets(
			record_id,agent_name,status,source_kind,title,dedupe_key,created_at,updated_at
		) VALUES(
			'practice-set-1','mingming','draft','mixed','待打印篮',
			'practice-set-dedupe-1',12,13
		);
		INSERT INTO k12_practice_set_items(
			set_record_id,item_index,item_id,subject,added_via,generation_status,
			question_markdown,expected_answer_markdown,verification_status,
			verification_evidence
		) VALUES(
			'practice-set-1',0,'dictation-generation-1','语文','accumulation','ready',
			'默写：桂花香','桂花香','verified','字符级比对'
		);
		INSERT INTO k12_accumulation_dictation_generations(
			generation_id,accumulation_id,agent_name,command_key,request_digest,status,
			source_snapshot_json,route_snapshot_json,practice_item_id,attempt,
			created_at,updated_at
		) VALUES(
			'generation-1','accumulation-1','mingming','dictation:accumulation-1',
			'digest-1','committed','{"content":"桂花香","subject":"语文"}',
			'{"provider":"rule","model":"dictation-format-v1"}',
			'dictation-generation-1',1,14,15
		);`); err != nil {
		t.Fatal(err)
	}

	if err := Run(ctx, db, []Migration{K12PracticeGenerationSourcesV86}); err != nil {
		t.Fatalf("apply V86: %v", err)
	}

	var sourceKind, sourceID, status, resultSetID, resultJSON string
	var sourceVersion int
	if err := db.QueryRow(`SELECT source_kind,source_id,source_version,status,
		result_set_id,result_item_ids_json
		FROM k12_practice_generation_jobs WHERE generation_job_id='generation-1'`).Scan(
		&sourceKind, &sourceID, &sourceVersion, &status, &resultSetID, &resultJSON,
	); err != nil {
		t.Fatal(err)
	}
	if sourceKind != "accumulation" || sourceID != "accumulation-1" ||
		sourceVersion != 3 || status != "committed" ||
		resultSetID != "practice-set-1" || resultJSON != `["dictation-generation-1"]` {
		t.Fatalf("shared migration mismatch: %s/%s/%d/%s/%s/%s",
			sourceKind, sourceID, sourceVersion, status, resultSetID, resultJSON)
	}
	var generationJobID, generationStatus string
	if err := db.QueryRow(`SELECT generation_job_id,generation_status
		FROM k12_practice_set_items WHERE set_record_id='practice-set-1' AND item_index=0`).Scan(
		&generationJobID, &generationStatus,
	); err != nil {
		t.Fatal(err)
	}
	if generationJobID != "generation-1" || generationStatus != "ready" {
		t.Fatalf("legacy committed item not attached to shared job: %q/%q",
			generationJobID, generationStatus)
	}
	if _, err := db.Exec(`UPDATE k12_accumulation_dictation_generations
		SET status='failed' WHERE generation_id='generation-1'`); err == nil {
		t.Fatal("legacy accumulation generation table remained writable after V86")
	}
	if _, err := db.Exec(`INSERT INTO k12_practice_generation_jobs(
		generation_job_id,agent_name,idempotency_key,request_digest,scope,
		variants_per_source,difficulty,total,status,source_kind,source_id,source_version,
		created_at,updated_at
	) VALUES(
		'generation-duplicate','mingming','different-command','different-digest','single',
		1,'same','1','queued','accumulation','accumulation-1',3,20,20
	)`); err == nil {
		t.Fatal("shared source identity accepted a second durable job")
	}
}

func TestK12PracticeGenerationSourcesV86RejectsLegacyGenerationIDCollision(t *testing.T) {
	db, err := sql.Open("sqlite", "file:k12-practice-sources-v86-collision?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := Run(ctx, db, migrationsBeforePracticeSourcesV86()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO agents(name) VALUES('mingming');
		INSERT INTO k12_accumulations(
			record_id,agent_name,status,subject,entry_type,content,dedupe_key,
			created_at,updated_at,row_version
		) VALUES(
			'accumulation-collision','mingming','active','语文','好词好句','桂花香',
			'accumulation-collision-dedupe',10,11,1
		);
		INSERT INTO k12_practice_generation_jobs(
			generation_job_id,agent_name,idempotency_key,request_digest,scope,
			variants_per_source,difficulty,total,textbook,status,result_item_ids_json,
			created_at,updated_at
		) VALUES(
			'generation-collision','mingming','unrelated-command','unrelated-digest',
			'custom',1,'same','1','','queued','[]',12,12
		);
		INSERT INTO k12_accumulation_dictation_generations(
			generation_id,accumulation_id,agent_name,command_key,request_digest,status,
			source_snapshot_json,route_snapshot_json,practice_item_id,attempt,
			created_at,updated_at
		) VALUES(
			'generation-collision','accumulation-collision','mingming',
			'dictation:accumulation-collision','dictation-digest','queued',
			'{"content":"桂花香","subject":"语文"}',
			'{"provider":"rule","model":"dictation-format-v1"}','',1,13,13
		);`); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, db, []Migration{K12PracticeGenerationSourcesV86}); err == nil {
		t.Fatal("V86 silently accepted a legacy generation ID collision")
	}
	var versionCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=86`).Scan(
		&versionCount,
	); err != nil {
		t.Fatal(err)
	}
	if versionCount != 0 {
		t.Fatalf("failed migration recorded V86 version rows=%d", versionCount)
	}
}

func TestK12PracticeGenerationSourcesV86IsRegisteredAndReplaySafe(t *testing.T) {
	db, err := sql.Open("sqlite", "file:k12-practice-sources-v86-fresh?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := Run(ctx, db, All); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("replay registered migrations: %v", err)
	}
	var versionCount, sourceColumns, auditTriggers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=86`).Scan(
		&versionCount,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('k12_practice_generation_jobs')
		WHERE name IN ('source_kind','source_id','source_version')`).Scan(&sourceColumns); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger'
		AND name LIKE 'k12_accum_dictation_audit_no_%'`).Scan(&auditTriggers); err != nil {
		t.Fatal(err)
	}
	if versionCount != 1 || sourceColumns != 3 || auditTriggers != 3 {
		t.Fatalf("registered V86 evidence version/columns/triggers=%d/%d/%d",
			versionCount, sourceColumns, auditTriggers)
	}
}

func TestK12PracticeGenerationSourcesV86SeparatesVersionedAndLegacyUniqueness(t *testing.T) {
	db, err := sql.Open("sqlite", "file:k12-practice-sources-v86-versioned?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := Run(ctx, db, All); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO agents(name) VALUES('mingming');
		INSERT INTO k12_practice_generation_jobs(
			generation_job_id,agent_name,idempotency_key,request_digest,scope,
			variants_per_source,difficulty,total,status,source_kind,source_id,
			source_version,created_at,updated_at
		) VALUES
			('versioned-1','mingming','versioned-command-1','versioned-digest-1',
			 'single',1,'same','1','queued','mistake','mistake-1',1,10,10),
			('versioned-2','mingming','versioned-command-2','versioned-digest-2',
			 'single',1,'same','1','queued','mistake','mistake-1',2,11,11);
		INSERT INTO k12_practice_generation_jobs(
			generation_job_id,agent_name,idempotency_key,request_digest,scope,
			variants_per_source,difficulty,total,status,source_mistake_id,
			created_at,updated_at
		) VALUES(
			'legacy-1','mingming','legacy-command-1','legacy-digest-1',
			'single',1,'same','1','queued','legacy-mistake-1',12,12
		);`); err != nil {
		t.Fatalf("insert versioned and first legacy jobs: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO k12_practice_generation_jobs(
		generation_job_id,agent_name,idempotency_key,request_digest,scope,
		variants_per_source,difficulty,total,status,source_mistake_id,
		created_at,updated_at
	) VALUES(
		'legacy-2','mingming','legacy-command-2','legacy-digest-2',
		'single',1,'same','1','generating','legacy-mistake-1',13,13
	)`); err == nil {
		t.Fatal("legacy source accepted a second active single generation")
	}
}
