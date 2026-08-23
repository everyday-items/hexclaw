package scenarioinstall

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func sampleManifest() *scenario.Manifest {
	return &scenario.Manifest{
		ID: "alpha", Version: "1.0.0", ContractVersion: 2, MinContractVersion: 2,
		MountPath: "/api/alpha",
		Resources: &scenario.Pack{Name: "alpha"},
	}
}

// TestStore_InstallUninstallLifecycle 安装收据落库 → 卸载标记 uninstalled（数据保留，只改状态）。
func TestStore_InstallUninstallLifecycle(t *testing.T) {
	db := newDB(t)
	s := New(db)
	ctx := context.Background()
	m := sampleManifest()
	rc := &scenario.Receipt{
		ScenarioID: "alpha", Version: "1.0.0", MountPath: "/api/alpha",
		Resources:   []scenario.ResourceRef{{Kind: scenario.KindView, Name: "v", Key: "alpha@1.0.0/view:v"}},
		InstalledAt: 42,
	}
	if err := s.RecordInstall(ctx, m, rc); err != nil {
		t.Fatalf("RecordInstall: %v", err)
	}
	var status, mount, receiptJSON string
	row := db.QueryRow(`SELECT status, mount_path, receipt_json FROM scenario_installations WHERE scenario_id='alpha'`)
	if err := row.Scan(&status, &mount, &receiptJSON); err != nil {
		t.Fatalf("查安装行: %v", err)
	}
	if status != "installed" || mount != "/api/alpha" {
		t.Errorf("安装行字段错: status=%q mount=%q", status, mount)
	}
	if receiptJSON == "" || receiptJSON == "{}" {
		t.Errorf("收据 JSON 应记录实际创建资源, got %q", receiptJSON)
	}

	if err := s.RecordUninstall(ctx, "alpha"); err != nil {
		t.Fatalf("RecordUninstall: %v", err)
	}
	var status2 string
	var uninstalledAt sql.NullInt64
	if err := db.QueryRow(`SELECT status, uninstalled_at FROM scenario_installations WHERE scenario_id='alpha'`).
		Scan(&status2, &uninstalledAt); err != nil {
		t.Fatal(err)
	}
	if status2 != "uninstalled" || !uninstalledAt.Valid {
		t.Errorf("卸载应标记 status=uninstalled + uninstalled_at, got %q %v", status2, uninstalledAt)
	}

	// 重装（卸载→再装新版本）：同 scenario_id 复位为 installed
	m.Version = "1.1.0"
	rc.Version = "1.1.0"
	if err := s.RecordInstall(ctx, m, rc); err != nil {
		t.Fatalf("重装 upsert: %v", err)
	}
	var ver, status3 string
	if err := db.QueryRow(`SELECT version, status FROM scenario_installations WHERE scenario_id='alpha'`).
		Scan(&ver, &status3); err != nil {
		t.Fatal(err)
	}
	if ver != "1.1.0" || status3 != "installed" {
		t.Errorf("重装应更新版本并复位状态, got %q %q", ver, status3)
	}
}

// TestStore_UninstallUnknown 卸载不存在的安装记录应报错（诚实失败）。
func TestStore_UninstallUnknown(t *testing.T) {
	s := New(newDB(t))
	if err := s.RecordUninstall(context.Background(), "ghost"); err == nil {
		t.Fatal("未安装场景的卸载记录应报错")
	}
}

// 验证启动恢复不会重写未变化的安装收据。
func TestStore_RecordInstallIsIdempotentForSameInstalledManifest(t *testing.T) {
	db := newDB(t)
	s := New(db)
	ctx := context.Background()
	m := sampleManifest()
	first := &scenario.Receipt{
		ScenarioID: "alpha", Version: "1.0.0", MountPath: "/api/alpha",
		Resources:   []scenario.ResourceRef{{Kind: scenario.KindView, Name: "v", Key: "alpha@1.0.0/view:v"}},
		InstalledAt: 42,
	}
	if err := s.RecordInstall(ctx, m, first); err != nil {
		t.Fatalf("first RecordInstall: %v", err)
	}
	var beforeInstalledAt, beforeUpdatedAt int64
	var beforeReceipt string
	if err := db.QueryRow(`SELECT installed_at, updated_at, receipt_json FROM scenario_installations WHERE scenario_id='alpha'`).
		Scan(&beforeInstalledAt, &beforeUpdatedAt, &beforeReceipt); err != nil {
		t.Fatalf("read first receipt: %v", err)
	}

	second := &scenario.Receipt{
		ScenarioID: "alpha", Version: "1.0.0", MountPath: "/api/alpha",
		Resources:   []scenario.ResourceRef{{Kind: scenario.KindView, Name: "v", Key: "alpha@1.0.0/view:v"}},
		InstalledAt: 99,
	}
	if err := s.RecordInstall(ctx, m, second); err != nil {
		t.Fatalf("second RecordInstall: %v", err)
	}
	var afterInstalledAt, afterUpdatedAt int64
	var afterReceipt string
	if err := db.QueryRow(`SELECT installed_at, updated_at, receipt_json FROM scenario_installations WHERE scenario_id='alpha'`).
		Scan(&afterInstalledAt, &afterUpdatedAt, &afterReceipt); err != nil {
		t.Fatalf("read second receipt: %v", err)
	}
	if afterInstalledAt != beforeInstalledAt || afterUpdatedAt != beforeUpdatedAt || afterReceipt != beforeReceipt {
		t.Fatalf("same installed manifest must not rewrite receipt: before=(%d,%d,%s) after=(%d,%d,%s)",
			beforeInstalledAt, beforeUpdatedAt, beforeReceipt, afterInstalledAt, afterUpdatedAt, afterReceipt)
	}
}
