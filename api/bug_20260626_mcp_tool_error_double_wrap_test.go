package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	hexmcp "github.com/hexagon-codes/hexclaw/mcp"
)

// bug-20260626：MCP 工具测试报错被「双重包裹」。
//
// 复现（用户截图）：测试 mysql_query 工具失败时显示
//
//	工具 "mysql_query" 执行失败：工具 "mysql_query" 执行失败：调用 MCP 工具 mysql_query 失败：...
//	                    ^^^^^^^^^^^^^^^^^^^^^^^^^^^ 同一前缀出现两次
//
// 根因：两层各自加了同样的 `工具 "<name>" 执行失败:` 前缀——
//  1. mcp/client.go  Manager.CallTool   → fmt.Errorf("工具 %q 执行失败: %w", ...)
//  2. api/handler_extended.go handleCallMCPTool → "工具 \"" + name + "\" 执行失败: " + err.Error()
//
// CallTool 返回的 error 已经是完整可读的消息，handler 不应再套一层同样的框。
//
// 不变量：handler 必须原样透出 CallTool 的错误，不得叠加自己的 `工具 "<name>" 执行失败:` 前缀
//
//	→ 工具名前缀在最终消息里只能出现一次；且「未找到」不得被误标成「执行失败」。
func TestHandleCallMCPTool_NoDoubleWrap(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)
	// 空 Manager：无任何已注册 server/工具 → CallTool 走「未找到」分支，返回
	// `工具 "mysql_query" 未找到`（已含工具名，已是完整消息）。
	srv.SetMCPManager(hexmcp.NewManager())

	body := `{"name":"mysql_query","arguments":{"sql":"show databases"}}`
	req := httptest.NewRequest("POST", "/api/v1/mcp/tools/call", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleCallMCPTool(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200（错误以 body.error 透出），得到 %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, w.Body.String())
	}
	errMsg := resp["error"]
	if errMsg == "" {
		t.Fatalf("期望 error 字段非空，得到: %s", w.Body.String())
	}

	// 核心不变量①：工具名前缀只能出现一次（handler 不得在 CallTool 已完整描述的错误上再套同样前缀）。
	if n := strings.Count(errMsg, `工具 "mysql_query"`); n != 1 {
		t.Errorf("[BUG 双重包裹] 工具名前缀应只出现 1 次，实际出现 %d 次: %q", n, errMsg)
	}
	// 核心不变量②：「未找到」错误不得被 handler 误标为「执行失败」。
	if strings.Contains(errMsg, "执行失败") {
		t.Errorf("[BUG] handler 把「未找到」误包成「执行失败」: %q", errMsg)
	}
}
