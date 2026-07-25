package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openCronIntegrityTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cron-integrity.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func execCronIntegritySQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func cronMigrationByVersion(t *testing.T, version int) Migration {
	t.Helper()
	for _, migration := range All {
		if migration.Version == version {
			return migration
		}
	}
	t.Fatalf("migration v%d not registered", version)
	return Migration{}
}

func createV15CronSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	execCronIntegritySQL(t, db, `CREATE TABLE cron_jobs (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		type TEXT NOT NULL DEFAULT 'cron',
		schedule TEXT NOT NULL,
		prompt TEXT NOT NULL DEFAULT '',
		spec_json TEXT NOT NULL DEFAULT '',
		source_prompt TEXT NOT NULL DEFAULT '',
		user_id TEXT NOT NULL,
		platform TEXT NOT NULL DEFAULT '',
		chat_id TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active',
		last_run_at DATETIME,
		next_run_at DATETIME NOT NULL,
		run_count INTEGER NOT NULL DEFAULT 0,
		meta TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	execCronIntegritySQL(t, db, `CREATE INDEX idx_cron_jobs_user ON cron_jobs(user_id)`)
	execCronIntegritySQL(t, db, `CREATE INDEX idx_cron_jobs_status ON cron_jobs(status,next_run_at)`)
	execCronIntegritySQL(t, db, `CREATE UNIQUE INDEX idx_cron_jobs_user_name ON cron_jobs(user_id,name)`)
	execCronIntegritySQL(t, db, `CREATE TABLE cron_job_runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id TEXT NOT NULL REFERENCES cron_jobs(id) ON DELETE CASCADE,
		status TEXT NOT NULL DEFAULT 'success',
		result TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0,
		run_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		stdout TEXT NOT NULL DEFAULT '',
		stderr TEXT NOT NULL DEFAULT '',
		exit_code INTEGER NOT NULL DEFAULT 0,
		data_json TEXT NOT NULL DEFAULT ''
	)`)
	execCronIntegritySQL(t, db, `CREATE TABLE cron_job_state (
		job_id TEXT NOT NULL REFERENCES cron_jobs(id) ON DELETE CASCADE,
		key TEXT NOT NULL,
		value TEXT NOT NULL DEFAULT '',
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(job_id,key)
	)`)
}

func TestCronV5DropsPromptInPlaceWithoutLosingMetaIndexesOrChildren(t *testing.T) {
	db := openCronIntegrityTestDB(t)
	createV15CronSchema(t, db)
	execCronIntegritySQL(t, db, `INSERT INTO cron_jobs
		(id,name,type,schedule,prompt,spec_json,source_prompt,user_id,platform,chat_id,status,
		 last_run_at,next_run_at,run_count,meta,created_at)
		VALUES ('job-v5','daily','cron','@daily','legacy-secret','{"runtime":"starlark","script":"emit(1)"}',
		        'private-source','u1','chat','c1','active',NULL,'2026-07-21T00:00:00Z',7,
		        '{"source_key":"agent-a/daily","deliver":["chat"]}','2026-07-01T00:00:00Z')`)
	execCronIntegritySQL(t, db, `INSERT INTO cron_job_runs
		(id,job_id,status,result,error,duration_ms,run_at,stdout,stderr,exit_code,data_json)
		VALUES (41,'job-v5','success','result','',9,'2026-07-01T01:00:00Z','stdout','stderr',0,'{"answer":42}')`)
	execCronIntegritySQL(t, db, `INSERT INTO cron_job_state(job_id,key,value,updated_at)
		VALUES ('job-v5','cursor','state-value','2026-07-01T02:00:00Z')`)

	if err := Run(context.Background(), db, []Migration{cronMigrationByVersion(t, 5)}); err != nil {
		t.Fatalf("run v5: %v", err)
	}
	if columnExistsForCronTest(t, db, "cron_jobs", "prompt") {
		t.Fatal("v5 must drop prompt")
	}
	for _, col := range []string{"spec_json", "source_prompt", "meta"} {
		if !columnExistsForCronTest(t, db, "cron_jobs", col) {
			t.Fatalf("v5 lost cron_jobs.%s", col)
		}
	}
	var meta, data, state string
	if err := db.QueryRow(`SELECT meta FROM cron_jobs WHERE id='job-v5'`).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if meta != `{"source_key":"agent-a/daily","deliver":["chat"]}` {
		t.Fatalf("meta changed: %q", meta)
	}
	if err := db.QueryRow(`SELECT data_json FROM cron_job_runs WHERE id=41 AND job_id='job-v5'`).Scan(&data); err != nil {
		t.Fatalf("run lost: %v", err)
	}
	if data != `{"answer":42}` {
		t.Fatalf("run payload changed: %q", data)
	}
	if err := db.QueryRow(`SELECT value FROM cron_job_state WHERE job_id='job-v5' AND key='cursor'`).Scan(&state); err != nil {
		t.Fatalf("state lost: %v", err)
	}
	if state != "state-value" {
		t.Fatalf("state changed: %q", state)
	}
	assertNamedIndexes(t, db, "cron_jobs", []string{
		"idx_cron_jobs_status", "idx_cron_jobs_user", "idx_cron_jobs_user_name",
	})
	assertForeignKeyTarget(t, db, "cron_job_runs", "cron_jobs", "job_id", "id")
	assertForeignKeyTarget(t, db, "cron_job_state", "cron_jobs", "job_id", "id")
}

func TestCronIntegrityV29RepairsSchemaAndMergesWithoutDroppingEvidence(t *testing.T) {
	db := openCronIntegrityTestDB(t)
	createPostV5BrokenCronSchema(t, db)
	execCronIntegritySQL(t, db, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, description TEXT NOT NULL DEFAULT '', applied_at INTEGER NOT NULL)`)
	execCronIntegritySQL(t, db, `INSERT INTO schema_migrations(version,description,applied_at) VALUES (28,'before cron repair',1)`)

	seedMergeGroup(t, db)
	seedConflictGroup(t, db)
	execCronIntegritySQL(t, db, `VACUUM`)

	if err := Run(context.Background(), db, All); err != nil {
		t.Fatalf("run v29: %v", err)
	}

	var latest int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	wantLatest := All[len(All)-1].Version
	if latest != wantLatest {
		t.Fatalf("latest migration=%d, want %d", latest, wantLatest)
	}
	assertCanonicalCronSchema(t, db)

	var jobCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_jobs WHERE user_id='merge-user' AND name='same-name'`).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 1 {
		t.Fatalf("merge group rows=%d, want 1", jobCount)
	}
	var survivor string
	if err := db.QueryRow(`SELECT id FROM cron_jobs WHERE user_id='merge-user' AND name='same-name'`).Scan(&survivor); err != nil {
		t.Fatal(err)
	}
	if survivor != "job-early" {
		t.Fatalf("normalized created_at survivor=%q, want job-early", survivor)
	}
	var mergedRunCount int64
	var mergedLastRun sql.NullTime
	if err := db.QueryRow(`SELECT run_count,last_run_at FROM cron_jobs WHERE id='job-early'`).
		Scan(&mergedRunCount, &mergedLastRun); err != nil {
		t.Fatal(err)
	}
	if mergedRunCount != 260 {
		t.Fatalf("merged parent run_count=%d, want 260", mergedRunCount)
	}
	wantLastRun := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	if !mergedLastRun.Valid || !mergedLastRun.Time.Equal(wantLastRun) {
		t.Fatalf("merged parent last_run_at=%v, want %v", mergedLastRun, wantLastRun)
	}
	var tieSurvivor string
	if err := db.QueryRow(`SELECT id FROM cron_jobs WHERE user_id='tie-user' AND name='tie-name'`).Scan(&tieSurvivor); err != nil {
		t.Fatal(err)
	}
	if tieSurvivor != "tie-a" {
		t.Fatalf("created_at tie survivor=%q, want tie-a", tieSurvivor)
	}

	var runCount, distinctRunIDs int
	if err := db.QueryRow(`SELECT COUNT(*),COUNT(DISTINCT id) FROM cron_job_runs WHERE job_id='job-early'`).Scan(&runCount, &distinctRunIDs); err != nil {
		t.Fatal(err)
	}
	if runCount != 253 || distinctRunIDs != 253 {
		t.Fatalf("merged runs=(%d rows,%d ids), want 253/253", runCount, distinctRunIDs)
	}
	var payload string
	if err := db.QueryRow(`SELECT data_json FROM cron_job_runs WHERE id=253`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != `{"run":253}` {
		t.Fatalf("run payload changed: %q", payload)
	}

	var stateCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_job_state WHERE job_id='job-early'`).Scan(&stateCount); err != nil {
		t.Fatal(err)
	}
	if stateCount != 150 {
		t.Fatalf("merged state count=%d, want 150", stateCount)
	}
	var winning, moved string
	if err := db.QueryRow(`SELECT value FROM cron_job_state WHERE job_id='job-early' AND key='key-000'`).Scan(&winning); err != nil {
		t.Fatal(err)
	}
	if winning != "survivor-000" {
		t.Fatalf("state conflict did not preserve survivor value: %q", winning)
	}
	if err := db.QueryRow(`SELECT value FROM cron_job_state WHERE job_id='job-early' AND key='key-149'`).Scan(&moved); err != nil {
		t.Fatal(err)
	}
	if moved != "loser-149" {
		t.Fatalf("loser-only state not moved: %q", moved)
	}

	var conflictRows, conflictPaused, renamed int
	if err := db.QueryRow(`SELECT COUNT(*),
		SUM(CASE WHEN status='paused' THEN 1 ELSE 0 END),
		SUM(CASE WHEN name<>'conflict-name' THEN 1 ELSE 0 END)
		FROM cron_jobs WHERE user_id='conflict-user'`).Scan(&conflictRows, &conflictPaused, &renamed); err != nil {
		t.Fatal(err)
	}
	if conflictRows != 2 || conflictPaused != 2 || renamed != 1 {
		t.Fatalf("conflict quarantine rows=%d paused=%d renamed=%d, want 2/2/1", conflictRows, conflictPaused, renamed)
	}
	var isolatedName string
	if err := db.QueryRow(`SELECT name FROM cron_jobs WHERE id='conflict-b'`).Scan(&isolatedName); err != nil {
		t.Fatal(err)
	}
	if isolatedName != "conflict-name · 隔离 · conflict-b" {
		t.Fatalf("deterministic isolated name=%q", isolatedName)
	}
	var conflictRunOwner string
	if err := db.QueryRow(`SELECT job_id FROM cron_job_runs WHERE id=900`).Scan(&conflictRunOwner); err != nil {
		t.Fatal(err)
	}
	if conflictRunOwner != "conflict-b" {
		t.Fatalf("isolated job history moved/deleted: owner=%q", conflictRunOwner)
	}

	var parentPayload, statePayload string
	if err := db.QueryRow(`SELECT payload_json FROM cron_job_merge_audit
		WHERE event_kind='merge_parent' AND loser_job_id='job-late'`).Scan(&parentPayload); err != nil {
		t.Fatalf("missing parent audit: %v", err)
	}
	if err := db.QueryRow(`SELECT payload_json FROM cron_job_merge_audit
		WHERE event_kind='state_conflict' AND loser_job_id='job-late' AND child_key='key-000'`).Scan(&statePayload); err != nil {
		t.Fatalf("missing state conflict audit: %v", err)
	}
	var parent map[string]any
	if err := json.Unmarshal([]byte(parentPayload), &parent); err != nil {
		t.Fatalf("parent audit JSON: %v", err)
	}
	for key, want := range map[string]string{
		"id": "job-late", "source_prompt": "private-late", "spec_json": `{"runtime":"starlark","script":"late"}`,
		"meta": `{"source_key":"same-source","private":"meta-late"}`,
	} {
		if got := fmt.Sprint(parent[key]); got != want {
			t.Fatalf("parent audit %s=%q, want %q", key, got, want)
		}
	}
	var stateRow map[string]any
	if err := json.Unmarshal([]byte(statePayload), &stateRow); err != nil {
		t.Fatalf("state audit JSON: %v", err)
	}
	if got := fmt.Sprint(stateRow["value"]); got != "loser-000" {
		t.Fatalf("state audit value=%q", got)
	}
	assertNoForeignKeys(t, db, "cron_job_merge_audit")

	if _, err := db.Exec(`INSERT INTO cron_jobs
		(id,name,type,schedule,spec_json,source_prompt,user_id,platform,chat_id,status,next_run_at,run_count,created_at,meta)
		VALUES ('new-duplicate','same-name','cron','@daily','{}','','merge-user','','','paused','2026-08-01',0,'2026-08-01','{}')`); err == nil {
		t.Fatal("unique (user_id,name) must reject a duplicate after repair")
	}
	result, err := db.Exec(`INSERT INTO cron_job_runs(job_id,status,data_json) VALUES ('job-early','success','{"after":true}')`)
	if err != nil {
		t.Fatalf("insert run after repair: %v", err)
	}
	newID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if newID <= 1200 {
		t.Fatalf("AUTOINCREMENT sequence regressed: new id=%d, want >1200", newID)
	}
	var schemaVersionBefore, schemaVersionAfter int
	if err := db.QueryRow(`PRAGMA schema_version`).Scan(&schemaVersionBefore); err != nil {
		t.Fatal(err)
	}
	if err := RepairCronIntegrityV29(context.Background(), db); err != nil {
		t.Fatalf("canonical runtime recheck: %v", err)
	}
	if err := db.QueryRow(`PRAGMA schema_version`).Scan(&schemaVersionAfter); err != nil {
		t.Fatal(err)
	}
	if schemaVersionAfter != schemaVersionBefore {
		t.Fatalf("canonical runtime recheck rewrote schema: before=%d after=%d", schemaVersionBefore, schemaVersionAfter)
	}

	var auditBefore int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_job_merge_audit`).Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}
	execCronIntegritySQL(t, db, `DELETE FROM schema_migrations WHERE version=29`)
	if err := Run(context.Background(), db, All); err != nil {
		t.Fatalf("v29 reentry: %v", err)
	}
	var auditAfter int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_job_merge_audit`).Scan(&auditAfter); err != nil {
		t.Fatal(err)
	}
	if auditAfter != auditBefore {
		t.Fatalf("audit is not idempotent: before=%d after=%d", auditBefore, auditAfter)
	}
}

