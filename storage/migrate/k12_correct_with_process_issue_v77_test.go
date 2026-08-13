package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

type v77AssessmentSchema struct {
	columns     []string
	foreignKeys []string
	indexes     []string
}

func TestREGK12CorrectWithProcessIssue20260809001V77WidensAssessmentStatusWithoutWeakeningShape(t *testing.T) {
	migration := findV77Migration(t)
	if migration.AtomicFunc == nil || migration.Func != nil || migration.SQL != "" {
		t.Fatalf("migration v77 must be one atomic table rebuild: %+v", migration)
	}

	t.Run("legacy round trip and terminal shape", func(t *testing.T) {
		db := openV77LegacyAssessmentDB(t)
		beforeSchema := readV77AssessmentSchema(t, db)
		beforeRow := readV77LegacyAssessmentRow(t, db)

		if err := applyMigration(context.Background(), db, migration); err != nil {
			t.Fatalf("apply V77: %v", err)
		}

		afterSchema := readV77AssessmentSchema(t, db)
		if !reflect.DeepEqual(afterSchema, beforeSchema) {
			t.Fatalf("V77 changed columns, foreign keys, or indexes\nbefore=%+v\nafter=%+v", beforeSchema, afterSchema)
		}
		if afterRow := readV77LegacyAssessmentRow(t, db); afterRow != beforeRow {
			t.Fatalf("V77 changed legacy assessment bytes\nbefore=%s\nafter=%s", beforeRow, afterRow)
		}

		var currentDDL string
		if err := db.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master
			WHERE type='table' AND name='k12_grading_assessment_items'`).Scan(&currentDDL); err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{
			"'correct_with_process_issue'",
			"PRIMARY KEY(job_id, problem_id, input_revision)",
			"UNIQUE(job_id, problem_id, published_revision)",
			"current_disposition",
			"parent_guide_invocation_id",
		} {
			if !strings.Contains(currentDDL, required) {
				t.Fatalf("V77 assessment DDL lost %q: %s", required, currentDDL)
			}
		}

		seedV77AssessmentParents(t, db, "valid")
		if err := insertV77ProcessAssessment(t.Context(), db, "valid", "solve-valid", "grade-valid", "guide-valid"); err != nil {
			t.Fatalf("V77 rejected canonical process-issue assessment: %v", err)
		}
		for _, test := range []struct {
			name  string
			solve any
			grade any
			guide any
		}{
			{name: "missing solve", solve: nil, grade: "grade-no-solve", guide: "guide-no-solve"},
			{name: "missing grade", solve: "solve-no-grade", grade: nil, guide: "guide-no-grade"},
			{name: "missing parent guide", solve: "solve-no-guide", grade: "grade-no-guide", guide: nil},
			{name: "detached parent guide", solve: "solve-detached", grade: "grade-detached", guide: "unknown-guide"},
		} {
			t.Run(test.name, func(t *testing.T) {
				suffix := strings.ReplaceAll(test.name, " ", "-")
				seedV77AssessmentParents(t, db, suffix)
				if err := insertV77ProcessAssessment(t.Context(), db, suffix, test.solve, test.grade, test.guide); err == nil {
					t.Fatalf("V77 accepted process-issue assessment with %s", test.name)
				}
			})
		}
		if _, err := db.ExecContext(t.Context(), `UPDATE k12_grading_assessment_items
			SET status='invented' WHERE problem_id='problem-valid'`); err == nil {
			t.Fatal("V77 widened status to arbitrary strings")
		}
		if _, err := db.ExecContext(t.Context(), `UPDATE k12_grading_assessment_items
			SET status='correct' WHERE problem_id='problem-valid'`); err == nil {
			t.Fatal("V77 weakened the existing parent-guide/status shape")
		}

		assertV77ForeignKeysClean(t, db)
		var recorded int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM schema_migrations WHERE version=77`).Scan(&recorded); err != nil || recorded != 1 {
			t.Fatalf("V77 version row count=%d err=%v", recorded, err)
		}
	})

	t.Run("version failure rolls back rebuild and data", func(t *testing.T) {
		db := openV77LegacyAssessmentDB(t)
		beforeSchema := readV77AssessmentSchema(t, db)
		beforeRow := readV77LegacyAssessmentRow(t, db)
		forced := errors.New("forced version failure")
		err := migration.AtomicFunc(context.Background(), db, func(context.Context, *sql.Tx) error {
			return forced
		})
		if !errors.Is(err, forced) {
			t.Fatalf("V77 did not return record-version failure: %v", err)
		}
		if got := readV77AssessmentSchema(t, db); !reflect.DeepEqual(got, beforeSchema) {
			t.Fatalf("V77 record-version failure did not roll back schema\nbefore=%+v\nafter=%+v", beforeSchema, got)
		}
		if got := readV77LegacyAssessmentRow(t, db); got != beforeRow {
			t.Fatalf("V77 record-version failure did not roll back data\nbefore=%s\nafter=%s", beforeRow, got)
		}
		var staging int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master
			WHERE type='table' AND name='k12_grading_assessment_items_v77'`).Scan(&staging); err != nil || staging != 0 {
			t.Fatalf("V77 staging table survived rollback: count=%d err=%v", staging, err)
		}
		assertV77ForeignKeysClean(t, db)
	})
}

func findV77Migration(t *testing.T) Migration {
	t.Helper()
	for _, migration := range All {
		if migration.Version == 77 {
			return migration
		}
	}
	t.Fatal("migration v77 is not registered in migrate.All")
	return Migration{}
}

func openV77LegacyAssessmentDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE schema_migrations(
			version INTEGER PRIMARY KEY,
			description TEXT NOT NULL DEFAULT '',
			applied_at INTEGER NOT NULL
		)`,
		`CREATE TABLE k12_grading_jobs(
			agent_name TEXT NOT NULL, record_id TEXT NOT NULL,
			PRIMARY KEY(agent_name,record_id)
		)`,
		`CREATE TABLE k12_problems(
			agent_name TEXT NOT NULL, problem_id TEXT NOT NULL,
			PRIMARY KEY(agent_name,problem_id)
		)`,
		`CREATE TABLE k12_attempts(
			agent_name TEXT NOT NULL, attempt_id TEXT NOT NULL, problem_id TEXT NOT NULL,
			PRIMARY KEY(agent_name,attempt_id,problem_id)
		)`,
		`CREATE TABLE k12_grading_item_invocations(item_invocation_id TEXT PRIMARY KEY)`,
		k12ProblemProgressiveAssessmentV48DDL,
		`ALTER TABLE k12_grading_assessment_items_v48 RENAME TO k12_grading_assessment_items`,
		`CREATE INDEX idx_k12_grading_assessment_items_job
			ON k12_grading_assessment_items(agent_name,job_id,problem_id,published_revision)`,
		`CREATE UNIQUE INDEX idx_k12_grading_assessment_items_current
			ON k12_grading_assessment_items(agent_name,job_id,problem_id)
			WHERE current_disposition='current'`,
		`INSERT INTO k12_grading_jobs(agent_name,record_id) VALUES('agent','job')`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("prepare V48 assessment schema: %v\n%s", err, statement)
		}
	}
	seedV77AssessmentParents(t, db, "legacy")
	if _, err := db.ExecContext(t.Context(), `INSERT INTO k12_grading_assessment_items(
		agent_name,job_id,problem_id,attempt_id,confirmed_version,input_revision,
		published_revision,current_disposition,structure_version,input_digest,status,
		result_json,result_digest,solve_invocation_id,grade_invocation_id,
		parent_guide_invocation_id,projection_record_id,projection_created,
		projection_status,created_at,updated_at
	) VALUES(
		'agent','job','problem-legacy','attempt-legacy',2,4,7,'superseded',3,
		'input-legacy','wrong','{"legacy":true}','sha256:legacy','solve-legacy',
		'grade-legacy','guide-legacy','projection-legacy',1,'committed',11,12
	)`); err != nil {
		t.Fatalf("seed V48 legacy assessment: %v", err)
	}
	assertV77ForeignKeysClean(t, db)
	return db
}

