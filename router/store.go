package router

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/hexagon-codes/toolkit/util/logger"
	"log/slog"
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
	DeleteRulesByInstance(ctx context.Context, platform, instanceID string) error
	DeleteRulesByPlatform(ctx context.Context, platform string) error
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
	// 顺序很关键：先创建基础表 → 去重历史数据 → 再创建 UNIQUE 索引
	// 否则对已有重复数据的库升级时，UNIQUE 索引会因冲突创建失败
	base := []string{
		`CREATE TABLE IF NOT EXISTS agents (
			name         TEXT PRIMARY KEY,
			display_name TEXT NOT NULL DEFAULT '',
			description  TEXT NOT NULL DEFAULT '',
			model        TEXT NOT NULL DEFAULT '',
			provider     TEXT NOT NULL DEFAULT '',
			system_prompt TEXT NOT NULL DEFAULT '',
			skills       TEXT NOT NULL DEFAULT '[]',
			max_tokens   INTEGER NOT NULL DEFAULT 0,
			temperature  REAL,
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
	for _, stmt := range base {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("init agent store: %w", err)
		}
	}
	s.runMigrations(ctx)
	s.migrateAgentsTemperatureNullable(ctx)
	// 必须在 UNIQUE 索引创建之前去重
	s.dedupeAgentRules(ctx)
	if _, err := s.db.ExecContext(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_rules_unique
			ON agent_rules(platform, instance_id, user_id, chat_id, agent_name)`); err != nil {
		return fmt.Errorf("create unique index: %w", err)
	}
	return nil
}

func (s *SQLiteStore) runMigrations(ctx context.Context) {
	stmts := []string{
		`ALTER TABLE agent_rules ADD COLUMN instance_id TEXT NOT NULL DEFAULT ''`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			if !strings.Contains(err.Error(), "duplicate") && !strings.Contains(err.Error(), "already exists") {
				logger.Warn("agent router migration warning", "warning", err, "stmt", stmt)
			}
		}
	}
	// dedupeAgentRules 的调用已移到 Init 中，保证时序：dedup 先于 UNIQUE 索引创建。
}

