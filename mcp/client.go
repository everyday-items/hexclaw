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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// ServerConfig MCP Server 配置
type ServerConfig struct {
	Name      string            `yaml:"name"`          // 名称标识
	Transport string            `yaml:"transport"`     // 传输方式: stdio / sse
	Command   string            `yaml:"command"`       // stdio 模式的命令（如 npx, uvx）
	Args      []string          `yaml:"args"`          // stdio 模式的命令参数
	Env       map[string]string `yaml:"env,omitempty"` // stdio 子进程环境变量（数据连接器走 MCP 的凭证注入：MYSQL_HOST/PASSWORD 等）
	Endpoint  string            `yaml:"endpoint"`      // sse 模式的端点 URL
	Enabled   bool              `yaml:"enabled"`       // 是否启用，默认 true
}

// ToolInfo 已发现的 MCP 工具信息
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ServerName  string `json:"server_name"`            // 来源 MCP Server
	InputSchema any    `json:"input_schema,omitempty"` // 参数 JSON Schema
}

// connectedServer 已连接的 MCP Server
type connectedServer struct {
	name      string
	tools     []hexagon.Tool // MCP 工具（已适配为 hexagon Tool）
	cleanup   func()         // stdio 模式的清理函数
	closer    io.Closer      // sse 模式的关闭接口
	connected bool           // 连接状态
	closeOnce sync.Once      // transport cleanup/Close 只执行一次
}

// Manager MCP 连接管理器
//
// 管理所有 MCP Server 连接，自动发现工具。
// 提供工具列表和健康检查能力。
//
// v0.4.0 H3：通过 AddLifecycleHook 接收连接状态变更事件；
// 通过 RegisterServer / UnregisterServer 在运行时动态加挂 / 卸载 server
// （flag mcp.lifecycle.v2 控制 hook 是否触发；动态 API 始终可用）。
type Manager struct {
	mu        sync.RWMutex
	servers   map[string]*connectedServer
	configs   []ServerConfig // 保存配置用于重连
	stopCh    chan struct{}
	closeOnce sync.Once
	revisions map[string]uint64 // per-name lifecycle generation; guarded by mu

	hooks hooksRegistry // v0.4.0 H3 LifecycleHook 列表
}

// NewManager 创建 MCP 管理器
func NewManager() *Manager {
	return &Manager{
		servers:   make(map[string]*connectedServer),
		stopCh:    make(chan struct{}),
		revisions: make(map[string]uint64),
	}
}

func (m *Manager) closedLocked() bool {
	select {
	case <-m.stopCh:
		return true
	default:
		return false
	}
}

func (m *Manager) bumpRevisionLocked(name string) uint64 {
	m.revisions[name]++
	return m.revisions[name]
}

// AddLifecycleHook 注册一个 LifecycleHook。flag mcp.lifecycle.v2 关闭时 hook
// 会被记录但不会触发；flag 开启时所有 hook 在 server 连接 / 断连时被调用。
//
// 多次调用会按注册顺序追加 hook 列表；nil hook 会被安静跳过。
func (m *Manager) AddLifecycleHook(h LifecycleHook) {
	m.hooks.add(h)
}

// Connect 连接所有配置的 MCP Server
//
// 遍历配置列表，逐个连接。单个 Server 连接失败不影响其他 Server。
// 返回总共发现的工具数量。
func (m *Manager) Connect(ctx context.Context, configs []ServerConfig) (int, error) {
	m.mu.Lock()
	if m.closedLocked() {
		m.mu.Unlock()
		return 0, fmt.Errorf("MCP Manager 已关闭")
	}
	m.mu.Unlock()
	totalTools := 0

	for _, cfg := range configs {
		if !cfg.Enabled {
			continue
		}
		m.mu.Lock()
		if m.closedLocked() {
			m.mu.Unlock()
			return totalTools, fmt.Errorf("MCP Manager 已关闭")
		}
		revision := m.bumpRevisionLocked(cfg.Name)
		m.mu.Unlock()

		server, err := m.connectServer(ctx, cfg)
		if err != nil {
			logger.Error("MCP Server", "name", cfg.Name, "error", err)
			continue
		}

		m.mu.Lock()
		if m.closedLocked() || m.revisions[cfg.Name] != revision {
			m.mu.Unlock()
			closeServer(server)
			return totalTools, fmt.Errorf("MCP Manager 在连接 %q 期间关闭或配置已变更", cfg.Name)
		}
		old := m.servers[cfg.Name]
		if old != nil {
			old.connected = false
		}
		m.servers[cfg.Name] = server
		m.mu.Unlock()
		closeServer(old)

		totalTools += len(server.tools)
		logger.Info("MCP Server", "name", cfg.Name, "len", len(server.tools))
		// v0.4.0 H3：触发 lifecycle hook（flag OFF 时为 no-op）
		m.hooks.fireConnected(ctx, cfg.Name, len(server.tools))
	}

	// 保存配置用于重连
	m.mu.Lock()
	if m.closedLocked() {
		m.mu.Unlock()
		return totalTools, fmt.Errorf("MCP Manager 已关闭")
	}
	m.configs = configs
	m.mu.Unlock()

	// 启动后台重连监控
	go m.reconnectLoop()

	return totalTools, nil
}

