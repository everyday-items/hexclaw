package migrate

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// TestV10_ScenarioInstallations §6.9 清单表 scenario_installations 落地（Manifest v2 安装收据）：
// 建表 + 关键列 + 幂等重跑。
func TestV10_ScenarioInstallations(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 表存在 + 关键列可写
	if _, err := db.ExecContext(ctx, `INSERT INTO scenario_installations
		(scenario_id, version, contract_version, status, mount_path, manifest_json, receipt_json, installed_at, updated_at)
		VALUES ('k12','0.5.0',2,'installed','/api/k12','{}','{}',1,1)`); err != nil {
		t.Fatalf("scenario_installations 表结构不符: %v", err)
	}
	// scenario_id 主键防重
	if _, err := db.ExecContext(ctx, `INSERT INTO scenario_installations
		(scenario_id, version, contract_version, status, mount_path, manifest_json, receipt_json, installed_at, updated_at)
		VALUES ('k12','0.5.1',2,'installed','/api/k12','{}','{}',2,2)`); err == nil {
		t.Fatal("scenario_id 应为主键，重复插入应失败")
	}
	// 幂等重跑
	if err := Run(ctx, db, All); err != nil {
		t.Fatalf("migrate 重跑应幂等: %v", err)
	}
	// 与类型化批 V9 共存：版本号必须高于 9
	var v int
	if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v < 10 {
		t.Fatalf("scenario_installations 应以 >=10 的新版本落地（与 V9 共存）, got %d", v)
	}
}