func seedV77AssessmentParents(t *testing.T, db *sql.DB, suffix string) {
	t.Helper()
	for _, seed := range []struct {
		statement string
		args      []any
	}{
		{
			statement: `INSERT INTO k12_problems(agent_name,problem_id) VALUES('agent',?)`,
			args:      []any{"problem-" + suffix},
		},
		{
			statement: `INSERT INTO k12_attempts(agent_name,attempt_id,problem_id)
				VALUES('agent',?,?)`,
			args: []any{"attempt-" + suffix, "problem-" + suffix},
		},
		{
			statement: `INSERT INTO k12_grading_item_invocations(item_invocation_id)
				VALUES(?),(?),(?)`,
			args: []any{"solve-" + suffix, "grade-" + suffix, "guide-" + suffix},
		},
	} {
		if _, err := db.ExecContext(t.Context(), seed.statement, seed.args...); err != nil {
			t.Fatalf("seed V77 assessment parents %s: %v", suffix, err)
		}
	}
}

func insertV77ProcessAssessment(ctx context.Context, db *sql.DB, suffix string, solve, grade, guide any) error {
	_, err := db.ExecContext(ctx, `INSERT INTO k12_grading_assessment_items(
		agent_name,job_id,problem_id,attempt_id,confirmed_version,input_revision,
		published_revision,current_disposition,structure_version,input_digest,status,
		result_json,result_digest,solve_invocation_id,grade_invocation_id,
		parent_guide_invocation_id,projection_record_id,projection_created,
		projection_status,created_at,updated_at
	) VALUES('agent','job',?,?,1,1,1,'current',1,?,'correct_with_process_issue',
		'{}',?,?,?,?, '',0,'committed',21,22)`,
		"problem-"+suffix,
		"attempt-"+suffix,
		"input-"+suffix,
		"sha256:"+suffix,
		solve,
		grade,
		guide,
	)
	return err
}

