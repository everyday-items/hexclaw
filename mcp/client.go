// Package mcp 提供 MCP (Model Context Protocol) 原生支持
//
// 让 HexClaw 连接外部 MCP Server，自动发现并使用其工具。
// 支持两种传输方式：
//   - stdio: 启动子进程通过标准输入输出通信（本地 MCP Server）
//   - sse: 通过 HTTP SSE 连接远程 MCP Server
//
// 配置文件中声明 MCP Server，启动时自动连接：
//
//	mcp:
//	  servers:
//	    - name: filesystem
//	      transport: stdio
//	      command: npx
//	      args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
//	    - name: github
//	      transport: sse
//	      endpoint: http://localhost:8080/sse
//
// 对标 OpenClaw 的 3200+ MCP Server 生态。
// 基于 hexagon 已集成的 modelcontextprotocol/go-sdk。
package mcp

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
)

// ServerConfig MCP Server 配置
type ServerConfig struct {
	Name      string   `yaml:"name"`      // 名称标识
	Transport string   `yaml:"transport"` // 传输方式: stdio / sse
	Command   string   `yaml:"command"`   // stdio 模式的命令（如 npx, uvx）
	Args      []string `yaml:"args"`      // stdio 模式的命令参数
	Endpoint  string   `yaml:"endpoint"`  // sse 模式的端点 URL
	Enabled   bool     `yaml:"enabled"`   // 是否启用，默认 true
}

// ToolInfo 已发现的 MCP 工具信息
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ServerName  string `json:"server_name"` // 来源 MCP Server
}

// connectedServer 已连接的 MCP Server
type connectedServer struct {
	name      string
	tools     []hexagon.Tool // MCP 工具（已适配为 hexagon Tool）
	cleanup   func()         // stdio 模式的清理函数
	closer    io.Closer      // sse 模式的关闭接口
	connected bool           // 连接状态
}

// Manager MCP 连接管理器
//
// 管理所有 MCP Server 连接，自动发现工具。
// 提供工具列表和健康检查能力。
type Manager struct {
	mu       sync.RWMutex
	servers  map[string]*connectedServer
	configs  []ServerConfig // 保存配置用于重连
	stopCh   chan struct{}
	closeOnce sync.Once
}

// NewManager 创建 MCP 管理器
func NewManager() *Manager {
	return &Manager{
		servers: make(map[string]*connectedServer),
		stopCh:  make(chan struct{}),
	}
}

// Connect 连接所有配置的 MCP Server
//
// 遍历配置列表，逐个连接。单个 Server 连接失败不影响其他 Server。
// 返回总共发现的工具数量。
func (m *Manager) Connect(ctx context.Context, configs []ServerConfig) (int, error) {
	totalTools := 0

	for _, cfg := range configs {
		if !cfg.Enabled {
			continue
		}

		server, err := m.connectServer(ctx, cfg)
		if err != nil {
			log.Printf("MCP Server %q 连接失败: %v", cfg.Name, err)
			continue
		}

		m.mu.Lock()
		m.servers[cfg.Name] = server
		m.mu.Unlock()

		totalTools += len(server.tools)
		log.Printf("MCP Server %q 已连接: 发现 %d 个工具", cfg.Name, len(server.tools))
	}

	// 保存配置用于重连
	m.configs = configs

	// 启动后台重连监控
	go m.reconnectLoop()

	return totalTools, nil
}

// reconnectLoop 定期检查断开的 Server 并尝试重连
func (m *Manager) reconnectLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.tryReconnect()
		}
	}
}

