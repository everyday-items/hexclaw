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

	"github.com/everyday-items/hexagon"
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
	name    string
	tools   []hexagon.Tool // MCP 工具（已适配为 hexagon Tool）
	cleanup func()         // stdio 模式的清理函数
	closer  io.Closer      // sse 模式的关闭接口
}

// Manager MCP 连接管理器
//
// 管理所有 MCP Server 连接，自动发现工具。
// 提供工具列表和健康检查能力。
type Manager struct {
	mu      sync.RWMutex
	servers map[string]*connectedServer
}

// NewManager 创建 MCP 管理器
func NewManager() *Manager {
	return &Manager{
		servers: make(map[string]*connectedServer),
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

	return totalTools, nil
}

// connectServer 连接单个 MCP Server
func (m *Manager) connectServer(ctx context.Context, cfg ServerConfig) (*connectedServer, error) {
	server := &connectedServer{name: cfg.Name}

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

	default:
		return nil, fmt.Errorf("不支持的传输方式: %q（支持 stdio/sse）", cfg.Transport)
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

// Close 关闭所有 MCP Server 连接
//
// 按顺序关闭所有连接，释放资源。
// 应在程序退出时调用。
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, server := range m.servers {
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
}
