package config

import (
	"os"
	"path/filepath"
	"testing"
)

// AUDIT bug#1: MCP 安装后重启消失 —— 真实复现
// 桌面 Rust 端启动时写入「只含 knowledge.enabled:true」的精简 config（sidecar.rs ensure_knowledge_enabled_yaml）。
// Writer.readConfig 把已存在文件 unmarshal 进【零值 Config】而非 DefaultConfig，
// 于是 AppendMCPServer 重新 marshal 时把 mcp.enabled / file_memory.enabled 等布尔默认值写成 false，
// 重启后 Go loader 的默认值被这些显式 false 覆盖 → MCP/记忆 失效。
func TestAudit_MCPPersist_AfterDesktopMinimalConfig_20260623(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hexclaw.yaml")

	// 1) 模拟桌面 Rust 端写入的精简配置（只开知识库）
	if err := os.WriteFile(path, []byte("knowledge:\n  enabled: true\n"), 0644); err != nil {
		t.Fatalf("预置精简配置失败: %v", err)
	}

	// 2) 用户从市场安装一个 MCP server（走 cfgWriter.AppendMCPServer）
	w := NewWriter(path)
	if err := w.AppendMCPServer("filesystem", "stdio", "npx", []string{"-y", "@modelcontextprotocol/server-filesystem"}, nil, ""); err != nil {
		t.Fatalf("AppendMCPServer 失败: %v", err)
	}

	// 3) 重启：Go loader 重新加载
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("重启 Load 失败: %v", err)
	}

	// server 在列表里
	found := false
	for _, s := range cfg.MCP.Servers {
		if s.Name == "filesystem" {
			found = true
		}
	}
	if !found {
		t.Errorf("重启后 MCP server 'filesystem' 消失")
	}
	// 关键断言：MCP 仍然启用（否则启动期 mcpMgr=nil，server 不连接 → 列表空）
	if !cfg.MCP.Enabled {
		t.Errorf("BUG#1: 重启后 cfg.MCP.Enabled=false —— mcpMgr 不初始化, MCP server/tools 消失")
	}
	// 连带断言：file_memory 也被零值化（与 bug#3 联动）
	if !cfg.FileMemory.Enabled {
		t.Errorf("BUG#1 连带: 重启后 cfg.FileMemory.Enabled=false —— 长期记忆被一并关闭")
	}
}
