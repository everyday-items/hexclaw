package router

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	_ "modernc.org/sqlite"
)

func routerReasoningPolicy(mode config.ReasoningPolicyMode, effort config.ReasoningEffort) *config.ReasoningPolicy {
	return &config.ReasoningPolicy{Mode: mode, Effort: effort}
}

func TestStoreReasoningPolicyCreateUpdateReloadRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "agents.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	agent := &AgentConfig{
		Name:            "planner",
		DisplayName:     "Planner",
		ReasoningPolicy: routerReasoningPolicy(config.ReasoningPolicyModeEffort, config.ReasoningEffortHigh),
	}
	if err := store.SaveAgent(ctx, agent); err != nil {
		t.Fatalf("create: %v", err)
	}
	agents, _, err := store.LoadAgents(ctx)
	if err != nil {
		t.Fatalf("load after create: %v", err)
	}
	if len(agents) != 1 || agents[0].ReasoningPolicy == nil ||
		*agents[0].ReasoningPolicy != *agent.ReasoningPolicy {
		t.Fatalf("create round-trip = %+v, want %+v", agents, agent.ReasoningPolicy)
	}

	agent.ReasoningPolicy = routerReasoningPolicy(config.ReasoningPolicyModeOff, "")
	if err := store.SaveAgent(ctx, agent); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close before reload: %v", err)
	}

	reopened, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reloadedStore := NewSQLiteStore(reopened)
	if err := reloadedStore.Init(ctx); err != nil {
		t.Fatalf("Init after reopen: %v", err)
	}
	agents, _, err = reloadedStore.LoadAgents(ctx)
	if err != nil {
		t.Fatalf("load after reopen: %v", err)
	}
	if len(agents) != 1 || agents[0].ReasoningPolicy == nil ||
		agents[0].ReasoningPolicy.Mode != config.ReasoningPolicyModeOff || agents[0].ReasoningPolicy.Effort != "" {
		t.Fatalf("reload policy = %+v, want mode=off", agents)
	}
}

func TestStoreReasoningPolicyRejectsInvalidCombinations(t *testing.T) {
	store, ctx := newInitStore(t)
	tests := []config.ReasoningPolicy{
		{Mode: config.ReasoningPolicyModeEffort},
		{Mode: config.ReasoningPolicyModeOn, Effort: config.ReasoningEffortHigh},
		{Mode: config.ReasoningPolicyMode("invalid")},
		{Mode: config.ReasoningPolicyModeEffort, Effort: config.ReasoningEffort("extreme")},
	}
	for i := range tests {
		err := store.SaveAgent(ctx, &AgentConfig{
			Name:            "invalid",
			ReasoningPolicy: &tests[i],
		})
		if err == nil {
			t.Fatalf("invalid policy %+v unexpectedly persisted", tests[i])
		}
	}
	agents, _, err := store.LoadAgents(ctx)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("invalid policy produced rows: %+v", agents)
	}
}

func TestStoreReasoningPolicyMigratesLegacyDatabaseWithoutFieldLoss(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	statements := []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE agents (
			name TEXT PRIMARY KEY,
			display_name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '',
			system_prompt TEXT NOT NULL DEFAULT '',
			skills TEXT NOT NULL DEFAULT '[]',
			max_tokens INTEGER NOT NULL DEFAULT 0,
			temperature REAL NOT NULL DEFAULT 0,
			metadata TEXT NOT NULL DEFAULT '{}',
			is_default INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE agent_rules (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			platform TEXT NOT NULL DEFAULT '',
			instance_id TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			chat_id TEXT NOT NULL DEFAULT '',
			agent_name TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
			priority INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO agents
			(name, display_name, description, model, provider, system_prompt, skills,
			 max_tokens, temperature, metadata, is_default)
		 VALUES
			('legacy', 'Legacy', 'preserve me', 'model-a', 'provider-a', 'prompt-a',
			 '["search"]', 4096, 0.7, '{"team":"core"}', 1)`,
		`INSERT INTO agent_rules (platform, agent_name, priority) VALUES ('web', 'legacy', 9)`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare legacy DB: %v\n%s", err, statement)
		}
	}

	store := NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("migrate legacy DB: %v", err)
	}
	agents, defaultName, err := store.LoadAgents(ctx)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(agents))
	}
	got := agents[0]
	if got.Name != "legacy" || got.DisplayName != "Legacy" || got.Description != "preserve me" ||
		got.Model != "model-a" || got.Provider != "provider-a" || got.SystemPrompt != "prompt-a" ||
		got.MaxTokens != 4096 || got.Temperature == nil || *got.Temperature != 0.7 ||
		len(got.Skills) != 1 || got.Skills[0] != "search" || got.Metadata["team"] != "core" ||
		defaultName != "legacy" {
		t.Fatalf("legacy fields drifted after migration: agent=%+v default=%q", got, defaultName)
	}
	if got.ReasoningPolicy == nil || got.ReasoningPolicy.Mode != config.ReasoningPolicyModeInherit ||
		got.ReasoningPolicy.Effort != "" {
		t.Fatalf("legacy reasoning policy = %+v, want inherit", got.ReasoningPolicy)
	}
	rules, err := store.LoadRules(ctx)
	if err != nil || len(rules) != 1 || rules[0].AgentName != "legacy" || rules[0].Priority != 9 {
		t.Fatalf("legacy rules drifted after migration: rules=%+v err=%v", rules, err)
	}
}

func TestDispatcherReasoningPolicyDefaultsLegacyAgentsToInherit(t *testing.T) {
	dispatcher := New()
	if err := dispatcher.Register(AgentConfig{Name: "legacy"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := dispatcher.GetAgent("legacy")
	if !ok || got.ReasoningPolicy == nil || got.ReasoningPolicy.Mode != config.ReasoningPolicyModeInherit {
		t.Fatalf("legacy agent policy = %+v, want inherit", got)
	}
}