// RegisterServer 在运行时动态注册并连接单个 MCP server。
//
// v0.4.0 H3：相对于批量 Connect，本 API 适合在用户从 UI 添加 MCP server 时
// 立即连接，无需重启。同名已存在会先 Unregister 再连接。
//
// 触发 OnServerConnected lifecycle hook（如果 flag 开启）。
func (m *Manager) RegisterServer(ctx context.Context, cfg ServerConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("RegisterServer: empty name")
	}
	m.mu.Lock()
	if m.closedLocked() {
		m.mu.Unlock()
		return fmt.Errorf("RegisterServer: manager closed")
	}
	revision := m.bumpRevisionLocked(cfg.Name)
	if !cfg.Enabled {
		// 不抛错，但也不连接 —— 调用方意图明确：先注册到 configs，后续手动 enable
		m.configs = appendOrReplaceConfig(m.configs, cfg)
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	server, err := m.connectServer(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect %q: %w", cfg.Name, err)
	}

	m.mu.Lock()
	if m.closedLocked() || m.revisions[cfg.Name] != revision {
		m.mu.Unlock()
		closeServer(server)
		return fmt.Errorf("RegisterServer %q superseded or manager closed", cfg.Name)
	}
	old := m.servers[cfg.Name]
	if old != nil {
		old.connected = false
	}
	m.servers[cfg.Name] = server
	m.configs = appendOrReplaceConfig(m.configs, cfg)
	m.mu.Unlock()
	closeServer(old)

	logger.Info("MCP Server registered", "name", cfg.Name, "tools", len(server.tools))
	if old != nil {
		m.hooks.fireDisconnected(ctx, cfg.Name, "server replaced")
	}
	m.hooks.fireConnected(ctx, cfg.Name, len(server.tools))
	return nil
}

// UnregisterServer 卸载并断开单个 MCP server。返回是否真的存在并被卸载。
//
// 触发 OnServerDisconnected lifecycle hook（如果 flag 开启）。
func (m *Manager) UnregisterServer(ctx context.Context, name string) bool {
	m.mu.Lock()
	if m.closedLocked() {
		m.mu.Unlock()
		return false
	}
	server, ok := m.servers[name]
	if !ok {
		m.mu.Unlock()
		return false
	}
	server.connected = false
	delete(m.servers, name)
	m.configs = removeConfig(m.configs, name)
	m.bumpRevisionLocked(name)
	m.mu.Unlock()
	closeServer(server)

	logger.Info("MCP Server unregistered", "name", name)
	m.hooks.fireDisconnected(ctx, name, "manual unregister")
	return true
}

// closeServer 调用 cleanup / closer 关闭单个 server 的传输层。
func closeServer(s *connectedServer) {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.cleanup != nil {
			s.cleanup()
		}
		if s.closer != nil {
			_ = s.closer.Close()
		}
	})
}

// appendOrReplaceConfig 替换同名 config 或追加。
func appendOrReplaceConfig(list []ServerConfig, cfg ServerConfig) []ServerConfig {
	for i, c := range list {
		if c.Name == cfg.Name {
			list[i] = cfg
			return list
		}
	}
	return append(list, cfg)
}

