package router

import (
	"context"
	"testing"

	_ "modernc.org/sqlite"
)

// newInitStore returns a fully initialized SQLiteStore backed by an in-memory DB.
func newInitStore(t *testing.T) (*SQLiteStore, context.Context) {
	t.Helper()
	ctx := context.Background()
	db := openMemDB(t)
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	return store, ctx
}

// TestStore_Init_Idempotent verifies that calling Init twice on the same DB is safe.
func TestStore_Init_Idempotent(t *testing.T) {
	store, ctx := newInitStore(t)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("second Init must be idempotent, got: %v", err)
	}
}

// TestStore_SaveAndLoadAgents covers SaveAgent (insert + upsert) and LoadAgents round-trip.
func TestStore_SaveAndLoadAgents(t *testing.T) {
	store, ctx := newInitStore(t)

	t.Run("insert_then_load_roundtrip", func(t *testing.T) {
		a := &AgentConfig{
			Name:         "research",
			DisplayName:  "Research Bot",
			Description:  "does research",
			Model:        "gpt-4o",
			Provider:     "openai",
			SystemPrompt: "be helpful",
			Skills:       []string{"search", "summarize"},
			MaxTokens:    2048,
			Temperature:  0.7,
			Metadata:     map[string]string{"team": "core"},
		}
		if err := store.SaveAgent(ctx, a); err != nil {
			t.Fatalf("SaveAgent: %v", err)
		}
		agents, def, err := store.LoadAgents(ctx)
		if err != nil {
			t.Fatalf("LoadAgents: %v", err)
		}
		if len(agents) != 1 {
			t.Fatalf("expected 1 agent, got %d", len(agents))
		}
		if def != "" {
			t.Fatalf("expected no default before SetDefault, got %q", def)
		}
		got := agents[0]
		if got.Name != a.Name || got.DisplayName != a.DisplayName ||
			got.Model != a.Model || got.Provider != a.Provider ||
			got.SystemPrompt != a.SystemPrompt || got.MaxTokens != a.MaxTokens ||
			got.Temperature != a.Temperature {
			t.Fatalf("scalar fields not round-tripped: %+v", got)
		}
		if len(got.Skills) != 2 || got.Skills[0] != "search" || got.Skills[1] != "summarize" {
			t.Fatalf("skills JSON not round-tripped: %+v", got.Skills)
		}
		if got.Metadata["team"] != "core" {
			t.Fatalf("metadata JSON not round-tripped: %+v", got.Metadata)
		}
	})

	t.Run("save_same_name_upserts_not_duplicates", func(t *testing.T) {
		a := &AgentConfig{Name: "research", DisplayName: "Updated Name", Model: "gpt-4o-mini"}
		if err := store.SaveAgent(ctx, a); err != nil {
			t.Fatalf("SaveAgent upsert: %v", err)
		}
		agents, _, err := store.LoadAgents(ctx)
		if err != nil {
			t.Fatalf("LoadAgents: %v", err)
		}
		if len(agents) != 1 {
			t.Fatalf("upsert should keep 1 row, got %d", len(agents))
		}
		if agents[0].DisplayName != "Updated Name" || agents[0].Model != "gpt-4o-mini" {
			t.Fatalf("upsert did not overwrite fields: %+v", agents[0])
		}
	})
}

// TestStore_SaveAgent_NilSlicesDefaultToEmptyJSON verifies nil Skills/Metadata
// are persisted as empty JSON containers rather than producing NULL/parse errors.
func TestStore_SaveAgent_NilSlicesDefaultToEmptyJSON(t *testing.T) {
	store, ctx := newInitStore(t)
	a := &AgentConfig{Name: "bare"} // Skills and Metadata are nil
	if err := store.SaveAgent(ctx, a); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}
	agents, _, err := store.LoadAgents(ctx)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if len(agents[0].Skills) != 0 {
		t.Fatalf("nil Skills should load as empty, got %+v", agents[0].Skills)
	}
	if len(agents[0].Metadata) != 0 {
		t.Fatalf("nil Metadata should load as empty, got %+v", agents[0].Metadata)
	}
}

