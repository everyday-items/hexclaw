package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/testing/mock"
)

// bug-20260626（配套 api 层 TestHandleCallMCPTool_NoDoubleWrap）：
// 锁定 Manager.CallTool 对「工具 Execute 失败」只加一层 `工具 "<name>" 执行失败:` 前缀。
//
// 这是双重包裹的第 1 层。第 2 层（api handler）已修为原样透出；本测试确保第 1 层始终
// 只产生一份前缀——两者合起来，用户截图里的「执行失败: 执行失败:」不再可能复现。
func TestCallTool_WrapsExecuteErrorOnce(t *testing.T) {
	m := NewManager()
	// 注入一个 Execute 必失败的工具——用一个**业务类**错误（SQL 语法错），
	// 刻意避开「连接已关」标志，以验证普通错误走「单层前缀 + 保留原因」路径。
	failing := mock.ErrorTool("mysql_query", errors.New("ER_PARSE_ERROR: You have an error in your SQL syntax"))
	m.servers["mysql"] = &connectedServer{
		name:      "mysql",
		connected: true,
		tools:     []hexagon.Tool{failing},
	}

	_, err := m.CallTool(context.Background(), "mysql_query", map[string]any{"sql": "show databases"})
	if err == nil {
		t.Fatal("期望 CallTool 返回错误")
	}
	msg := err.Error()
	// 前缀只出现一次。
	if n := strings.Count(msg, "执行失败"); n != 1 {
		t.Errorf("CallTool 应只加一层「执行失败」前缀，实际出现 %d 次: %q", n, msg)
	}
	// 且保留了底层真实原因（不吞错）。
	if !strings.Contains(msg, "ER_PARSE_ERROR") {
		t.Errorf("应保留底层失败原因，实际: %q", msg)
	}
}

// bug-20260626 #3c：stdio MCP 子进程退出后，工具调用拿到传输层错误
// （`connection closed: calling "tools/call": client is closing: EOF`）——对用户是天书。
//
// 不变量：连接已关 / 进程退出类错误必须翻译成可操作的友好提示
// 「MCP 服务进程已退出，请检查数据库连接配置…」，且不把底层传输术语（EOF/client is closing）糊用户脸上。
func TestCallTool_FriendlyMessageOnConnClosed(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"wrapped-eof", fmt.Errorf(`connection closed: calling "tools/call": client is closing: %w`, io.EOF)},
		{"client-is-closing", errors.New(`connection closed: calling "tools/call": client is closing: EOF`)},
		{"broken-pipe", errors.New("write |1: broken pipe")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager()
			failing := mock.ErrorTool("mysql_query", tc.err)
			m.servers["mysql"] = &connectedServer{
				name:      "mysql",
				connected: true,
				tools:     []hexagon.Tool{failing},
			}

			_, err := m.CallTool(context.Background(), "mysql_query", map[string]any{"sql": "show databases"})
			if err == nil {
				t.Fatal("期望 CallTool 返回错误")
			}
			msg := err.Error()
			if !strings.Contains(msg, "MCP 服务进程已退出") || !strings.Contains(msg, "数据库连接配置") {
				t.Errorf("连接关闭应给可操作的友好提示，实际: %q", msg)
			}
			// 友好提示不暴露底层传输术语。
			if strings.Contains(msg, "EOF") || strings.Contains(msg, "client is closing") || strings.Contains(msg, "broken pipe") {
				t.Errorf("友好提示不应暴露底层传输术语，实际: %q", msg)
			}
		})
	}
}

// bug-20260626 #3c 回归（误报防护）：工具**自身**的业务错误里恰好含 "EOF" 子串
// （如 JSON 解析 "unexpected EOF"、上游返回 "...: EOF"），不得被误判成「进程已退出」——
// 那会把真正的业务错误吞掉、换成误导用户去查数据库配置。
// 只有「传输层/进程关闭」(io.EOF 类型 / client is closing / connection closed / broken pipe…) 才翻译。
func TestCallTool_DoesNotMisclassifyToolEOFSubstring(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"json-unexpected-eof", errors.New("unexpected EOF while parsing JSON response")},
		{"upstream-eof-in-payload", errors.New("ERROR 1064: syntax error near 'EOF'")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager()
			failing := mock.ErrorTool("mysql_query", tc.err)
			m.servers["mysql"] = &connectedServer{
				name:      "mysql",
				connected: true,
				tools:     []hexagon.Tool{failing},
			}

			_, err := m.CallTool(context.Background(), "mysql_query", map[string]any{"sql": "x"})
			if err == nil {
				t.Fatal("期望 CallTool 返回错误")
			}
			msg := err.Error()
			// 不得被误判为「进程已退出」。
			if strings.Contains(msg, "MCP 服务进程已退出") {
				t.Errorf("[BUG 误报] 含 EOF 子串的业务错误被误判成连接关闭: %q", msg)
			}
			// 应原样保留工具业务错误（走普通「执行失败」单层包裹）。
			if !strings.Contains(msg, tc.err.Error()) {
				t.Errorf("业务错误原因应保留，实际: %q", msg)
			}
		})
	}
}
