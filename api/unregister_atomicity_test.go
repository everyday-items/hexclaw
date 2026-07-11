package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

func TestHandleUnregisterAgentDefaultReassignmentFailureRollsBackEverything(t *testing.T) {
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "atomic-unregister.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	agentStore := agentrouter.NewSQLiteStore(store.DB())
	if err := agentStore.Init(ctx); err != nil {
		t.Fatal(err)
	}

	dispatcher := agentrouter.New()
	for _, name := range []string{"beta", "alpha"} {
		cfg := agentrouter.AgentConfig{Name: name}
		if err := dispatcher.Register(cfg); err != nil {
			t.Fatal(err)
		}
		if err := agentStore.SaveAgent(ctx, &cfg); err != nil {
			t.Fatal(err)
		}
	}
	if err := agentStore.SetDefault(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	rule := agentrouter.Rule{Platform: "web", AgentName: "beta"}
	if err := agentStore.SaveRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.AddRule(rule); err != nil {
		t.Fatal(err)
	}

	// Force the second durable step (new-default publication) to fail after
	// DELETE has executed. The transaction must restore the deleted Agent and
	// its cascaded rules.
	if _, err := store.DB().ExecContext(ctx, `
		CREATE TRIGGER fail_default_reassignment
		BEFORE UPDATE OF is_default ON agents
		WHEN NEW.name = 'alpha' AND NEW.is_default = 1
		BEGIN
			SELECT RAISE(ABORT, 'injected default reassignment failure');
		END
	`); err != nil {
		t.Fatal(err)
	}

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, store)
	srv.SetAgentRouter(dispatcher)
	srv.SetAgentStore(agentStore)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/beta", nil)
	req.SetPathValue("name", "beta")
	w := httptest.NewRecorder()
	srv.handleUnregisterAgent(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", w.Code, w.Body.String())
	}
	if _, ok := dispatcher.GetAgent("beta"); !ok {
		t.Fatal("failed transaction published in-memory Agent deletion")
	}
	if got := dispatcher.DefaultAgent(); got != "beta" {
		t.Fatalf("failed transaction published in-memory default=%q", got)
	}
	if rules := dispatcher.ListRules(); len(rules) != 1 || rules[0].AgentName != "beta" {
		t.Fatalf("failed transaction published in-memory rule deletion: %+v", rules)
	}

	agents, storedDefault, err := agentStore.LoadAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundBeta := false
	for _, agent := range agents {
		foundBeta = foundBeta || agent.Name == "beta"
	}
	if !foundBeta || storedDefault != "beta" {
		t.Fatalf("durable rollback incomplete: agents=%+v default=%q", agents, storedDefault)
	}
	rules, err := agentStore.LoadRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].AgentName != "beta" {
		t.Fatalf("durable rule rollback incomplete: %+v", rules)
	}
}
