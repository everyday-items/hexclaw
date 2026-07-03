package api

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hexagon-codes/hexclaw/autonomy"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
)

func openAutonomyTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// HX-3（RED 取证）：workflow 的 tool 节点直连 mcpMgr.CallTool 绕过 PermissionHook。
//
// function_first 下 workflow 源对连接器（MCP）工具属外部写入面，需任务级授权；
// 未授权时运行时必须拦下——否则「preflight 在创建时告知需授权、运行时兜底」的
// 承诺对 workflow tool 节点不成立（agent 节点走 eng.Process 有闸，tool 节点没有）。
func newAutonomyGatedWorkflowServer(t *testing.T) (*Server, *autonomy.GrantStore) {
	t.Helper()
	cfg := config.DefaultConfig()
	hook := engine.NewPermissionHook(engine.NewPermissionHub(0),
		engine.WithSystemDispatchPolicy(engine.NewSystemDispatchPolicyFromConfig(cfg.Security.Autonomy)),
	)
	srv := NewServer(cfg, &mockEngine{}, nil, nil)
	// 复用与 handler_autonomy_test 相同的内存库装配
	db := openAutonomyTestDB(t)
	grants := autonomy.NewGrantStore(db)
	if err := grants.Init(context.Background()); err != nil {
		t.Fatalf("grants Init: %v", err)
	}
	hook.SetSystemDispatchPolicy(engine.NewSystemDispatchPolicyFromConfig(cfg.Security.Autonomy))
	srv.SetAutonomy(hook, nil, grants, "")
	return srv, grants
}

func workflowWithConnectorTool() *WorkflowData {
	return &WorkflowData{
		ID:   "wf-gate",
		Name: "写标签工作流",
		Nodes: []any{
			map[string]any{"id": "n1", "type": "tool", "data": map[string]any{"tool": "github.issues.write_label"}},
		},
	}
}

func TestWorkflowToolNodeGatedForUnauthorizedConnector(t *testing.T) {
	srv, _ := newAutonomyGatedWorkflowServer(t)
	wf := workflowWithConnectorTool()
	exec := newWorkflowExecutor(srv, wf, RunWorkflowRequest{})

	node := &workflowNode{ID: "n1", Type: "tool", Data: map[string]any{"tool": "github.issues.write_label"}}
	_, err := exec.executeTool(context.Background(), node, "", nil)
	if err == nil {
		t.Fatal("未授权连接器工具在 workflow tool 节点应被运行时闸拦下（当前直连 mcpMgr 绕过了 PermissionHook）")
	}
	if !strings.Contains(err.Error(), "authorization") && !strings.Contains(err.Error(), "授权") {
		t.Fatalf("拦截错误应指向授权缺失，得到: %v", err)
	}
}

func TestWorkflowToolNodeAllowedWithTaskGrant(t *testing.T) {
	srv, grants := newAutonomyGatedWorkflowServer(t)
	if _, err := grants.Create(context.Background(), autonomy.Grant{
		TaskRef: "workflow:wf-gate", Source: "workflow",
		Entries: []string{"github.issues.write_label"},
	}); err != nil {
		t.Fatalf("Create grant: %v", err)
	}
	wf := workflowWithConnectorTool()
	exec := newWorkflowExecutor(srv, wf, RunWorkflowRequest{})

	node := &workflowNode{ID: "n1", Type: "tool", Data: map[string]any{"tool": "github.issues.write_label"}}
	// 有授权 → 过闸后进入真实 CallTool；mcpMgr 为 nil 时返回「未初始化」错误，
	// 但绝不能是「授权缺失」错误——闸已放行是关键。
	_, err := exec.executeTool(context.Background(), node, "", nil)
	if err != nil && (strings.Contains(err.Error(), "authorization") || strings.Contains(err.Error(), "授权")) {
		t.Fatalf("已授权的连接器工具不应被授权闸拦下，得到: %v", err)
	}
}
