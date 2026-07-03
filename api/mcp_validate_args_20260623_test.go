package api

import "testing"

// 校验 validateMCPCommand 的功能优先策略：
// MCP 子进程经 exec(argv) 启动（非 shell），args 里的 shell 元字符、JSON、连接串、`~`
// 都应放行，避免合法 MCP server 配置在 handler 校验阶段被误拒。
func TestValidateMCPCommand_ArgsAllowTildePath(t *testing.T) {
	// SQLite（增量3 连接中心 + registry 一键装都会产出 ~ 路径）必须通过。
	if err := validateMCPCommand("uvx", []string{"mcp-server-sqlite", "--db-path", "~/data.db"}); err != nil {
		t.Errorf("uvx mcp-server-sqlite --db-path ~/data.db 应通过，err=%v", err)
	}
	if err := validateMCPCommand("npx", []string{"-y", "@modelcontextprotocol/server-filesystem", "~"}); err != nil {
		t.Errorf("裸 ~ 家目录 arg 应通过，err=%v", err)
	}
	// 连接串作 arg（postgres/redis，密码已百分号编码）应通过。
	if err := validateMCPCommand("npx", []string{"-y", "@modelcontextprotocol/server-postgres", "postgresql://u:p%40ss@h:5432/db"}); err != nil {
		t.Errorf("postgres 连接串 arg 应通过，err=%v", err)
	}
	if err := validateMCPCommand("npx", []string{"-y", "@gongrzhe/server-redis-mcp", "redis://:p%40ss@10.0.0.5:6379"}); err != nil {
		t.Errorf("redis 连接串 arg 应通过，err=%v", err)
	}
}

func TestValidateMCPCommand_ArgsAllowShellMetaBecauseExecArgv(t *testing.T) {
	for _, arg := range []string{"a;rm -rf", "x|y", "$(whoami)", "a&b", "`id`", "a>b", "a<b", "a'b", "a\"b", `{"k":"v"}`} {
		if err := validateMCPCommand("custom-mcp", []string{"-y", arg}); err != nil {
			t.Errorf("exec argv arg %q should be allowed, got %v", arg, err)
		}
	}
	if err := validateMCPCommand("~npx", nil); err != nil {
		t.Errorf("function-first command with ~ should be allowed: %v", err)
	}
	if err := validateMCPCommand("rm", []string{"-rf", "/"}); err != nil {
		t.Errorf("function-first custom command should be allowed: %v", err)
	}
	for _, bad := range []struct {
		command string
		args    []string
	}{
		{"bad\ncmd", nil},
		{"ok", []string{"bad\x00arg"}},
	} {
		if err := validateMCPCommand(bad.command, bad.args); err == nil {
			t.Errorf("control chars must still be rejected: command=%q args=%v", bad.command, bad.args)
		}
	}
}
