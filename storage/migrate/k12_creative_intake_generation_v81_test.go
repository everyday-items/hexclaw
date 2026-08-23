package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestK12CreativeIntakeGenerationV81BackfillsOnlyCurrentIntakesWithInitialGeneration(
	t *testing.T,
) {
	ctx := context.Background()
	db, err := sql.Open(
		"sqlite",
		"file:"+t.TempDir()+"/creative-intake-v81.db?_pragma=foreign_keys(1)",
	)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if len(All) < 81 || All[79].Version != 80 || All[80].Version != 81 {
		t.Fatalf("V80/V81 migration order drift: total=%d", len(All))
	}
	if err := Run(ctx, db, All[:80]); err != nil {
		t.Fatalf("run migration chain through V80: %v", err)
	}
	seedK12CreativeIntakeGenerationV81(t, db)

	// promoted_generation_id 是 V81 新列；其余 intake 字段及所有历史作品表必须逐字不变。
	legacyBefore := snapshotK12CreativeV81Tables(t, db, true)
	if err := Run(ctx, db, []Migration{K12CreativeIntakeGenerationV81}); err != nil {
		t.Fatalf("run V81 migration: %v", err)
	}
	legacyAfter := snapshotK12CreativeV81Tables(t, db, true)
	assertK12CreativeV81SnapshotsEqual(t, legacyBefore, legacyAfter, "V81")

	for intakeID, want := range map[string]struct {
		entryKind          string
		promotedWorkID     string
		promotedVersionID  string
		promotedGeneration string
	}{
		"intake-auto": {
			entryKind: "auto", promotedWorkID: "work-auto",
			promotedVersionID: "v1", promotedGeneration: "generation-auto-1",
		},
		"intake-new": {
			entryKind: "new_work", promotedWorkID: "work-new",
			promotedVersionID: "v1", promotedGeneration: "generation-new-1",
		},
		"intake-revision": {
			entryKind: "revision", promotedWorkID: "work-revision",
			promotedVersionID: "v2", promotedGeneration: "",
		},
		"intake-without-generation": {
			entryKind: "new_work", promotedWorkID: "work-without-generation",
			promotedVersionID: "v1", promotedGeneration: "",
		},
	} {
		assertK12CreativeV81Intake(t, db, intakeID, want.entryKind,
			want.promotedWorkID, want.promotedVersionID, want.promotedGeneration)
	}

	firstRun := snapshotK12CreativeV81Tables(t, db, false)
	// 直接重入 AtomicFunc，覆盖“列已存在且回填已执行”分支；recordVersion 由迁移框架负责，
	// 此处用 no-op 避免伪造第二条迁移账本。
	if err := migrateK12CreativeIntakeGenerationV81(
		ctx,
		db,
		func(context.Context, *sql.Tx) error { return nil },
	); err != nil {
		t.Fatalf("rerun V81 atomic migration: %v", err)
	}
	if err := Run(ctx, db, []Migration{K12CreativeIntakeGenerationV81}); err != nil {
		t.Fatalf("rerun V81 through migration ledger: %v", err)
	}
	rerun := snapshotK12CreativeV81Tables(t, db, false)
	assertK12CreativeV81SnapshotsEqual(t, firstRun, rerun, "V81 rerun")

	var migrationRows int
	if err := db.QueryRow(
		`SELECT count(*) FROM schema_migrations WHERE version=81`,
	).Scan(&migrationRows); err != nil {
		t.Fatal(err)
	}
	if migrationRows != 1 {
		t.Fatalf("V81 migration ledger rows=%d want=1", migrationRows)
	}
	assertK12CreativeV81NoForeignKeyViolations(t, db)
}

