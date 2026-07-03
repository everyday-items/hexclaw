package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/testing/mock"
)

// BUG-20260704：stdio MCP 子进程自亡后「状态谎报 + 永不自愈」。
//
// 真机现象：数据连接器卡片徽章显示「已连接」，点「测试」却报
// 「工具 "mysql_query" 执行失败: MCP 服务进程已退出…」。根因链：
//  1. connectedServer.connected 只在 connectServer 置 true、仅手动 close 置 false，
//     子进程自亡无感知点 → ServerStatuses 永远谎报 Connected=true（UI 徽章的事实源）；
//  2. tryReconnect 的 needReconnect = !exists || !connected 对「死而仍标 connected」
//     恒 false → 30s 重连循环永远跳过，不自愈；
//  3. CallTool 的 isMCPConnClosed 分支已精确识别进程退出，却只翻译文案不翻状态。
//
// 不变量：CallTool 识别到传输层「进程已退出」错误时，必须把属主 server 标记为
// disconnected —— 状态即刻真实（徽章翻「未就绪」），且下个 reconnect tick 自动重拉自愈。
func TestCallTool_ConnClosedMarksServerDisconnected(t *testing.T) {
	m := NewManager()
	dead := mock.ErrorTool("mysql_query",
		fmt.Errorf(`connection closed: calling "tools/call": client is closing: %w`, io.EOF))
	m.configs = []ServerConfig{{Name: "mysql", Enabled: true, Transport: "stdio", Command: "definitely-not-a-real-cmd"}}
	m.servers["mysql"] = &connectedServer{
		name:      "mysql",
		connected: true,
		tools:     []hexagon.Tool{dead},
	}

	_, err := m.CallTool(context.Background(), "mysql_query", map[string]any{"sql": "SELECT 1"})
	if err == nil {
		t.Fatal("期望 CallTool 返回错误")
	}

	st := statusOf(t, m, "mysql")
	if st.Connected {
		t.Errorf("识别到进程退出后 ServerStatuses 仍报 Connected=true —— 徽章谎报「已连接」且 tryReconnect 永远跳过（不自愈）")
	}
}

// 对照：业务类错误（SQL 语法错等）绝不能误翻状态——server 活得好好的，
// 只是这条查询错了；误标 disconnected 会触发无谓的重连抖动。
func TestCallTool_BusinessErrorKeepsServerConnected(t *testing.T) {
	m := NewManager()
	failing := mock.ErrorTool("mysql_query", errors.New("ER_PARSE_ERROR: You have an error in your SQL syntax"))
	m.configs = []ServerConfig{{Name: "mysql", Enabled: true, Transport: "stdio", Command: "x"}}
	m.servers["mysql"] = &connectedServer{
		name:      "mysql",
		connected: true,
		tools:     []hexagon.Tool{failing},
	}

	if _, err := m.CallTool(context.Background(), "mysql_query", map[string]any{"sql": "show databasse"}); err == nil {
		t.Fatal("期望 CallTool 返回错误")
	}

	if st := statusOf(t, m, "mysql"); !st.Connected {
		t.Errorf("业务错误不应翻转连接状态，实际 Connected=false")
	}
}

func statusOf(t *testing.T, m *Manager, name string) ServerStatus {
	t.Helper()
	for _, st := range m.ServerStatuses() {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("ServerStatuses 未包含 %q", name)
	return ServerStatus{}
}
