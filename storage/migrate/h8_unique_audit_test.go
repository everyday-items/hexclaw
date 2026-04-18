package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestH8_UniqueAudit_Fix_v0_3_12 回归测试：
// 修复前（v0.3.12 之前）kb_documents 和 cron_jobs 无业务唯一键保护；
// kb_documents: 同一教材重上传 → 记录重复 → 知识检索返回重复命中 + DB 体积累加
// cron_jobs: 同用户同名任务重复声明 → 定时触发 2 次 → 误触发+成本翻倍
// 修复后：Version 2 migration 加 UNIQUE + 对历史重复保留最新一条。
func TestH8_UniqueAudit_Fix_v0_3_12(t *testing.T) {
	t.Run("before_fix_behavior_kb_documents_allow_dupes", func(t *testing.T) {
		db := openMigrated(t, 1) // 只跑 v1，未应用 v2
		defer db.Close()

		// 插入同 source+title 两条
		now := time.Now()
		mustExec(t, db, `INSERT INTO kb_documents (id, title, content, source, source_type, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, "d1", "人教版数学三年级", "c1", "book.pdf", "upload", now, now)
		mustExec(t, db, `INSERT INTO kb_documents (id, title, content, source, source_type, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, "d2", "人教版数学三年级", "c2", "book.pdf", "upload", now.Add(time.Second), now.Add(time.Second))

		var count int
		db.QueryRow("SELECT COUNT(*) FROM kb_documents WHERE source='book.pdf'").Scan(&count)
		if count != 2 {
			t.Errorf("修复前应允许重复，实际 %d", count)
		}
		t.Logf("修复前：同 source+title 重复上传产生 %d 条记录", count)
	})

	t.Run("after_fix_kb_documents_migration_dedupes_and_blocks", func(t *testing.T) {
		db := openMigrated(t, 1)
		defer db.Close()

		now := time.Now()
		// 插入 3 条重复（作为历史遗留）
		for i, id := range []string{"d1", "d2", "d3"} {
			mustExec(t, db, `INSERT INTO kb_documents (id, title, content, source, source_type, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, id, "数学书", "c"+id, "math.pdf", "upload",
				now.Add(time.Duration(i)*time.Second), now.Add(time.Duration(i)*time.Second))
		}

		// 跑 v2 migration
		runMigration(t, db, 2)

		// 应只保留 1 条（MIN(rowid) = d1，最早插入的那条；deterministic 无时间戳碰撞风险）
		var count int
		db.QueryRow("SELECT COUNT(*) FROM kb_documents WHERE source='math.pdf'").Scan(&count)
		if count != 1 {
			t.Errorf("期望 dedupe 后保留 1 条，实际 %d", count)
		}
		var keptID string
		db.QueryRow("SELECT id FROM kb_documents WHERE source='math.pdf'").Scan(&keptID)
		if keptID != "d1" {
			t.Errorf("应保留 MIN(rowid) 的 d1，实际保留 %q", keptID)
		}
		t.Logf("修复后：dedupe 3 条 → 保留最早（d1，rowid 最小）✓")

		// 再插入相同 source+title 应被唯一索引拒绝
		_, err := db.Exec(`INSERT INTO kb_documents (id, title, content, source, source_type, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, "d4", "数学书", "c4", "math.pdf", "upload", now, now)
		if err == nil {
			t.Error("修复后同 source+title 应被 UNIQUE 拦截")
		}
	})

	t.Run("after_fix_kb_documents_empty_source_not_constrained", func(t *testing.T) {
		// 手动输入（source 为空）不受唯一性约束，允许多条同标题
		db := openMigrated(t, 2)
		defer db.Close()

		now := time.Now()
		for _, id := range []string{"m1", "m2"} {
			_, err := db.Exec(`INSERT INTO kb_documents (id, title, content, source, source_type, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, id, "笔记", "content", "", "manual", now, now)
			if err != nil {
				t.Errorf("source 空的记录应允许多条：%v", err)
			}
		}
	})

	t.Run("before_fix_cron_jobs_allow_dupes", func(t *testing.T) {
		db := openMigrated(t, 1)
		defer db.Close()

		now := time.Now()
		mustExec(t, db, `INSERT INTO cron_jobs (id, name, schedule, prompt, user_id, next_run_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, "j1", "每周错题卷", "0 19 * * 5", "出 10 题", "u1", now, now)
		mustExec(t, db, `INSERT INTO cron_jobs (id, name, schedule, prompt, user_id, next_run_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, "j2", "每周错题卷", "0 19 * * 5", "出 10 题", "u1", now.Add(time.Second), now.Add(time.Second))

		var count int
		db.QueryRow("SELECT COUNT(*) FROM cron_jobs WHERE user_id='u1' AND name='每周错题卷'").Scan(&count)
		if count != 2 {
			t.Errorf("修复前应允许重复，实际 %d", count)
		}
	})

	t.Run("after_fix_cron_jobs_migration_dedupes_and_blocks", func(t *testing.T) {
		db := openMigrated(t, 1)
		defer db.Close()

		now := time.Now()
		for i, id := range []string{"j1", "j2", "j3"} {
			mustExec(t, db, `INSERT INTO cron_jobs (id, name, schedule, prompt, user_id, next_run_at, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				id, "每周错题卷", "0 19 * * 5", "prompt", "u1",
				now.Add(time.Duration(i)*time.Second), now.Add(time.Duration(i)*time.Second))
		}

		runMigration(t, db, 2)

		var count int
		db.QueryRow("SELECT COUNT(*) FROM cron_jobs WHERE user_id='u1' AND name='每周错题卷'").Scan(&count)
		if count != 1 {
			t.Errorf("dedupe 后应保留 1 条，实际 %d", count)
		}

		var keptID string
		db.QueryRow("SELECT id FROM cron_jobs WHERE user_id='u1'").Scan(&keptID)
		if keptID != "j1" {
			t.Errorf("应保留 MIN(rowid) 的 j1，实际 %q", keptID)
		}

		// 再插入同 user_id+name 应被唯一索引拒绝
		_, err := db.Exec(`INSERT INTO cron_jobs (id, name, schedule, prompt, user_id, next_run_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, "j4", "每周错题卷", "0 19 * * 5", "p", "u1", now, now)
		if err == nil {
			t.Error("应被 UNIQUE 拦截")
		}
	})

	t.Run("after_fix_cron_jobs_different_users_ok", func(t *testing.T) {
		// 不同用户可以有同名任务
		db := openMigrated(t, 2)
		defer db.Close()
		now := time.Now()
		for _, user := range []string{"alice", "bob"} {
			_, err := db.Exec(`INSERT INTO cron_jobs (id, name, schedule, prompt, user_id, next_run_at, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				user+"-j1", "每周错题卷", "0 19 * * 5", "prompt", user, now, now)
			if err != nil {
				t.Errorf("不同用户同名不应冲突：%v", err)
			}
		}
	})

	t.Run("after_fix_handles_identical_created_at_timestamps", func(t *testing.T) {
		// F6 回归：即使 created_at 毫秒级完全相同（批量写入 / 迁移脚本场景），
		// MIN(rowid) tiebreak 仍能保证只保留一行，UNIQUE INDEX 不会因"多行匹配 MAX"失败。
		db := openMigrated(t, 1)
		defer db.Close()

		sameTime := time.Now()
		for _, id := range []string{"t1", "t2", "t3"} {
			mustExec(t, db, `INSERT INTO kb_documents (id, title, content, source, source_type, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				id, "教材", "c", "同一时刻.pdf", "batch", sameTime, sameTime)
		}

		// 此时 3 行 created_at 完全相同；旧实现 MAX(created_at) 条件会匹配全部 3 行
		// → 一条都不删 → CREATE UNIQUE INDEX 报错。新实现用 MIN(rowid) 可正常处理。
		runMigration(t, db, 2)

		var count int
		db.QueryRow("SELECT COUNT(*) FROM kb_documents WHERE source='同一时刻.pdf'").Scan(&count)
		if count != 1 {
			t.Errorf("时间戳碰撞 tiebreak 失败：期望 1 行，实际 %d", count)
		}
	})

	t.Run("after_fix_migration_is_idempotent", func(t *testing.T) {
		// 跑两次 v2 migration 不应报错（IF NOT EXISTS 保护）
		db := openMigrated(t, 2)
		defer db.Close()
		runMigration(t, db, 2) // 第二次跑
		// 不 panic 即为成功
	})
}

// openMigrated 打开一个临时 SQLite + 应用到指定版本
func openMigrated(t *testing.T, upToVersion int) *sql.DB {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}

	// 建 schema_migrations 表
	mustExec(t, db, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at DATETIME NOT NULL
	)`)

	for _, m := range All {
		if m.Version > upToVersion {
			break
		}
		if _, err := db.Exec(m.SQL); err != nil {
			t.Fatalf("apply migration %d: %v", m.Version, err)
		}
	}
	return db
}

// runMigration 运行特定版本的 migration
func runMigration(t *testing.T, db *sql.DB, version int) {
	t.Helper()
	for _, m := range All {
		if m.Version == version {
			if _, err := db.Exec(m.SQL); err != nil {
				t.Fatalf("run migration %d: %v", version, err)
			}
			return
		}
	}
	t.Fatalf("migration version %d not found", version)
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
