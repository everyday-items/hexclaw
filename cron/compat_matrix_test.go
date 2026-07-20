package cron

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// 方法 9 — 兼容性矩阵：覆盖从所有可能的"启动前 schema"过渡到 v2 schema。
//
// 矩阵：
//
//   schema 形态                                     | 期望
//   ------------------------------------------------|----------------------------------
//   F1 全新空库（无表）                              | CREATE → 直接得到 v2 schema
//   F2 v1 schema（prompt NOT NULL + 无 spec_json）   | ALTER ADD + 表重建 → 落到 v2，旧任务清理
//   F3 v1.5 半 v2（prompt 列存在 + 已加 spec_json）  | 表重建 → 落到 v2
//   F4 v2 fresh（无 prompt 列）                      | no-op，schema 保持 v2
//   F5 v1 但已没旧任务（行 0）                       | 表重建 → 落到 v2
//
// 通过 PRAGMA table_info 验证：v2 schema 不含 prompt 列、含 spec_json/source_prompt。

func tableHasColumn(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		_ = rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk)
		if name == col {
			return true
		}
	}
	return false
}

func assertV2Schema(t *testing.T, db *sql.DB) {
	t.Helper()
	if tableHasColumn(t, db, "cron_jobs", "prompt") {
		t.Fatal("v2 schema 不应再含 prompt 列")
	}
	for _, c := range []string{"spec_json", "source_prompt"} {
		if !tableHasColumn(t, db, "cron_jobs", c) {
			t.Fatalf("v2 schema 缺少必须列 %q", c)
		}
	}
}

// F1
func TestCompat_F1_EmptyDB(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	assertV2Schema(t, db)
}

// F2
func TestCompat_F2_V1WithLegacyJobs(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// 构造 v1：prompt NOT NULL，无 spec_json
	mustExec(t, db, `CREATE TABLE cron_jobs (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, type TEXT NOT NULL DEFAULT 'cron',
		schedule TEXT NOT NULL, prompt TEXT NOT NULL, user_id TEXT NOT NULL,
		platform TEXT DEFAULT '', chat_id TEXT DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active', last_run_at DATETIME,
		next_run_at DATETIME NOT NULL, run_count INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`)
	mustExec(t, db, `INSERT INTO cron_jobs (id, name, type, schedule, prompt, user_id, next_run_at)
		VALUES ('legacy-1','旧任务','cron','@daily','旧 prompt','u1', ?)`, time.Now())

	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	assertV2Schema(t, db)

	// 旧任务保留但暂停，不进入调度。
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs WHERE status='paused'`).Scan(&n)
	if n != 1 {
		t.Errorf("旧任务应保留并暂停，实际 %d", n)
	}
	if len(s.jobs) != 0 {
		t.Errorf("隔离任务不应加载，jobs=%d", len(s.jobs))
	}
}

// F3 v1.5 半 v2（prompt 列 + spec_json 列共存，有 v2 任务）
func TestCompat_F3_V1HalfWithV2Job(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	mustExec(t, db, `CREATE TABLE cron_jobs (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, type TEXT NOT NULL DEFAULT 'cron',
		schedule TEXT NOT NULL, prompt TEXT NOT NULL DEFAULT '', user_id TEXT NOT NULL,
		platform TEXT DEFAULT '', chat_id TEXT DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active', last_run_at DATETIME,
		next_run_at DATETIME NOT NULL, run_count INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		spec_json TEXT NOT NULL DEFAULT '',
		source_prompt TEXT NOT NULL DEFAULT '')`)

	// 1 条 v2 任务（spec_json 非空）+ 1 条 v1 残留（prompt 非空 spec_json 空）
	mustExec(t, db, `INSERT INTO cron_jobs (id, name, type, schedule, prompt, user_id, next_run_at, spec_json, source_prompt)
		VALUES ('v2-1','v2 任务','cron','@daily','','u1', ?, '{"runtime":"python3","script":"print(1)"}', 'p')`, time.Now().Add(time.Hour))
	mustExec(t, db, `INSERT INTO cron_jobs (id, name, type, schedule, prompt, user_id, next_run_at)
		VALUES ('legacy','旧','cron','@daily','旧','u1', ?)`, time.Now().Add(time.Hour))

	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	assertV2Schema(t, db)

	// v2 任务活跃保留、v1 暂停保留。
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs`).Scan(&n)
	if n != 2 {
		t.Errorf("应保留 v1/v2 共 2 条任务，实际 %d", n)
	}
	var id string
	_ = db.QueryRowContext(ctx, `SELECT id FROM cron_jobs WHERE status='active'`).Scan(&id)
	if id != "v2-1" {
		t.Errorf("活跃的应是 v2-1，实际 %q", id)
	}
	var legacyStatus string
	_ = db.QueryRowContext(ctx, `SELECT status FROM cron_jobs WHERE id='legacy'`).Scan(&legacyStatus)
	if legacyStatus != "paused" {
		t.Errorf("legacy status=%q, want paused", legacyStatus)
	}
}

