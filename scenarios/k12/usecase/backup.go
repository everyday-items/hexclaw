package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// HexbakVersion 备份格式版本（嵌入归档头，支撑演进）。
const HexbakVersion = 1

// ErrChecksumMismatch 恢复时校验和不符（归档损坏/被篡改）。
var ErrChecksumMismatch = errors.New("hexbak checksum mismatch")

// ErrVersionUnsupported 归档版本高于当前应用可识别版本（PRD §3.12.8：提示先升级应用）。
var ErrVersionUnsupported = errors.New("hexbak version newer than supported")

// Hexbak 家庭学习档案备份（M4-1，商业化数据资产的耐久底座）。
type Hexbak struct {
	Version    int                    `json:"version"`
	AgentName  string                 `json:"agent_name"`
	ExportedAt int64                  `json:"exported_at"`
	Records    []*records.AgentRecord `json:"records"`
	// Profile 孩子档案（T2.6 全量导出 PRD §3.12.4-1：不止 records，还含档案）。可空（老归档/无档案）。
	// 注：学情记忆 + 实例配置的导出需跨子系统新 port（Insights 目前只写不可导），属后续。
	Profile  *k12.ChildProfile `json:"profile,omitempty"`
	Checksum string            `json:"checksum"` // sha256(records 规范 JSON)
}

// Backup 导出某实例的全部记录为 .hexbak（含 checksum）。
func (d Deps) Backup(ctx context.Context, agentName string) (*Hexbak, error) {
	if agentName == "" {
		return nil, fmt.Errorf("usecase: agentName 不可空")
	}
	recs, err := d.Records.ExportAgent(ctx, agentName)
	if err != nil {
		return nil, fmt.Errorf("usecase: 导出档案: %w", err)
	}
	sum, err := checksumRecords(recs)
	if err != nil {
		return nil, err
	}
	bak := &Hexbak{
		Version: HexbakVersion, AgentName: agentName, ExportedAt: d.now(),
		Records: recs, Checksum: sum,
	}
	// T2.6 全量导出：附孩子档案（有档案存储且档案存在时）。
	if d.Profiles != nil {
		if p, perr := d.Profiles.GetProfile(ctx, agentName); perr == nil && p.GradeTerm != "" {
			pc := p
			bak.Profile = &pc
		}
	}
	return bak, nil
}

// RestoreWithSnapshot 恢复前先对当前状态做一次快照（PRD §3.12.9：操作前自动快照便于回退），
// 再执行恢复。返回 (导入条数, 操作前快照, err)。快照失败不阻断恢复（best-effort）。
func (d Deps) RestoreWithSnapshot(ctx context.Context, bak *Hexbak) (int, *Hexbak, error) {
	var snapshot *Hexbak
	if bak != nil && bak.AgentName != "" {
		snapshot, _ = d.Backup(ctx, bak.AgentName)
	}
	n, err := d.Restore(ctx, bak)
	return n, snapshot, err
}

// Restore 从 .hexbak 逐字段还原记录（先校验 checksum；record_id 冲突则覆盖）。
// 返回导入条数。
func (d Deps) Restore(ctx context.Context, bak *Hexbak) (int, error) {
	if bak == nil {
		return 0, fmt.Errorf("usecase: nil hexbak")
	}
	// 版本兼容 gate（PRD §3.12.8）：归档版本高于当前应用支持版本 → 拒绝，提示先升级应用，
	// 绝不按残缺格式部分导入。低版本（旧归档）向前兼容，正常读。
	if bak.Version > HexbakVersion {
		return 0, fmt.Errorf("%w: 归档 v%d > 支持 v%d，请先升级应用", ErrVersionUnsupported, bak.Version, HexbakVersion)
	}
	sum, err := checksumRecords(bak.Records)
	if err != nil {
		return 0, err
	}
	if sum != bak.Checksum {
		return 0, ErrChecksumMismatch
	}
	// T1.2：单事务原子导入——任一条失败整批回滚，绝不部分导入（不止前置 checksum 挡文件损坏，
	// 也挡写库中途故障，贯彻 PRD §3.12.8「不部分导入」）。
	if err := d.Records.ImportRecords(ctx, bak.Records); err != nil {
		return 0, fmt.Errorf("usecase: 恢复档案: %w", err)
	}
	// T2.6：还原孩子档案（归档含 Profile 且有档案存储时）。
	if bak.Profile != nil && d.Profiles != nil {
		if err := d.Profiles.SaveProfile(ctx, bak.AgentName, *bak.Profile); err != nil {
			return len(bak.Records), fmt.Errorf("usecase: 恢复档案-孩子档案: %w", err)
		}
	}
	return len(bak.Records), nil
}

// checksumRecords 对记录集算 sha256（ExportAgent 已确定性排序，序列化字节稳定）。
func checksumRecords(recs []*records.AgentRecord) (string, error) {
	b, err := json.Marshal(recs)
	if err != nil {
		return "", fmt.Errorf("usecase: 序列化校验和: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
