package mcp

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMySQLMCPEnvKeystone_E2E 真机 E2E：经 hexclaw 自己的 mcp.Manager 代码路径
// （ServerConfig.Env → connectServer → hexagon.ConnectMCPStdioWithEnv → buildStdioCmd → 子进程 env）
// 启动真实 @benborla29/mcp-server-mysql，连接本地 MySQL `dev` 库做真实数据操作。
//
// 判别性证明：SELECT DATABASE() 必须返回 "dev" —— 该值只能来自 env(MYSQL_DB=dev) 被注入子进程，
// 从而证明 env keystone 经 Manager 的真实代码路径生效（非默认连接）。
//
// 默认 SKIP（需 npx + 本地 MySQL）。运行：HEXCLAW_MYSQL_E2E=1 go test ./mcp/ -run E2E -v
func TestMySQLMCPEnvKeystone_E2E(t *testing.T) {
	if os.Getenv("HEXCLAW_MYSQL_E2E") == "" {
		t.Skip("需 HEXCLAW_MYSQL_E2E=1 + 本地 MySQL + npx")
	}
	m := NewManager()
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cfg := ServerConfig{
		Name:      "mysql-e2e",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"-y", "@benborla29/mcp-server-mysql"},
		Env: map[string]string{
			"MYSQL_HOST": "127.0.0.1", "MYSQL_PORT": "3306",
			"MYSQL_USER": "root", "MYSQL_PASS": "123456", "MYSQL_DB": "dev",
			"ALLOW_INSERT_OPERATION": "true", "ALLOW_UPDATE_OPERATION": "true",
			"ALLOW_DELETE_OPERATION": "true", "ALLOW_DDL_OPERATION": "true",
		},
	}
	if err := m.AddServer(ctx, cfg); err != nil {
		t.Fatalf("AddServer(env) 连接失败（env keystone 断裂？）: %v", err)
	}

	// 定位 sql 工具
	var tool string
	for _, ti := range m.ToolInfos() {
		n := strings.ToLower(ti.Name)
		if strings.Contains(n, "query") || strings.Contains(n, "sql") {
			tool = ti.Name
			break
		}
	}
	if tool == "" {
		t.Fatalf("未发现 mysql 工具，ToolInfos=%v", m.ToolInfos())
	}
	t.Logf("mysql MCP 工具: %s", tool)

	call := func(sql string) string {
		res, err := m.CallTool(ctx, tool, map[string]any{"sql": sql})
		if err != nil {
			t.Fatalf("CallTool(%q) err: %v", sql, err)
		}
		return res
	}

	// ★判别性：env(MYSQL_DB=dev) 经 Manager 代码注入 → SELECT DATABASE() == dev
	if db := call("SELECT DATABASE() AS db"); !strings.Contains(db, "dev") {
		t.Fatalf("env keystone 未生效：SELECT DATABASE() 应含 'dev'，实际=%q", db)
	}

	// 真实数据操作（dev 库可随意增删）
	call("DROP TABLE IF EXISTS e2e_go_users")
	call("CREATE TABLE e2e_go_users (id INT PRIMARY KEY, name VARCHAR(50))")
	call("INSERT INTO e2e_go_users (id,name) VALUES (1,'热巴'),(2,'古丽')")
	if cnt := call("SELECT COUNT(*) AS n FROM e2e_go_users"); !strings.Contains(cnt, "2") {
		t.Fatalf("INSERT/COUNT 异常: %q", cnt)
	}
	if rows := call("SELECT * FROM e2e_go_users WHERE id=1"); !strings.Contains(rows, "热巴") {
		t.Fatalf("SELECT 数据异常: %q", rows)
	}
	call("DROP TABLE e2e_go_users")
	t.Log("MySQL MCP env keystone 真机 E2E 全链路通过（经 hexclaw mcp.Manager 代码路径）")
}
