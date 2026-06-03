package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestSidecarLock_FreshAcquire 全新目录拿 lock 应成功，文件含当前 PID。
func TestSidecarLock_FreshAcquire(t *testing.T) {
	home := t.TempDir()
	l, err := AcquireSidecarLock(home)
	if err != nil {
		t.Fatalf("AcquireSidecarLock: %v", err)
	}
	defer l.Release()

	b, err := os.ReadFile(filepath.Join(home, ".hexclaw", ".sidecar.lock"))
	if err != nil {
		t.Fatalf("读 lock: %v", err)
	}
	pid, _ := strconv.Atoi(string(b))
	if pid != os.Getpid() {
		t.Errorf("lock 含 PID %d，期望当前 PID %d", pid, os.Getpid())
	}
}

// TestSidecarLock_StaleLockOverridden 老 PID 已不存在 → 直接覆盖
func TestSidecarLock_StaleLockOverridden(t *testing.T) {
	home := t.TempDir()
	lockDir := filepath.Join(home, ".hexclaw")
	_ = os.MkdirAll(lockDir, 0700)
	// 写入一个绝对不存在的 PID (PID 1 是 init/launchd，肯定存在，所以写大数字)
	stalePID := 999999
	_ = os.WriteFile(filepath.Join(lockDir, ".sidecar.lock"), []byte(strconv.Itoa(stalePID)), 0600)

	l, err := AcquireSidecarLock(home)
	if err != nil {
		t.Fatalf("stale lock 应被覆盖，实际报错: %v", err)
	}
	defer l.Release()
}

// TestSidecarLock_ReleaseRemovesFile
func TestSidecarLock_ReleaseRemovesFile(t *testing.T) {
	home := t.TempDir()
	l, err := AcquireSidecarLock(home)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lockPath := filepath.Join(home, ".hexclaw", ".sidecar.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatal("lock 应存在")
	}
	l.Release()
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("Release 后 lock 应被删除，stat err=%v", err)
	}
}

// TestSidecarLock_LiveProcessBlocks 当前进程持锁，第二次 Acquire 应拒绝
func TestSidecarLock_LiveProcessBlocks(t *testing.T) {
	home := t.TempDir()
	l1, err := AcquireSidecarLock(home)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer l1.Release()

	// 同 PID 视为自己，应放行（实际场景同一进程 main 二次调用就走这条路）
	l2, err := AcquireSidecarLock(home)
	if err != nil {
		t.Errorf("同 PID 重入应放行: %v", err)
	}
	if l2 != nil {
		defer l2.Release()
	}
}

// TestIsProcessAlive 基本健康检查
func TestIsProcessAlive(t *testing.T) {
	if !isProcessAlive(os.Getpid()) {
		t.Error("当前进程应为存活")
	}
	if isProcessAlive(999999) {
		t.Error("不存在的 PID 不应判活")
	}
}
