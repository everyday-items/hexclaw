package api

import (
	"encoding/json"
	"testing"

	hexmcp "github.com/hexagon-codes/hexclaw/mcp"
)

// AUDIT 2026-06-23 数据连接器走 MCP 的 env keystone（hexclaw 层契约）：
// 新增 MCP server 请求必须能携带 env（MySQL/Redis 等 stdio MCP 靠环境变量配凭证），
// 且 env 要落到 hexmcp.ServerConfig.Env → connectServer → hexagon.ConnectMCPStdioWithEnv。
func TestAddMCPServerRequest_ParsesEnv(t *testing.T) {
	body := `{"name":"mysql","command":"npx","args":["-y","@benborla29/mcp-server-mysql"],"env":{"MYSQL_HOST":"localhost","MYSQL_PASSWORD":"s3cret"}}`
	var req addMCPServerRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Env["MYSQL_HOST"] != "localhost" || req.Env["MYSQL_PASSWORD"] != "s3cret" {
		t.Fatalf("env 未解析进请求: %+v", req.Env)
	}
}

func TestAddMCPServerRequest_ParsesSecretMutations(t *testing.T) {
	body := `{"name":"postgres","command":"npx","args":["-y","server-postgres","postgresql://user@localhost/db"],"secret_args":[{"index":2,"mode":"preserve","credential_ref":"sidecar-connection:v1:connection-1:password"}]}`
	var req addMCPServerRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.SecretArgs) != 1 || req.SecretArgs[0].Mode != "preserve" || req.SecretArgs[0].Index != 2 {
		t.Fatalf("secret mutation 未解析进请求: %#v", req.SecretArgs)
	}
}

// ServerConfig 必须有 Env 字段且 connectServer 用它（编译期 + 字段存在性钉死）。
func TestServerConfig_CarriesEnv(t *testing.T) {
	cfg := hexmcp.ServerConfig{
		Name:    "redis",
		Command: "npx",
		Env:     map[string]string{"REDIS_URL": "redis://localhost:6379"},
	}
	if cfg.Env["REDIS_URL"] != "redis://localhost:6379" {
		t.Fatalf("ServerConfig.Env 未携带: %+v", cfg.Env)
	}
}
