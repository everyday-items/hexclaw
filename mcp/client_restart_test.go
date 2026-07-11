package mcp

import (
	"context"
	"strings"
	"testing"
)

// M3-20260710：RestartServer 契约——未配置/已禁用必须显式报错（不静默成功），
// 配置存在但连接失败时不得破坏现有连接表。
func TestRestartServer_Contract(t *testing.T) {
	m := NewManager()

	t.Run("未配置的名字报错", func(t *testing.T) {
		err := m.RestartServer(context.Background(), "ghost")
		if err == nil || !strings.Contains(err.Error(), "not configured") {
			t.Fatalf("期望 not configured 错误，实际 %v", err)
		}
	})

	t.Run("已禁用的配置报错", func(t *testing.T) {
		m.mu.Lock()
		m.configs = []ServerConfig{{Name: "off", Enabled: false}}
		m.mu.Unlock()
		err := m.RestartServer(context.Background(), "off")
		if err == nil || !strings.Contains(err.Error(), "disabled") {
			t.Fatalf("期望 disabled 错误，实际 %v", err)
		}
	})

	t.Run("连接失败保留原连接表(不放大故障)", func(t *testing.T) {
		m.mu.Lock()
		m.configs = []ServerConfig{{Name: "bad", Enabled: true, Transport: "stdio", Command: "/nonexistent-cmd-20260710"}}
		m.servers["bad"] = &connectedServer{name: "bad", connected: true}
		m.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 3_000_000_000)
		defer cancel()
		err := m.RestartServer(ctx, "bad")
		if err == nil {
			t.Fatalf("期望连接失败错误")
		}
		m.mu.RLock()
		_, still := m.servers["bad"]
		m.mu.RUnlock()
		if !still {
			t.Fatalf("重启失败不得移除原有连接记录")
		}
	})
}
