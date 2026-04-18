package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDBHealthCheck_Fix3 回归测试：
// 修复前启动时不体检 data.db，若文件已达 GB 级，SQLite 崩溃恢复会把进程钉在启动
// 阶段 4 分钟以上（线上 373MB 实测），HTTP 端口永不监听。
// 修复后：启动前调用 evaluateDBHealth 分级响应（OK/Warn/Refuse），> 2GB 拒绝启动。
func TestDBHealthCheck_Fix3(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "data.db")

	t.Run("before_fix_no_health_check_allows_huge_db", func(t *testing.T) {
		// 说明性断言：修复前无体检函数，任何大小 DB 都会进入 SQLite 初始化路径
		// 这里只记录语义，不实际构造 2GB 文件
		t.Logf("修复前：无 evaluateDBHealth，任何大小的 data.db 都会尝试打开 → 可能启动卡死 4 分钟+")
	})

	t.Run("after_fix_nonexistent_file_returns_ok", func(t *testing.T) {
		os.Remove(dbPath)
		status, size, journal := evaluateDBHealth(dbPath)
		if status != DBHealthOK {
			t.Errorf("文件不存在应返回 OK，实际 %v", status)
		}
		if size != 0 || journal {
			t.Errorf("期望 size=0 journal=false，实际 size=%d journal=%v", size, journal)
		}
		t.Logf("修复后：首次运行（文件不存在）→ OK ✓")
	})

	t.Run("after_fix_small_file_returns_ok", func(t *testing.T) {
		writeFileSize(t, dbPath, 1*1024*1024) // 1 MB
		defer os.Remove(dbPath)
		status, size, _ := evaluateDBHealth(dbPath)
		if status != DBHealthOK {
			t.Errorf("1MB 文件应返回 OK，实际 %v", status)
		}
		t.Logf("修复后：1MB DB → OK（size=%d）✓", size)
	})

	t.Run("after_fix_warn_threshold_returns_warn", func(t *testing.T) {
		// 刚好过 warn 阈值（200MB）
		writeFileSize(t, dbPath, dbWarnSize+1)
		defer os.Remove(dbPath)
		status, size, _ := evaluateDBHealth(dbPath)
		if status != DBHealthWarn {
			t.Errorf("期望 Warn，实际 %v（size=%d）", status, size)
		}
		t.Logf("修复后：%d bytes（≥ 200MB）→ Warn ✓", size)
	})

	t.Run("after_fix_refuse_threshold_returns_refuse", func(t *testing.T) {
		// 刚好过 refuse 阈值（2GB）— 用 sparse file 避免真的写 2GB
		createSparseFile(t, dbPath, dbRefuseSize+1)
		defer os.Remove(dbPath)
		status, size, _ := evaluateDBHealth(dbPath)
		if status != DBHealthRefuse {
			t.Errorf("期望 Refuse，实际 %v（size=%d）", status, size)
		}
		t.Logf("修复后：%d bytes（≥ 2GB）→ Refuse ✓", size)
	})

	t.Run("after_fix_detects_journal_leftover", func(t *testing.T) {
		writeFileSize(t, dbPath, 1024)
		defer os.Remove(dbPath)
		journalPath := dbPath + "-journal"
		if err := os.WriteFile(journalPath, []byte("leftover"), 0644); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(journalPath)

		_, _, journal := evaluateDBHealth(dbPath)
		if !journal {
			t.Error("存在 -journal 文件时应返回 journalExists=true")
		}
		t.Logf("修复后：检测到 -journal 残留 → 提示用户上次未优雅退出 ✓")
	})

	t.Run("after_fix_expand_home_path", func(t *testing.T) {
		// expandHome 把 ~/ 展开为绝对路径（供 checkDBHealth 用）
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home dir")
		}
		got := expandHome("~/test.db")
		want := filepath.Join(home, "test.db")
		if got != want {
			t.Errorf("expandHome 错误：got=%q want=%q", got, want)
		}
	})
}

// writeFileSize 写一个指定字节数的文件（dense，适合小文件）
func writeFileSize(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if size > 0 {
		buf := make([]byte, 4096)
		written := int64(0)
		for written < size {
			n := int64(len(buf))
			if written+n > size {
				n = size - written
			}
			if _, err := f.Write(buf[:n]); err != nil {
				t.Fatal(err)
			}
			written += n
		}
	}
}

// createSparseFile 用 Truncate 造 sparse file（不占用实际磁盘空间）
func createSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}