// TestStore_DeleteAgent covers both the success path and the not-found error path.
func TestStore_DeleteAgent(t *testing.T) {
	store, ctx := newInitStore(t)
	if err := store.SaveAgent(ctx, &AgentConfig{Name: "a1"}); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}

	t.Run("delete_existing", func(t *testing.T) {
		if err := store.DeleteAgent(ctx, "a1"); err != nil {
			t.Fatalf("DeleteAgent: %v", err)
		}
		agents, _, err := store.LoadAgents(ctx)
		if err != nil {
			t.Fatalf("LoadAgents: %v", err)
		}
		if len(agents) != 0 {
			t.Fatalf("expected 0 agents after delete, got %d", len(agents))
		}
	})

	t.Run("delete_missing_returns_error", func(t *testing.T) {
		if err := store.DeleteAgent(ctx, "ghost"); err == nil {
			t.Fatal("deleting a missing agent should return an error")
		}
	})
}

// TestStore_DeleteAgent_CascadesRules verifies the ON DELETE CASCADE foreign key
// removes the agent's rules when the agent is deleted.
func TestStore_DeleteAgent_CascadesRules(t *testing.T) {
	store, ctx := newInitStore(t)
	if err := store.SaveAgent(ctx, &AgentConfig{Name: "a1"}); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}
	r := &Rule{Platform: "feishu", AgentName: "a1"}
	if err := store.SaveRule(ctx, r); err != nil {
		t.Fatalf("SaveRule: %v", err)
	}
	if err := store.DeleteAgent(ctx, "a1"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	rules, err := store.LoadRules(ctx)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("expected rules to cascade-delete with agent, got %d", len(rules))
	}
}

// TestStore_SetDefault covers marking a default agent and the not-found error path.
func TestStore_SetDefault(t *testing.T) {
	store, ctx := newInitStore(t)
	for _, n := range []string{"a1", "a2"} {
		if err := store.SaveAgent(ctx, &AgentConfig{Name: n}); err != nil {
			t.Fatalf("SaveAgent %q: %v", n, err)
		}
	}

	t.Run("set_existing_default", func(t *testing.T) {
		if err := store.SetDefault(ctx, "a2"); err != nil {
			t.Fatalf("SetDefault: %v", err)
		}
		_, def, err := store.LoadAgents(ctx)
		if err != nil {
			t.Fatalf("LoadAgents: %v", err)
		}
		if def != "a2" {
			t.Fatalf("expected default a2, got %q", def)
		}
	})

	t.Run("switch_default_clears_previous", func(t *testing.T) {
		if err := store.SetDefault(ctx, "a1"); err != nil {
			t.Fatalf("SetDefault: %v", err)
		}
		_, def, err := store.LoadAgents(ctx)
		if err != nil {
			t.Fatalf("LoadAgents: %v", err)
		}
		if def != "a1" {
			t.Fatalf("expected exactly one default a1, got %q", def)
		}
	})

	t.Run("set_missing_returns_error", func(t *testing.T) {
		if err := store.SetDefault(ctx, "ghost"); err == nil {
			t.Fatal("SetDefault on missing agent should return an error")
		}
	})
}

