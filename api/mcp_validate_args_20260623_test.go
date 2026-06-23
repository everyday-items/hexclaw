package api

import "testing"

// 校验 validateMCPCommand 的 args 危险字符策略：
// MCP 子进程经 exec(argv) 启动（非 shell），args 里的 `~` 不构成注入，且 connectServer 会展开
// `~`/`~/`（SQLite --db-path ~/data.db、连接器本地路径）。旧实现 args 也禁 `~` → 这类合法路径
// 在 handler 校验阶段被误拒，永远到不了展开逻辑。本测试钉死：args 放行 `~`，但其余 shell 元字符仍禁。
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

// 真正的 shell 元字符在 args 中仍必须被拒（放行 `~` 不等于放开注入面）。
func TestValidateMCPCommand_ArgsStillRejectShellMeta(t *testing.T) {
	for _, bad := range []string{"a;rm -rf", "x|y", "$(whoami)", "a&b", "`id`", "a>b", "a<b", "a'b", "a\"b"} {
		if err := validateMCPCommand("npx", []string{"-y", bad}); err == nil {
			t.Errorf("args 含 shell 元字符 %q 必须被拒", bad)
		}
	}
	// command 仍严格禁 `~`（可执行名不该带 `~`）。
	if err := validateMCPCommand("~npx", nil); err == nil {
		t.Error("command 含 `~` 必须被拒")
	}
	// 命令白名单仍生效。
	if err := validateMCPCommand("rm", []string{"-rf", "/"}); err == nil {
		t.Error("非白名单命令必须被拒")
	}
}
