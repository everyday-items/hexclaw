package api

// BUG-20260703 D3（后端半边）：provider 失效后该 Agent 连人设都改不了（校验连坐）。
//
// 症状：handleUpdateAgent 对合并后的完整配置无条件跑 validateAgentLLMConfig——Agent
// 原绑的 provider 被禁用/移除后，即使本次请求只改 display_name/system_prompt（完全
// 不碰 model/provider），也因存量 provider 校验不过而 400。编辑被 provider 状态连坐
// 锁死，用户无路可走。
//
// 修法：只在请求真的改动 model/provider 时才校验其有效性；存量值原样保留不重审。
// 真改 LLM 配置时的校验保持不放松（BUG-20260625 §3-2 禁用 provider 拒绝语义不回退）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

func newEditLockoutServer(t *testing.T) (*Server, *agentrouter.Dispatcher, *agentrouter.SQLiteStore) {
	t.Helper()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "d3-lockout.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 存储失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化 SQLite 存储失败: %v", err)
	}
	agentStore := agentrouter.NewSQLiteStore(store.DB())
	if err := agentStore.Init(context.Background()); err != nil {
		t.Fatalf("初始化 AgentStore 失败: %v", err)
	}

	dispatcher := agentrouter.New()
	// Agent 注册时 provider "vanished" 还有效；随后该 provider 从配置中消失。
	original := agentrouter.AgentConfig{
		Name: "coder", DisplayName: "Coder", Model: "gpt-4o", Provider: "vanished",
		SystemPrompt: "旧人设",
	}
	if err := dispatcher.Register(original); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := agentStore.SaveAgent(context.Background(), &original); err != nil {
		t.Fatalf("落库失败: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{} // provider 已不存在
	srv := NewServer(cfg, &mockEngine{}, nil, store)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentStore(agentStore)
	return srv, dispatcher, agentStore
}

func putAgent(t *testing.T, srv *Server, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/agents/"+name, strings.NewReader(body))
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	srv.handleUpdateAgent(w, req)
	return w
}

func TestBug20260703D3_EditPersonaSucceedsWhenProviderVanished(t *testing.T) {
	srv, dispatcher, _ := newEditLockoutServer(t)

	// 只改人设，不碰 model/provider → 不应被存量 provider 校验连坐。
	w := putAgent(t, srv, "coder", `{"system_prompt":"新人设","display_name":"Coder 2"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("provider 失效时仅改人设应能保存，实际 %d: %s", w.Code, w.Body.String())
	}
	got, ok := dispatcher.GetAgent("coder")
	if !ok {
		t.Fatal("Agent 消失")
	}
	if got.SystemPrompt != "新人设" || got.DisplayName != "Coder 2" {
		t.Fatalf("人设未更新: %+v", got)
	}
	if got.Model != "gpt-4o" || got.Provider != "vanished" {
		t.Fatalf("未改动的 LLM 配置不应被动: model=%q provider=%q", got.Model, got.Provider)
	}
}

// 前端编辑弹窗把未动的 provider/model 原样回传（非省略字段）——与存量相等的回传
// 同样不得触发连坐校验。
func TestBug20260703D3_EditWithUnchangedLLMFieldsEchoedBack(t *testing.T) {
	srv, _, _ := newEditLockoutServer(t)

	w := putAgent(t, srv, "coder",
		`{"system_prompt":"回传人设","provider":"vanished","model":"gpt-4o"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("回传未变 LLM 字段仅改人设应能保存，实际 %d: %s", w.Code, w.Body.String())
	}
}

// 守门不放松：真改 model/provider 到无效值仍必须 400。
func TestBug20260703D3_ChangingToInvalidProviderStillRejected(t *testing.T) {
	srv, dispatcher, _ := newEditLockoutServer(t)

	w := putAgent(t, srv, "coder", `{"provider":"still-missing","model":"some-model"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("改到无效 provider 应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	got, _ := dispatcher.GetAgent("coder")
	if got.Provider != "vanished" {
		t.Fatalf("校验失败后配置不应被改动: %+v", got)
	}
}