// TestStore_SaveAndLoadRules covers SaveRule ID write-back, ordering, and LoadRules.
func TestStore_SaveAndLoadRules(t *testing.T) {
	store, ctx := newInitStore(t)
	if err := store.SaveAgent(ctx, &AgentConfig{Name: "a1"}); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}

	t.Run("save_writes_back_id", func(t *testing.T) {
		r := &Rule{Platform: "feishu", AgentName: "a1", Priority: 5}
		if err := store.SaveRule(ctx, r); err != nil {
			t.Fatalf("SaveRule: %v", err)
		}
		if r.ID <= 0 {
			t.Fatalf("SaveRule should write back a positive ID, got %d", r.ID)
		}
	})

	t.Run("conflict_upsert_takes_max_priority_and_returns_id", func(t *testing.T) {
		// Same 5-tuple, lower priority: ON CONFLICT keeps MAX(priority).
		dup := &Rule{Platform: "feishu", AgentName: "a1", Priority: 1}
		if err := store.SaveRule(ctx, dup); err != nil {
			t.Fatalf("SaveRule conflict: %v", err)
		}
		if dup.ID <= 0 {
			t.Fatalf("conflict path must back-fill ID, got %d", dup.ID)
		}
		rules, err := store.LoadRules(ctx)
		if err != nil {
			t.Fatalf("LoadRules: %v", err)
		}
		if len(rules) != 1 {
			t.Fatalf("conflict should not add a row, got %d rules", len(rules))
		}
		if rules[0].Priority != 5 {
			t.Fatalf("MAX(priority) should retain 5, got %d", rules[0].Priority)
		}
	})

	t.Run("load_orders_by_priority_desc", func(t *testing.T) {
		// Add a higher-priority rule; LoadRules orders by priority DESC.
		hi := &Rule{Platform: "discord", AgentName: "a1", Priority: 100}
		if err := store.SaveRule(ctx, hi); err != nil {
			t.Fatalf("SaveRule: %v", err)
		}
		rules, err := store.LoadRules(ctx)
		if err != nil {
			t.Fatalf("LoadRules: %v", err)
		}
		if len(rules) < 2 {
			t.Fatalf("expected >=2 rules, got %d", len(rules))
		}
		if rules[0].Priority < rules[1].Priority {
			t.Fatalf("rules not ordered by priority DESC: %+v", rules)
		}
	})
}

// TestStore_DeleteRule covers single-rule deletion success and not-found error.
func TestStore_DeleteRule(t *testing.T) {
	store, ctx := newInitStore(t)
	if err := store.SaveAgent(ctx, &AgentConfig{Name: "a1"}); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}
	r := &Rule{Platform: "feishu", AgentName: "a1"}
	if err := store.SaveRule(ctx, r); err != nil {
		t.Fatalf("SaveRule: %v", err)
	}

	t.Run("delete_existing", func(t *testing.T) {
		if err := store.DeleteRule(ctx, r.ID); err != nil {
			t.Fatalf("DeleteRule: %v", err)
		}
		rules, err := store.LoadRules(ctx)
		if err != nil {
			t.Fatalf("LoadRules: %v", err)
		}
		if len(rules) != 0 {
			t.Fatalf("expected 0 rules after delete, got %d", len(rules))
		}
	})

	t.Run("delete_missing_returns_error", func(t *testing.T) {
		if err := store.DeleteRule(ctx, 999999); err == nil {
			t.Fatal("deleting a missing rule should return an error")
		}
	})
}

// TestStore_DeleteRulesByAgent removes all rules for an agent regardless of count.
func TestStore_DeleteRulesByAgent(t *testing.T) {
	store, ctx := newInitStore(t)
	for _, n := range []string{"a1", "a2"} {
		if err := store.SaveAgent(ctx, &AgentConfig{Name: n}); err != nil {
			t.Fatalf("SaveAgent %q: %v", n, err)
		}
	}
	rules := []Rule{
		{Platform: "feishu", AgentName: "a1"},
		{Platform: "discord", AgentName: "a1"},
		{Platform: "feishu", AgentName: "a2"},
	}
	for i := range rules {
		if err := store.SaveRule(ctx, &rules[i]); err != nil {
			t.Fatalf("SaveRule %d: %v", i, err)
		}
	}

	if err := store.DeleteRulesByAgent(ctx, "a1"); err != nil {
		t.Fatalf("DeleteRulesByAgent: %v", err)
	}
	remaining, err := store.LoadRules(ctx)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(remaining) != 1 || remaining[0].AgentName != "a2" {
		t.Fatalf("expected only a2's rule to remain, got %+v", remaining)
	}

	// Deleting for an agent with no rules is a no-op (not an error).
	if err := store.DeleteRulesByAgent(ctx, "nobody"); err != nil {
		t.Fatalf("DeleteRulesByAgent on empty set should be a no-op, got: %v", err)
	}
}

