package api

// BUG-20260703 G1/G2：Agent 注册/注销/设默认的持久化错误被 `_ =` 静默吞掉仍返 200。
//
// G1 症状：handleRegisterAgent/handleUnregisterAgent/handleSetDefaultAgent 对
// agentStore.SaveAgent/DeleteAgent/SetDefault 的错误一律忽略——Agent 已入内存但 DB
// 落库失败时用户收到 200，重启后 Agent 蒸发。对照 handleUpdateAgent/handleAddRule
// 对持久化错误返 500 的既有纪律。
//
// G2 症状：注销默认 Agent 后 Dispatcher 在内存重选新默认（smallestAgentName），但
// 从不 SetDefault 落库——DB 里 is_default 随删除的行消失，LoadAgents 返回空 default，
// 仅因 LoadAll 兜底也取 smallest 才巧合一致。显式落库消除巧合依赖。
//
// 附带：注销 Agent 只清内存规则，agent_rules 表从不清理（DeleteRulesByAgent 零调用
// 方）——孤儿规则重启即还魂，引用已不存在的 Agent。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// failingAgentStore：可注入失败的 router.Store 替身（含 A1 新增的两个级联方法，
// 保证接口扩展后本替身仍满足接口）。
type failingAgentStore struct {
	failSaveAgent  bool
	failDeleteAgnt bool
	failSetDefault bool
}

var errAgentStoreDown = errors.New("模拟持久化失败：磁盘只读")

func (f *failingAgentStore) Init(ctx context.Context) error { return nil }
func (f *failingAgentStore) LoadAgents(ctx context.Context) ([]agentrouter.AgentConfig, string, error) {
	return nil, "", nil
}
func (f *failingAgentStore) SaveAgent(ctx context.Context, agent *agentrouter.AgentConfig) error {
	if f.failSaveAgent {
		return errAgentStoreDown
	}
	return nil
}
func (f *failingAgentStore) DeleteAgent(ctx context.Context, name string) error {
	if f.failDeleteAgnt {
		return errAgentStoreDown
	}
	return nil
}
func (f *failingAgentStore) DeleteAgentAndSetDefault(ctx context.Context, name, nextDefault string, wasDefault bool) error {
	if err := f.DeleteAgent(ctx, name); err != nil {
		return err
	}
	if wasDefault {
		return f.SetDefault(ctx, nextDefault)
	}
	return nil
}
func (f *failingAgentStore) SetDefault(ctx context.Context, name string) error {
	if f.failSetDefault {
		return errAgentStoreDown
	}
	return nil
}
func (f *failingAgentStore) LoadRules(ctx context.Context) ([]agentrouter.Rule, error) {
	return nil, nil
}
func (f *failingAgentStore) SaveRule(ctx context.Context, rule *agentrouter.Rule) error { return nil }
func (f *failingAgentStore) DeleteRule(ctx context.Context, id int) error               { return nil }
func (f *failingAgentStore) DeleteRulesByAgent(ctx context.Context, agentName string) error {
	return nil
}
func (f *failingAgentStore) DeleteRulesByInstance(ctx context.Context, platform, instanceID string) error {
	return nil
}
func (f *failingAgentStore) DeleteRulesByPlatform(ctx context.Context, platform string) error {
	return nil
}

func newAgentPersistServer(t *testing.T, store *failingAgentStore) (*Server, *agentrouter.Dispatcher) {
	t.Helper()
	dispatcher := agentrouter.New()
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentStore(store)
	return srv, dispatcher
}

func TestBug20260703G1_RegisterAgentPersistFailureReturns500(t *testing.T) {
	srv, dispatcher := newAgentPersistServer(t, &failingAgentStore{failSaveAgent: true})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents",
		strings.NewReader(`{"name":"tutor","display_name":"Tutor"}`))
	w := httptest.NewRecorder()
	srv.handleRegisterAgent(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("SaveAgent 失败仍返回 %d（吞错假成功），期望 500: %s", w.Code, w.Body.String())
	}
	// 落库失败必须回滚内存注册：否则用户重试永远 409 冲突，且重启后 Agent 蒸发。
	if _, ok := dispatcher.GetAgent("tutor"); ok {
		t.Fatal("落库失败后内存注册应回滚，Agent 不应残留")
	}
}

