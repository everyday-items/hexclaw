package mcp

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/toolkit/util/logger"
)

// RestartServer 重启单个已配置的 MCP Server（M3-20260710，对齐原型 app.html:1927 服务器行「重启」）。
//
// 语义：按名找到配置 → 建立新连接（成功后才替换旧连接，失败保留原状不放大故障）→
// 关闭旧连接。与 tryReconnect 同一替换模式，但不要求「当前已断开」——用户显式重启
// 用于 stdio 进程僵死 / 改了外部依赖后手动拉起。
func (m *Manager) RestartServer(ctx context.Context, name string) error {
	m.mu.Lock()
	if m.closedLocked() {
		m.mu.Unlock()
		return fmt.Errorf("mcp: manager closed")
	}
	var cfg *ServerConfig
	for i := range m.configs {
		if m.configs[i].Name == name {
			c := m.configs[i]
			cfg = &c
			break
		}
	}
	if cfg == nil {
		m.mu.Unlock()
		return fmt.Errorf("mcp: server %q not configured", name)
	}
	if !cfg.Enabled {
		m.mu.Unlock()
		return fmt.Errorf("mcp: server %q is disabled", name)
	}
	// Reserving a revision at invocation time gives deterministic "latest
	// invocation wins" semantics even when an older connector finishes later.
	revision := m.bumpRevisionLocked(name)
	m.mu.Unlock()

	newServer, err := m.connectServer(ctx, *cfg)
	if err != nil {
		return fmt.Errorf("mcp: restart %q: %w", name, err)
	}

	m.mu.Lock()
	if m.closedLocked() {
		m.mu.Unlock()
		closeServer(newServer)
		return fmt.Errorf("mcp: manager closed during restart %q", name)
	}
	if m.revisions[name] != revision {
		m.mu.Unlock()
		closeServer(newServer)
		return fmt.Errorf("mcp: restart %q superseded by newer lifecycle revision", name)
	}
	configured := false
	for i := range m.configs {
		if m.configs[i].Name == name && m.configs[i].Enabled {
			configured = true
			break
		}
	}
	if !configured {
		m.mu.Unlock()
		closeServer(newServer)
		return fmt.Errorf("mcp: restart %q canceled because server was removed or disabled", name)
	}
	old := m.servers[name]
	if old != nil {
		old.connected = false
	}
	m.servers[name] = newServer
	m.mu.Unlock()

	// Never invoke arbitrary transport cleanup under Manager.mu.
	closeServer(old)
	if old != nil {
		m.hooks.fireDisconnected(ctx, name, "manual restart")
	}
	m.hooks.fireConnected(ctx, name, len(newServer.tools))

	logger.Info("MCP Server restarted", "name", name, "tools", len(newServer.tools))
	return nil
}
