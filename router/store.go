package router

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// Store 持久化 Agent 和路由规则
type Store interface {
	Init(ctx context.Context) error
	LoadAgents(ctx context.Context) (agents []AgentConfig, defaultName string, err error)
	SaveAgent(ctx context.Context, agent *AgentConfig) error
	DeleteAgent(ctx context.Context, name string) error
	SetDefault(ctx context.Context, name string) error
	LoadRules(ctx context.Context) ([]Rule, error)
	SaveRule(ctx context.Context, rule *Rule) error
	DeleteRule(ctx context.Context, id int) error
	DeleteRulesByAgent(ctx context.Context, agentName string) error
}

// SQLiteStore 基于 SQLite 的 Agent/Rule 持久化实现
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore 创建 SQLite 持久化
func NewSQLiteStore(db *sql.DB) *SQLiteStore {
	return &SQLiteStore{db: db}
}

// Init 建表（幂等）
func (s *SQLiteStore) Init(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS agents (
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
		`CREATE TABLE IF NOT EXISTS agent_rules (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			platform    TEXT NOT NULL DEFAULT '',
			instance_id TEXT NOT NULL DEFAULT '',
			user_id     TEXT NOT NULL DEFAULT '',
			chat_id     TEXT NOT NULL DEFAULT '',
			agent_name  TEXT NOT NULL REFERENCES agents(name) ON DELETE CASCADE,
			priority    INTEGER NOT NULL DEFAULT 0,
			created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_rules_agent ON agent_rules(agent_name)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("init agent store: %w", err)
		}
	}
	s.runMigrations(ctx)
	return nil
}

func (s *SQLiteStore) runMigrations(ctx context.Context) {
	stmts := []string{
		`ALTER TABLE agent_rules ADD COLUMN instance_id TEXT NOT NULL DEFAULT ''`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			if !strings.Contains(err.Error(), "duplicate") && !strings.Contains(err.Error(), "already exists") {
				log.Printf("agent router migration warning: %v (stmt: %.80s)", err, stmt)
			}
		}
	}
}

// LoadAgents 从 DB 加载全部 Agent
func (s *SQLiteStore) LoadAgents(ctx context.Context) ([]AgentConfig, string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, display_name, description, model, provider, system_prompt,
		        skills, max_tokens, temperature, metadata, is_default
		 FROM agents ORDER BY created_at`)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	var agents []AgentConfig
	var defaultName string
	for rows.Next() {
		var a AgentConfig
		var skillsJSON, metaJSON string
		var isDefault int
		if err := rows.Scan(&a.Name, &a.DisplayName, &a.Description, &a.Model,
			&a.Provider, &a.SystemPrompt, &skillsJSON, &a.MaxTokens,
			&a.Temperature, &metaJSON, &isDefault); err != nil {
			return nil, "", err
		}
		_ = json.Unmarshal([]byte(skillsJSON), &a.Skills)
		_ = json.Unmarshal([]byte(metaJSON), &a.Metadata)
		if isDefault == 1 {
			defaultName = a.Name
		}
		agents = append(agents, a)
	}
	return agents, defaultName, rows.Err()
}

// SaveAgent 插入或更新 Agent（upsert）
func (s *SQLiteStore) SaveAgent(ctx context.Context, a *AgentConfig) error {
	skillsJSON, _ := json.Marshal(a.Skills)
	if a.Skills == nil {
		skillsJSON = []byte("[]")
	}
	metaJSON, _ := json.Marshal(a.Metadata)
	if a.Metadata == nil {
		metaJSON = []byte("{}")
	}
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agents (name, display_name, description, model, provider, system_prompt,
		                     skills, max_tokens, temperature, metadata, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		    display_name=excluded.display_name, description=excluded.description,
		    model=excluded.model, provider=excluded.provider,
		    system_prompt=excluded.system_prompt, skills=excluded.skills,
		    max_tokens=excluded.max_tokens, temperature=excluded.temperature,
		    metadata=excluded.metadata, updated_at=excluded.updated_at`,
		a.Name, a.DisplayName, a.Description, a.Model, a.Provider, a.SystemPrompt,
		string(skillsJSON), a.MaxTokens, a.Temperature, string(metaJSON), now, now,
	)
	return err
}

// DeleteAgent 删除 Agent（级联删除规则）
func (s *SQLiteStore) DeleteAgent(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("agent %q 不存在", name)
	}
	return nil
}

// SetDefault 设置默认 Agent
func (s *SQLiteStore) SetDefault(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(ctx, `UPDATE agents SET is_default = 0 WHERE is_default = 1`); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE agents SET is_default = 1 WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("agent %q 不存在", name)
	}
	return tx.Commit()
}

// LoadRules 从 DB 加载全部路由规则
func (s *SQLiteStore) LoadRules(ctx context.Context) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, platform, instance_id, user_id, chat_id, agent_name, priority
		 FROM agent_rules ORDER BY priority DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ID, &r.Platform, &r.InstanceID, &r.UserID,
			&r.ChatID, &r.AgentName, &r.Priority); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// SaveRule 插入规则，回写 rule.ID
func (s *SQLiteStore) SaveRule(ctx context.Context, rule *Rule) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_rules (platform, instance_id, user_id, chat_id, agent_name, priority)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		rule.Platform, rule.InstanceID, rule.UserID, rule.ChatID, rule.AgentName, rule.Priority,
	)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	rule.ID = int(id)
	return nil
}

// DeleteRule 删除单条规则
func (s *SQLiteStore) DeleteRule(ctx context.Context, id int) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agent_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("规则 ID=%d 不存在", id)
	}
	return nil
}

// DeleteRulesByAgent 删除指定 Agent 的所有规则
func (s *SQLiteStore) DeleteRulesByAgent(ctx context.Context, agentName string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_rules WHERE agent_name = ?`, agentName)
	return err
}

// Sync 将 Dispatcher 当前内存状态全量同步到 DB（用于从配置文件初始化）
func Sync(ctx context.Context, store Store, d *Dispatcher) error {
	agents := d.ListAgents()
	for i := range agents {
		if err := store.SaveAgent(ctx, &agents[i]); err != nil {
			log.Printf("sync agent %q: %v", agents[i].Name, err)
		}
	}
	defaultName := d.DefaultAgent()
	if defaultName != "" {
		_ = store.SetDefault(ctx, defaultName)
	}
	for _, r := range d.ListRules() {
		rule := r
		if err := store.SaveRule(ctx, &rule); err != nil {
			log.Printf("sync rule for %q: %v", r.AgentName, err)
		}
	}
	return nil
}
