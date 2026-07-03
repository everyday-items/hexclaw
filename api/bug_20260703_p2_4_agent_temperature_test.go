package api

// BUG-20260703 P2-4（handler 半边）：温度三态契约。
//   - register：字段缺席=未设（nil）；显式 0=确定性采样；越界 [0,2] 拒绝。
//   - update：字段缺席=不改；显式 null=清除回未设；数值=设置（OptionalFloat 三态，
//     普通 *float64 无法区分「缺席」与「null」）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

func newTemperatureServer(t *testing.T) (*Server, *agentrouter.Dispatcher) {
	t.Helper()
	dispatcher := agentrouter.New()
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetAgentRouter(dispatcher)
	return srv, dispatcher
}

func registerAgentReq(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleRegisterAgent(w, req)
	return w
}

func updateAgentReq(t *testing.T, srv *Server, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/agents/"+name, strings.NewReader(body))
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	srv.handleUpdateAgent(w, req)
	return w
}

func TestBug20260703P24_RegisterTemperatureTriState(t *testing.T) {
	srv, dispatcher := newTemperatureServer(t)

	// 缺席 = 未设（nil，跟随模型默认）
	if w := registerAgentReq(t, srv, `{"name":"follow"}`); w.Code != http.StatusOK {
		t.Fatalf("注册失败: %d %s", w.Code, w.Body.String())
	}
	if got, _ := dispatcher.GetAgent("follow"); got.Temperature != nil {
		t.Fatalf("缺席 temperature 应为 nil，实际 %v", *got.Temperature)
	}

	// 显式 0 = 确定性采样（旧 float64 契约下 0 会被当未设吞掉）
	if w := registerAgentReq(t, srv, `{"name":"deterministic","temperature":0}`); w.Code != http.StatusOK {
		t.Fatalf("注册失败: %d %s", w.Code, w.Body.String())
	}
	if got, _ := dispatcher.GetAgent("deterministic"); got.Temperature == nil || *got.Temperature != 0 {
		t.Fatalf("显式 temperature=0 应为非 nil 的 0，实际 %v", got.Temperature)
	}

	// 越界拒绝
	if w := registerAgentReq(t, srv, `{"name":"toohot","temperature":3.5}`); w.Code != http.StatusBadRequest {
		t.Fatalf("temperature=3.5 应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if _, ok := dispatcher.GetAgent("toohot"); ok {
		t.Fatal("校验失败不应注册进内存")
	}
}

func TestBug20260703P24_UpdateTemperatureTriState(t *testing.T) {
	srv, dispatcher := newTemperatureServer(t)
	if w := registerAgentReq(t, srv, `{"name":"tutor","temperature":0.7}`); w.Code != http.StatusOK {
		t.Fatalf("注册失败: %d %s", w.Code, w.Body.String())
	}

	// 缺席 = 不改
	if w := updateAgentReq(t, srv, "tutor", `{"display_name":"家教"}`); w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}
	if got, _ := dispatcher.GetAgent("tutor"); got.Temperature == nil || *got.Temperature != 0.7 {
		t.Fatalf("缺席字段不应改动温度，实际 %v", got.Temperature)
	}

	// 数值 = 设置（含 0）
	if w := updateAgentReq(t, srv, "tutor", `{"temperature":0}`); w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}
	if got, _ := dispatcher.GetAgent("tutor"); got.Temperature == nil || *got.Temperature != 0 {
		t.Fatalf("temperature=0 应设为非 nil 的 0，实际 %v", got.Temperature)
	}

	// null = 清除回未设
	if w := updateAgentReq(t, srv, "tutor", `{"temperature":null}`); w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}
	if got, _ := dispatcher.GetAgent("tutor"); got.Temperature != nil {
		t.Fatalf("temperature=null 应清除回 nil，实际 %v", *got.Temperature)
	}

	// 越界拒绝且不落
	if w := updateAgentReq(t, srv, "tutor", `{"temperature":-0.5}`); w.Code != http.StatusBadRequest {
		t.Fatalf("temperature=-0.5 应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if got, _ := dispatcher.GetAgent("tutor"); got.Temperature != nil {
		t.Fatalf("校验失败不应改动配置，实际 %v", *got.Temperature)
	}
}