func TestCronIntegrityV29RunCountOverflowRollsBackWithoutMutation(t *testing.T) {
	db := openCronIntegrityTestDB(t)
	createPostV5BrokenCronSchema(t, db)
	execCronIntegritySQL(t, db, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, description TEXT NOT NULL DEFAULT '', applied_at INTEGER NOT NULL)`)
	execCronIntegritySQL(t, db, `INSERT INTO schema_migrations(version,description,applied_at)
		VALUES (28,'before cron repair',1)`)
	for _, row := range []struct {
		id, created string
		runCount    int64
	}{
		{"overflow-a", "2026-07-01T00:00:00Z", math.MaxInt64},
		{"overflow-b", "2026-07-02T00:00:00Z", 1},
	} {
		execCronIntegritySQL(t, db, `INSERT INTO cron_jobs
			(id,name,type,schedule,spec_json,source_prompt,user_id,status,next_run_at,run_count,created_at,meta)
			VALUES (?,'overflow','cron','@daily','{}','','overflow-user','active','2026-08-01',?,?,
			        '{"source_key":"same-source"}')`, row.id, row.runCount, row.created)
	}

	err := Run(context.Background(), db, All)
	if err == nil || !strings.Contains(err.Error(), "run_count overflow") {
		t.Fatalf("v29 must fail closed on run_count overflow, got %v", err)
	}
	var rows, auditRows, v29Rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cron_jobs WHERE user_id='overflow-user'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("overflow rollback parent rows=%d, want 2", rows)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='cron_job_merge_audit'`).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if auditRows != 0 {
		t.Fatal("overflow rollback left merge audit schema behind")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=29`).Scan(&v29Rows); err != nil {
		t.Fatal(err)
	}
	if v29Rows != 0 {
		t.Fatal("overflow rollback recorded migration v29")
	}
}

func TestCronIntegrityV29RollsBackOnForeignKeyViolation(t *testing.T) {
	db := openCronIntegrityTestDB(t)
	createPostV5BrokenCronSchema(t, db)
	execCronIntegritySQL(t, db, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, description TEXT NOT NULL DEFAULT '', applied_at INTEGER NOT NULL)`)
	execCronIntegritySQL(t, db, `INSERT INTO schema_migrations(version,description,applied_at) VALUES (28,'before cron repair',1)`)
	execCronIntegritySQL(t, db, `PRAGMA foreign_keys=OFF`)
	execCronIntegritySQL(t, db, `INSERT INTO cron_job_runs(id,job_id,status,data_json)
		VALUES (77,'missing-parent','success','{"must":"survive-rollback"}')`)
	execCronIntegritySQL(t, db, `PRAGMA foreign_keys=ON`)

	err := Run(context.Background(), db, All)
	if err == nil {
		t.Fatal("v29 must reject an orphan instead of committing an invalid repair")
	}
	var versionCount int
	if scanErr := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=29`).Scan(&versionCount); scanErr != nil {
		t.Fatal(scanErr)
	}
	if versionCount != 0 {
		t.Fatal("failed repair recorded migration v29")
	}
	if columnExistsForCronTest(t, db, "cron_jobs", "prompt") {
		t.Fatal("fixture should be post-v5 and have no prompt")
	}
	var payload string
	if scanErr := db.QueryRow(`SELECT data_json FROM cron_job_runs WHERE id=77`).Scan(&payload); scanErr != nil {
		t.Fatalf("rollback lost original orphan evidence: %v", scanErr)
	}
	if payload != `{"must":"survive-rollback"}` {
		t.Fatalf("rollback changed payload: %q", payload)
	}
	var foreignKeys int
	if scanErr := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); scanErr != nil {
		t.Fatal(scanErr)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys=%d after rollback, want 1", foreignKeys)
	}
}

func createPostV5BrokenCronSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	execCronIntegritySQL(t, db, `CREATE TABLE cron_jobs (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		type TEXT DEFAULT 'cron',
		schedule TEXT NOT NULL,
		spec_json TEXT DEFAULT '',
		source_prompt TEXT DEFAULT '',
		user_id TEXT NOT NULL,
		platform TEXT DEFAULT '',
		chat_id TEXT DEFAULT '',
		status TEXT DEFAULT 'active',
		last_run_at DATETIME,
		next_run_at DATETIME,
		run_count INTEGER DEFAULT 0,
		created_at DATETIME,
		meta TEXT DEFAULT '{}'
	)`)
	execCronIntegritySQL(t, db, `CREATE TABLE cron_job_runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id TEXT NOT NULL,
		status TEXT DEFAULT 'success',
		result TEXT DEFAULT '',
		error TEXT DEFAULT '',
		duration_ms INTEGER DEFAULT 0,
		run_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		stdout TEXT DEFAULT '',
		stderr TEXT DEFAULT '',
		exit_code INTEGER DEFAULT 0,
		data_json TEXT DEFAULT ''
	)`)
	execCronIntegritySQL(t, db, `CREATE TABLE cron_job_state (
		job_id TEXT NOT NULL,
		key TEXT NOT NULL,
		value TEXT DEFAULT '',
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(job_id,key)
	)`)
}

func seedMergeGroup(t *testing.T, db *sql.DB) {
	t.Helper()
	jobs := []struct {
		id, user, name, created, sourcePrompt, spec, meta string
	}{
		{"job-null", "merge-user", "same-name", "", "private-null", `{"runtime":"starlark","script":"null"}`, `{"source_key":"same-source"}`},
		{"job-late", "merge-user", "same-name", "2026-07-02T00:00:00Z", "private-late", `{"runtime":"starlark","script":"late"}`, `{"source_key":"same-source","private":"meta-late"}`},
		{"job-early", "merge-user", "same-name", "2026-07-01T00:00:00Z", "private-early", `{"runtime":"starlark","script":"early"}`, `{"source_key":"same-source"}`},
		{"tie-b", "tie-user", "tie-name", "2026-07-03T00:00:00Z", "tie-b", `{}`, `{}`},
		{"tie-a", "tie-user", "tie-name", "2026-07-03T00:00:00Z", "tie-a", `{}`, `{}`},
	}
	for _, job := range jobs {
		var created any = job.created
		if job.created == "" {
			created = nil
		}
		execCronIntegritySQL(t, db, `INSERT INTO cron_jobs
			(id,name,type,schedule,spec_json,source_prompt,user_id,platform,chat_id,status,next_run_at,run_count,created_at,meta)
			VALUES (?,?, 'cron','@daily',?,?,?,'','','active','2026-08-01T00:00:00Z',0,?,?)`,
			job.id, job.name, job.spec, job.sourcePrompt, job.user, created, job.meta)
	}
	execCronIntegritySQL(t, db, `UPDATE cron_jobs SET run_count=3,last_run_at='2026-07-03T00:00:00Z' WHERE id='job-early'`)
	execCronIntegritySQL(t, db, `UPDATE cron_jobs SET run_count=250,last_run_at='2026-07-06T00:00:00Z' WHERE id='job-late'`)
	execCronIntegritySQL(t, db, `UPDATE cron_jobs SET run_count=7,last_run_at='2026-07-04T00:00:00Z' WHERE id='job-null'`)
	for id := 1; id <= 253; id++ {
		owner := "job-late"
		if id <= 3 {
			owner = "job-early"
		}
		execCronIntegritySQL(t, db, `INSERT INTO cron_job_runs
			(id,job_id,status,result,error,duration_ms,run_at,stdout,stderr,exit_code,data_json)
			VALUES (?,?,'success','result','',1,'2026-07-03T00:00:00Z','stdout','stderr',0,?)`,
			id, owner, fmt.Sprintf(`{"run":%d}`, id))
	}
	for key := 0; key < 100; key++ {
		execCronIntegritySQL(t, db, `INSERT INTO cron_job_state(job_id,key,value,updated_at) VALUES (?,?,?,'2026-07-03')`,
			"job-early", fmt.Sprintf("key-%03d", key), fmt.Sprintf("survivor-%03d", key))
	}
	for key := 0; key < 150; key++ {
		execCronIntegritySQL(t, db, `INSERT INTO cron_job_state(job_id,key,value,updated_at) VALUES (?,?,?,'2026-07-04')`,
			"job-late", fmt.Sprintf("key-%03d", key), fmt.Sprintf("loser-%03d", key))
	}
	// Preserve a sequence high-water mark even when the highest row was deleted.
	execCronIntegritySQL(t, db, `INSERT INTO cron_job_runs(id,job_id,status) VALUES (1200,'job-early','success')`)
	execCronIntegritySQL(t, db, `DELETE FROM cron_job_runs WHERE id=1200`)
}

func seedConflictGroup(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, row := range []struct{ id, source, created string }{
		{"conflict-a", "source-a", "2026-07-01T00:00:00Z"},
		{"conflict-b", "source-b", "2026-07-02T00:00:00Z"},
	} {
		execCronIntegritySQL(t, db, `INSERT INTO cron_jobs
			(id,name,type,schedule,spec_json,source_prompt,user_id,status,next_run_at,created_at,meta)
			VALUES (?,'conflict-name','cron','@daily','{}','private','conflict-user','active','2026-08-01',?,?)`,
			row.id, row.created, fmt.Sprintf(`{"source_key":%q}`, row.source))
	}
	execCronIntegritySQL(t, db, `INSERT INTO cron_job_runs(id,job_id,status,data_json)
		VALUES (900,'conflict-b','success','{"conflict":true}')`)
}

func columnExistsForCronTest(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func assertNamedIndexes(t *testing.T, db *sql.DB, table string, want []string) {
	t.Helper()
	rows, err := db.Query(`PRAGMA index_list(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(name, "sqlite_autoindex_") {
			continue
		}
		got = append(got, name)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s indexes=%v, want %v", table, got, want)
	}
}