// TestSync_PersistsDispatcherState verifies Sync writes agents, default, and rules
// from an in-memory Dispatcher into the Store, and that they reload identically.
func TestSync_PersistsDispatcherState(t *testing.T) {
	store, ctx := newInitStore(t)

	d := New()
	if err := d.Register(AgentConfig{Name: "a1", DisplayName: "First"}); err != nil {
		t.Fatalf("Register a1: %v", err)
	}
	if err := d.Register(AgentConfig{Name: "a2"}); err != nil {
		t.Fatalf("Register a2: %v", err)
	}
	if err := d.SetDefault("a2"); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	if err := d.AddRule(Rule{Platform: "feishu", AgentName: "a1", Priority: 3}); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	if err := Sync(ctx, store, d); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	agents, def, err := store.LoadAgents(ctx)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected 2 synced agents, got %d", len(agents))
	}
	if def != "a2" {
		t.Fatalf("expected synced default a2, got %q", def)
	}
	rules, err := store.LoadRules(ctx)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(rules) != 1 || rules[0].AgentName != "a1" || rules[0].Priority != 3 {
		t.Fatalf("expected one synced rule for a1 with priority 3, got %+v", rules)
	}
}

// TestSync_Idempotent verifies repeated Sync of the same state does not multiply
// rows (the regression behind the 2^n rule explosion).
func TestSync_Idempotent(t *testing.T) {
	store, ctx := newInitStore(t)
	d := New()
	if err := d.Register(AgentConfig{Name: "a1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := d.AddRule(Rule{Platform: "feishu", AgentName: "a1"}); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	for i := 0; i < 5; i++ {
		if err := Sync(ctx, store, d); err != nil {
			t.Fatalf("Sync iter %d: %v", i, err)
		}
	}
	rules, err := store.LoadRules(ctx)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("repeated Sync must stay at 1 rule, got %d", len(rules))
	}
	agents, _, err := store.LoadAgents(ctx)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("repeated Sync must stay at 1 agent, got %d", len(agents))
	}
}

// TestStore_LoadAndDispatcher_RoundTrip exercises the full persistence loop:
// build a Dispatcher, Sync it, reload from the store, LoadAll into a fresh
// Dispatcher, and confirm routing behaves identically.
func TestStore_LoadAndDispatcher_RoundTrip(t *testing.T) {
	store, ctx := newInitStore(t)
	src := New()
	if err := src.Register(AgentConfig{Name: "support"}); err != nil {
		t.Fatalf("Register support: %v", err)
	}
	if err := src.Register(AgentConfig{Name: "vip"}); err != nil {
		t.Fatalf("Register vip: %v", err)
	}
	if err := src.SetDefault("support"); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	if err := src.AddRule(Rule{UserID: "u-boss", AgentName: "vip", Priority: 1}); err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	if err := Sync(ctx, store, src); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	agents, def, err := store.LoadAgents(ctx)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	rules, err := store.LoadRules(ctx)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}

	dst := New()
	dst.LoadAll(agents, def, rules)

	if got := dst.Route(RouteRequest{UserID: "u-boss"}); got == nil || got.AgentName != "vip" {
		t.Fatalf("reloaded dispatcher should route u-boss to vip, got %+v", got)
	}
	if got := dst.Route(RouteRequest{UserID: "stranger"}); got == nil || got.AgentName != "support" {
		t.Fatalf("reloaded dispatcher should fall back to default support, got %+v", got)
	}
}
