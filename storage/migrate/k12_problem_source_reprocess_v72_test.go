package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12ProblemSourceReprocessV72IsRegisteredAndBackfillsAuditableInput(t *testing.T) {
	var registered *Migration
	preV72 := make([]Migration, 0, len(All))
	for index := range All {
		migration := All[index]
		if migration.Version <= 71 {
			preV72 = append(preV72, migration)
		}
		if migration.Version == 72 {
			registered = &All[index]
		}
	}
	if registered == nil {
		t.Fatal("migration v72 is not registered in migrate.All")
	}
	if registered.AtomicFunc == nil || registered.Func != nil || registered.SQL != "" {
		t.Fatalf("migration v72 must be one additive AtomicFunc: %+v", *registered)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := Run(ctx, db, preV72); err != nil {
		t.Fatalf("migrate legacy fixture through V71: %v", err)
	}
	seedK12ProblemSourceReprocessV72LegacyFixture(t, db)

	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("migrate legacy fixture through V72: %v", err)
	}

	var versionCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=72`).
		Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if versionCount != 1 {
		t.Fatalf("V72 migration ledger count=%d, want 1", versionCount)
	}
	for _, table := range []string{
		"k12_problem_input_revisions",
		"k12_page_assets",
		"k12_problem_source_reprocess_jobs",
	} {
		exists, tableErr := tableExists(ctx, db, table)
		if tableErr != nil || !exists {
			t.Fatalf("V72 table %s: exists=%v err=%v", table, exists, tableErr)
		}
	}
	for _, column := range []string{"request_json", "affected_problem_ids_json"} {
		exists, columnErr := columnExists(
			ctx,
			db,
			"k12_problem_source_action_receipts",
			column,
		)
		if columnErr != nil || !exists {
			t.Fatalf("receipt column %s: exists=%v err=%v", column, exists, columnErr)
		}
	}

	var requestJSON, affectedJSON string
	if err := db.QueryRow(`
SELECT request_json,affected_problem_ids_json
FROM k12_problem_source_action_receipts
WHERE command_receipt_id='receipt-v72-legacy'
`).Scan(&requestJSON, &affectedJSON); err != nil {
		t.Fatal(err)
	}
	if requestJSON != `{}` || affectedJSON != `[]` {
		t.Fatalf("legacy receipt invented request/exact-set: request=%q affected=%q", requestJSON, affectedJSON)
	}

	var (
		inputRevision, structureVersion                 int
		pageAssetID, stemRaw, answerRaw, answerBBox     string
		questionCanonical, answerCanonical, inputDigest string
		currentDisposition, originKind                  string
		sourceRegion, originCommandReceipt              sql.NullString
	)
	if err := db.QueryRow(`
SELECT structure_version,input_revision,page_asset_id,source_region_json,
       stem_raw,answer_raw,answer_bbox_json,
       question_canonical_markdown,answer_canonical_markdown,input_digest,
       current_disposition,origin_command_receipt_id,origin_kind
FROM k12_problem_input_revisions
WHERE agent_name='agent-v72' AND submission_id='submission-v72'
  AND problem_id='problem-v72'
`).Scan(
		&structureVersion,
		&inputRevision,
		&pageAssetID,
		&sourceRegion,
		&stemRaw,
		&answerRaw,
		&answerBBox,
		&questionCanonical,
		&answerCanonical,
		&inputDigest,
		&currentDisposition,
		&originCommandReceipt,
		&originKind,
	); err != nil {
		t.Fatal(err)
	}
	if structureVersion != 1 || inputRevision != 3 ||
		pageAssetID != "asset://agent-v72/legacy-page.png" || sourceRegion.Valid ||
		stemRaw != "raw question" || answerRaw != "raw answer" ||
		answerBBox != `{"x":0.1,"y":0.2,"w":0.3,"h":0.4}` ||
		questionCanonical != "canonical question" || answerCanonical != "canonical answer" ||
		inputDigest != "sha256:legacy-input" || currentDisposition != "current" ||
		originCommandReceipt.Valid || originKind != "legacy_unverified" {
		t.Fatalf(
			"legacy input backfill drift: structure=%d revision=%d asset=%q region=%v raw=(%q,%q,%q) canonical=(%q,%q) digest=%q disposition=%q receipt=%v origin=%q",
			structureVersion,
			inputRevision,
			pageAssetID,
			sourceRegion,
			stemRaw,
			answerRaw,
			answerBBox,
			questionCanonical,
			answerCanonical,
			inputDigest,
			currentDisposition,
			originCommandReceipt,
			originKind,
		)
	}

	// AttemptBBox remains answer-anchor evidence. V72 must not infer a
	// source-pixel crop from its incompatible normalized coordinate system.
	if sourceRegion.Valid {
		t.Fatalf("legacy source region was fabricated from AttemptBBox: %q", sourceRegion.String)
	}
	var unconfirmedHeads int
	if err := db.QueryRow(`
SELECT COUNT(*) FROM k12_problem_input_revisions
WHERE agent_name='agent-v72' AND submission_id='submission-v72'
  AND problem_id='problem-v72-unconfirmed'
`).Scan(&unconfirmedHeads); err != nil {
		t.Fatal(err)
	}
	if unconfirmedHeads != 0 {
		t.Fatalf("V72 backfilled %d synthetic heads for unconfirmed v0 Attempt", unconfirmedHeads)
	}
	if _, err := db.Exec(`
UPDATE k12_problem_input_revisions SET stem_raw='mutated'
WHERE agent_name='agent-v72' AND submission_id='submission-v72'
  AND structure_version=1 AND problem_id='problem-v72' AND input_revision=3
`); err == nil {
		t.Fatal("immutable input evidence accepted an in-place raw OCR update")
	}
	if _, err := db.Exec(`
UPDATE k12_problem_input_revisions
SET current_disposition='superseded',updated_at=updated_at+1
WHERE agent_name='agent-v72' AND submission_id='submission-v72'
  AND structure_version=1 AND problem_id='problem-v72' AND input_revision=3
`); err != nil {
		t.Fatalf("current-head disposition must remain CAS-mutable: %v", err)
	}
}

func TestK12ProblemSourceReprocessV72PageAssetAndLeaseConstraints(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("run full migration chain: %v", err)
	}
	seedK12ProblemSourceReprocessV72RuntimeParents(t, db)

	digest := strings.Repeat("b", 64)
	assetID := "asset://agent-v72/" + digest + ".png"
	if _, err := db.Exec(`
INSERT INTO k12_page_assets (
    owner_scope,page_asset_id,agent_name,content_digest,media_type,size_bytes,
    pixel_width,pixel_height,orientation_policy,orientation_policy_version,transform_chain_json,
    storage_state,ready_at,last_error,created_at,updated_at
) VALUES (
    'owner-v72',?,'agent-v72',?,'image/png',68,1,1,'unverified','unverified-v1','[]',
    'ready',100,'',100,100
)
`, assetID, digest); err != nil {
		t.Fatalf("insert verified owner-scoped PageAsset metadata: %v", err)
	}
	if _, err := db.Exec(`
INSERT INTO k12_page_assets (
    owner_scope,page_asset_id,agent_name,content_digest,media_type,size_bytes,
    pixel_width,pixel_height,orientation_policy,orientation_policy_version,transform_chain_json,
    storage_state,ready_at,last_error,created_at,updated_at
) VALUES (
    'other-owner',?,'agent-v72',?,'image/png',68,1,1,'unverified','unverified-v1','[]',
    'ready',100,'',100,100
)
`, assetID, digest); err == nil {
		t.Fatal("same agent PageAsset was rebound to another owner scope")
	}
	if _, err := db.Exec(`
UPDATE k12_page_assets SET pixel_width=2
WHERE owner_scope='owner-v72' AND page_asset_id=?
`, assetID); err == nil {
		t.Fatal("PageAsset identity metadata accepted an in-place dimension change")
	}

	uppercaseDigest := strings.Repeat("A", 64)
	if _, err := db.Exec(`
INSERT INTO k12_page_assets (
    owner_scope,page_asset_id,agent_name,content_digest,media_type,size_bytes,
    pixel_width,pixel_height,orientation_policy,orientation_policy_version,transform_chain_json,
    storage_state,ready_at,last_error,created_at,updated_at
) VALUES (
    'owner-v72','asset://agent-v72/' || ? || '.png','agent-v72',?,'image/png',
    68,1,1,'unverified','unverified-v1','[]','ready',100,'',100,100
)
`, uppercaseDigest, uppercaseDigest); err == nil {
		t.Fatal("PageAsset accepted a non-canonical uppercase digest")
	}

	stagedDigest := strings.Repeat("d", 64)
	stagedAssetID := "asset://agent-v72/" + stagedDigest + ".png"
	if _, err := db.Exec(`
INSERT INTO k12_page_assets (
    owner_scope,page_asset_id,agent_name,content_digest,media_type,size_bytes,
    pixel_width,pixel_height,orientation_policy,orientation_policy_version,transform_chain_json,
    storage_state,ready_at,last_error,created_at,updated_at
) VALUES (
    'owner-v72',?,'agent-v72',?,'image/png',68,1,1,'unverified','unverified-v1','[]',
    'staging',0,'',101,101
)
`, stagedAssetID, stagedDigest); err != nil {
		t.Fatalf("insert staged PageAsset metadata: %v", err)
	}
	if _, err := db.Exec(`
UPDATE k12_page_assets SET storage_state='ready'
WHERE owner_scope='owner-v72' AND page_asset_id=?
`, stagedAssetID); err == nil {
		t.Fatal("PageAsset became ready without a durable ready_at gate")
	}
	if _, err := db.Exec(`
UPDATE k12_page_assets SET storage_state='ready',ready_at=102,updated_at=102
WHERE owner_scope='owner-v72' AND page_asset_id=?
`, stagedAssetID); err != nil {
		t.Fatalf("valid PageAsset staging -> ready transition: %v", err)
	}
	var orientationPolicy, orientationPolicyVersion, storageState string
	var readyAt int
	if err := db.QueryRow(`
SELECT orientation_policy,orientation_policy_version,storage_state,ready_at
FROM k12_page_assets
WHERE owner_scope='owner-v72' AND page_asset_id=?
`, assetID).Scan(&orientationPolicy, &orientationPolicyVersion, &storageState, &readyAt); err != nil {
		t.Fatal(err)
	}
	if orientationPolicy != "unverified" || orientationPolicyVersion != "unverified-v1" ||
		storageState != "ready" || readyAt != 100 {
		t.Fatalf(
			"PageAsset readiness/orientation drift: orientation=%q version=%q state=%q ready_at=%d",
			orientationPolicy,
			orientationPolicyVersion,
			storageState,
			readyAt,
		)
	}

	requestJSON := `{"action":"correct_text","payload":{"question_canonical_markdown":"fixed"}}`
	affectedJSON := `["problem-v72"]`
	if _, err := db.Exec(`
INSERT INTO k12_problem_source_reprocess_jobs (
    work_id,command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,
    problem_id,action,structure_version,input_revision,input_digest,
    affected_problem_ids_json,request_json,status,created_at,updated_at
) VALUES (
    'work-v72','receipt-v72-runtime','owner-v72','agent-v72','dispatch-v72',
    'job-v72','problem-v72','correct_text',1,2,'sha256:v72-input',
    ?,?,'queued',100,100
)
`, affectedJSON, requestJSON); err != nil {
		t.Fatalf("insert durable source reprocess work: %v", err)
	}
	if _, err := db.Exec(`
UPDATE k12_problem_source_reprocess_jobs SET status='running'
WHERE work_id='work-v72'
`); err == nil {
		t.Fatal("running work without a lease was accepted")
	}
	if _, err := db.Exec(`
UPDATE k12_problem_source_reprocess_jobs
SET status='running',lease_owner='worker-v72',lease_epoch=1,
    lease_expires_at=200,attempt_count=1,updated_at=101
WHERE work_id='work-v72'
`); err != nil {
		t.Fatalf("valid leased claim failed: %v", err)
	}

	var gotAffected, gotRequest, status, leaseOwner string
	var leaseEpoch, leaseExpiresAt, attemptCount int
	if err := db.QueryRow(`
SELECT affected_problem_ids_json,request_json,status,lease_owner,lease_epoch,
       lease_expires_at,attempt_count
FROM k12_problem_source_reprocess_jobs WHERE work_id='work-v72'
`).Scan(
		&gotAffected,
		&gotRequest,
		&status,
		&leaseOwner,
		&leaseEpoch,
		&leaseExpiresAt,
		&attemptCount,
	); err != nil {
		t.Fatal(err)
	}
	if gotAffected != affectedJSON || gotRequest != requestJSON || status != "running" ||
		leaseOwner != "worker-v72" || leaseEpoch != 1 || leaseExpiresAt != 200 ||
		attemptCount != 1 {
		t.Fatalf(
			"lease-ready work drift: affected=%q request=%q status=%q owner=%q epoch=%d expires=%d attempts=%d",
			gotAffected,
			gotRequest,
			status,
			leaseOwner,
			leaseEpoch,
			leaseExpiresAt,
			attemptCount,
		)
	}
}

func TestK12ProblemSourceReprocessV72NoOpsWithoutK12Parents(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.Exec(`
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,description TEXT NOT NULL DEFAULT '',applied_at INTEGER NOT NULL
)
`); err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(ctx, db, K12ProblemSourceReprocessV72); err != nil {
		t.Fatalf("optional V72 without K12 schema: %v", err)
	}
	for _, table := range []string{
		"k12_problem_input_revisions",
		"k12_page_assets",
		"k12_problem_source_reprocess_jobs",
	} {
		exists, tableErr := tableExists(ctx, db, table)
		if tableErr != nil {
			t.Fatal(tableErr)
		}
		if exists {
			t.Fatalf("optional V72 created dangling table %s", table)
		}
	}
}

func seedK12ProblemSourceReprocessV72LegacyFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	seedK12ProblemSourceReprocessV72RuntimeParents(t, db)
	if _, err := db.Exec(`
