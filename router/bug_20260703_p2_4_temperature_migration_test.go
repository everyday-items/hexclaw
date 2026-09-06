package router

// BUG-20260703 P2-4：AgentConfig.Temperature 指针化（nil=未设跟随模型默认，显式 0=
// 确定性采样）。旧 schema agents.temperature REAL NOT NULL DEFAULT 0 无法表达 nil，
// Init 走官方表重建迁移：列改可空 + 历史 0（旧 `>0` 判定语义下即「未设」）回填 NULL。
// 用真实文件 DB（:memory: 在连接池下每连接一个库，迁移的单连接语义测不真）。

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openLegacySchemaDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	// 旧版 schema：temperature NOT NULL DEFAULT 0
	stmts := []string{
		`CREATE TABLE agents (
			name         TEXT PRIMARY KEY,
			display_name TEXT NOT NULL DEFAULT '',
			description  TEXT NOT NULL DEFAULT '',
			model        TEXT NOT NULL DEFAULT '',
			provider     TEXT NOT NULL DEFAULT '',
			system_prompt TEXT NOT NULL DEFAULT '',
			skills       TEXT NOT NULL DEFAULT '[]',
			max_tokens   INTEGER NOT NULL DEFAULT 0,
			temperature  REAL NOT NULL DEFAULT 0,
			metadata     TEXT NOT NULL DEFAULT '{}',
			is_default   INTEGER NOT NULL DEFAULT 0,
			created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE agent_rules (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			platform    TEXT NOT NULL DEFAULT '',
			instance_id TEXT NOT NULL DEFAULT '',
			user_id     TEXT NOT NULL DEFAULT '',
			chat_id     TEXT NOT NULL DEFAULT '',
			agent_name  TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
			priority    INTEGER NOT NULL DEFAULT 0,
			created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO agents (name, temperature) VALUES ('unset-agent', 0)`,
		`INSERT INTO agents (name, temperature) VALUES ('warm-agent', 0.7)`,
		`INSERT INTO agent_rules (platform, agent_name) VALUES ('telegram', 'warm-agent')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("准备旧 schema 失败: %v\n%s", err, stmt)
		}
	}
	return db
}

func TestBug20260703P24_TemperatureMigrationRebuildsColumn(t *testing.T) {
	db := openLegacySchemaDB(t)
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER agent_metadata_observer
		AFTER UPDATE OF metadata ON agents BEGIN
			UPDATE agent_rules SET priority=priority+1 WHERE agent_name=NEW.name;
		END`); err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init（含迁移）失败: %v", err)
	}

	// 列已可空
	var notNull int
	if err := db.QueryRowContext(ctx,
		`SELECT "notnull" FROM pragma_table_info('agents') WHERE name = 'temperature'`,
	).Scan(&notNull); err != nil {
		t.Fatalf("读列元数据失败: %v", err)
	}
	if notNull != 0 {
		t.Fatal("迁移后 temperature 列应可空")
	}

	// 历史 0 → NULL（未设）；非 0 值原样保留
	agents, _, err := store.LoadAgents(ctx)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	byName := map[string]*float64{}
	for _, a := range agents {
		byName[a.Name] = a.Temperature
	}
	if byName["unset-agent"] != nil {
		t.Fatalf("历史 temperature=0（旧语义未设）应迁移为 nil，实际 %v", *byName["unset-agent"])
	}
	if byName["warm-agent"] == nil || *byName["warm-agent"] != 0.7 {
		t.Fatalf("历史 temperature=0.7 应原样保留，实际 %v", byName["warm-agent"])
	}

	// 迁移不丢规则，且 FK 级联在重建后依然生效
	rules, err := store.LoadRules(ctx)
	if err != nil || len(rules) != 1 {
		t.Fatalf("迁移后规则应完好: err=%v rules=%d", err, len(rules))
	}
	if _, err := db.ExecContext(ctx, `UPDATE agents SET metadata='{}' WHERE name='warm-agent'`); err != nil {
		t.Fatal(err)
	}
	rules, err = store.LoadRules(ctx)
	if err != nil || len(rules) != 1 || rules[0].Priority != 1 {
		t.Fatalf("table rebuild lost the original metadata trigger: rules=%+v err=%v", rules, err)
	}
	if err := store.DeleteAgent(ctx, "warm-agent"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	rules, _ = store.LoadRules(ctx)
	if len(rules) != 0 {
		t.Fatal("表重建后 agent_rules 的 FK ON DELETE CASCADE 应依然生效")
	}

	// 幂等：再跑一次 Init 不报错不重迁移
	if err := store.Init(ctx); err != nil {
		t.Fatalf("二次 Init 应幂等: %v", err)
	}
}

func TestBug20260703P24_ExplicitZeroTemperatureRoundTrips(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "zero.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	zero := 0.0
	if err := store.SaveAgent(ctx, &AgentConfig{Name: "deterministic", Temperature: &zero}); err != nil {
		t.Fatalf("SaveAgent(0): %v", err)
	}
	if err := store.SaveAgent(ctx, &AgentConfig{Name: "default-follow"}); err != nil {
		t.Fatalf("SaveAgent(nil): %v", err)
	}

	agents, _, err := store.LoadAgents(ctx)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	byName := map[string]*float64{}
	for _, a := range agents {
		byName[a.Name] = a.Temperature
	}
	if byName["deterministic"] == nil || *byName["deterministic"] != 0 {
		t.Fatalf("显式 0 应往返为非 nil 的 0，实际 %v", byName["deterministic"])
	}
	if byName["default-follow"] != nil {
		t.Fatalf("未设应往返为 nil，实际 %v", *byName["default-follow"])
	}
}
