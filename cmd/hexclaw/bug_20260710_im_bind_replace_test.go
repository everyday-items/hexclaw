package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	agentrouter "github.com/hexagon-codes/hexclaw/router"

	_ "modernc.org/sqlite"
)

func newIMBinderFixture(t *testing.T) (*k12IMBinder, *agentrouter.Dispatcher, *agentrouter.SQLiteStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := agentrouter.NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	dispatcher := agentrouter.New()
	for _, name := range []string{"default", "child-a", "child-b"} {
		if err := dispatcher.Register(agentrouter.AgentConfig{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	return &k12IMBinder{router: dispatcher, store: store}, dispatcher, store, db
}

// 2026-07-18 §3.12 限绑裁决升级：同一私聊目标不再允许直接重绑到另一个实例
// （原「重绑即替换」语义废止，防入站照片归属歧义）；本测试保留 BUG-20260710 的
// 核心不变量——同一 IM scope 永远只有一条规则、内存路由与持久化一致——
// 但换绑现在必须先解绑（拒绝路径契约见 im_bind_exclusive_test.go）。
func TestBUG20260710_IMRebindSameScopeKeepsSingleRule(t *testing.T) {
	ctx := context.Background()
	binder, dispatcher, store, _ := newIMBinderFixture(t)
	if err := binder.Bind(ctx, "dingtalk", "bot-1", "family", "child-a"); err != nil {
		t.Fatal(err)
	}
	// §3.12 限绑：同 scope 换绑另一个孩子被拒绝，原绑定保持。
	if err := binder.Bind(ctx, "dingtalk", "bot-1", "family", "child-b"); err == nil {
		t.Fatal("同一私聊目标换绑另一实例必须被拒绝（§3.12 限绑）")
	}

	got := dispatcher.Route(agentrouter.RouteRequest{Platform: "dingtalk", InstanceID: "bot-1", ChatID: "family"})
	if got == nil || got.Rule == nil || got.AgentName != "child-a" {
		t.Fatalf("拒绝换绑后必须仍路由到 child-a，got=%+v", got)
	}
	rules, err := store.LoadRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].AgentName != "child-a" {
		t.Fatalf("同一 IM scope 必须仅持久化一条规则且保持原绑定，got=%+v", rules)
	}
}

func TestBUG20260710_IMBindPersistenceFailureDoesNotPublishMemoryRule(t *testing.T) {
	ctx := context.Background()
	binder, dispatcher, _, db := newIMBinderFixture(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := binder.Bind(ctx, "dingtalk", "bot-1", "family", "child-a"); err == nil {
		t.Fatal("持久化失败必须向上返回错误")
	}

	got := dispatcher.Route(agentrouter.RouteRequest{Platform: "dingtalk", InstanceID: "bot-1", ChatID: "family"})
	if got != nil && got.Rule != nil {
		t.Fatalf("持久化失败不得发布幽灵内存规则，got=%+v", got)
	}
}

func TestBUG20260710_IMBindUnknownAgentDoesNotCorruptPersistentRule(t *testing.T) {
	ctx := context.Background()
	binder, dispatcher, store, _ := newIMBinderFixture(t)
	if err := binder.Bind(ctx, "dingtalk", "bot-1", "family", "child-a"); err != nil {
		t.Fatal(err)
	}
	if err := binder.Bind(ctx, "dingtalk", "bot-1", "family", "missing-child"); err == nil {
		t.Fatal("绑定未注册 Agent 必须失败")
	}

	got := dispatcher.Route(agentrouter.RouteRequest{Platform: "dingtalk", InstanceID: "bot-1", ChatID: "family"})
	if got == nil || got.Rule == nil || got.AgentName != "child-a" {
		t.Fatalf("无效重绑后内存路由必须保持 child-a，got=%+v", got)
	}
	rules, err := store.LoadRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].AgentName != "child-a" {
		t.Fatalf("无效重绑不得先污染持久化规则，got=%+v", rules)
	}
}
