package migrate

import (
	"context"
	"database/sql"
	"fmt"
)

// ModelCapabilityProbePolicyVersionV84 为已应用早期 V83 建表版本的数据库补齐策略版本列。
// 新库由 V83 直接创建该列；这里仍需覆盖升级路径，不能依赖修改过的历史建表 SQL。
var ModelCapabilityProbePolicyVersionV84 = Migration{
	Version:     84,
	Description: "LLM model capability probe policy version upgrade",
	Func:        migrateModelCapabilityProbePolicyVersionV84,
}

func migrateModelCapabilityProbePolicyVersionV84(ctx context.Context, db *sql.DB) error {
	hasColumn, err := columnExists(
		ctx, db, "llm_model_capability_probe_receipts", "probe_policy_version",
	)
	if err != nil {
		return fmt.Errorf("inspect model capability probe policy version: %w", err)
	}
	if hasColumn {
		return nil
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE llm_model_capability_probe_receipts
		ADD COLUMN probe_policy_version TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add model capability probe policy version: %w", err)
	}
	return nil
}
