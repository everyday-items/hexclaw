package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/hexagon-codes/hexagon"
)

// 发现#2 根因修复：旧路径 handleAddMCPServer→AddServer 同步连接，npx/uvx 首次冷装下载 >
// 15s 超时即 400 且**不入 m.configs**→reconnectLoop 永不接管→「添加数据源」首次冷装永久失败。
// AddServerBestEffort：先登记 config(Enabled) 再尽力即时连接，连接失败不致命，交后台重连。

// 可切换行为的 stdio 连接桩：connectErr 非空 → 模拟连接失败（冷装超时）；否则返回 tools。
func stubStdioToggle(t *testing.T) (setErr func(error), setTools func([]hexagon.Tool)) {
	t.Helper()
	orig := hexagon.ConnectMCPStdioWithEnv
	var curErr error
	var curTools []hexagon.Tool
	hexagon.ConnectMCPStdioWithEnv = func(ctx context.Context, command string, env map[string]string, args ...string) ([]hexagon.Tool, func(), error) {
		if curErr != nil {
			return nil, nil, curErr
		}
		return curTools, func() {}, nil
	}
	t.Cleanup(func() { hexagon.ConnectMCPStdioWithEnv = orig })
	return func(e error) { curErr = e }, func(ts []hexagon.Tool) { curErr = nil; curTools = ts }
}

// ★根因证明：即时连接失败时，config 仍登记(Enabled) 且不致命；随后后台重连能把它拉起。
func TestAddServerBestEffort_ColdInstallRegistersThenReconnects(t *testing.T) {
	setErr, setTools := stubStdioToggle(t)
	m := NewManager()
	cfg := ServerConfig{Name: "mysql", Transport: "stdio", Command: "npx", Args: []string{"-y", "@benborla29/mcp-server-mysql"}}

	// 1) 模拟首次冷装：即时连接超时失败。
	setErr(fmt.Errorf("npx download timed out: context deadline exceeded"))
	connected, err := m.AddServerBestEffort(context.Background(), cfg)
	if err != nil {
		t.Fatalf("冷装即时失败不应是致命错误，err=%v", err)
	}
	if connected {
		t.Fatal("即时连接失败时 connected 应为 false")
	}
	// config 必须已登记(Enabled)——否则 reconnectLoop 永不接管（旧 bug）。
	m.mu.RLock()
	cfgCount := len(m.configs)
	cfgEnabled := cfgCount == 1 && m.configs[0].Name == "mysql" && m.configs[0].Enabled
	m.mu.RUnlock()
	if !cfgEnabled {
		t.Fatalf("即时连接失败后 config 必须登记且 Enabled，configs=%v", m.configs)
	}
	if len(m.ServerNames()) != 0 {
		t.Fatalf("未连接成功时不应有 live server，got %v", m.ServerNames())
	}

	// 2) 暖装就绪：后台 reconnectLoop 应把已登记的 config 拉起。
	setTools([]hexagon.Tool{fakeTool("mysql_query")})
	m.tryReconnect()
	if names := m.ServerNames(); len(names) != 1 || names[0] != "mysql" {
		t.Fatalf("重连后 server 应上线，got %v", names)
	}
	if got := len(m.Tools()); got != 1 {
		t.Errorf("重连后应发现 1 个工具，got %d", got)
	}
}

// 暖装路径：即时连接成功 → connected=true 且 server 立即可用。
func TestAddServerBestEffort_WarmSuccess(t *testing.T) {
	_, setTools := stubStdioToggle(t)
	setTools([]hexagon.Tool{fakeTool("read"), fakeTool("write")})
	m := NewManager()

	connected, err := m.AddServerBestEffort(context.Background(), ServerConfig{Name: "fs", Transport: "stdio", Command: "stub"})
	if err != nil {
		t.Fatalf("AddServerBestEffort: %v", err)
	}
	if !connected {
		t.Fatal("暖装即时连接成功时 connected 应为 true")
	}
	if names := m.ServerNames(); len(names) != 1 || names[0] != "fs" {
		t.Fatalf("server 应注册，got %v", names)
	}
	m.mu.RLock()
	dup := len(m.configs)
	m.mu.RUnlock()
	if dup != 1 {
		t.Errorf("config 不应重复，got %d", dup)
	}
}

// 不可恢复错误仍返回 err：name 空 / Manager 已关闭。
func TestAddServerBestEffort_FatalGuards(t *testing.T) {
	stubStdioToggle(t)
	m := NewManager()
	if _, err := m.AddServerBestEffort(context.Background(), ServerConfig{Transport: "stdio", Command: "x"}); err == nil {
		t.Error("空 name 必须报错")
	}
	m.Close()
	if _, err := m.AddServerBestEffort(context.Background(), ServerConfig{Name: "fs", Transport: "stdio", Command: "stub"}); err == nil {
		t.Error("Manager 关闭后必须报错")
	}
}