// tryReconnect 对所有断开的 Server 尝试重连
func (m *Manager) tryReconnect() {
	for _, cfg := range m.configs {
		if !cfg.Enabled {
			continue
		}

		m.mu.RLock()
		server, exists := m.servers[cfg.Name]
		needReconnect := !exists || !server.connected
		m.mu.RUnlock()

		if !needReconnect {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		newServer, err := m.connectServer(ctx, cfg)
		cancel()

		if err != nil {
			log.Printf("MCP Server %q 重连失败: %v", cfg.Name, err)
			continue
		}

		m.mu.Lock()
		// 清理旧连接
		if old, ok := m.servers[cfg.Name]; ok {
			if old.cleanup != nil {
				old.cleanup()
			}
			if old.closer != nil {
				old.closer.Close()
			}
		}
		m.servers[cfg.Name] = newServer
		m.mu.Unlock()

		log.Printf("MCP Server %q 已重连: 发现 %d 个工具", cfg.Name, len(newServer.tools))
	}
}

// connectServer 连接单个 MCP Server
func (m *Manager) connectServer(ctx context.Context, cfg ServerConfig) (*connectedServer, error) {
	server := &connectedServer{name: cfg.Name, connected: true}

	switch cfg.Transport {
	case "stdio":
		if cfg.Command == "" {
			return nil, fmt.Errorf("stdio 传输需要指定 command")
		}
		tools, cleanup, err := hexagon.ConnectMCPStdio(ctx, cfg.Command, cfg.Args...)
		if err != nil {
			return nil, fmt.Errorf("stdio 连接失败: %w", err)
		}
		server.tools = tools
		server.cleanup = cleanup

	case "sse":
		if cfg.Endpoint == "" {
			return nil, fmt.Errorf("sse 传输需要指定 endpoint")
		}
		tools, closer, err := hexagon.ConnectMCPSSE(ctx, cfg.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("sse 连接失败: %w", err)
		}
		server.tools = tools
		server.closer = closer

	case "streamable", "http":
		if cfg.Endpoint == "" {
			return nil, fmt.Errorf("streamable HTTP 传输需要指定 endpoint")
		}
		tools, closer, err := hexagon.ConnectMCPStreamable(ctx, cfg.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("streamable HTTP 连接失败: %w", err)
		}
		server.tools = tools
		server.closer = closer

	default:
		return nil, fmt.Errorf("不支持的传输方式: %q（支持 stdio/sse/streamable）", cfg.Transport)
	}

	return server, nil
}

// Tools 获取所有已发现的 MCP 工具
//
// 返回所有已连接 MCP Server 的工具列表，
// 可直接注册到 Agent 引擎。
func (m *Manager) Tools() []hexagon.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var tools []hexagon.Tool
	for _, server := range m.servers {
		tools = append(tools, server.tools...)
	}
	return tools
}

// ToolInfos 获取工具信息列表（轻量级，用于 API 展示）
func (m *Manager) ToolInfos() []ToolInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var infos []ToolInfo
	for _, server := range m.servers {
		for _, t := range server.tools {
			infos = append(infos, ToolInfo{
				Name:        t.Name(),
				Description: t.Description(),
				ServerName:  server.name,
			})
		}
	}
	return infos
}

// ListToolDefinitions returns all discovered MCP tools as LLM tool definitions.
// Used by ToolCollector to inject MCP tools into LLM requests.
func (m *Manager) ListToolDefinitions() []llm.ToolDefinition {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var defs []llm.ToolDefinition
	for _, srv := range m.servers {
		if !srv.connected {
			continue
		}
		for _, t := range srv.tools {
			// Convert hexagon.Tool (ai-core/tool.Tool) to llm.ToolDefinition
			// tool.Tool has: Name(), Description(), Schema() *schema.Schema
			// llm.ToolDefinition has: Type="function", Function{Name, Description, Parameters *Schema}
			// llm.Schema = schema.Schema, so Schema() output can be used directly
			def := llm.NewToolDefinition(t.Name(), t.Description(), t.Schema())
			defs = append(defs, def)
		}
	}
	return defs
}

// ListToolInfos returns tool metadata for all connected servers.
func (m *Manager) ListToolInfos() []ToolInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var infos []ToolInfo
	for _, srv := range m.servers {
		if !srv.connected {
			continue
		}
		for _, t := range srv.tools {
			infos = append(infos, ToolInfo{
				Name:        t.Name(),
				Description: t.Description(),
				ServerName:  srv.name,
			})
		}
	}
	return infos
}

// ServerNames 获取所有已连接的 Server 名称
func (m *Manager) ServerNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.servers))
	for name := range m.servers {
		names = append(names, name)
	}
	return names
}

