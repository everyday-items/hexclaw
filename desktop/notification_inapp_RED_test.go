package desktop

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// RED: 业务通知只能进入应用队列/回调，不得启动 macOS/Linux 系统通知命令。
func TestNotifySource_DoesNotInvokeSystemNotificationCommand(t *testing.T) {
	source, err := os.ReadFile("desktop.go")
	if err != nil {
		t.Fatalf("read desktop notification source: %v", err)
	}
	if bytes.Contains(source, []byte("sendSystemNotification(title, body)")) {
		t.Fatalf("NotifySource still has a system-notification side effect")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows 当前没有该系统通知出口")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "invoked")
	command := "osascript"
	if runtime.GOOS == "linux" {
		command = "notify-send"
	}
	if err := os.WriteFile(filepath.Join(dir, command), []byte("#!/bin/sh\nprintf invoked > \""+marker+"\"\n"), 0o700); err != nil {
		t.Fatalf("write fake notification command: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	NewService("test").NotifySource("任务失败", "401", NotifyError, "cron")
	for i := 0; i < 20; i++ {
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("business notification invoked the system notification command")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