func readV77AssessmentSchema(t *testing.T, db *sql.DB) v77AssessmentSchema {
	t.Helper()
	var snapshot v77AssessmentSchema
	rows, queryErr := db.QueryContext(t.Context(), `PRAGMA table_info(k12_grading_assessment_items)`)
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		snapshot.columns = append(snapshot.columns, fmt.Sprintf(
			"%d|%s|%s|%d|%t|%s|%d",
			cid, name, columnType, notNull, defaultValue.Valid, defaultValue.String, primaryKey,
		))
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	rows, queryErr = db.QueryContext(t.Context(), `PRAGMA foreign_key_list(k12_grading_assessment_items)`)
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	for rows.Next() {
		var id, sequence int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &sequence, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		snapshot.foreignKeys = append(snapshot.foreignKeys, fmt.Sprintf(
			"%d|%d|%s|%s|%s|%s|%s|%s",
			id, sequence, table, from, to, onUpdate, onDelete, match,
		))
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	rows, queryErr = db.QueryContext(t.Context(), `SELECT name,sql FROM sqlite_master
		WHERE type='index' AND tbl_name='k12_grading_assessment_items' AND sql IS NOT NULL
		ORDER BY name`)
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		snapshot.indexes = append(snapshot.indexes, name+"|"+strings.Join(strings.Fields(definition), " "))
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(snapshot.foreignKeys)
	return snapshot
}

func readV77LegacyAssessmentRow(t *testing.T, db *sql.DB) string {
	t.Helper()
	var row string
	if err := db.QueryRowContext(t.Context(), `SELECT json_array(
		agent_name,job_id,problem_id,attempt_id,confirmed_version,input_revision,
		published_revision,current_disposition,structure_version,input_digest,status,
		result_json,result_digest,solve_invocation_id,grade_invocation_id,
		parent_guide_invocation_id,projection_record_id,projection_created,
		projection_status,created_at,updated_at
	) FROM k12_grading_assessment_items WHERE problem_id='problem-legacy'`).Scan(&row); err != nil {
		t.Fatal(err)
	}
	return row
}

func assertV77ForeignKeysClean(t *testing.T, db *sql.DB) {
	t.Helper()
	var violations int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("foreign_key_check violations=%d", violations)
	}
}
