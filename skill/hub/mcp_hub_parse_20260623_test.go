package hub

import "testing"

// 旧 bug：把 mcp-registry.json（对象 {servers:[...]}）当裸数组反序列化 → 必然解析失败，
// CLI(`hexclaw mcp`) + agentic 安装技能的 MCP 目录形同虚设。parseMcpServers 修此根因：
// 按 .servers 解析对象格式（与 skillHub index 一致），并兼容极老裸数组格式。
func TestParseMcpServers_ObjectFormat(t *testing.T) {
	data := []byte(`{
		"version": "1.3.1",
		"updated_at": "2026-06-23",
		"servers": [
			{"name":"mysql","command":"npx","args":["-y","@benborla29/mcp-server-mysql"],
			 "env":{"MYSQL_HOST":"localhost","MYSQL_PORT":"3306","MYSQL_PASS":"","MYSQL_DB":""}},
			{"name":"redis","command":"npx","args":["-y","@gongrzhe/server-redis-mcp","redis://localhost:6379"]}
		]
	}`)
	servers, err := parseMcpServers(data)
	if err != nil {
		t.Fatalf("对象格式应解析成功: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("应解析出 2 个 server，得 %d", len(servers))
	}
	if servers[0].Name != "mysql" || servers[0].Env["MYSQL_PASS"] != "" {
		t.Errorf("mysql env 未解析进结构: %+v", servers[0])
	}
	if _, bad := servers[0].Env["MYSQL_PASSWORD"]; bad {
		t.Error("不应出现 MYSQL_PASSWORD（registry 已修为 MYSQL_PASS）")
	}
	// redis URL 作为 arg（非 env）。
	if len(servers[1].Args) != 3 || servers[1].Args[2] != "redis://localhost:6379" {
		t.Errorf("redis URL 应作为最后一个 arg: %+v", servers[1].Args)
	}
}

func TestParseMcpServers_BareArrayFallback(t *testing.T) {
	data := []byte(`[{"name":"x","command":"npx","args":["-y","pkg"]}]`)
	servers, err := parseMcpServers(data)
	if err != nil {
		t.Fatalf("裸数组回退应成功: %v", err)
	}
	if len(servers) != 1 || servers[0].Name != "x" {
		t.Fatalf("裸数组未解析: %+v", servers)
	}
}
