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

func TestBUG20260710_IMRebindReplacesRouteAndPersistentRule(t *testing.T) {
	ctx := context.Background()
	binder, dispatcher, store, _ := newIMBinderFixture(t)
	if err := binder.Bind(ctx, "dingtalk", "bot-1", "family", "child-a"); err != nil {
		t.Fatal(err)
	}
	if err := binder.Bind(ctx, "dingtalk", "bot-1", "family", "child-b"); err != nil {
		t.Fatal(err)
	}

	got := dispatcher.Route(agentrouter.RouteRequest{Platform: "dingtalk", InstanceID: "bot-1", ChatID: "family"})
	if got == nil || got.Rule == nil || got.AgentName != "child-b" {
		t.Fatalf("重绑后必须路由到 child-b，got=%+v", got)
	}
	rules, err := store.LoadRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].AgentName != "child-b" {
		t.Fatalf("同一 IM scope 必须仅持久化最新一条规则，got=%+v", rules)
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
