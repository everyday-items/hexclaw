package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// AUDIT 2026-06-23（P0，hex-test R2 对抗审计发现）：MCP server 的 env（stdio 进程环境变量，
// 如数据连接器的 DB 凭证 MYSQL_HOST/MYSQL_PASSWORD）此前在持久化路径被丢弃——
// handler_misc.go 调 AppendMCPServer 时不传 env、Writer 无 env 参、MCPServerConfig 无 Env 字段。
// 症状：带凭证的 MCP server 当前会话可用，但后端重启后重载为空 env → 连接器拿空环境鉴权失败。
//
// 根因覆盖盲区：上一会话的 mcp_env 测试只覆盖 live Manager.AddServer 路径（带 env），
// 零测试触及 cfgWriter.AppendMCPServer 的「写盘→重载」往返——正好测了能用的一半、漏了坏的一半
// （feedback_test_effectiveness 跨进程状态累积型 bug）。本测试钉死该往返。
func TestAudit_MCPServerEnvSurvivesPersist_20260623(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	w := NewWriter(path)

	env := map[string]string{
		"MYSQL_HOST":     "localhost",
		"MYSQL_PASSWORD": "s3cr3t",
	}
	if err := w.AppendMCPServer("mysql", "stdio", "npx", []string{"-y", "mcp-server-mysql"}, env, ""); err != nil {
		t.Fatalf("AppendMCPServer failed: %v", err)
	}

	// 写盘→重载往返：模拟"重启后从配置文件重新加载"。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("written config invalid: %v", err)
	}
	if len(cfg.MCP.Servers) != 1 {
		t.Fatalf("expected 1 MCP server, got %d", len(cfg.MCP.Servers))
	}

	got := cfg.MCP.Servers[0].Env
	if got == nil {
		t.Fatalf("BUG: MCP server env 重启后丢失（持久化未写 env）—— 凭证全没")
	}
	if got["MYSQL_HOST"] != "localhost" || got["MYSQL_PASSWORD"] != "s3cr3t" {
		t.Fatalf("env 往返不一致：want %v, got %v", env, got)
	}
}

// 无 env 的 server（多数 hub 安装路径）不应凭空多出 env: 键（omitempty），保持 YAML 干净。
func TestAudit_MCPServerNoEnvOmitsKey_20260623(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	w := NewWriter(path)
	if err := w.AppendMCPServer("fs", "stdio", "npx", []string{"-y", "server"}, nil, ""); err != nil {
		t.Fatalf("AppendMCPServer failed: %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "env:") {
		t.Errorf("无 env 的 server 不应写出 env: 键（omitempty），got:\n%s", string(data))
	}
}
