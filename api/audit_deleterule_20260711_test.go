package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

// failingDeleteRuleStore 嵌入 router.Store 接口，只覆写 DeleteRule 使其返回错误，
// 模拟"内存删除成功但 DB 删除失败"的场景（R9 / AP-174）。
// 其余方法未被 handleDeleteRule 调用，保持 nil 接口即可（不会触达）。
type failingDeleteRuleStore struct {
	agentrouter.Store
	called bool
}

func (f *failingDeleteRuleStore) DeleteRule(_ context.Context, _ int) error {
	f.called = true
	return fmt.Errorf("simulated DB failure: connection reset")
}

// TestR9_AP174_DeleteRule_DBErrorNotSwallowed 验证 DELETE /agents/rules/{id}
// 在 DB 删除失败时不得吞错返回 200。
//
// 真断言（非弱断言）：
//   - 内存 RemoveRule 成功（规则确实存在），随后 DB DeleteRule 返回 error；
//   - 若 handler 用 `_ =` 吞掉该错误，响应恒为 200（RED，本测试 FAIL）——
//     这正是 bug：重启后规则从 DB 重载"还魂"，前端却以为已删除；
//   - 修复后必须把 DB 失败反映为非 2xx（应为 500），本测试断言 code != 200
//     且为 5xx，且确认 DeleteRule 确被调用（证明错误来自真实 DB 路径，非绕过）。
func TestR9_AP174_DeleteRule_DBErrorNotSwallowed(t *testing.T) {
	dispatcher := agentrouter.New()
	if err := dispatcher.Register(agentrouter.AgentConfig{Name: "coder"}); err != nil {
		t.Fatalf("注册 Agent 失败: %v", err)
	}
	// 内存里放一条 ID=1 的规则，确保 RemoveRule 成功，流程能走到 DB 删除
	if err := dispatcher.AddRule(agentrouter.Rule{ID: 1, AgentName: "coder"}); err != nil {
		t.Fatalf("添加规则失败: %v", err)
	}

	store := &failingDeleteRuleStore{}

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentStore(store)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/rules/1", nil)
	req.SetPathValue("id", "1")
	w := httptest.NewRecorder()

	srv.handleDeleteRule(w, req)

	if !store.called {
		t.Fatalf("DeleteRule 未被调用——测试未覆盖真实 DB 删除路径")
	}
	if w.Code == http.StatusOK {
		t.Fatalf("BUG AP-174: DB 删除失败却返回 200（错误被 `_ =` 吞掉），body=%s", w.Body.String())
	}
	if w.Code < 500 || w.Code >= 600 {
		t.Fatalf("期望 5xx（DB 删除失败），实际 %d: %s", w.Code, w.Body.String())
	}
}
