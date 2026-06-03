package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SidecarLock 防止两个 sidecar 实例同时跑（端口冲突 / DB WAL 锁残留 /
// 配置文件竞争）。
//
// 实现：单文件 PID lock + 启动时 stale 检测。
//
// 真实踩坑：本会话 6 次在 pkill + ditto 替换流程里出现"app 被换、sidecar 还在跑"
// 或"两个 sidecar 同 16060 端口"。引入此 lock 后老进程被识别即清理。
type SidecarLock struct {
	path string
	pid  int
}

// AcquireSidecarLock 在 ~/.hexclaw/.sidecar.lock 写入当前 PID。
//
// 行为：
//   - 文件不存在 → 写入当前 PID
//   - 文件含 PID 且进程仍活 → 返错（拒启动）
//   - 文件含 PID 但进程已死 → 视为 stale，覆盖
//
// Release() 必须在退出前调用（main.go defer）。
func AcquireSidecarLock(home string) (*SidecarLock, error) {
	dir := filepath.Join(home, ".hexclaw")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("创建 ~/.hexclaw: %w", err)
	}
	lockPath := filepath.Join(dir, ".sidecar.lock")

	if b, err := os.ReadFile(lockPath); err == nil {
		oldPID, perr := strconv.Atoi(strings.TrimSpace(string(b)))
		if perr == nil && oldPID > 0 && oldPID != os.Getpid() {
			if isProcessAlive(oldPID) {
				return nil, fmt.Errorf("sidecar 已在跑（pid=%d）— 检查重复启动或先 kill 老进程", oldPID)
			}
			// stale lock：进程已死，可覆盖
		}
	}

	myPID := os.Getpid()
	tmp := lockPath + ".tmp." + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(myPID)), 0600); err != nil {
		return nil, fmt.Errorf("写 lock tmp: %w", err)
	}
	if err := os.Rename(tmp, lockPath); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("rename lock: %w", err)
	}
	return &SidecarLock{path: lockPath, pid: myPID}, nil
}

// Release 删除 lock 文件。即使删除失败也不致命（下次启动 stale 检测会处理）。
func (l *SidecarLock) Release() {
	if l == nil {
		return
	}
	_ = os.Remove(l.path)
}

// isProcessAlive 跨平台检测 PID 是否仍存活。
//   - Unix: kill(pid, 0) 不发信号但触发权限/存在校验
//   - 不存在 → ESRCH；权限不足 → EPERM（视为存活，避免误杀）
func isProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// Permission denied — 进程存在但属于别的用户（应该不会出现在桌面端单用户场景）
	return strings.Contains(err.Error(), "permission denied")
}
