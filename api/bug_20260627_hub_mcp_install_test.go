package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	hexmcp "github.com/hexagon-codes/hexclaw/mcp"
	"github.com/hexagon-codes/hexclaw/skill/hub"
)

// BUG-20260627（BUG-20260626 链路续审）：MCP 市场「点击安装没反应/装不上」的第二条成因——
// hub 安装路径（installFromHub → installMCPFromClawHubEntry）用 strict AddServer + 15s 超时，
// 冷装（npx/uvx 首次下载组件 >窗口）即时连接失败 → 直接 400 且 **AppendMCPServer 未执行**（不持久化、
// 不注册、reconnectLoop 永不接管）→ 用户点了「安装」拿到一个错误且什么都没装上。
//
// 这与直接安装路径 handleAddMCPServer 已修的 AddServerBestEffort 不一致（同一冷装场景，一条容忍一条硬失败）。
// 正解：hub 安装也走 best-effort——即时连不上不致命，注册 + 持久化，交后台 reconnectLoop 拉起。
func TestBug20260627_HubMCPInstall_ColdInstallNotHardFail(t *testing.T) {
	// 桩：模拟冷装即时连接失败（npx 下载未就绪）。
	orig := hexagon.ConnectMCPStdioWithEnv
	hexagon.ConnectMCPStdioWithEnv = func(ctx context.Context, command string, env map[string]string, args ...string) ([]hexagon.Tool, func(), error) {
		return nil, nil, fmt.Errorf("cold install: npx download timeout")
	}
	defer func() { hexagon.ConnectMCPStdioWithEnv = orig }()

	mgr := hexmcp.NewManager()
	defer mgr.Close()
	srv := NewServer(config.DefaultConfig(), &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)
	srv.mcpMgr = mgr

	req := httptest.NewRequest("POST", "/api/v1/skills/install", nil)
	w := httptest.NewRecorder()
	entry, err := hub.ValidatePinnedMCPServer(hub.MCPServerMetaFromSkill(hub.SkillMeta{
		Name:       "filesystem",
		Type:       "mcp",
		Command:    "npx",
		Args:       []string{"-y", "@modelcontextprotocol/server-filesystem@2026.7.10"},
		ConfigHint: "需配置目录",
		Status:     "pinned",
		Artifact: &hub.MCPArtifact{
			Ecosystem:      "npm",
			Package:        "@modelcontextprotocol/server-filesystem",
			Version:        "2026.7.10",
			Integrity:      "sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==",
			SourceRegistry: "https://registry.npmjs.org",
		},
	}))
	if err != nil {
		t.Fatalf("validate pinned test entry: %v", err)
	}
	srv.installMCPFromClawHubEntry(w, req, entry)

	// ① 冷装即时连接失败不应硬失败（旧 strict AddServer → 400）。
	if w.Code != http.StatusOK {
		t.Fatalf("[BUG-20260627] hub 安装冷装不应硬失败，应 200 + 后台重连，got code=%d body=%s", w.Code, w.Body.String())
	}
	// ② server 必须已注册（交 reconnectLoop 拉起），而非丢弃。
	if !slices.Contains(mgr.ConfiguredServerNames(), "filesystem") {
		t.Fatalf("[BUG-20260627] hub 冷装应注册 server 交后台重连（不丢弃），got=%v", mgr.ConfiguredServerNames())
	}
}
