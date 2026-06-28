package mcp

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

// BUG-20260626 回归：MCP 市场装了 server（如 MySQL / readfile-filesystem），「再点进去都没有了」。
//
// 用户反馈：
//   - 在 MCP 市场装了一个 MySQL、一个 readfile，再点进去（服务器 tab / 重新进入）都没有了。
//   - 并且很多市场工具点击安装「没反应」——装了也不出现，看着像没装上。
//
// 根因：installFromMarketplace → POST /api/v1/mcp/servers → AddServerBestEffort。冷装时（MySQL 需要
//   运行中的库 + env、filesystem 首次需 npx 下载组件）即时连接在 10s 窗口内失败 → 只登记 m.configs（交
//   reconnectLoop 后台拉起），**不写入 m.servers**。而 ServerNames()/ServerStatuses() 只遍历 m.servers
//   （已连接子集）→ 已安装但尚未连上的 server 在「列表」和「状态」里双双消失 = 用户看到的「都没有了」。
//
// 不变量（最佳实践：列表=已配置意图，状态=连接实况）：
//   - 已配置（Enabled）的 server 必须出现在 UI 列表（ConfiguredServerNames）与状态（ServerStatuses）里，
//     未连上时状态为 disconnected（灰徽章），连上后翻绿——绝不从列表中消失。
//   - ServerNames()「live（已连接）」语义保持不变（启动计数 / 内部路由用），不被本次修复破坏。
func TestBug20260626_ColdInstalledServerStaysVisibleInListAndStatus(t *testing.T) {
	setErr, _ := stubStdioToggle(t)
	m := NewManager()

	// 模拟「市场一键安装」MySQL：冷装即时连接失败（无运行库 / 组件首次下载超时）。
	setErr(fmt.Errorf("npx download timed out: context deadline exceeded"))
	mysql := ServerConfig{Name: "mysql", Transport: "stdio", Command: "npx", Args: []string{"-y", "@benborla29/mcp-server-mysql"}}
	connected, err := m.AddServerBestEffort(context.Background(), mysql)
	if err != nil {
		t.Fatalf("冷装即时失败不应致命，err=%v", err)
	}
	if connected {
		t.Fatal("即时连接失败时 connected 应为 false")
	}

	// ① 状态：已安装但未连上的 server 必须出现在 ServerStatuses 里且标记为未连接（否则 UI 徽章丢失/列表消失）。
	statuses := m.ServerStatuses()
	var found *ServerStatus
	for i := range statuses {
		if statuses[i].Name == "mysql" {
			found = &statuses[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("[BUG-20260626] 已安装(已配置)的 mysql 必须出现在 ServerStatuses，否则用户看到「都没有了」，got=%v", statuses)
	}
	if found.Connected {
		t.Errorf("未连上的 mysql 状态应为未连接(disconnected)，got Connected=true")
	}

	// ② UI 服务器列表（GET /api/v1/mcp/servers → ConfiguredServerNames）必须包含已安装的 mysql。
	if names := m.ConfiguredServerNames(); !slices.Contains(names, "mysql") {
		t.Fatalf("[BUG-20260626] UI 服务器列表必须包含已安装的 mysql（即便尚未连上），got=%v", names)
	}

	// ③ live 语义不变：ServerNames() 仍只反映已连接（未连上时为 0）——内部路由 / 启动计数依赖此契约，
	//    不被本次修复破坏（这是 mcp/best_effort_20260623_test.go 已锁的不变量）。
	if len(m.ServerNames()) != 0 {
		t.Fatalf("ServerNames() 应保持 live 语义（未连接=0），got=%v", m.ServerNames())
	}
}

// 连带修复回归：已配置但尚未连上的冷装 server 必须可被删除（否则修复可见性后「看得到却删不掉」）。
func TestBug20260626_ColdInstalledServerIsRemovable(t *testing.T) {
	setErr, _ := stubStdioToggle(t)
	m := NewManager()
	setErr(fmt.Errorf("cold install timeout"))
	if _, err := m.AddServerBestEffort(context.Background(), ServerConfig{Name: "readfile", Transport: "stdio", Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem"}}); err != nil {
		t.Fatalf("冷装即时失败不应致命，err=%v", err)
	}
	// 此刻 readfile 只在 configs（未连上、不在 m.servers）。
	if !slices.Contains(m.ConfiguredServerNames(), "readfile") {
		t.Fatalf("前置：readfile 应在已配置列表，got=%v", m.ConfiguredServerNames())
	}
	// 删除「只在 configs」的冷装 server 必须成功（旧逻辑因不在 m.servers 报「不存在」）。
	if err := m.RemoveServer("readfile"); err != nil {
		t.Fatalf("[BUG-20260626] 已配置(未连)的 server 必须可删除，got err=%v", err)
	}
	if slices.Contains(m.ConfiguredServerNames(), "readfile") {
		t.Fatalf("删除后 readfile 不应再出现在列表，got=%v", m.ConfiguredServerNames())
	}
	// 真正不存在的仍报错（不破坏 TestRemoveServer_NotFound 契约）。
	if err := m.RemoveServer("ghost"); err == nil {
		t.Error("删除不存在的 server 应报错")
	}
}