func seedK12CreativeIntakeGenerationV81(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('agent-v81')`); err != nil {
		t.Fatal(err)
	}

	works := []struct {
		workID, workType, initialGeneration string
	}{
		{"work-auto", "art", "generation-auto-1"},
		{"work-new", "writing", "generation-new-1"},
		{"work-revision", "art", "generation-revision-1"},
		{"work-without-generation", "writing", ""},
	}
	for index, work := range works {
		if _, err := db.Exec(`INSERT INTO k12_creative_works (
			record_id,agent_name,status,work_type,dedupe_key,source_session_id,
			version,created_at,updated_at,display_name,work_title,task_requirement,
			title_task_provenance_json,source_intake_id,
			initial_feedback_generation_id,latest_feedback_generation_id,
			feedback_state,row_version
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			work.workID, "agent-v81", "feedback_ready", work.workType,
			"dedupe-"+work.workID, "session-v81", 7, 100+index, 200+index,
			"display-"+work.workID, "title-"+work.workID, "task-"+work.workID,
			`{"source":"legacy-v80"}`, "source-"+work.workID,
			work.initialGeneration, work.initialGeneration,
			map[bool]string{true: "succeeded", false: ""}[work.initialGeneration != ""],
			3,
		); err != nil {
			t.Fatalf("insert %s: %v", work.workID, err)
		}
		if _, err := db.Exec(`INSERT INTO k12_creative_work_versions (
			work_record_id,version_index,version_id,source_asset_id,
			content_markdown,practice_card_done_at,ocr_job_id,ocr_raw,
			ocr_version,ocr_confirmed_digest,content_confirmed_at
		) VALUES (?,?,?,? ,?,?,?,?,?,?,?)`,
			work.workID, 0, "v1", "asset://agent-v81/"+work.workID,
			"legacy-content-"+work.workID, 301+index, "ocr-"+work.workID,
			"legacy-ocr-"+work.workID, 2, "sha256:ocr-"+work.workID, 401+index,
		); err != nil {
			t.Fatalf("insert legacy version for %s: %v", work.workID, err)
		}
		if _, err := db.Exec(`INSERT INTO k12_work_feedback (
			work_record_id,version_index,feedback_markdown,feedback_source,
			feedback_skill,route_snapshot_json,invocation_id
		) VALUES (?,?,?,?,?,?,?)`,
			work.workID, 0, "legacy-feedback-"+work.workID, "ai",
			"legacy-skill@1", `{"provider":"legacy"}`, "invocation-"+work.workID,
		); err != nil {
			t.Fatalf("insert legacy feedback for %s: %v", work.workID, err)
		}
		if work.initialGeneration == "" {
			continue
		}
		if _, err := db.Exec(`INSERT INTO k12_work_feedback_generations (
			generation_id,work_id,agent_name,generation_no,command_key,
			request_digest,status,feedback_type,source_snapshot_json,
			request_snapshot_json,route_snapshot_json,invocation_snapshot_json,
			feedback_json,projection_markdown,failure_reason,attempt,
			created_at,updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			work.initialGeneration, work.workID, "agent-v81", 1,
			"command-"+work.workID, "sha256:request-"+work.workID, "succeeded",
			work.workType, `{"source":"legacy-v80"}`, `{"request":"legacy-v80"}`,
			`{"route":"legacy-v80"}`, `{"invocation":"legacy-v80"}`,
			`{"feedback":"legacy-v80"}`, "generation-projection-"+work.workID,
			"", 1, 500+index, 600+index,
		); err != nil {
			t.Fatalf("insert generation for %s: %v", work.workID, err)
		}
	}

	intakes := []struct {
		intakeID, workID, workType, entryKind, policy, targetWorkID string
		baseVersionID, promotedVersionID                            string
	}{
		{"intake-auto", "work-auto", "art", "auto", "automatic", "", "", "v1"},
		{"intake-new", "work-new", "writing", "new_work", "explicit_commit", "", "", "v1"},
		{"intake-revision", "work-revision", "art", "revision", "explicit_commit", "work-revision", "v1", "v2"},
		{"intake-without-generation", "work-without-generation", "writing", "new_work", "explicit_commit", "", "", "v1"},
	}
	for index, intake := range intakes {
		dispatchID := "dispatch-" + intake.intakeID
		taskIntent := "writing"
		if intake.workType == "art" {
			taskIntent = "artwork"
		}
		if _, err := db.Exec(`INSERT INTO k12_image_task_dispatches (
			dispatch_id,agent_name,learner_id,source_kind,source_ref,
			source_session_id,source_asset_refs_json,source_digest,
			task_intent,intent_evidence_json,intent_confidence,
			confirmation_candidates_json,status,target_object_type,target_object_id,
			classification_route_snapshot_json,classification_invocation_id,
			route_policy_snapshot_json,idempotency_key,request_digest,
			attempt_generation,retry_safe,failure_kind,version,created_at,updated_at,
			routing_provenance,creative_entry_json,operation_route_request_json
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			dispatchID, "agent-v81", "learner-v81", "desktop",
			"source-"+intake.intakeID, "session-v81", `[]`,
			"sha256:source-"+intake.intakeID, taskIntent,
			`["legacy-v80"]`, 1, `[]`, "routed", "creative_work_intake",
			intake.intakeID, `{}`, "", `{}`, "dispatch-key-"+intake.intakeID,
			"sha256:dispatch-"+intake.intakeID, 1, 0, "", 9,
			700+index, 800+index, "parent_selected",
			fmt.Sprintf(`{"kind":%q}`, intake.entryKind), `{}`,
		); err != nil {
			t.Fatalf("insert dispatch %s: %v", dispatchID, err)
		}
		if _, err := db.Exec(`INSERT INTO k12_creative_work_intakes (
			intake_id,dispatch_id,agent_name,learner_id,work_type,
			source_asset_refs_json,source_digest,work_title_candidate_json,
			task_requirement_candidate_json,ocr_evidence_json,
			route_policy_snapshot_json,operation_invocations_json,status,
			confirmation_provenance,promoted_work_id,idempotency_key,request_digest,
			attempt_generation,retry_safe,failure_kind,version,created_at,updated_at,
			entry_kind,promotion_policy,target_work_id,base_version_id,
			promoted_version_id,commit_receipt_json
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			intake.intakeID, dispatchID, "agent-v81", "learner-v81", intake.workType,
			`[]`, "sha256:intake-"+intake.intakeID, "", "", "", `{}`, `[]`,
			"promoted", "parent_confirmed", intake.workID,
			"intake-key-"+intake.intakeID, "sha256:intake-request-"+intake.intakeID,
			1, 0, "", 11, 900+index, 1000+index, intake.entryKind, intake.policy,
			intake.targetWorkID, intake.baseVersionID, intake.promotedVersionID,
			`{"command_digest":"sha256:legacy-v80"}`,
		); err != nil {
			t.Fatalf("insert intake %s: %v", intake.intakeID, err)
		}
	}
}

type k12CreativeV81TableSnapshot struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Digest  string   `json:"digest"`
}

func snapshotK12CreativeV81Tables(
	t *testing.T,
	db *sql.DB,
	excludePromotedGeneration bool,
) map[string]k12CreativeV81TableSnapshot {
	t.Helper()
	excludes := map[string]map[string]bool{}
	if excludePromotedGeneration {
		excludes["k12_creative_work_intakes"] = map[string]bool{
			"promoted_generation_id": true,
		}
	}
	orders := map[string][]string{
		"k12_creative_work_intakes":     {"intake_id"},
		"k12_creative_works":            {"record_id"},
		"k12_creative_work_versions":    {"work_record_id", "version_index"},
		"k12_work_feedback":             {"work_record_id", "version_index"},
		"k12_work_feedback_generations": {"work_id", "generation_no"},
	}
	out := make(map[string]k12CreativeV81TableSnapshot, len(orders))
	for table, order := range orders {
		out[table] = snapshotK12CreativeV81Table(t, db, table, order, excludes[table])
	}
	return out
}

func snapshotK12CreativeV81Table(
	t *testing.T,
	db *sql.DB,
	table string,
	order []string,
	exclude map[string]bool,
) k12CreativeV81TableSnapshot {
	t.Helper()
	columnRows, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		t.Fatal(err)
	}
	var columns []string
	for columnRows.Next() {
		var column string
		if err := columnRows.Scan(&column); err != nil {
			_ = columnRows.Close()
			t.Fatal(err)
		}
		if !exclude[column] {
			columns = append(columns, column)
		}
	}
	if err := columnRows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(columns) == 0 {
		t.Fatalf("%s has no snapshot columns", table)
	}

	quotedColumns := make([]string, len(columns))
	for i, column := range columns {
		quotedColumns[i] = quoteK12CreativeV81Identifier(column)
	}
	quotedOrder := make([]string, len(order))
	for i, column := range order {
		quotedOrder[i] = quoteK12CreativeV81Identifier(column)
	}
	query := fmt.Sprintf(
		"SELECT %s FROM %s ORDER BY %s",
		strings.Join(quotedColumns, ","),
		quoteK12CreativeV81Identifier(table),
		strings.Join(quotedOrder, ","),
	)
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	var values [][]any
	for rows.Next() {
		row := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range row {
			dest[i] = &row[i]
		}
		if err := rows.Scan(dest...); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		for i, value := range row {
			if raw, ok := value.([]byte); ok {
				row[i] = string(raw)
			}
		}
		values = append(values, row)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	}{Columns: columns, Rows: values})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	return k12CreativeV81TableSnapshot{
		Columns: columns,
		Rows:    values,
		Digest:  "sha256:" + hex.EncodeToString(sum[:]),
	}
}

func assertK12CreativeV81SnapshotsEqual(
	t *testing.T,
	want, got map[string]k12CreativeV81TableSnapshot,
	stage string,
) {
	t.Helper()
	for table, wantSnapshot := range want {
		gotSnapshot, ok := got[table]
		if !ok {
			t.Fatalf("%s snapshot missing %s", stage, table)
		}
		if !reflect.DeepEqual(gotSnapshot, wantSnapshot) {
			t.Fatalf(
				"%s rewrote %s: got digest=%s rows=%v want digest=%s rows=%v",
				stage, table, gotSnapshot.Digest, gotSnapshot.Rows,
				wantSnapshot.Digest, wantSnapshot.Rows,
			)
		}
	}
}

func assertK12CreativeV81Intake(
	t *testing.T,
	db *sql.DB,
	intakeID, wantKind, wantWork, wantVersion, wantGeneration string,
) {
	t.Helper()
	var kind, workID, versionID, generationID string
	if err := db.QueryRow(`SELECT entry_kind,promoted_work_id,
		promoted_version_id,promoted_generation_id
		FROM k12_creative_work_intakes WHERE intake_id=?`, intakeID).
		Scan(&kind, &workID, &versionID, &generationID); err != nil {
		t.Fatal(err)
	}
	if kind != wantKind || workID != wantWork || versionID != wantVersion ||
		generationID != wantGeneration {
		t.Fatalf(
			"intake %s = kind=%q work=%q version=%q generation=%q, want %q/%q/%q/%q",
			intakeID, kind, workID, versionID, generationID,
			wantKind, wantWork, wantVersion, wantGeneration,
		)
	}
}

func assertK12CreativeV81NoForeignKeyViolations(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID int64
		var parent string
		var foreignKeyID int
		if err := rows.Scan(&table, &rowID, &parent, &foreignKeyID); err != nil {
			t.Fatal(err)
		}
		t.Fatalf(
			"foreign-key violation: table=%s row=%d parent=%s key=%d",
			table, rowID, parent, foreignKeyID,
		)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func quoteK12CreativeV81Identifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
