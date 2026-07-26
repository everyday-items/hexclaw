package migrate

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12ManualCreativeIntakeV34AddsExplicitCommitLedger(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Run(context.Background(), db, All[:33]); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), db, []Migration{K12ManualCreativeIntakeV34}); err != nil {
		t.Fatal(err)
	}
	for table, columns := range map[string][]string{
		"k12_image_task_dispatches": {
			"routing_provenance", "creative_entry_json", "operation_route_request_json",
		},
		"k12_creative_work_intakes": {
			"entry_kind", "promotion_policy", "target_work_id", "base_version_id",
			"promoted_version_id", "commit_receipt_json",
		},
	} {
		for _, column := range columns {
			has, checkErr := columnExists(context.Background(), db, table, column)
			if checkErr != nil {
				t.Fatal(checkErr)
			}
			if !has {
				t.Fatalf("%s.%s missing", table, column)
			}
		}
	}
}

func TestK12ManualCreativeIntakeV34IsRegisteredBeforeLatestMigration(t *testing.T) {
	if K12ManualCreativeIntakeV34.Version != 34 {
		t.Fatalf("K12ManualCreativeIntakeV34.Version=%d, want 34", K12ManualCreativeIntakeV34.Version)
	}
	if len(All) < K12ManualCreativeIntakeV34.Version ||
		All[K12ManualCreativeIntakeV34.Version-1].Version != K12ManualCreativeIntakeV34.Version {
		t.Fatalf("migration v34 is not registered at its ordered position")
	}
}

func TestK12ManualCreativeIntakeV34RestartIsBoundedAndDoesNotRewriteHistory(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "k12-v34-restart.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, db, All[:33]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('history-agent')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO k12_image_task_dispatches(
    dispatch_id,agent_name,learner_id,source_kind,source_ref,source_session_id,
    source_asset_refs_json,source_digest,message_intent,task_intent,
    intent_evidence_json,intent_confidence,confirmation_candidates_json,status,
    target_object_type,target_object_id,classification_route_snapshot_json,
    classification_invocation_id,route_policy_snapshot_json,idempotency_key,
    request_digest,attempt_generation,retry_safe,failure_kind,version,created_at,updated_at
) VALUES(
    'history-dispatch','history-agent','learner-1','desktop','history-source','session-1',
    '["asset://history-agent/source.png"]','sha256:source','','writing',
    '["historical"]',1,'[]','routed','creative_work_intake','history-intake',
    '{"provider":"legacy","model":"legacy-model"}','history-classification',
    '{"provider":"legacy","model":"legacy-model"}','desktop:history-source:g1',
    'sha256:request',1,0,'',7,101,202
)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO k12_creative_work_intakes(
    intake_id,dispatch_id,agent_name,learner_id,work_type,source_asset_refs_json,
    source_digest,work_title_candidate_json,task_requirement_candidate_json,
    ocr_evidence_json,route_policy_snapshot_json,operation_invocations_json,status,
    confirmation_provenance,promoted_work_id,idempotency_key,request_digest,
    attempt_generation,retry_safe,failure_kind,version,created_at,updated_at
) VALUES(
    'history-intake','history-dispatch','history-agent','learner-1','writing',
    '["asset://history-agent/source.png"]','sha256:source','','','',
    '{"provider":"legacy","model":"legacy-model"}','[]','promoted',
    'evidence_auto_freeze','history-work','creative:history-intake',
    'sha256:intake',1,0,'',9,101,202
)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	type historicalIntake struct {
		EntryKind       string
		PromotionPolicy string
		TargetWorkID    string
		BaseVersionID   string
		PromotedVersion string
		CommitReceipt   string
		Status          string
		PromotedWorkID  string
		Version         int
		CreatedAt       int64
		UpdatedAt       int64
	}
	var baseline historicalIntake
	var baselineSize int64
	for restart := 0; restart < 20; restart++ {
		db, err = sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := Run(ctx, db, All); err != nil {
			db.Close()
			t.Fatalf("restart %d migrate: %v", restart, err)
		}
		var migrationRows, v34Rows int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&migrationRows); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=34`).Scan(&v34Rows); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if migrationRows != len(All) || v34Rows != 1 {
			db.Close()
			t.Fatalf("restart %d migration ledger grew: rows=%d v34=%d",
				restart, migrationRows, v34Rows)
		}
		for table, want := range map[string]int{
			"k12_image_task_dispatches": 3,
			"k12_creative_work_intakes": 6,
		} {
			var columns int
			if err := db.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name IN (
                    'routing_provenance','creative_entry_json','operation_route_request_json',
                    'entry_kind','promotion_policy','target_work_id','base_version_id',
                    'promoted_version_id','commit_receipt_json'
                )`,
				table,
			).Scan(&columns); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if columns != want {
				db.Close()
				t.Fatalf("restart %d %s V34 columns=%d want=%d",
					restart, table, columns, want)
			}
		}
		var identityTriggers int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
            WHERE type='trigger' AND name IN (
                'k12_image_dispatch_manual_identity_immutable',
                'k12_creative_intake_manual_identity_immutable'
            )`).Scan(&identityTriggers); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if identityTriggers != 2 {
			db.Close()
			t.Fatalf("restart %d manual identity triggers=%d want=2",
				restart, identityTriggers)
		}
		var current historicalIntake
		if err := db.QueryRow(`SELECT
            entry_kind,promotion_policy,target_work_id,base_version_id,
            promoted_version_id,commit_receipt_json,status,promoted_work_id,
            version,created_at,updated_at
            FROM k12_creative_work_intakes WHERE intake_id='history-intake'`).
			Scan(
				&current.EntryKind, &current.PromotionPolicy,
				&current.TargetWorkID, &current.BaseVersionID,
				&current.PromotedVersion, &current.CommitReceipt,
				&current.Status, &current.PromotedWorkID,
				&current.Version, &current.CreatedAt, &current.UpdatedAt,
			); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if restart == 0 {
			baseline = current
			if baseline.EntryKind != "auto" ||
				baseline.PromotionPolicy != "automatic" ||
				baseline.PromotedVersion != "v1" {
				db.Close()
				t.Fatalf("initial historical backfill drift: %+v", baseline)
			}
		} else if !reflect.DeepEqual(current, baseline) {
			db.Close()
			t.Fatalf("restart %d rewrote historical intake: got=%+v want=%+v",
				restart, current, baseline)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if restart == 0 {
			baselineSize = info.Size()
		} else if info.Size() > baselineSize+8192 {
			t.Fatalf("restart %d database grew without bound: size=%d baseline=%d",
				restart, info.Size(), baselineSize)
		}
	}
}