// CallTool 调用指定 MCP 工具
//
// 在所有已连接 Server 中查找指定名称的工具并执行。
func (m *Manager) CallTool(ctx context.Context, toolName string, args map[string]any) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, server := range m.servers {
		for _, t := range server.tools {
			if t.Name() == toolName {
				result, err := t.Execute(ctx, args)
				if err != nil {
					return "", fmt.Errorf("工具 %q 执行失败: %w", toolName, err)
				}
				return result.String(), nil
			}
		}
	}
	return "", fmt.Errorf("工具 %q 未找到", toolName)
}

// ServerStatus MCP Server 状态信息
type ServerStatus struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	ToolCount int    `json:"tool_count"`
}

// ServerStatuses 获取所有 MCP Server 的状态
func (m *Manager) ServerStatuses() []ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	statuses := make([]ServerStatus, 0, len(m.servers))
	for name, server := range m.servers {
		statuses = append(statuses, ServerStatus{
			Name:      name,
			Connected: server.connected,
			ToolCount: len(server.tools),
		})
	}
	return statuses
}

// AddServer 动态添加并连接 MCP Server
//
// 在运行时添加新的 MCP Server（无需重启）。
// 如果同名 Server 已存在，先断开旧连接再连接新的。
func (m *Manager) AddServer(ctx context.Context, cfg ServerConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("server name 不能为空")
	}
	if !cfg.Enabled {
		cfg.Enabled = true
	}

	select {
	case <-m.stopCh:
		return fmt.Errorf("Manager 已关闭")
	default:
	}

	server, err := m.connectServer(ctx, cfg)
	if err != nil {
		return fmt.Errorf("连接 MCP Server %q 失败: %w", cfg.Name, err)
	}

	m.mu.Lock()
	// double-check: Close 可能在 connectServer 期间被调用
	select {
	case <-m.stopCh:
		m.mu.Unlock()
		if server.cleanup != nil {
			server.cleanup()
		}
		if server.closer != nil {
			server.closer.Close()
		}
		return fmt.Errorf("Manager 已关闭")
	default:
	}
	if old, ok := m.servers[cfg.Name]; ok {
		if old.cleanup != nil {
			old.cleanup()
		}
		if old.closer != nil {
			old.closer.Close()
		}
	}
	m.servers[cfg.Name] = server

	// 同步更新 configs 以支持重连
	found := false
	for i, c := range m.configs {
		if c.Name == cfg.Name {
			m.configs[i] = cfg
			found = true
			break
		}
	}
	if !found {
		m.configs = append(m.configs, cfg)
	}
	m.mu.Unlock()

	log.Printf("MCP Server %q 已动态添加: 发现 %d 个工具", cfg.Name, len(server.tools))
	return nil
}

// RemoveServer 动态移除 MCP Server
//
// 断开指定 Server 的连接并从管理器中移除。
func (m *Manager) RemoveServer(name string) error {
	select {
	case <-m.stopCh:
		return fmt.Errorf("Manager 已关闭")
	default:
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	server, ok := m.servers[name]
	if !ok {
		return fmt.Errorf("MCP Server %q 不存在", name)
	}

	server.connected = false
	if server.cleanup != nil {
		server.cleanup()
	}
	if server.closer != nil {
		server.closer.Close()
	}
	delete(m.servers, name)

	// 从 configs 中移除
	for i, c := range m.configs {
		if c.Name == name {
			m.configs = append(m.configs[:i], m.configs[i+1:]...)
			break
		}
	}

	log.Printf("MCP Server %q 已动态移除", name)
	return nil
}

// Close 关闭所有 MCP Server 连接
//
// 按顺序关闭所有连接，释放资源。
// 应在程序退出时调用。
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		close(m.stopCh)

		m.mu.Lock()
		defer m.mu.Unlock()

		for name, server := range m.servers {
			server.connected = false
			if server.cleanup != nil {
				server.cleanup()
			}
			if server.closer != nil {
				if err := server.closer.Close(); err != nil {
					log.Printf("MCP Server %q 关闭出错: %v", name, err)
				}
			}
			log.Printf("MCP Server %q 已断开", name)
		}

		m.servers = make(map[string]*connectedServer)
	})
}
