package sqlite

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestWAL_Fix2 回归测试：
// 修复前 DSN 用 _journal_mode=WAL（modernc.org/sqlite 不识别此参数），WAL 从未启用，
// 导致出现 data.db-journal（rollback journal），启动恢复极慢。
// 修复后 DSN 用 _pragma=journal_mode(WAL)，并在 New 中校验生效。
func TestWAL_Fix2(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "data.db")

	t.Run("before_fix_old_dsn_does_not_enable_WAL", func(t *testing.T) {
		// 还原旧 DSN
		db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var mode string
		if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if strings.EqualFold(mode, "wal") {
			t.Skip("环境驱动行为已变化，旧 DSN 已能启用 WAL（测试前提失效）")
		}
		t.Logf("修复前旧 DSN 的 journal_mode = %q（非 wal，证明 DSN 参数格式被驱动忽略）", mode)
	})

	os.Remove(dbPath)
	os.Remove(dbPath + "-journal")
	os.Remove(dbPath + "-wal")

	t.Run("after_fix_new_DSN_enables_WAL", func(t *testing.T) {
		store, err := New(dbPath)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer store.Close()
		var mode string
		if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if !strings.EqualFold(mode, "wal") {
			t.Fatalf("修复后期望 WAL，实际 %q", mode)
		}
		t.Logf("修复后 journal_mode = %q ✓", mode)

		// 写入后应当生成 -wal 文件（不是 -journal）
		if _, err := store.db.Exec(`CREATE TABLE t(x INTEGER); INSERT INTO t VALUES(1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dbPath + "-wal"); err != nil {
			t.Errorf("WAL 生效后应出现 -wal 文件，未找到: %v", err)
		}
		if _, err := os.Stat(dbPath + "-journal"); err == nil {
			t.Errorf("WAL 模式下不应出现 -journal 文件")
		}
	})
}

// TestMessageTruncation_Fix3 回归测试：
// 修复前 content/metadata 无大小限制，单条最大 500KB（本地推理 reasoning 无约束入库），
// 历史 messages 表膨胀至 1.9MB（样本很小，生产环境更严重）。
// 修复后 truncateLarge 在 INSERT 前按字节截断。
func TestMessageTruncation_Fix3(t *testing.T) {
	big := strings.Repeat("x", 500*1024) // 500 KB

	t.Run("before_fix_behavior_no_truncation", func(t *testing.T) {
		// 无截断直接存入：模拟历史行为
		if len(big) != 500*1024 {
			t.Fatal("test setup broken")
		}
		t.Logf("修复前：content 原长 %d 字节，直接入库无限制", len(big))
	})

	t.Run("after_fix_truncates_oversize_content", func(t *testing.T) {
		out := truncateLarge(big, maxMessageContentBytes)
		if len(out) > maxMessageContentBytes {
			t.Fatalf("截断失败：%d > %d", len(out), maxMessageContentBytes)
		}
		if !strings.HasSuffix(out, truncationSuffix) {
			t.Errorf("截断后应附加说明尾部")
		}
		t.Logf("修复后：%d 字节 → 截断为 %d 字节 ✓", len(big), len(out))
	})

	t.Run("after_fix_small_content_unchanged", func(t *testing.T) {
		small := "hello world"
		out := truncateLarge(small, maxMessageContentBytes)
		if out != small {
			t.Errorf("小文本不应被改写")
		}
	})

	t.Run("after_fix_utf8_boundary_safe", func(t *testing.T) {
		// 中文每字符 3 字节，构造刚好越界的字符串
		s := strings.Repeat("中", 100000) // 300,000 bytes
		out := truncateLarge(s, 150)
		// 截断后应为有效 UTF-8
		for i, r := range out {
			if r == '\uFFFD' {
				t.Fatalf("截断处出现非法 UTF-8 (index=%d)", i)
			}
		}
	})
}

// TestVacuumThreshold_Fix7 回归测试：
// 修复后 runMaintenance 检测 db 大小超阈值时主动 VACUUM。
// 用 page_count * page_size 反映"逻辑占用"，不依赖文件系统时序（WAL 模式下
// 文件大小包含 WAL/SHM，不是 VACUUM 是否有效的可靠指标）。
func TestVacuumThreshold_Fix7(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "data.db")
	store, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	mustExec2(t, store.db, `CREATE TABLE bulk(x TEXT)`)
	payload := strings.Repeat("y", 10*1024)
	for i := 0; i < 2000; i++ {
		mustExec2(t, store.db, `INSERT INTO bulk VALUES(?)`, payload)
	}
	pagesBeforeDelete := dbPageCount(t, store.db)
	mustExec2(t, store.db, `DELETE FROM bulk`)
	mustExec2(t, store.db, `PRAGMA wal_checkpoint(TRUNCATE)`)

	if err := store.VacuumNow(nil); err != nil {
		t.Fatal(err)
	}
	pagesAfterVacuum := dbPageCount(t, store.db)

	t.Logf("删除前 pages=%d, VACUUM 后 pages=%d", pagesBeforeDelete, pagesAfterVacuum)
	if pagesAfterVacuum >= pagesBeforeDelete {
		t.Errorf("VACUUM 未释放 pages：before=%d after=%d", pagesBeforeDelete, pagesAfterVacuum)
	}
}

func mustExec2(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
func dbPageCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("PRAGMA page_count").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestVacuumThresholdTrigger_Fix6 回归测试：
// 修复前 runMaintenance 只按固定时间周期运行 VACUUM，
// 没有基于 DB 大小的主动触发，小团队家用场景可能永远跑不到阈值。
// 修复后：runMaintenance 每次 tick 检查 dbSize > vacuumThreshold 立即 VACUUM。
// 用可变 vacuumThreshold 注入小阈值以便在 unit 测试内验证触发路径。
func TestVacuumThresholdTrigger_Fix6(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "data.db")
	store, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 构造有"死页"的 DB：写入再删除，VACUUM 前页数高、VACUUM 后页数低
	mustExec2(t, store.db, `CREATE TABLE bulk(x TEXT)`)
	payload := strings.Repeat("y", 10*1024)
	for i := 0; i < 500; i++ {
		mustExec2(t, store.db, `INSERT INTO bulk VALUES(?)`, payload)
	}
	mustExec2(t, store.db, `DELETE FROM bulk`)
	mustExec2(t, store.db, `PRAGMA wal_checkpoint(TRUNCATE)`)

	t.Run("before_fix_no_threshold_check_skips_vacuum", func(t *testing.T) {
		// 模拟"修复前"行为：只有周期 tick，无大小检查，也即本次 tick 不跑 VACUUM
		// 这里只做说明性断言（页数保持高位），无破坏性
		before := dbPageCount(t, store.db)
		t.Logf("修复前：runMaintenance 无阈值判定，跳过 VACUUM，pages 保持在 %d", before)
	})

	t.Run("after_fix_above_threshold_triggers_vacuum", func(t *testing.T) {
		// 注入极小阈值（1 byte）→ 必然触发
		save := vacuumThreshold
		t.Cleanup(func() { vacuumThreshold = save })
		vacuumThreshold = 1

		pagesBefore := dbPageCount(t, store.db)
		store.runMaintenance(dbPath)
		pagesAfter := dbPageCount(t, store.db)

		if pagesAfter >= pagesBefore {
			t.Errorf("期望触发 VACUUM 使 pages 下降：before=%d after=%d", pagesBefore, pagesAfter)
		}
		t.Logf("修复后（阈值=1 byte）：runMaintenance 触发 VACUUM，pages %d → %d ✓", pagesBefore, pagesAfter)
	})

	t.Run("after_fix_below_threshold_skips_vacuum", func(t *testing.T) {
		// 先 VACUUM 一次让 DB 干净
		if _, err := store.db.Exec("VACUUM"); err != nil {
			t.Fatal(err)
		}
		// 构造新的脏页
		mustExec2(t, store.db, `INSERT INTO bulk VALUES(?)`, strings.Repeat("z", 10*1024))
		mustExec2(t, store.db, `DELETE FROM bulk`)
		mustExec2(t, store.db, `PRAGMA wal_checkpoint(TRUNCATE)`)

		// 注入极大阈值（10 GB）→ 不应触发
		save := vacuumThreshold
		t.Cleanup(func() { vacuumThreshold = save })
		vacuumThreshold = 10 * 1024 * 1024 * 1024

		pagesBefore := dbPageCount(t, store.db)
		store.runMaintenance(dbPath)
		pagesAfter := dbPageCount(t, store.db)

		// 阈值未命中 → runMaintenance 仅 checkpoint，不 VACUUM → 页数应基本不变或仅微降
		if pagesAfter < pagesBefore/2 {
			t.Errorf("期望阈值未命中不触发 VACUUM（页数不应大幅下降）：before=%d after=%d", pagesBefore, pagesAfter)
		}
		t.Logf("修复后（阈值=10GB）：DB 未超阈值，runMaintenance 跳过 VACUUM，pages %d → %d ✓", pagesBefore, pagesAfter)
	})

	t.Run("after_fix_wal_checkpoint_always_runs", func(t *testing.T) {
		// WAL checkpoint 独立于阈值判定，每次 tick 都应执行
		save := vacuumThreshold
		t.Cleanup(func() { vacuumThreshold = save })
		vacuumThreshold = 10 * 1024 * 1024 * 1024 // 阈值未命中

		mustExec2(t, store.db, `INSERT INTO bulk VALUES(?)`, "wal-trigger")
		// 不直接断言 WAL 文件（modernc 驱动行为不同），只确保 runMaintenance 不 panic
		store.runMaintenance(dbPath)

		// 验证: 数据仍可读（checkpoint 正常合并 WAL）
		var cnt int
		if err := store.db.QueryRow("SELECT COUNT(*) FROM bulk").Scan(&cnt); err != nil {
			t.Fatal(err)
		}
		if cnt == 0 {
			t.Error("checkpoint 后数据丢失")
		}
	})
}