INSERT INTO k12_attempts (
    attempt_id,agent_name,submission_id,problem_id,answer_state,answer_raw,
    answer_markdown,confirmed_version,input_digest,bbox_json,created_at,updated_at
) VALUES (
    'attempt-v72','agent-v72','submission-v72','problem-v72','present','raw answer',
    'canonical answer',3,'sha256:legacy-input',
    '{"x":0.1,"y":0.2,"w":0.3,"h":0.4}',90,95
);
INSERT INTO k12_problems (
    problem_id,agent_name,submission_id,page_asset_id,ordinal,problem_kind,
    parent_problem_id,subproblem_no,subject,stem_raw,stem_markdown,
    confirmation_required,confirmation_reasons_json,canonical_version,
    created_at,updated_at
) VALUES (
    'problem-v72-unconfirmed','agent-v72','submission-v72',
    'asset://agent-v72/legacy-page.png',1,'standalone',NULL,'','math',
    'unconfirmed raw question','unconfirmed canonical question',
    1,'["source_unclear"]',1,90,95
);
INSERT INTO k12_attempts (
    attempt_id,agent_name,submission_id,problem_id,answer_state,answer_raw,
    answer_markdown,confirmed_version,input_digest,bbox_json,created_at,updated_at
) VALUES (
    'attempt-v72-unconfirmed','agent-v72','submission-v72',
    'problem-v72-unconfirmed','present','unconfirmed raw answer',
    'unconfirmed canonical answer',0,'','',90,95
);
INSERT INTO k12_problem_structure_snapshots (
    agent_name,submission_id,structure_version,structure_digest,mapping_state,
    current_disposition,created_at,updated_at
) VALUES ('agent-v72','submission-v72',1,'sha256:structure-v72','resolved','current',80,95);
INSERT INTO k12_problem_structure_members (
    agent_name,submission_id,structure_version,problem_id,ordinal,problem_kind,
    parent_problem_id,subproblem_no,source_number_path_json,display_label,
    dependency_group_id,input_revision
) VALUES (
    'agent-v72','submission-v72',1,'problem-v72',0,'standalone','','','[]','1',
    'problem:problem-v72',3
),(
    'agent-v72','submission-v72',1,'problem-v72-unconfirmed',1,'standalone','','','[]','2',
    'problem:problem-v72-unconfirmed',1
);
INSERT INTO k12_problem_source_action_receipts (
    command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,problem_id,
    idempotency_key,request_digest,action,structure_version,
    expected_input_revision,result_input_revision,response_json,created_at,updated_at
) VALUES (
    'receipt-v72-legacy','owner-v72','agent-v72','dispatch-v72','job-v72','problem-v72',
    'receipt-v72-legacy-key',?,'correct_text',1,2,3,'{}',90,90
)
`, strings.Repeat("a", 64)); err != nil {
		t.Fatalf("seed V71 legacy source-action facts: %v", err)
	}
}

func seedK12ProblemSourceReprocessV72RuntimeParents(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
INSERT INTO agents (name) VALUES ('agent-v72');
INSERT INTO k12_grading_jobs (
    record_id,agent_name,status,submission_id,source_kind,idempotency_key,
    dedupe_key,created_at,updated_at
) VALUES (
    'job-v72','agent-v72','active','submission-v72','desktop','job-v72-key',
    'job-v72-dedupe',80,80
);
INSERT INTO k12_image_task_dispatches (
    dispatch_id,agent_name,learner_id,source_kind,source_ref,source_session_id,
    source_asset_refs_json,source_digest,message_intent,task_intent,
    intent_evidence_json,intent_confidence,confirmation_candidates_json,status,
    target_object_type,target_object_id,classification_route_snapshot_json,
    classification_invocation_id,route_policy_snapshot_json,idempotency_key,
    request_digest,attempt_generation,retry_safe,failure_kind,version,created_at,updated_at
) VALUES (
    'dispatch-v72','agent-v72','learner-v72','desktop','message-v72','session-v72',
    '["asset://agent-v72/legacy-page.png"]','sha256:dispatch-v72','grade','completed_homework',
    '[]',1,'[]','routed','homework_submission','submission-v72','{}',
    'invocation-v72','{}','dispatch-v72-key','sha256:request-v72',1,0,'',1,80,80
);
INSERT INTO k12_problems (
    problem_id,agent_name,submission_id,page_asset_id,ordinal,problem_kind,
    parent_problem_id,subproblem_no,subject,stem_raw,stem_markdown,
    confirmation_required,confirmation_reasons_json,canonical_version,
    created_at,updated_at
) VALUES (
    'problem-v72','agent-v72','submission-v72','asset://agent-v72/legacy-page.png',
    0,'standalone',NULL,'','math','raw question','canonical question',
    1,'["source_unclear"]',3,85,95
)
`); err != nil {
		t.Fatalf("seed V72 runtime parent facts: %v", err)
	}

	// The runtime-schema test starts after V72, so it must create the command
	// parent using the new normalized receipt columns. The legacy backfill test
	// intentionally supplies its pre-V72 receipt separately.
	hasRequest, err := columnExists(
		context.Background(),
		db,
		"k12_problem_source_action_receipts",
		"request_json",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRequest {
		return
	}
	if _, err := db.Exec(`
INSERT INTO k12_problem_source_action_receipts (
    command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,problem_id,
    idempotency_key,request_digest,action,structure_version,
    expected_input_revision,result_input_revision,response_json,created_at,updated_at,
    request_json,affected_problem_ids_json
) VALUES (
    'receipt-v72-runtime','owner-v72','agent-v72','dispatch-v72','job-v72','problem-v72',
    'receipt-v72-runtime-key',?,'correct_text',1,1,2,'{}',100,100,
    '{"action":"correct_text","payload":{"question_canonical_markdown":"fixed"}}',
    '["problem-v72"]'
)
`, strings.Repeat("c", 64)); err != nil {
		t.Fatalf("seed V72 runtime command receipt: %v", err)
	}
}
