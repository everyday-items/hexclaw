package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	hexmcp "github.com/hexagon-codes/hexclaw/mcp"
)

// BUG-20260626 端到端契约：MCP 市场装了 server（MySQL/readfile）「再点进去都没有了」。
//
// GET /api/v1/mcp/servers 必须返回「已安装(已配置)」的 server——含市场一键安装后冷装尚未连上的
// （即时连接 10s 内未完成，转后台重连）。旧实现只回 ServerNames()（已连接子集）→ 冷装 server 从
// UI 列表消失，用户以为「没装上 / 装了又没了」。GET /api/v1/mcp/status 同步把它标为未连接。
func TestBug20260626_ListMCPServers_IncludesColdInstalled(t *testing.T) {
	// 桩：模拟冷装即时连接失败（npx 下载未就绪），server 只登记 config、不进 live 集。
	orig := hexagon.ConnectMCPStdioWithEnv
	hexagon.ConnectMCPStdioWithEnv = func(ctx context.Context, command string, env map[string]string, args ...string) ([]hexagon.Tool, func(), error) {
		return nil, nil, fmt.Errorf("cold install: npx download timeout")
	}
	defer func() { hexagon.ConnectMCPStdioWithEnv = orig }()

	mgr := hexmcp.NewManager()
	defer mgr.Close()
	if _, err := mgr.AddServerBestEffort(context.Background(), hexmcp.ServerConfig{
		Name: "mysql", Transport: "stdio", Command: "npx", Args: []string{"-y", "@benborla29/mcp-server-mysql"},
	}); err != nil {
		t.Fatalf("AddServerBestEffort: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.MCP.Servers = []config.MCPServerConfig{{
		Name:      "mysql",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"-y", "@benborla29/mcp-server-mysql"},
		Enabled:   true,
	}}
	srv := NewServer(cfg, &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)
	srv.mcpMgr = mgr

	// ① GET /api/v1/mcp/servers → 列表必须含 mysql
	req := httptest.NewRequest("GET", "/api/v1/mcp/servers", nil)
	w := httptest.NewRecorder()
	srv.handleListMCPServers(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	var listResp struct {
		Servers []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Transport   string `json:"transport"`
			ToolCount   int    `json:"tool_count"`
		} `json:"servers"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var mysqlSeen bool
	for _, server := range listResp.Servers {
		if server.Name == "mysql" {
			mysqlSeen = true
			if server.Description == "" || server.Transport == "unknown" {
				t.Errorf("mysql projection must include description and transport, got=%+v", server)
			}
		}
	}
	if !mysqlSeen {
		t.Fatalf("[BUG-20260626] GET /mcp/servers 必须含已安装的 mysql（冷装未连也要在列表），got=%v", listResp.Servers)
	}

	// ② GET /api/v1/mcp/status → mysql 应存在且 connected=false（灰徽章），不消失
	req2 := httptest.NewRequest("GET", "/api/v1/mcp/status", nil)
	w2 := httptest.NewRecorder()
	srv.handleMCPStatus(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("status code=%d", w2.Code)
	}
	var statusResp struct {
		Servers []struct {
			Name      string `json:"name"`
			Connected bool   `json:"connected"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("decode status: %v (body=%s)", err, w2.Body.String())
	}
	var seen bool
	for _, s := range statusResp.Servers {
		if s.Name == "mysql" {
			seen = true
			if s.Connected {
				t.Errorf("未连上的 mysql 状态应为 connected=false")
			}
		}
	}
	if !seen {
		t.Fatalf("[BUG-20260626] GET /mcp/status 必须含 mysql，got=%s", strings.TrimSpace(w2.Body.String()))
	}
}
