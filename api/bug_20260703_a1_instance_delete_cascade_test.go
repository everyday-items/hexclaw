package api

// BUG-20260703 A1：删除 IM 实例不级联清理路由规则（功能未闭环 + 跨表删除不联动）。
//
// 症状：handleDeleteInstance/handleDeleteInstanceByID 只调 instanceMgr.Delete（仅
// DELETE FROM platform_instances），全程不碰 agent_rules。删除实例后其 instance 级
// 绑定规则成孤儿；删掉某平台最后一个实例后，遗留 platform 级规则（instance_id=''）
// 也残留——重建同平台实例会静默继承旧绑定。
//
// 绑定粒度定论（2026-07-03 用户拍板）：instance 级。级联语义：
//   - 删任一实例 → 清该 (platform, instance_id=实例名) 的规则；
//   - 删平台最后一个实例 → 顺带清该平台全部规则（含遗留 platform 级 instance_id=''）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/instances"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

type instanceCascadeHarness struct {
	srv        *Server
	mgr        *instances.Manager
	dispatcher *agentrouter.Dispatcher
	agentStore *agentrouter.SQLiteStore
}

func newInstanceCascadeHarness(t *testing.T) *instanceCascadeHarness {
	t.Helper()

	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "a1-cascade.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 存储失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化 SQLite 存储失败: %v", err)
	}

	mgr := instances.NewManager(store.DB())
	if err := mgr.Init(context.Background()); err != nil {
		t.Fatalf("初始化实例管理器失败: %v", err)
	}

	agentStore := agentrouter.NewSQLiteStore(store.DB())
	if err := agentStore.Init(context.Background()); err != nil {
		t.Fatalf("初始化 AgentStore 失败: %v", err)
	}

	dispatcher := agentrouter.New()
	tutor := agentrouter.AgentConfig{Name: "tutor", DisplayName: "Tutor"}
	if err := dispatcher.Register(tutor); err != nil {
		t.Fatalf("注册 Agent 失败: %v", err)
	}
	// agent_rules.agent_name 有 FK 引用 agents(name)，规则落库前 agent 必须先落库。
	if err := agentStore.SaveAgent(context.Background(), &tutor); err != nil {
		t.Fatalf("落库 Agent 失败: %v", err)
	}

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, store)
	srv.SetInstanceManager(mgr)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentStore(agentStore)

	return &instanceCascadeHarness{srv: srv, mgr: mgr, dispatcher: dispatcher, agentStore: agentStore}
}

func (h *instanceCascadeHarness) addInstance(t *testing.T, provider, name string) *instances.Instance {
	t.Helper()
	inst := &instances.Instance{Provider: provider, Name: name, Enabled: false, Config: []byte(`{"token":"x"}`)}
	if err := h.mgr.Upsert(context.Background(), inst); err != nil {
		t.Fatalf("保存实例 %s 失败: %v", name, err)
	}
	saved, err := h.mgr.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("读取实例 %s 失败: %v", name, err)
	}
	return saved
}

func (h *instanceCascadeHarness) addRule(t *testing.T, platform, instanceID string) {
	t.Helper()
	rule := agentrouter.Rule{Platform: platform, InstanceID: instanceID, AgentName: "tutor"}
	if err := h.agentStore.SaveRule(context.Background(), &rule); err != nil {
		t.Fatalf("持久化规则失败: %v", err)
	}
	if err := h.dispatcher.AddRule(rule); err != nil {
		t.Fatalf("内存加规则失败: %v", err)
	}
}

// 双端（内存 Dispatcher + SQLite Store）各数一遍指定 (platform, instance_id) 的规则条数，
// 必须一致；不一致=内存/持久化漂移，本身就是 bug。
func (h *instanceCascadeHarness) countRules(t *testing.T, platform, instanceID string) int {
	t.Helper()
	mem := 0
	for _, r := range h.dispatcher.ListRules() {
		if r.Platform == platform && r.InstanceID == instanceID {
			mem++
		}
	}
	stored, err := h.agentStore.LoadRules(context.Background())
	if err != nil {
		t.Fatalf("加载规则失败: %v", err)
	}
	db := 0
	for _, r := range stored {
		if r.Platform == platform && r.InstanceID == instanceID {
			db++
		}
	}
	if mem != db {
		t.Fatalf("内存(%d)与持久化(%d)规则数漂移: platform=%s instance=%s", mem, db, platform, instanceID)
	}
	return db
}

func TestBug20260703A1_DeleteInstanceCascadesInstanceRules(t *testing.T) {
	h := newInstanceCascadeHarness(t)
	h.addInstance(t, "telegram", "tg-a")
	h.addInstance(t, "telegram", "tg-b")
	h.addRule(t, "telegram", "tg-a") // tg-a 的 instance 级绑定
	h.addRule(t, "telegram", "tg-b") // tg-b 的 instance 级绑定
	h.addRule(t, "telegram", "")     // 遗留 platform 级绑定

	instA, err := h.mgr.Get(context.Background(), "tg-a")
	if err != nil {
		t.Fatalf("读取 tg-a 失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/platforms/instances/by-id/"+instA.ID, nil)
	req.SetPathValue("id", instA.ID)
	w := httptest.NewRecorder()
	h.srv.handleDeleteInstanceByID(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("删除实例期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	if n := h.countRules(t, "telegram", "tg-a"); n != 0 {
		t.Fatalf("删除实例 tg-a 后其 instance 级规则应被级联清理，仍残留 %d 条", n)
	}
	// 非最后一个实例：其它实例的规则与 platform 级遗留规则都必须保留
	if n := h.countRules(t, "telegram", "tg-b"); n != 1 {
		t.Fatalf("删除 tg-a 不应波及 tg-b 的规则，实际剩 %d 条", n)
	}
	if n := h.countRules(t, "telegram", ""); n != 1 {
		t.Fatalf("平台还有存活实例，platform 级规则不应被清，实际剩 %d 条", n)
	}
}

func TestBug20260703A1_DeleteLastInstanceCleansPlatformRules(t *testing.T) {
	h := newInstanceCascadeHarness(t)
	h.addInstance(t, "telegram", "tg-only")
	h.addInstance(t, "feishu", "fs-main") // 异平台对照组
	h.addRule(t, "telegram", "tg-only")
	h.addRule(t, "telegram", "") // 遗留 platform 级绑定
	h.addRule(t, "feishu", "")   // 异平台 platform 级规则，不得被波及

	// 走 by-name 入口，两个删除 handler 都要覆盖级联
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/platforms/instances/tg-only", nil)
	req.SetPathValue("name", "tg-only")
	w := httptest.NewRecorder()
	h.srv.handleDeleteInstance(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("删除实例期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	if n := h.countRules(t, "telegram", "tg-only"); n != 0 {
		t.Fatalf("删除最后实例后 instance 级规则应被清理，仍残留 %d 条", n)
	}
	if n := h.countRules(t, "telegram", ""); n != 0 {
		t.Fatalf("平台最后一个实例已删，platform 级遗留规则应被清理（否则重建实例静默继承旧绑定），仍残留 %d 条", n)
	}
	if n := h.countRules(t, "feishu", ""); n != 1 {
		t.Fatalf("异平台规则不得被波及，实际剩 %d 条", n)
	}
}