// dedupeAgentRules 清理历史遗留的重复路由规则（见 Fix #8 背景）
// 使用 MIN(id) 保留最早一条，删除其余重复项。
func (s *SQLiteStore) dedupeAgentRules(ctx context.Context) {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM agent_rules
		WHERE id NOT IN (
			SELECT MIN(id) FROM agent_rules
			GROUP BY platform, instance_id, user_id, chat_id, agent_name
		)
	`)
	if err != nil {
		logger.Warn("agent_rules dedup warning", "error", err)
	}
}

// migrateAgentsTemperatureNullable 把 agents.temperature 从 NOT NULL DEFAULT 0 重建为可空
// （BUG-20260703 P2-4）：NULL=未设跟随模型默认、显式 0=确定性采样。历史行的 0 在旧
// `>0` 判定语义下就是「未设」→ 如实回填 NULL。SQLite 无法 ALTER 掉 NOT NULL，走官方
// 表重建流程；重建后表名不变，agent_rules 的按名 FK 引用如故。全程钉在单一连接上
// （PRAGMA foreign_keys 是连接级状态，连接池换连接会让它悄然失效）。幂等：列已可空
// 直接跳过；失败只告警不阻断启动（旧列语义继续可用）。
func (s *SQLiteStore) migrateAgentsTemperatureNullable(ctx context.Context) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		logger.Warn("temperature 迁移: 取连接失败", "error", err)
		return
	}
	defer conn.Close()

	var notNull int
	err = conn.QueryRowContext(ctx,
		`SELECT "notnull" FROM pragma_table_info('agents') WHERE name = 'temperature'`,
	).Scan(&notNull)
	if err != nil || notNull == 0 {
		return // 列缺失（异常库）或已可空：无事可做
	}

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		logger.Warn("temperature 迁移: 关闭 FK 检查失败", "error", err)
		return
	}
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()

	stmts := []string{
		`BEGIN IMMEDIATE`,
		`CREATE TABLE agents_mig_tempnull (
			name         TEXT PRIMARY KEY,
			display_name TEXT NOT NULL DEFAULT '',
			description  TEXT NOT NULL DEFAULT '',
			model        TEXT NOT NULL DEFAULT '',
			provider     TEXT NOT NULL DEFAULT '',
			system_prompt TEXT NOT NULL DEFAULT '',
			skills       TEXT NOT NULL DEFAULT '[]',
			max_tokens   INTEGER NOT NULL DEFAULT 0,
			temperature  REAL,
			metadata     TEXT NOT NULL DEFAULT '{}',
			is_default   INTEGER NOT NULL DEFAULT 0,
			created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO agents_mig_tempnull
		 SELECT name, display_name, description, model, provider, system_prompt, skills,
		        max_tokens, CASE WHEN temperature = 0 THEN NULL ELSE temperature END,
		        metadata, is_default, created_at, updated_at
		 FROM agents`,
		`DROP TABLE agents`,
		`ALTER TABLE agents_mig_tempnull RENAME TO agents`,
		`COMMIT`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			logger.Warn("temperature 迁移失败（保持旧列语义）", "error", err, "stmt", stmt)
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
			return
		}
	}
	logger.Info("agents.temperature 已迁移为可空列（0 值与未设可区分）")
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
		// temperature 可空（BUG-20260703 P2-4）：NULL=未设跟随模型默认，显式 0=确定性采样
		var temperature sql.NullFloat64
		if err := rows.Scan(&a.Name, &a.DisplayName, &a.Description, &a.Model,
			&a.Provider, &a.SystemPrompt, &skillsJSON, &a.MaxTokens,
			&temperature, &metaJSON, &isDefault); err != nil {
			return nil, "", err
		}
		if temperature.Valid {
			v := temperature.Float64
			a.Temperature = &v
		}
		if err := json.Unmarshal([]byte(skillsJSON), &a.Skills); err != nil {
			slog.Warn("failed to parse agent skills JSON", "agent", a.Name, "error", err)
		}
		if err := json.Unmarshal([]byte(metaJSON), &a.Metadata); err != nil {
			slog.Warn("failed to parse agent metadata JSON", "agent", a.Name, "error", err)
		}
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

// SetDefault 设置默认 Agent；name 为空 = 清除默认（与 Dispatcher.SetDefault 语义对齐，
// BUG-20260703 G1：吞错修复后「清除默认」不得被误报为 agent 不存在）
func (s *SQLiteStore) SetDefault(ctx context.Context, name string) error {
	if name == "" {
		_, err := s.db.ExecContext(ctx, `UPDATE agents SET is_default = 0 WHERE is_default = 1`)
		return err
	}
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

// SaveRule 插入或更新规则（upsert，基于 5 元组唯一约束），回写 rule.ID
//
// 使用 ON CONFLICT DO UPDATE 防御重复 INSERT：
//   - 内存与 DB 通过 Sync 双向同步时，同一规则可能被重复写入
//   - 历史 bug：每次启动 rules 数量指数翻倍（2^n），最终触发 DB 膨胀至 GB 级
//
// priority 更新采用"取最大值"策略，保护手动调整的优先级不被默认值覆盖。
func (s *SQLiteStore) SaveRule(ctx context.Context, rule *Rule) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_rules (platform, instance_id, user_id, chat_id, agent_name, priority)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(platform, instance_id, user_id, chat_id, agent_name)
		 DO UPDATE SET priority = MAX(priority, excluded.priority)`,
		rule.Platform, rule.InstanceID, rule.UserID, rule.ChatID, rule.AgentName, rule.Priority,
	)
	if err != nil {
		return err
	}
	if id, idErr := res.LastInsertId(); idErr == nil && id > 0 {
		rule.ID = int(id)
		return nil
	}
	// 冲突路径：LastInsertId 不可靠，回查 ID
	return s.db.QueryRowContext(ctx,
		`SELECT id FROM agent_rules
		 WHERE platform=? AND instance_id=? AND user_id=? AND chat_id=? AND agent_name=?`,
		rule.Platform, rule.InstanceID, rule.UserID, rule.ChatID, rule.AgentName,
	).Scan(&rule.ID)
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

// DeleteRulesByInstance 删除某平台实例的所有规则（BUG-20260703 A1：删实例级联清绑定）
func (s *SQLiteStore) DeleteRulesByInstance(ctx context.Context, platform, instanceID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_rules WHERE platform = ? AND instance_id = ?`, platform, instanceID)
	return err
}

// DeleteRulesByPlatform 删除某平台的全部规则（BUG-20260703 A1：平台最后一个实例删除后
// 清遗留，含 platform 级（instance_id 为空串）的历史绑定——否则重建实例会静默继承旧绑定）
func (s *SQLiteStore) DeleteRulesByPlatform(ctx context.Context, platform string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_rules WHERE platform = ?`, platform)
	return err
}

// Sync 将 Dispatcher 当前内存状态全量同步到 DB（用于从配置文件初始化）
//
// Rule 侧已通过 UNIQUE 约束 + ON CONFLICT 兜底重复写入；
// 这里仍保留全量 upsert 语义以便 priority 或其他字段能被覆盖刷新。
func Sync(ctx context.Context, store Store, d *Dispatcher) error {
	agents := d.ListAgents()
	for i := range agents {
		if err := store.SaveAgent(ctx, &agents[i]); err != nil {
			logger.Info("sync agent", "name", agents[i].Name, "error", err)
		}
	}
	defaultName := d.DefaultAgent()
	if defaultName != "" {
		_ = store.SetDefault(ctx, defaultName)
	}
	for _, r := range d.ListRules() {
		rule := r
		if err := store.SaveRule(ctx, &rule); err != nil {
			logger.Info("sync rule for", "agent_name", r.AgentName, "error", err)
		}
	}
	return nil
}
