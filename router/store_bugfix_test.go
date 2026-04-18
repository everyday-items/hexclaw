package router

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// TestAgentRules_UniqueConstraint_Fix8 回归测试：
// 修复前：SaveRule 用普通 INSERT，无 UNIQUE 约束，同一 5 元组规则被
// Sync 反复写入。每次启动规则数翻倍（2^n），543 万条全是同一规则。
// 修复后：UNIQUE INDEX + ON CONFLICT DO UPDATE，重复写入是 no-op。
func TestAgentRules_UniqueConstraint_Fix8(t *testing.T) {
	t.Run("before_fix_behavior_no_unique_constraint_allows_duplicates", func(t *testing.T) {
		// 还原"修复前"的建表：没有 UNIQUE 索引
		db := openMemDB(t)
		defer db.Close()
		mustExec(t, db, `CREATE TABLE agents (name TEXT PRIMARY KEY)`)
		mustExec(t, db, `INSERT INTO agents(name) VALUES('a1')`)
		mustExec(t, db, `CREATE TABLE agent_rules (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			platform TEXT NOT NULL DEFAULT '',
			instance_id TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			chat_id TEXT NOT NULL DEFAULT '',
			agent_name TEXT NOT NULL REFERENCES agents(name),
			priority INTEGER NOT NULL DEFAULT 0
		)`)

		// 模拟 Sync 被调用 23 次：每次把已有规则重新 INSERT（历史 bug 行为）
		for i := 0; i < 100; i++ {
			mustExec(t, db,
				`INSERT INTO agent_rules (platform, agent_name) VALUES ('feishu','a1')`)
		}
		count := mustCount(t, db, "SELECT COUNT(*) FROM agent_rules")
		if count != 100 {
			t.Fatalf("pre-fix baseline: expected 100 duplicates but got %d", count)
		}
		t.Logf("修复前：同一规则被插入 %d 次（线上 543 万行的复现）", count)
	})

	t.Run("after_fix_behavior_unique_constraint_prevents_duplicates", func(t *testing.T) {
		ctx := context.Background()
		db := openMemDB(t)
		defer db.Close()
		mustExec(t, db, `CREATE TABLE agents (name TEXT PRIMARY KEY)`)
		mustExec(t, db, `INSERT INTO agents(name) VALUES('a1')`)

		store := NewSQLiteStore(db)
		if err := store.Init(ctx); err != nil {
			t.Fatalf("store.Init: %v", err)
		}

		// 重复调用 SaveRule 相同 5 元组 100 次
		for i := 0; i < 100; i++ {
			r := Rule{Platform: "feishu", AgentName: "a1", Priority: 1}
			if err := store.SaveRule(ctx, &r); err != nil {
				t.Fatalf("SaveRule iter %d: %v", i, err)
			}
		}
		count := mustCount(t, db, "SELECT COUNT(*) FROM agent_rules")
		if count != 1 {
			t.Fatalf("修复后期望唯一保留 1 条，实际 %d 条（UNIQUE 约束未生效）", count)
		}
		t.Logf("修复后：同一规则 INSERT 100 次 → DB 保留 %d 条", count)
	})

	t.Run("after_fix_different_keys_still_stored_separately", func(t *testing.T) {
		ctx := context.Background()
		db := openMemDB(t)
		defer db.Close()
		mustExec(t, db, `CREATE TABLE agents (name TEXT PRIMARY KEY)`)
		mustExec(t, db, `INSERT INTO agents(name) VALUES('a1')`)
		mustExec(t, db, `INSERT INTO agents(name) VALUES('a2')`)

		store := NewSQLiteStore(db)
		if err := store.Init(ctx); err != nil {
			t.Fatal(err)
		}

		// 不同 5 元组应当各自保留
		rules := []Rule{
			{Platform: "feishu", AgentName: "a1"},
			{Platform: "feishu", UserID: "u1", AgentName: "a1"},
			{Platform: "discord", AgentName: "a2"},
		}
		for i := range rules {
			if err := store.SaveRule(ctx, &rules[i]); err != nil {
				t.Fatalf("rule %d: %v", i, err)
			}
		}
		count := mustCount(t, db, "SELECT COUNT(*) FROM agent_rules")
		if count != 3 {
			t.Fatalf("期望 3 条不同规则，实际 %d", count)
		}
	})

	t.Run("after_fix_dedup_cleans_historical_duplicates", func(t *testing.T) {
		ctx := context.Background()
		db := openMemDB(t)
		defer db.Close()
		mustExec(t, db, `CREATE TABLE agents (name TEXT PRIMARY KEY)`)
		mustExec(t, db, `INSERT INTO agents(name) VALUES('a1')`)
		// 模拟升级前 DB 已有重复（先建无约束表）
		mustExec(t, db, `CREATE TABLE agent_rules (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			platform TEXT NOT NULL DEFAULT '',
			instance_id TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			chat_id TEXT NOT NULL DEFAULT '',
			agent_name TEXT NOT NULL REFERENCES agents(name),
			priority INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`)
		for i := 0; i < 500; i++ {
			mustExec(t, db,
				`INSERT INTO agent_rules (platform, agent_name) VALUES ('feishu','a1')`)
		}
		before := mustCount(t, db, "SELECT COUNT(*) FROM agent_rules")
		if before != 500 {
			t.Fatalf("seed failed: %d", before)
		}

		// Init 应触发 dedupeAgentRules
		store := NewSQLiteStore(db)
		if err := store.Init(ctx); err != nil {
			t.Fatal(err)
		}
		after := mustCount(t, db, "SELECT COUNT(*) FROM agent_rules")
		if after != 1 {
			t.Fatalf("Init 后应去重至 1 条，实际 %d", after)
		}
		t.Logf("历史重复 500 条 → 启动 Init 自动去重后 %d 条", after)
	})
}

func openMemDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	return db
}
func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
func mustCount(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
