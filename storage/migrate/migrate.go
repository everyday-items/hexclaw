// Package migrate 提供 SQLite 数据库版本迁移
//
// 所有表结构变更集中管理，按版本顺序执行，幂等可重入。
// 替代此前各模块分散的 Init() 建表逻辑。
//
// 设计参考：
//   - 业界生产级 SQL 迁移实践（手动执行 + 幂等保证）
//   - Tauri plugin-sql 的版本化迁移（hexclaw-desktop 前端已采用）
//   - golang-migrate/migrate 的 version tracking 思路
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hexagon-codes/toolkit/util/logger"
)

// AtomicMigrationFunc runs a migration-specific transaction and invokes
// recordVersion inside that same transaction immediately before commit. It is
// reserved for migrations that need connection-scoped PRAGMAs around DDL.
type AtomicMigrationFunc func(
	ctx context.Context,
	db *sql.DB,
	recordVersion func(context.Context, *sql.Tx) error,
) error

// Migration 单次迁移定义。
//
// 三种模式（互斥）：
//   - SQL: 纯 SQL 串（最常见），整段在一个事务里跑
//   - Func: Go 函数（用于需要 schema 检测 / 条件分支的迁移，事务由 Func 自己管理）
//   - AtomicFunc: Func 的连接级变体，数据与版本行由同一事务提交
//
// 优先级为 AtomicFunc > Func > SQL；都空则 NOOP（仅写 schema_migrations 占位）。
type Migration struct {
	Version     int
	Description string
	SQL         string
	Func        func(ctx context.Context, db *sql.DB) error
	AtomicFunc  AtomicMigrationFunc
}

// Run 执行所有未应用的迁移
//
// 工作流程：
//  1. 确保 schema_migrations 表存在
//  2. 读取当前已应用的最高版本
//  3. 按顺序执行所有更高版本的迁移（每个迁移在独立事务中）
//  4. 记录每个迁移的执行时间
func Run(ctx context.Context, db *sql.DB, migrations []Migration) error {
	// 确保迁移跟踪表存在
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     INTEGER PRIMARY KEY,
			description TEXT    NOT NULL DEFAULT '',
			applied_at  INTEGER NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("创建 schema_migrations 表失败: %w", err)
	}

	// 读取当前版本
	var currentVersion int
	row := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations")
	if err := row.Scan(&currentVersion); err != nil {
		return fmt.Errorf("读取当前迁移版本失败: %w", err)
	}

	// 执行未应用的迁移
	applied := 0
	for _, m := range migrations {
		if m.Version <= currentVersion {
			continue
		}

		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("迁移 v%d (%s) 失败: %w", m.Version, m.Description, err)
		}
		applied++
		logger.Info("[migrate] 已应用 v", "version", m.Version, "description", m.Description)
	}

	if applied > 0 {
		logger.Info("[migrate] 共应用", "applied", applied, "currentVersion", currentVersion, "version", migrations[len(migrations)-1].Version)
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m Migration) error {
	if m.AtomicFunc != nil {
		recordVersion := func(recordCtx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(recordCtx,
				"INSERT INTO schema_migrations (version, description, applied_at) VALUES (?, ?, ?)",
				m.Version, m.Description, time.Now().Unix(),
			); err != nil {
				return fmt.Errorf("记录迁移版本失败: %w", err)
			}
			return nil
		}
		if err := m.AtomicFunc(ctx, db, recordVersion); err != nil {
			return fmt.Errorf("执行 AtomicFunc 失败: %w", err)
		}
		var recordedDescription string
		if err := db.QueryRowContext(ctx,
			"SELECT description FROM schema_migrations WHERE version=?", m.Version,
		).Scan(&recordedDescription); err != nil {
			return fmt.Errorf("AtomicFunc 未原子提交迁移版本: %w", err)
		}
		if recordedDescription != m.Description {
			return fmt.Errorf("AtomicFunc 迁移版本描述不一致: got %q want %q", recordedDescription, m.Description)
		}
		return nil
	}

	// Func 模式：DDL 由 Func 自管事务（SQLite ALTER 等不能与 DML 共事务）。
	if m.Func != nil {
		if err := m.Func(ctx, db); err != nil {
			return fmt.Errorf("执行 Func 失败: %w", err)
		}
		if _, err := db.ExecContext(ctx,
			"INSERT INTO schema_migrations (version, description, applied_at) VALUES (?, ?, ?)",
			m.Version, m.Description, time.Now().Unix(),
		); err != nil {
			return fmt.Errorf("记录迁移版本失败: %w", err)
		}
		return nil
	}

	// SQL 模式：整段 SQL 在单个事务里跑（默认幂等不强求；DDL 中 fail 整体 rollback）
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if m.SQL != "" {
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			return fmt.Errorf("执行 SQL 失败: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, description, applied_at) VALUES (?, ?, ?)",
		m.Version, m.Description, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("记录迁移版本失败: %w", err)
	}

	return tx.Commit()
}