// removeConfig 移除同名 config。
func removeConfig(list []ServerConfig, name string) []ServerConfig {
	out := list[:0]
	for _, c := range list {
		if c.Name != name {
			out = append(out, c)
		}
	}
	return out
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
	// 在锁下复制 configs，避免迭代时数据竞争
	m.mu.RLock()
	cfgsCopy := make([]ServerConfig, len(m.configs))
	copy(cfgsCopy, m.configs)
	m.mu.RUnlock()

	for _, cfg := range cfgsCopy {
		if !cfg.Enabled {
			continue
		}

		m.mu.RLock()
		server, exists := m.servers[cfg.Name]
		needReconnect := !exists || !server.connected
		revision := m.revisions[cfg.Name]
		closed := m.closedLocked()
		m.mu.RUnlock()

		if closed || !needReconnect {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		newServer, err := m.connectServer(ctx, cfg)
		cancel()

		if err != nil {
			logger.Error("MCP Server", "name", cfg.Name, "error", err)
			continue
		}

		m.mu.Lock()
		current, exists := m.servers[cfg.Name]
		stillNeedsReconnect := !exists || !current.connected
		configured := false
		for i := range m.configs {
			if m.configs[i].Name == cfg.Name && m.configs[i].Enabled {
				configured = true
				break
			}
		}
		if m.closedLocked() || m.revisions[cfg.Name] != revision || !configured || !stillNeedsReconnect {
			m.mu.Unlock()
			closeServer(newServer)
			continue
		}
		old := current
		if old != nil {
			old.connected = false
		}
		m.servers[cfg.Name] = newServer
		m.bumpRevisionLocked(cfg.Name)
		m.mu.Unlock()
		closeServer(old)

		logger.Info("MCP Server", "name", cfg.Name, "len", len(newServer.tools))
		if old != nil {
			m.hooks.fireDisconnected(ctx, cfg.Name, "automatic reconnect")
		}
		m.hooks.fireConnected(ctx, cfg.Name, len(newServer.tools))
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
		// 解析 args 中的路径：~ 展开 + 符号链接解析（macOS /tmp → /private/tmp 等）
		homeDir, _ := os.UserHomeDir()
		resolvedArgs := make([]string, len(cfg.Args))
		for i, arg := range cfg.Args {
			// ~ 展开（跨平台：macOS/Linux/Windows 均由 os.UserHomeDir 处理）
			if homeDir != "" {
				if arg == "~" {
					arg = homeDir
				} else if strings.HasPrefix(arg, "~/") {
					arg = filepath.Join(homeDir, arg[2:])
				}
			}
			if filepath.IsAbs(arg) {
				if resolved, err := filepath.EvalSymlinks(arg); err == nil {
					resolvedArgs[i] = resolved
					continue
				}
			}
			resolvedArgs[i] = arg
		}
		tools, cleanup, err := hexagon.ConnectMCPStdioWithEnv(ctx, cfg.Command, cfg.Env, resolvedArgs...)
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
			info := ToolInfo{
				Name:        t.Name(),
				Description: t.Description(),
				ServerName:  server.name,
			}
			if s := t.Schema(); s != nil {
				info.InputSchema = s
			}
			infos = append(infos, info)
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
	result, _, err := m.CallToolWithOwner(ctx, toolName, args)
	return result, err
}

// CallToolWithOwner executes a tool and returns the exact server selected by
// the same lookup. Callers must not perform a separate owner lookup: map order
// and concurrent lifecycle changes could otherwise pair the result with a
// different server.
func (m *Manager) CallToolWithOwner(ctx context.Context, toolName string, args map[string]any) (string, string, error) {
	// Copy the tool reference under lock, then release before executing
	m.mu.RLock()
	var found hexagon.Tool
	var owner string                 // 属主 server 名——进程死亡时用于翻转其连接状态
	var ownerServer *connectedServer // 选中时的连接世代；重启替换后旧调用不得污染新连接
	for name, server := range m.servers {
		for _, t := range server.tools {
			if t.Name() == toolName {
				found = t
				owner = name
				ownerServer = server
				break
			}
		}
		if found != nil {
			break
		}
	}
	m.mu.RUnlock()

	if found == nil {
		return "", "", fmt.Errorf("工具 %q 未找到", toolName)
	}

	result, err := found.Execute(ctx, args)
	if err != nil {
		// stdio MCP 子进程退出（多因数据源连不上而自退）→ 传输层抛 EOF/连接已关，对用户是天书。
		// 翻译成可操作的友好提示；底层传输错误只进日志，不糊用户脸上（bug-20260626 #3c）。
		if isMCPConnClosed(err) {
			logger.Error("MCP 工具调用失败：连接已关闭/进程退出", "tool", toolName, "server", owner, "error", err)
			// BUG-20260704：识别到进程退出必须同步翻转属主 server 的连接状态——
			// 否则 ServerStatuses 谎报「已连接」（UI 徽章事实源），且 tryReconnect 因
			// connected 仍为 true 永远跳过该 server，不自愈。翻转后下个 30s tick 自动重拉。
			m.markServerDisconnected(ctx, owner, ownerServer, "stdio process exited (detected on tool call)")
			return "", owner, fmt.Errorf("工具 %q 执行失败: MCP 服务进程已退出，请检查数据库连接配置（主机/端口/账号/密码）后重试", toolName)
		}
		return "", owner, fmt.Errorf("工具 %q 执行失败: %w", toolName, err)
	}
	return result.String(), owner, nil
}

// markServerDisconnected 把指定 server 标记为断连（幂等）：状态即刻对 ServerStatuses
// 可见，tryReconnect 下个 tick 会尝试重连。仅在状态真实翻转时触发 lifecycle hook。
func (m *Manager) markServerDisconnected(ctx context.Context, name string, expected *connectedServer, reason string) {
	if name == "" {
		return
	}
	m.mu.Lock()
	server, ok := m.servers[name]
	// A tool executes outside Manager.mu. Restart/replace may publish a fresh
	// connectedServer under the same name while an old call is still in flight;
	// only the exact connection generation that produced the error may be
	// marked disconnected.
	if !ok || server != expected || !server.connected {
		m.mu.Unlock()
		return
	}
	server.connected = false
	m.mu.Unlock()
	m.hooks.fireDisconnected(ctx, name, reason)
}

// isMCPConnClosed 判断错误是否源自 MCP stdio 子进程退出 / 传输层关闭。
// 这类错误（EOF / client is closing / connection closed / broken pipe / 已关闭的连接）
// 对用户毫无可操作性，应翻译成「进程退出，检查配置」的友好提示。
func isMCPConnClosed(err error) bool {
	// 类型化判定优先（精准）：io.EOF / 管道已关。
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	// 字符串判定只收**明确属于传输/进程关闭**的短语。
	// ⚠️ 不匹配裸 "EOF"——工具自身的业务错误可能含 "EOF"（如 JSON "unexpected EOF"、
	// SQL "near 'EOF'"），裸匹配会把真错误误判成「进程已退出」并吞掉（bug-20260626 #3c 回归）。
	s := err.Error()
	for _, marker := range []string{
		"client is closing",
		"connection closed",
		"broken pipe",
		"file already closed",
		"use of closed",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// ServerStatus MCP Server 状态信息
type ServerStatus struct {
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
	Connected bool   `json:"connected"`
	ToolCount int    `json:"tool_count"`
}

func classifyServerKind(cfg ServerConfig) string {
	haystack := strings.ToLower(strings.Join(append([]string{cfg.Name, cfg.Command, cfg.Endpoint}, cfg.Args...), " "))
	switch {
	case strings.Contains(haystack, "mysql"):
		return "mysql"
	case strings.Contains(haystack, "postgres") || strings.Contains(haystack, "pgsql"):
		return "postgres"
	case strings.Contains(haystack, "sqlite"):
		return "sqlite"
	case strings.Contains(haystack, "redis"):
		return "redis"
	case strings.Contains(haystack, "filesystem") || strings.Contains(haystack, "readfile"):
		return "filesystem"
	case strings.Contains(haystack, "github"):
		return "github"
	default:
		return "mcp"
	}
}

// ServerStatuses 获取所有 MCP Server 的状态。
//
// ★以「已配置(Enabled)」为事实源：已安装但尚未连上的 server（冷装 npx/uvx 下载中、数据源未就绪等）
// 也必须出现在状态里（Connected=false），交 UI 显示「未连接」灰徽章，绝不从列表消失（修复 BUG-20260626：
// 市场装了 MySQL/readfile 再进去「都没有了」）。连接态/工具数取自 m.servers（已连接子集）。
func (m *Manager) ServerStatuses() []ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := make(map[string]bool, len(m.configs)+len(m.servers))
	statuses := make([]ServerStatus, 0, len(m.configs)+len(m.servers))

	for _, cfg := range m.configs {
		if !cfg.Enabled || seen[cfg.Name] {
			continue
		}
		seen[cfg.Name] = true
		st := ServerStatus{Name: cfg.Name, Kind: classifyServerKind(cfg)}
		if server, ok := m.servers[cfg.Name]; ok {
			st.Connected = server.connected
			st.ToolCount = len(server.tools)
		}
		statuses = append(statuses, st)
	}
	// 防御：任何已连接但未登记 configs 的 server（理论不应出现）也并入，避免漏报。
	for name, server := range m.servers {
		if seen[name] {
			continue
		}
		seen[name] = true
		statuses = append(statuses, ServerStatus{
			Name:      name,
			Kind:      "mcp",
			Connected: server.connected,
			ToolCount: len(server.tools),
		})
	}
	return statuses
}

// ConfiguredServerNames 返回所有「已配置(Enabled)」的 MCP Server 名——UI 服务器列表
// （GET /api/v1/mcp/servers）的事实源。
//
// 与 ServerNames()（仅返回已连接的 live 子集，供内部工具路由 / 启动计数）不同：本方法把「已安装但
// 尚未连上」的 server 也纳入，使「市场一键安装」后 server 立即出现在列表（状态显示未连接），不因冷装
// 未即时连上而从 UI 消失（修复 BUG-20260626）。连接实况由 ServerStatuses 提供。
func (m *Manager) ConfiguredServerNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := make(map[string]bool, len(m.configs)+len(m.servers))
	names := make([]string, 0, len(m.configs)+len(m.servers))
	for _, cfg := range m.configs {
		if !cfg.Enabled || seen[cfg.Name] {
			continue
		}
		seen[cfg.Name] = true
		names = append(names, cfg.Name)
	}
	// 防御：已连接但未登记 configs 的 server 也并入。
	for name := range m.servers {
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// FilesystemRoots returns the current configured absolute roots for one named
// filesystem MCP server. It reads configs on every call so runtime replacement
// is visible to path-resolution hooks without a stale boot snapshot.
func (m *Manager) FilesystemRoots(name string) []string {
	m.mu.RLock()
	var cfg *ServerConfig
	for i := range m.configs {
		if m.configs[i].Name == name && m.configs[i].Enabled {
			copyCfg := m.configs[i]
			copyCfg.Args = append([]string(nil), copyCfg.Args...)
			cfg = &copyCfg
			break
		}
	}
	m.mu.RUnlock()
	if cfg == nil {
		return nil
	}
	isFilesystem := false
	for _, arg := range cfg.Args {
		if strings.Contains(arg, "server-filesystem") {
			isFilesystem = true
			break
		}
	}
	if !isFilesystem {
		return nil
	}
	home, _ := os.UserHomeDir()
	var roots []string
	for _, arg := range cfg.Args {
		if arg == "~" && home != "" {
			arg = home
		} else if strings.HasPrefix(arg, "~/") && home != "" {
			arg = filepath.Join(home, arg[2:])
		}
		if filepath.IsAbs(arg) {
			roots = append(roots, filepath.Clean(arg))
		}
	}
	return roots
}

// AddServer 动态添加并连接 MCP Server
//
// 在运行时添加新的 MCP Server（无需重启）。
// 如果同名 Server 已存在，先断开旧连接再连接新的。
func (m *Manager) AddServer(ctx context.Context, cfg ServerConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("server name 不能为空")
	}
	cfg.Enabled = true
	m.mu.Lock()
	if m.closedLocked() {
		m.mu.Unlock()
		return fmt.Errorf("Manager 已关闭")
	}
	revision := m.bumpRevisionLocked(cfg.Name)
	m.mu.Unlock()

	server, err := m.connectServer(ctx, cfg)
	if err != nil {
		return fmt.Errorf("连接 MCP Server %q 失败: %w", cfg.Name, err)
	}

	m.mu.Lock()
	if m.closedLocked() || m.revisions[cfg.Name] != revision {
		m.mu.Unlock()
		closeServer(server)
		return fmt.Errorf("Manager 已关闭或添加操作已被更新操作取代")
	}
	old := m.servers[cfg.Name]
	if old != nil {
		old.connected = false
	}
	m.servers[cfg.Name] = server
	m.configs = appendOrReplaceConfig(m.configs, cfg)
	m.mu.Unlock()
	closeServer(old)

	logger.Info("MCP Server", "name", cfg.Name, "len", len(server.tools))
	return nil
}

// AddServerBestEffort 注册并尽力连接 MCP Server（运行时动态添加，如「连接中心 → 添加数据源」）。
//
// 与 AddServer（严格：即时连接失败即不注册）不同：本方法**先尝试一次即时连接，再在同一把锁下登记
// cfg(Enabled=true)**——成功则同时落 server，失败则只落 cfg，交后台 reconnectLoop(30s) 在 npx/uvx
// 缓存就绪后自动拉起。这样「添加数据源」首次冷装不再硬失败（根因：旧路径连接失败即 400 且不入 configs，
// 重连循环永不接管）。
//
// ★为何「连接在前、登记在后」：connectServer 期间 cfg 尚未进 m.configs，reconnectLoop 看不到它，
// 不会对同名并发再连一次（避免双开子进程）。登记与落 server 在同一把锁内完成，登记后状态即自洽。
//
// 返回 (connected, err)：err 仅在不可恢复时（name 空 / Manager 已关闭）非 nil；
// 即时连接失败属可恢复，返回 (false, nil)。
func (m *Manager) AddServerBestEffort(ctx context.Context, cfg ServerConfig) (bool, error) {
	if cfg.Name == "" {
		return false, fmt.Errorf("server name 不能为空")
	}
	cfg.Enabled = true

	m.mu.Lock()
	if m.closedLocked() {
		m.mu.Unlock()
		return false, fmt.Errorf("Manager 已关闭")
	}
	revision := m.bumpRevisionLocked(cfg.Name)
	m.mu.Unlock()

	// 即时连接（此刻 cfg 未登记 → reconnectLoop 不会对同名并发连接，杜绝双开子进程）。
	server, connErr := m.connectServer(ctx, cfg)

	m.mu.Lock()
	if m.closedLocked() || m.revisions[cfg.Name] != revision {
		m.mu.Unlock()
		closeServer(server)
		return false, fmt.Errorf("Manager 已关闭或添加操作已被更新操作取代")
	}
	// 登记 config（替换同名或追加），使 reconnectLoop 拥有它——连接失败时由后台 30s 周期重试拉起。
	m.configs = appendOrReplaceConfig(m.configs, cfg)
	if connErr != nil {
		m.mu.Unlock()
		logger.Warn("MCP Server", "name", cfg.Name, "即时连接失败，转后台重连", connErr)
		return false, nil
	}
	old := m.servers[cfg.Name]
	if old != nil {
		old.connected = false
	}
	m.servers[cfg.Name] = server
	m.mu.Unlock()
	closeServer(old)

	logger.Info("MCP Server", "name", cfg.Name, "len", len(server.tools))
	return true, nil
}

// RemoveServer 动态移除 MCP Server
//
// 断开指定 Server 的连接并从管理器中移除。
func (m *Manager) RemoveServer(name string) error {
	m.mu.Lock()
	if m.closedLocked() {
		m.mu.Unlock()
		return fmt.Errorf("Manager 已关闭")
	}

	// 从 configs 中移除（已配置但尚未连上的冷装 server 只在 configs 里——必须能被删除，
	// 否则 BUG-20260626 修复后用户看得到却删不掉）。
	inConfigs := false
	for i, c := range m.configs {
		if c.Name == name {
			m.configs = append(m.configs[:i], m.configs[i+1:]...)
			inConfigs = true
			break
		}
	}

	// 断开并移除已连接实例（若有）。
	server, connected := m.servers[name]
	if connected {
		server.connected = false
		delete(m.servers, name)
	}

	// 既不在 configs 也不在 servers → 确实不存在。
	if !inConfigs && !connected {
		m.mu.Unlock()
		return fmt.Errorf("MCP Server %q 不存在", name)
	}
	m.bumpRevisionLocked(name)
	m.mu.Unlock()

	closeServer(server)
	logger.Info("MCP Server", "name", name)
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
		servers := make(map[string]*connectedServer, len(m.servers))
		for name, server := range m.servers {
			server.connected = false
			servers[name] = server
			m.bumpRevisionLocked(name)
		}
		m.servers = make(map[string]*connectedServer)
		m.mu.Unlock()

		for name, server := range servers {
			closeServer(server)
			logger.Info("MCP Server", "name", name)
		}
	})
}
