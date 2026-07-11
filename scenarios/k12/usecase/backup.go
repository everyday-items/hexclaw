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
const HexbakVersion = 2

// ErrChecksumMismatch 恢复时校验和不符（归档损坏或载荷不一致）。
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
	Checksum string            `json:"checksum"` // v2: sha256(归档头+records+profile 规范 JSON)
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
	bak := &Hexbak{
		Version: HexbakVersion, AgentName: agentName, ExportedAt: d.now(),
		Records: recs,
	}
	// T2.6 全量导出：附孩子档案（有档案存储且档案存在时）。
	if d.Profiles != nil {
		p, perr := d.Profiles.GetProfile(ctx, agentName)
		if perr != nil && !errors.Is(perr, records.ErrNotFound) {
			return nil, fmt.Errorf("usecase: 导出孩子档案: %w", perr)
		}
		if perr == nil && p != (k12.ChildProfile{}) {
			pc := p
			bak.Profile = &pc
		}
	}
	sum, err := checksumHexbak(bak)
	if err != nil {
		return nil, err
	}
	bak.Checksum = sum
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

// Restore 从 .hexbak 幂等合并记录（先校验 checksum；record_id 冲突以导入方为准，
// 归档中不存在的当前记录保留），并精确恢复归档内的孩子档案。
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
	if bak.AgentName == "" {
		return 0, fmt.Errorf("%w: agentName 不可空", ErrInvalidInput)
	}
	var sum string
	var err error
	if bak.Version >= 2 {
		sum, err = checksumHexbak(bak)
	} else {
		// v1 只签名 records：为兼容老归档仍允许恢复记录，但绝不恢复未签名的 Profile。
		sum, err = checksumRecords(bak.Records)
	}
	if err != nil {
		return 0, err
	}
	if sum != bak.Checksum {
		return 0, ErrChecksumMismatch
	}
	if bak.Version >= 2 && bak.Profile != nil && bak.Profile.GradeTerm != "" && !k12.ValidGradeTerm(bak.Profile.GradeTerm) {
		return 0, fmt.Errorf("%w: 归档档案含非法年级 %q", ErrInvalidInput, bak.Profile.GradeTerm)
	}
	if bak.Version >= 2 {
		// In v2 a nil profile is signed semantic data: it means clear the K12
		// profile namespace. Whenever profiles are enabled (or one is present in
		// the archive), fail closed unless records + profile share one durability
		// boundary; compensation cannot close a process-crash window.
		if d.ArchiveRestorer == nil && (d.Profiles != nil || bak.Profile != nil) {
			return 0, fmt.Errorf("%w: 未配置 records/profile 原子恢复能力", ErrInvalidInput)
		}
		if d.ArchiveRestorer != nil {
			if err := d.ArchiveRestorer.RestoreArchive(ctx, bak.AgentName, bak.Records, bak.Profile); err != nil {
				return 0, fmt.Errorf("usecase: 原子恢复档案: %w", err)
			}
			return len(bak.Records), nil
		}
	}
	// v1 has no signed profile. Records still use the same agent-scoped atomic
	// merge contract; only the unsigned profile is intentionally ignored.
	if err := d.Records.ImportAgentRecords(ctx, bak.AgentName, bak.Records); err != nil {
		return 0, fmt.Errorf("usecase: 恢复档案: %w", err)
	}
	return len(bak.Records), nil
}

// checksumHexbak 对 v2 归档除 Checksum 外的完整语义载荷算 sha256。
func checksumHexbak(bak *Hexbak) (string, error) {
	if bak == nil {
		return "", fmt.Errorf("usecase: nil hexbak")
	}
	payload := struct {
		Version    int                    `json:"version"`
		AgentName  string                 `json:"agent_name"`
		ExportedAt int64                  `json:"exported_at"`
		Records    []*records.AgentRecord `json:"records"`
		Profile    *k12.ChildProfile      `json:"profile,omitempty"`
	}{bak.Version, bak.AgentName, bak.ExportedAt, bak.Records, bak.Profile}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("usecase: 序列化校验和: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
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
