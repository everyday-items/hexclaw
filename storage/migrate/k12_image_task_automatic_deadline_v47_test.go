package migrate

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestK12ImageTaskAutomaticDeadlineV47AddsDurableBudgetAndConservativeBackfill(t *testing.T) {
	db, err := sql.Open("sqlite", "file:k12-image-task-deadline-v47?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	v47Index := -1
	for i, migration := range All {
		if migration.Version == 47 {
			v47Index = i
			break
		}
	}
	if v47Index < 1 || All[v47Index-1].Version != 46 {
		t.Fatalf("V47 must be registered directly after V46, index=%d", v47Index)
	}
	if err := Run(context.Background(), db, All[:v47Index]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO agents
			(name,display_name,description,model,provider,system_prompt,metadata,created_at,updated_at)
		VALUES('deadline-agent','','','','','','{}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);

		INSERT INTO k12_image_task_dispatches
			(dispatch_id,agent_name,learner_id,source_kind,source_ref,source_asset_refs_json,
			 source_digest,status,classification_route_snapshot_json,classification_invocation_id,
			 route_policy_snapshot_json,idempotency_key,request_digest,created_at,updated_at)
		VALUES
			('dispatch-running','deadline-agent','learner-1','desktop','session:running','[]',
			 'sha256:running','routing','{}','invocation-running','{}','key-running',
			 'sha256:request-running',100,120),
			('dispatch-waiting','deadline-agent','learner-1','desktop','session:waiting','[]',
			 'sha256:waiting','awaiting_confirmation','{}','invocation-waiting','{}','key-waiting',
			 'sha256:request-waiting',200,220),
			('dispatch-failed','deadline-agent','learner-1','desktop','session:failed','[]',
			 'sha256:failed','failed','{}','invocation-failed','{}','key-failed',
			 'sha256:request-failed',300,320);

		INSERT INTO k12_image_task_invocations
			(invocation_id,agent_name,dispatch_id,operation,operation_key,request_digest,
			 route_snapshot_json,status,attempt,created_at,updated_at)
		VALUES
			('invocation-running','deadline-agent','dispatch-running','classification',
			 'operation-running','sha256:invocation-running','{}','prepared',1,100,120);
	`); err != nil {
		t.Fatal(err)
	}

	before := time.Now().Unix()
	if err := Run(context.Background(), db, []Migration{K12ImageTaskAutomaticDeadlineV47}); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Unix()

	for table, columns := range map[string][]string{
		"k12_image_task_dispatches": {
			"automatic_budget_seconds",
			"automatic_started_at",
			"automatic_deadline_at",
			"automatic_remaining_seconds",
		},
		"k12_image_task_invocations": {"deadline_at"},
	} {
		for _, column := range columns {
			has, err := columnExists(context.Background(), db, table, column)
			if err != nil {
				t.Fatal(err)
			}
			if !has {
				t.Fatalf("%s.%s missing", table, column)
			}
		}
	}

	type automaticWindow struct {
		budget    int
		startedAt int64
		deadline  int64
		remaining int
	}
	loadWindow := func(dispatchID string) automaticWindow {
		t.Helper()
		var got automaticWindow
		if err := db.QueryRow(`
			SELECT automatic_budget_seconds,automatic_started_at,
			       automatic_deadline_at,automatic_remaining_seconds
			FROM k12_image_task_dispatches
			WHERE dispatch_id=?`, dispatchID,
		).Scan(&got.budget, &got.startedAt, &got.deadline, &got.remaining); err != nil {
			t.Fatal(err)
		}
		return got
	}

	running := loadWindow("dispatch-running")
	if running.budget != 300 || running.remaining != 300 {
		t.Fatalf("running budget=%d remaining=%d", running.budget, running.remaining)
	}
	if running.startedAt < before || running.startedAt > after {
		t.Fatalf("running started_at=%d want [%d,%d]", running.startedAt, before, after)
	}
	if running.deadline != running.startedAt+300 {
		t.Fatalf("running deadline=%d started_at=%d", running.deadline, running.startedAt)
	}

	waiting := loadWindow("dispatch-waiting")
	if waiting.budget != 300 || waiting.startedAt != 0 || waiting.deadline != 0 || waiting.remaining != 300 {
		t.Fatalf("waiting window=%+v", waiting)
	}

	failed := loadWindow("dispatch-failed")
	if failed.budget != 300 || failed.startedAt != 0 || failed.deadline != 0 || failed.remaining != 0 {
		t.Fatalf("failed window=%+v", failed)
	}

	var invocationDeadline int64
	if err := db.QueryRow(`
		SELECT deadline_at
		FROM k12_image_task_invocations
		WHERE invocation_id='invocation-running'
	`).Scan(&invocationDeadline); err != nil {
		t.Fatal(err)
	}
	if invocationDeadline != 0 {
		t.Fatalf("legacy invocation deadline_at=%d want 0", invocationDeadline)
	}

	for _, index := range []string{
		"idx_k12_image_dispatch_recovery_deadline",
		"idx_k12_image_invocation_recovery_deadline",
	} {
		var count int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM sqlite_master
			WHERE type='index' AND name=?`, index,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s index count=%d", index, count)
		}
	}
}