func TestBug20260703G1_UnregisterAgentPersistFailureReturns500(t *testing.T) {
	srv, dispatcher := newAgentPersistServer(t, &failingAgentStore{failDeleteAgnt: true})
	if err := dispatcher.Register(agentrouter.AgentConfig{Name: "tutor"}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/tutor", nil)
	req.SetPathValue("name", "tutor")
	w := httptest.NewRecorder()
	srv.handleUnregisterAgent(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("DeleteAgent 失败仍返回 %d（吞错假成功），期望 500: %s", w.Code, w.Body.String())
	}
}

func TestBug20260703G1_SetDefaultPersistFailureReturns500(t *testing.T) {
	srv, dispatcher := newAgentPersistServer(t, &failingAgentStore{failSetDefault: true})
	if err := dispatcher.Register(agentrouter.AgentConfig{Name: "tutor"}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/agents/default",
		strings.NewReader(`{"name":"tutor"}`))
	w := httptest.NewRecorder()
	srv.handleSetDefaultAgent(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("SetDefault 失败仍返回 %d（吞错假成功），期望 500: %s", w.Code, w.Body.String())
	}
}

// G2 + 孤儿规则：用真 SQLite 走完整链路。
func TestBug20260703G2_UnregisterDefaultAgentPersistsNewDefaultAndCleansRules(t *testing.T) {
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "g2-default.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 存储失败: %v", err)
	}
	defer store.Close()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化 SQLite 存储失败: %v", err)
	}
	agentStore := agentrouter.NewSQLiteStore(store.DB())
	if err := agentStore.Init(context.Background()); err != nil {
		t.Fatalf("初始化 AgentStore 失败: %v", err)
	}

	dispatcher := agentrouter.New()
	ctx := context.Background()
	// 先注册 beta（成为默认），再注册 alpha；两层都落库。
	for _, name := range []string{"beta", "alpha"} {
		cfg := agentrouter.AgentConfig{Name: name}
		if err := dispatcher.Register(cfg); err != nil {
			t.Fatalf("注册 %s 失败: %v", name, err)
		}
		if err := agentStore.SaveAgent(ctx, &cfg); err != nil {
			t.Fatalf("落库 %s 失败: %v", name, err)
		}
	}
	if err := agentStore.SetDefault(ctx, "beta"); err != nil {
		t.Fatalf("落库默认失败: %v", err)
	}
	// beta 的路由规则（注销后不清理即孤儿）
	rule := agentrouter.Rule{Platform: "telegram", AgentName: "beta"}
	if err := agentStore.SaveRule(ctx, &rule); err != nil {
		t.Fatalf("落库规则失败: %v", err)
	}

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, store)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentStore(agentStore)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/beta", nil)
	req.SetPathValue("name", "beta")
	w := httptest.NewRecorder()
	srv.handleUnregisterAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("注销期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	// G2：内存重选的新默认（alpha=smallest）必须显式落库，而非依赖 LoadAll 兜底巧合。
	if got := dispatcher.DefaultAgent(); got != "alpha" {
		t.Fatalf("内存默认应重选为 alpha，实际 %q", got)
	}
	_, storedDefault, err := agentStore.LoadAgents(ctx)
	if err != nil {
		t.Fatalf("LoadAgents 失败: %v", err)
	}
	if storedDefault != "alpha" {
		t.Fatalf("注销默认 Agent 后新默认未落库：期望 alpha，实际 %q", storedDefault)
	}

	// 孤儿规则：注销 Agent 后其持久化规则必须清理，否则重启还魂引用不存在的 Agent。
	rules, err := agentStore.LoadRules(ctx)
	if err != nil {
		t.Fatalf("LoadRules 失败: %v", err)
	}
	for _, r := range rules {
		if r.AgentName == "beta" {
			t.Fatalf("注销后 agent_rules 仍残留 beta 的规则（孤儿）：%+v", r)
		}
	}
}

// G1 连带：SetDefault("") 清除默认是合法操作；修掉吞错后 store 层必须支持
// 空名清除语义，不得把「清除」误报为 500。
func TestBug20260703G1_ClearDefaultAgentSucceeds(t *testing.T) {
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "g1-clear.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 存储失败: %v", err)
	}
	defer store.Close()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化 SQLite 存储失败: %v", err)
	}
	agentStore := agentrouter.NewSQLiteStore(store.DB())
	if err := agentStore.Init(context.Background()); err != nil {
		t.Fatalf("初始化 AgentStore 失败: %v", err)
	}

	dispatcher := agentrouter.New()
	ctx := context.Background()
	cfg := agentrouter.AgentConfig{Name: "tutor"}
	if err := dispatcher.Register(cfg); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := agentStore.SaveAgent(ctx, &cfg); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	if err := agentStore.SetDefault(ctx, "tutor"); err != nil {
		t.Fatalf("落库默认失败: %v", err)
	}

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, store)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentStore(agentStore)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/agents/default",
		strings.NewReader(`{"name":""}`))
	w := httptest.NewRecorder()
	srv.handleSetDefaultAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("清除默认期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	_, storedDefault, err := agentStore.LoadAgents(ctx)
	if err != nil {
		t.Fatalf("LoadAgents 失败: %v", err)
	}
	if storedDefault != "" {
		t.Fatalf("清除默认后持久化 default 应为空，实际 %q", storedDefault)
	}
}
