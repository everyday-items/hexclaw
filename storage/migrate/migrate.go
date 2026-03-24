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
	"log"
	"time"
)

// Migration 单次迁移定义
type Migration struct {
	Version     int
	Description string
	SQL         string
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
		log.Printf("[migrate] 已应用 v%d: %s", m.Version, m.Description)
	}

	if applied > 0 {
		log.Printf("[migrate] 共应用 %d 个迁移（当前版本: v%d → v%d）",
			applied, currentVersion, migrations[len(migrations)-1].Version)
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 执行迁移 SQL
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("执行 SQL 失败: %w", err)
	}

	// 记录迁移版本
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, description, applied_at) VALUES (?, ?, ?)",
		m.Version, m.Description, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("记录迁移版本失败: %w", err)
	}

	return tx.Commit()
}