// F4 v2 fresh — 二次 Init 不应再触发重建
func TestCompat_F4_V2Idempotent(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init1: %v", err)
	}
	job := &Job{
		Name: "x", Schedule: "@hourly", SourcePrompt: "p",
		Spec: minimalSpec(), UserID: "u1",
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// 第二次 Init 不应破坏数据
	s2 := newTestScheduler(t, db)
	if err := s2.Init(ctx); err != nil {
		t.Fatalf("Init2: %v", err)
	}
	assertV2Schema(t, db)
	if len(s2.jobs) != 1 {
		t.Errorf("二次 Init 后应仍有 1 条任务，实际 %d", len(s2.jobs))
	}
}

// F5 v1 schema 但空表
func TestCompat_F5_V1Empty(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	mustExec(t, db, `CREATE TABLE cron_jobs (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, type TEXT NOT NULL DEFAULT 'cron',
		schedule TEXT NOT NULL, prompt TEXT NOT NULL, user_id TEXT NOT NULL,
		platform TEXT DEFAULT '', chat_id TEXT DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active', last_run_at DATETIME,
		next_run_at DATETIME NOT NULL, run_count INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`)

	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	assertV2Schema(t, db)

	// 后续 AddJob 应成功（验证 INSERT 不再撞 NOT NULL）
	job := &Job{
		Name: "x", Schedule: "@hourly", SourcePrompt: "p",
		Spec: minimalSpec(), UserID: "u1",
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob after migration: %v", err)
	}
}

// F6 ESC 真实复现：模拟用户机器 schema（含 meta、UNIQUE INDEX）
func TestCompat_F6_RealUserSchema_WithIndexes(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	mustExec(t, db, `CREATE TABLE cron_jobs (
		id          TEXT    PRIMARY KEY,
		name        TEXT    NOT NULL,
		type        TEXT    NOT NULL DEFAULT 'cron',
		schedule    TEXT    NOT NULL,
		prompt      TEXT    NOT NULL,
		user_id     TEXT    NOT NULL,
		platform    TEXT    NOT NULL DEFAULT '',
		chat_id     TEXT    NOT NULL DEFAULT '',
		status      TEXT    NOT NULL DEFAULT 'active',
		last_run_at DATETIME,
		next_run_at DATETIME NOT NULL,
		run_count   INTEGER NOT NULL DEFAULT 0,
		meta        TEXT    NOT NULL DEFAULT '{}',
		created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	mustExec(t, db, `CREATE INDEX idx_cron_jobs_user ON cron_jobs(user_id)`)
	mustExec(t, db, `CREATE INDEX idx_cron_jobs_status ON cron_jobs(status, next_run_at)`)
	mustExec(t, db, `CREATE UNIQUE INDEX idx_cron_jobs_user_name ON cron_jobs(user_id, name)`)

	mustExec(t, db, `INSERT INTO cron_jobs (id, name, type, schedule, prompt, user_id, next_run_at)
		VALUES ('legacy','旧','cron','@daily','x','u1', ?)`, time.Now())

	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	assertV2Schema(t, db)

	// AddJob 端到端通（防"NOT NULL constraint failed: cron_jobs.prompt"回归）
	job := &Job{
		Name: "post-migration", Schedule: "@hourly", SourcePrompt: "p",
		Spec: minimalSpec(), UserID: "u1",
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("回归：AddJob 失败 %v —— migration 未真正铲掉 prompt NOT NULL 约束", err)
	}
	if !strings.HasPrefix(job.ID, "cron-") {
		t.Errorf("ID: %q", job.ID)
	}
}

func mustExec(t *testing.T, db *sql.DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