func assertForeignKeyTarget(t *testing.T, db *sql.DB, table, wantTable, wantFrom, wantTo string) {
	t.Helper()
	rows, err := db.Query(`PRAGMA foreign_key_list(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, seq int
		var target, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &target, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatal(err)
		}
		if target == wantTable && from == wantFrom && to == wantTo && strings.EqualFold(onDelete, "CASCADE") {
			return
		}
	}
	t.Fatalf("%s missing FK %s -> %s.%s ON DELETE CASCADE", table, wantFrom, wantTable, wantTo)
}

func assertNoForeignKeys(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_list(?)`, table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("%s has %d FK(s), want none", table, count)
	}
}

func assertCanonicalCronSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	wantJobs := []string{"id", "name", "type", "schedule", "spec_json", "source_prompt", "user_id", "platform", "chat_id", "status", "last_run_at", "next_run_at", "run_count", "created_at", "meta"}
	wantRuns := []string{"id", "job_id", "status", "result", "error", "duration_ms", "run_at", "stdout", "stderr", "exit_code", "data_json"}
	wantState := []string{"job_id", "key", "value", "updated_at"}
	for table, want := range map[string][]string{"cron_jobs": wantJobs, "cron_job_runs": wantRuns, "cron_job_state": wantState} {
		rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			got = append(got, name)
		}
		rows.Close()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s columns=%v, want %v", table, got, want)
		}
	}
	assertNamedIndexes(t, db, "cron_jobs", []string{"idx_cron_jobs_status", "idx_cron_jobs_user", "idx_cron_jobs_user_name"})
	assertNamedIndexes(t, db, "cron_job_runs", []string{"idx_cron_job_runs_job", "idx_cron_job_runs_job_id"})
	assertForeignKeyTarget(t, db, "cron_job_runs", "cron_jobs", "job_id", "id")
	assertForeignKeyTarget(t, db, "cron_job_state", "cron_jobs", "job_id", "id")
	var fkEnabled int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fkEnabled); err != nil {
		t.Fatal(err)
	}
	if fkEnabled != 1 {
		t.Fatalf("foreign_keys=%d, want 1", fkEnabled)
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check=%q", integrity)
	}
}
