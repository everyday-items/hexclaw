package cron

import (
	"context"
	"database/sql"
	"strings"

	"github.com/hexagon-codes/toolkit/util/logger"
)

// addColumnExpectDuplicate 跑 ALTER TABLE ADD COLUMN，对"列已存在"这一预期错误
// 静默忽略；其他错误（语法 / 权限 / IO）必须打 warn 日志，避免在生产环境 schema
// 漂移而被静默吞掉（参考本次表重建迁移 30 分钟漏掉 DROP COLUMN 失败的教训）。
//
// 不是 panic — migration 失败让 Init() 继续是可接受的（detectAndCleanupLegacyJobs
// 会再尝试 rebuild），但必须留下可观测信号。
func addColumnExpectDuplicate(ctx context.Context, db *sql.DB, table, sqlStmt string) {
	if _, err := db.ExecContext(ctx, sqlStmt); err != nil {
		if isDuplicateColumnErr(err) {
			return // 预期路径：列已存在
		}
		logger.Warn("[cron] migration ALTER 失败（非 duplicate column）",
			"table", table, "sql", sqlStmt, "error", err.Error())
	}
}

// isDuplicateColumnErr 识别"列已存在"错误。
//   - SQLite: "duplicate column name: X"
//   - SQLite3 driver 也用此措辞
//   - Postgres: "column \"X\" of relation \"Y\" already exists"  (SQLSTATE 42701)
//   - MySQL: "Duplicate column name 'X'" (errno 1060)
//
// 当前 hexclaw 桌面端只用 SQLite，但守得宽一点便于未来后端用其他 DB。
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") ||
		strings.Contains(msg, "already exists")
}
