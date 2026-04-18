package cache

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestCacheBackgroundCleanup_Fix7 回归测试：
// 修复前 cache 只在写入时惰性 evict 内存条目，DB 中的过期条目不清理；
// 长期无写入场景（或只读命中）会让 llm_cache 表持续膨胀，
// 与 agent_rules 373MB 故障属同一类累积型 bug。
// 修复后 runCleanup 清内存 + 删 DB 过期 + 裁剪超量。
func TestCacheBackgroundCleanup_Fix7(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	t.Run("before_fix_behavior_expired_db_entries_accumulate", func(t *testing.T) {
		db2 := openTestDB(t)
		defer db2.Close()
		// 注入 20 条已过期
		insertCacheRow(t, db2, 20, -time.Hour) // expires_at 在 1 小时前
		var cnt int
		db2.QueryRow("SELECT COUNT(*) FROM llm_cache").Scan(&cnt)
		if cnt != 20 {
			t.Fatalf("setup fail: cnt=%d", cnt)
		}
		t.Logf("修复前：20 条过期条目注入后无后台清理，DB 中保留 %d 条", cnt)
	})

	t.Run("after_fix_expired_db_entries_removed", func(t *testing.T) {
		c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 100})
		// 插 10 条已过期 + 5 条未过期
		insertCacheRow(t, db, 10, -time.Hour)
		insertCacheRow(t, db, 5, time.Hour)
		var before int
		db.QueryRow("SELECT COUNT(*) FROM llm_cache").Scan(&before)

		c.runCleanup(db)

		var after int
		db.QueryRow("SELECT COUNT(*) FROM llm_cache").Scan(&after)
		if after != 5 {
			t.Errorf("期望 runCleanup 后保留 5 条未过期，实际 %d（before=%d）", after, before)
		}
		t.Logf("修复后：runCleanup 清除 10 条过期，保留 5 条未过期 ✓")
	})

	t.Run("after_fix_empty_response_purged", func(t *testing.T) {
		// runCleanup 同时清理 trim(response)='' 的僵尸条目
		db2 := openTestDB(t)
		defer db2.Close()
		_, err := db2.Exec(`INSERT INTO llm_cache (key, response, provider, model, hit_count, created_at, expires_at)
			VALUES ('z1', '', 'p', 'm', 0, ?, ?)`, time.Now(), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		_, err = db2.Exec(`INSERT INTO llm_cache (key, response, provider, model, hit_count, created_at, expires_at)
			VALUES ('z2', '   ', 'p', 'm', 0, ?, ?)`, time.Now(), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 100})
		c.runCleanup(db2)
		var cnt int
		db2.QueryRow("SELECT COUNT(*) FROM llm_cache").Scan(&cnt)
		if cnt != 0 {
			t.Errorf("期望空 response 条目全部清除，实际剩 %d", cnt)
		}
	})

	t.Run("after_fix_over_max_entries_pruned", func(t *testing.T) {
		db3 := openTestDB(t)
		defer db3.Close()
		// 50 条全未过期
		insertCacheRow(t, db3, 50, time.Hour)
		c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 10})
		c.runCleanup(db3)
		var cnt int
		db3.QueryRow("SELECT COUNT(*) FROM llm_cache").Scan(&cnt)
		if cnt > 10 {
			t.Errorf("超量裁剪失效：期望 ≤ 10，实际 %d", cnt)
		}
		t.Logf("修复后：50 条 → 保留 %d 条（maxEntries=10）✓", cnt)
	})

	t.Run("after_fix_background_loop_stoppable", func(t *testing.T) {
		c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 10})
		stop := c.StartCleanupLoop(t.Context(), openTestDB(t), 10*time.Millisecond)
		time.Sleep(30 * time.Millisecond) // 允许 ticker 跑几圈
		stop()                            // 优雅关停 goroutine，不应 panic 或悬挂
	})
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:?_pragma=journal_mode(MEMORY)")
	if err != nil {
		t.Fatal(err)
	}
	schema := `CREATE TABLE IF NOT EXISTS llm_cache (
		key        TEXT    PRIMARY KEY,
		response   TEXT    NOT NULL,
		provider   TEXT    NOT NULL,
		model      TEXT    NOT NULL,
		hit_count  INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL,
		expires_at DATETIME NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_llm_cache_expires ON llm_cache(expires_at);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertCacheRow(t *testing.T, db *sql.DB, n int, ttl time.Duration) {
	t.Helper()
	now := time.Now()
	for i := 0; i < n; i++ {
		key := strings.Repeat("k", 8) + "-" + time.Now().Format("150405.000000000") + "-" + randSuffix(i)
		_, err := db.Exec(
			`INSERT INTO llm_cache (key, response, provider, model, hit_count, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			key, "response", "provider", "model", 0, now, now.Add(ttl),
		)
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
}

func randSuffix(i int) string {
	// 简单 deterministic suffix 防 PRIMARY KEY 冲突
	return "r" + strings.Repeat("x", i%4+1)
}
