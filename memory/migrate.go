package memory

import (
	"context"
	"database/sql"
	"strings"
)

// 砍薄版迁移（方案 §5）：旧记忆薄版 library.MemoryStore（SQLite `memories` 表，kind=standing|fact）
// 已并入统一文件记忆。本迁移在升级首启时一次性把历史 memories 行搬进 FileMemory：
//
//	standing → Type=rule（_global 常驻保证带）
//	fact     → Type=fact（T2 检索）
//
// **幂等**：迁移成功后 DROP TABLE memories —— 表存在=待迁移，表不存在=已迁移/全新装，下次启动直接跳过。
// 迁移是增强、不阻断启动：任何错误返回给调用方记日志即可，绝不 panic。凭证（PII）不迁入（守红线）。

// MigrateLegacyMemories 把旧 `memories` 表迁入 FileMemory，返回迁入条数。表不存在 → (0,nil)。
func MigrateLegacyMemories(ctx context.Context, db *sql.DB, fm *FileMemory) (int, error) {
	if db == nil || fm == nil {
		return 0, nil
	}
	// 表不存在 → 无需迁移。
	var name string
	switch err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='memories'`).Scan(&name); err {
	case sql.ErrNoRows:
		return 0, nil
	case nil:
		// 继续
	default:
		return 0, err
	}

	rows, err := db.QueryContext(ctx, `SELECT kind, content FROM memories`)
	if err != nil {
		return 0, err
	}
	type legacy struct{ kind, content string }
	var items []legacy
	for rows.Next() {
		var k, c string
		if scanErr := rows.Scan(&k, &c); scanErr != nil {
			rows.Close()
			return 0, scanErr
		}
		items = append(items, legacy{k, c})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	migrated := 0
	for _, it := range items {
		content := strings.TrimSpace(it.content)
		if content == "" || LooksSensitive(content) {
			continue
		}
		memType := "fact"
		if strings.EqualFold(strings.TrimSpace(it.kind), "standing") {
			memType = "rule" // standing → rule：_global 常驻保证带（薄版 standing 的语义保留）
		}
		if err := fm.SaveStructuredEntry(content, memType, "manual", "", EntryMeta{}); err != nil {
			return migrated, err
		}
		migrated++
	}

	// 迁移完成 → 删表（幂等标记）。
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS memories`); err != nil {
		return migrated, err
	}
	return migrated, nil
}
