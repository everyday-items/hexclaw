package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	v53LegacyTextbookKey = "k12.textbook_edition"
	v53MathTextbookKey   = "k12.textbook_edition.math"
)

// K12SubjectTextbooksV53 promotes the existing per-subject metadata keys to
// the canonical profile source. The legacy scalar remains a derived math
// projection for old consumers.
var K12SubjectTextbooksV53 = Migration{
	Version:     53,
	Description: "K12 六科教材 canonical metadata 与 legacy math 派生镜像",
	Func:        migrateK12SubjectTextbooksV53,
}

func migrateK12SubjectTextbooksV53(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 V53 迁移事务: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT name,metadata FROM agents`)
	if err != nil {
		return fmt.Errorf("读取 V53 agent metadata: %w", err)
	}
	type agentMetadata struct {
		name string
		meta map[string]string
	}
	updates := make([]agentMetadata, 0)
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			rows.Close()
			return fmt.Errorf("扫描 V53 agent metadata: %w", err)
		}
		var meta map[string]string
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			rows.Close()
			return fmt.Errorf("解析 V53 agent %q metadata: %w", name, err)
		}
		canonicalMath := strings.TrimSpace(meta[v53MathTextbookKey])
		if canonicalMath == "" {
			canonicalMath = strings.TrimSpace(meta[v53LegacyTextbookKey])
		}
		if canonicalMath == "" {
			continue
		}
		if meta[v53MathTextbookKey] == canonicalMath &&
			meta[v53LegacyTextbookKey] == canonicalMath {
			continue
		}
		meta[v53MathTextbookKey] = canonicalMath
		meta[v53LegacyTextbookKey] = canonicalMath
		updates = append(updates, agentMetadata{name: name, meta: meta})
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭 V53 agent metadata rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("迭代 V53 agent metadata: %w", err)
	}
	for _, update := range updates {
		raw, err := json.Marshal(update.meta)
		if err != nil {
			return fmt.Errorf("编码 V53 agent %q metadata: %w", update.name, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET metadata=? WHERE name=?`,
			string(raw), update.name); err != nil {
			return fmt.Errorf("写入 V53 agent %q metadata: %w", update.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 V53 迁移事务: %w", err)
	}
	return nil
}
