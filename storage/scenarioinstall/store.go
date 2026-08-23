// Package scenarioinstall 实现 scenario.InstallationRecorder——ScenarioManifest v2 的
// 安装收据台账（scenario_installations，迁移 V10；架构设计 §6.2/§6.3/§6.9）。
//
// 语义：
//   - 安装/重装 = upsert 复位 installed（同 scenario_id 一行，卸载→再装新版本即升级路径）；
//   - 卸载 = 仅标记 status=uninstalled + uninstalled_at；不删行、不删业务数据
//     （数据保留策略，见迁移 V10 注释）。
package scenarioinstall

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hexagon-codes/hexclaw/scenario"
)

// Store SQLite 台账实现（与平台同库，§6.15 单库原则）。
type Store struct {
	db *sql.DB
}

// 编译期确认实现安装台账端口。
var _ scenario.InstallationRecorder = (*Store)(nil)

// New 建台账存储（db 须已跑过 migrate V10）。
func New(db *sql.DB) *Store { return &Store{db: db} }

// RecordInstall 写入/复位一条安装记录（manifest 声明头 + 实际创建资源收据）。
func (s *Store) RecordInstall(ctx context.Context, m *scenario.Manifest, r *scenario.Receipt) error {
	manifestJSON, err := m.DeclarationJSON()
	if err != nil {
		return fmt.Errorf("scenarioinstall: 序列化 manifest 声明: %w", err)
	}
	receiptJSON, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("scenarioinstall: 序列化收据: %w", err)
	}
	// 同版本同 manifest 的启动恢复是幂等操作，不能刷新持久收据时间戳或正文。
	var existingVersion, existingStatus, existingManifest string
	err = s.db.QueryRowContext(ctx, `
SELECT version, status, manifest_json
FROM scenario_installations
WHERE scenario_id = ?`, m.ID).Scan(&existingVersion, &existingStatus, &existingManifest)
	switch {
	case err == nil && existingStatus == "installed" && existingVersion == m.Version && existingManifest == string(manifestJSON):
		return nil
	case err != nil && err != sql.ErrNoRows:
		return fmt.Errorf("scenarioinstall: 读取既有安装记录 %s: %w", m.ID, err)
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO scenario_installations
    (scenario_id, version, contract_version, status, mount_path, manifest_json, receipt_json, installed_at, uninstalled_at, updated_at)
VALUES (?, ?, ?, 'installed', ?, ?, ?, ?, NULL, ?)
ON CONFLICT(scenario_id) DO UPDATE SET
    version          = excluded.version,
    contract_version = excluded.contract_version,
    status           = 'installed',
    mount_path       = excluded.mount_path,
    manifest_json    = excluded.manifest_json,
    receipt_json     = excluded.receipt_json,
    installed_at     = excluded.installed_at,
    uninstalled_at   = NULL,
    updated_at       = excluded.updated_at`,
		m.ID, m.Version, m.ContractVersion, m.MountPath,
		string(manifestJSON), string(receiptJSON), r.InstalledAt, now)
	if err != nil {
		return fmt.Errorf("scenarioinstall: 写安装记录 %s: %w", m.ID, err)
	}
	return nil
}

// RecordUninstall 标记卸载（保留行与业务数据）。未安装的场景返回错误（诚实失败）。
func (s *Store) RecordUninstall(ctx context.Context, scenarioID string) error {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `
UPDATE scenario_installations
SET status = 'uninstalled', uninstalled_at = ?, updated_at = ?
WHERE scenario_id = ? AND status = 'installed'`, now, now, scenarioID)
	if err != nil {
		return fmt.Errorf("scenarioinstall: 写卸载记录 %s: %w", scenarioID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("scenarioinstall: 场景 %s 无在装记录，无法标记卸载", scenarioID)
	}
	return nil
}
