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
// v3 为 DD-025 restore-as 增加不可变 archive_id；v4 把作品已确认 OCR
// canonical evidence 纳入 checksum；v5 纳入 V19 Problem/Attempt canonical
// ledger 与其 page asset。v2/v3/v4 校验字节契约继续兼容。
const HexbakVersion = 5

// ErrChecksumMismatch 恢复时校验和不符（归档损坏或载荷不一致）。
var ErrChecksumMismatch = errors.New("hexbak checksum mismatch")

// ErrVersionUnsupported 归档版本高于当前应用可识别版本（PRD §3.12.8：提示先升级应用）。
var ErrVersionUnsupported = errors.New("hexbak version newer than supported")

// Hexbak 家庭学习档案备份（M4-1，商业化数据资产的耐久底座）。
type Hexbak struct {
	Version    int                    `json:"version"`
	ArchiveID  string                 `json:"archive_id,omitempty"`
	AgentName  string                 `json:"agent_name"`
	ExportedAt int64                  `json:"exported_at"`
	Records    []*records.AgentRecord `json:"records"`
	// Assets v3 起内嵌 canonical records 引用的内容寻址图片；[]byte 以 JSON base64
	// 编码进入 .hexbak，checksum 同时覆盖 manifest 与原始字节。
	Assets []HexbakAsset `json:"assets,omitempty"`
	// CreativeWorkOCR v4 起只携带被作品版本引用的 confirmed OCR canonical
	// evidence。运行中的 pending/processing/failed Job 不属于可恢复事实。
	CreativeWorkOCR []k12.CreativeWorkOCRArchiveEvidence `json:"creative_work_ocr,omitempty"`
	// ProblemAttempts v5 起覆盖所有 V19 canonical submission。SubmissionID、
	// ProblemID、AttemptID 保持稳定；page_asset_id 引用的原图进入 Assets exact-set。
	ProblemAttempts []k12.ProblemAttemptSnapshot `json:"problem_attempts,omitempty"`
	// Profile 孩子档案（T2.6 全量导出 PRD §3.12.4-1：不止 records，还含档案）。可空（老归档/无档案）。
	// 注：学情记忆 + 实例配置的导出需跨子系统新 port（Insights 目前只写不可导），属后续。
	Profile  *k12.ChildProfile `json:"profile,omitempty"`
	Checksum string            `json:"checksum"` // v2+: sha256(该版本完整语义载荷的规范 JSON)
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
	problemAttempts, err := PackHexbakProblemAttempts(ctx, d.Records, agentName)
	if err != nil {
		return nil, fmt.Errorf("usecase: 打包 Problem/Attempt ledger: %w", err)
	}
	bak.ProblemAttempts = problemAttempts
	recordAssets, err := PackHexbakAssets(agentName, recs)
	if err != nil {
		return nil, fmt.Errorf("usecase: 打包档案资产: %w", err)
	}
	problemAssets, err := PackHexbakProblemAttemptAssets(agentName, problemAttempts)
	if err != nil {
		return nil, fmt.Errorf("usecase: 打包 Problem 页面资产: %w", err)
	}
	bak.Assets, err = mergeHexbakAssets(recordAssets, problemAssets)
	if err != nil {
		return nil, fmt.Errorf("usecase: 合并档案资产: %w", err)
	}
	ocrEvidence, err := PackHexbakCreativeWorkOCR(ctx, d.Records, agentName, recs)
	if err != nil {
		return nil, fmt.Errorf("usecase: 打包作文 OCR 确认证据: %w", err)
	}
	bak.CreativeWorkOCR = ocrEvidence
	if err := SealHexbak(bak); err != nil {
		return nil, err
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
	if err := VerifyHexbak(bak); err != nil {
		return 0, err
	}
	restoreBak, err := materializeHexbakForRestore(bak)
	if err != nil {
		return 0, fmt.Errorf("usecase: materialize archive evidence: %w", err)
	}
	// 冻结#2 / §5.4 零容忍：备份恢复是「初中可绕过进入」最隐蔽的通道——恢复一份含初中
	// 档案的 .hexbak 即绕过建档守门。恢复路径同样收窄小学 12 档白名单（ValidProfileGradeTerm）。
	if restoreBak.Version >= 2 && restoreBak.Profile != nil && restoreBak.Profile.GradeTerm != "" && !k12.ValidProfileGradeTerm(restoreBak.Profile.GradeTerm) {
		return 0, fmt.Errorf("%w: 归档档案年级 %q 不在当前开放学段（仅小学一至六年级）", ErrInvalidInput, restoreBak.Profile.GradeTerm)
	}
	if restoreBak.Version >= 2 {
		// In v2 a nil profile is signed semantic data: it means clear the K12
		// profile namespace. Whenever profiles are enabled (or one is present in
		// the archive), fail closed unless records + profile share one durability
		// boundary; compensation cannot close a process-crash window.
		if d.ArchiveRestorer == nil && (d.Profiles != nil || restoreBak.Profile != nil ||
			len(restoreBak.CreativeWorkOCR) > 0 || len(restoreBak.ProblemAttempts) > 0) {
			return 0, fmt.Errorf("%w: 未配置 records/profile 原子恢复能力", ErrInvalidInput)
		}
		if d.ArchiveRestorer != nil {
			if restoreBak.Version >= 3 {
				if full, ok := d.ArchiveRestorer.(HexbakArchiveRestorer); ok {
					if err := full.RestoreHexbak(ctx, cloneHexbak(restoreBak)); err != nil {
						return 0, fmt.Errorf("usecase: 原子恢复 v%d 档案: %w", restoreBak.Version, err)
					}
					return len(restoreBak.Records), nil
				}
				if len(restoreBak.Assets) > 0 || len(restoreBak.CreativeWorkOCR) > 0 || len(restoreBak.ProblemAttempts) > 0 {
					return 0, fmt.Errorf("%w: 未配置 v%d 内容文件/OCR/Problem-Attempt 原子恢复能力", ErrInvalidInput, restoreBak.Version)
				}
			}
			if err := d.ArchiveRestorer.RestoreArchive(ctx, restoreBak.AgentName, restoreBak.Records, restoreBak.Profile); err != nil {
				return 0, fmt.Errorf("usecase: 原子恢复档案: %w", err)
			}
			return len(restoreBak.Records), nil
		}
	}
	// v1 has no signed profile. Records still use the same agent-scoped atomic
	// merge contract; only the unsigned profile is intentionally ignored.
	if err := d.Records.ImportAgentRecords(ctx, restoreBak.AgentName, restoreBak.Records); err != nil {
		return 0, fmt.Errorf("usecase: 恢复档案: %w", err)
	}
	return len(restoreBak.Records), nil
}

// checksumHexbak 对 v2+ 归档除 Checksum 外的完整语义载荷算 sha256。
// v2/v3/v4 必须保持历史字段集合，新增字段不得改变旧归档校验结果。
func checksumHexbak(bak *Hexbak) (string, error) {
	if bak == nil {
		return "", fmt.Errorf("usecase: nil hexbak")
	}
	var payload any
	if bak.Version == 2 {
		payload = struct {
			Version    int                    `json:"version"`
			AgentName  string                 `json:"agent_name"`
			ExportedAt int64                  `json:"exported_at"`
			Records    []*records.AgentRecord `json:"records"`
			Profile    *k12.ChildProfile      `json:"profile,omitempty"`
		}{bak.Version, bak.AgentName, bak.ExportedAt, bak.Records, bak.Profile}
	} else if bak.Version == 3 {
		payload = struct {
			Version    int                    `json:"version"`
			ArchiveID  string                 `json:"archive_id"`
			AgentName  string                 `json:"agent_name"`
			ExportedAt int64                  `json:"exported_at"`
			Records    []*records.AgentRecord `json:"records"`
			Assets     []HexbakAsset          `json:"assets,omitempty"`
			Profile    *k12.ChildProfile      `json:"profile,omitempty"`
		}{bak.Version, bak.ArchiveID, bak.AgentName, bak.ExportedAt, bak.Records, bak.Assets, bak.Profile}
	} else if bak.Version == 4 {
		payload = struct {
			Version         int                                  `json:"version"`
			ArchiveID       string                               `json:"archive_id"`
			AgentName       string                               `json:"agent_name"`
			ExportedAt      int64                                `json:"exported_at"`
			Records         []*records.AgentRecord               `json:"records"`
			Assets          []HexbakAsset                        `json:"assets,omitempty"`
			CreativeWorkOCR []k12.CreativeWorkOCRArchiveEvidence `json:"creative_work_ocr,omitempty"`
			Profile         *k12.ChildProfile                    `json:"profile,omitempty"`
		}{bak.Version, bak.ArchiveID, bak.AgentName, bak.ExportedAt, bak.Records,
			bak.Assets, bak.CreativeWorkOCR, bak.Profile}
	} else {
		payload = struct {
			Version         int                                  `json:"version"`
			ArchiveID       string                               `json:"archive_id"`
			AgentName       string                               `json:"agent_name"`
			ExportedAt      int64                                `json:"exported_at"`
			Records         []*records.AgentRecord               `json:"records"`
			Assets          []HexbakAsset                        `json:"assets,omitempty"`
			CreativeWorkOCR []k12.CreativeWorkOCRArchiveEvidence `json:"creative_work_ocr,omitempty"`
			ProblemAttempts []k12.ProblemAttemptSnapshot         `json:"problem_attempts,omitempty"`
			Profile         *k12.ChildProfile                    `json:"profile,omitempty"`
		}{bak.Version, bak.ArchiveID, bak.AgentName, bak.ExportedAt, bak.Records,
			bak.Assets, bak.CreativeWorkOCR, bak.ProblemAttempts, bak.Profile}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("usecase: 序列化校验和: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// SealHexbak 为 v2+ 归档生成校验和。v3+ 缺少 archive_id 时按该版本
// 语义载荷派生稳定内容 ID。
func SealHexbak(bak *Hexbak) error {
	if bak == nil {
		return fmt.Errorf("usecase: nil hexbak")
	}
	if bak.Version == 0 {
		bak.Version = HexbakVersion
	}
	if bak.Version >= 3 && bak.ArchiveID == "" {
		var seed any
		if bak.Version == 3 {
			seed = struct {
				Version    int                    `json:"version"`
				AgentName  string                 `json:"agent_name"`
				ExportedAt int64                  `json:"exported_at"`
				Records    []*records.AgentRecord `json:"records"`
				Assets     []HexbakAsset          `json:"assets,omitempty"`
				Profile    *k12.ChildProfile      `json:"profile,omitempty"`
			}{bak.Version, bak.AgentName, bak.ExportedAt, bak.Records, bak.Assets, bak.Profile}
		} else if bak.Version == 4 {
			seed = struct {
				Version         int                                  `json:"version"`
				AgentName       string                               `json:"agent_name"`
				ExportedAt      int64                                `json:"exported_at"`
				Records         []*records.AgentRecord               `json:"records"`
				Assets          []HexbakAsset                        `json:"assets,omitempty"`
				CreativeWorkOCR []k12.CreativeWorkOCRArchiveEvidence `json:"creative_work_ocr,omitempty"`
				Profile         *k12.ChildProfile                    `json:"profile,omitempty"`
			}{bak.Version, bak.AgentName, bak.ExportedAt, bak.Records, bak.Assets,
				bak.CreativeWorkOCR, bak.Profile}
		} else {
			seed = struct {
				Version         int                                  `json:"version"`
				AgentName       string                               `json:"agent_name"`
				ExportedAt      int64                                `json:"exported_at"`
				Records         []*records.AgentRecord               `json:"records"`
				Assets          []HexbakAsset                        `json:"assets,omitempty"`
				CreativeWorkOCR []k12.CreativeWorkOCRArchiveEvidence `json:"creative_work_ocr,omitempty"`
				ProblemAttempts []k12.ProblemAttemptSnapshot         `json:"problem_attempts,omitempty"`
				Profile         *k12.ChildProfile                    `json:"profile,omitempty"`
			}{bak.Version, bak.AgentName, bak.ExportedAt, bak.Records, bak.Assets,
				bak.CreativeWorkOCR, bak.ProblemAttempts, bak.Profile}
		}
		b, err := json.Marshal(seed)
		if err != nil {
			return fmt.Errorf("usecase: 序列化 archive id: %w", err)
		}
		sum := sha256.Sum256(b)
		bak.ArchiveID = "hexbak-" + hex.EncodeToString(sum[:16])
	}
	sum, err := checksumHexbak(bak)
	if err != nil {
		return err
	}
	bak.Checksum = sum
	return nil
}

// VerifyHexbak 验证受支持归档的版本、必要头与原始 checksum，不做任何写入。
func VerifyHexbak(bak *Hexbak) error {
	if bak == nil {
		return fmt.Errorf("usecase: nil hexbak")
	}
	if bak.Version > HexbakVersion {
		return fmt.Errorf("%w: 归档 v%d > 支持 v%d，请先升级应用", ErrVersionUnsupported, bak.Version, HexbakVersion)
	}
	if bak.Version <= 0 {
		return fmt.Errorf("%w: 非法归档版本 %d", ErrInvalidInput, bak.Version)
	}
	if bak.AgentName == "" {
		return fmt.Errorf("%w: agentName 不可空", ErrInvalidInput)
	}
	if bak.Version >= 3 && bak.ArchiveID == "" {
		return fmt.Errorf("%w: v3 archive_id 不可空", ErrInvalidInput)
	}
	var (
		sum string
		err error
	)
	if bak.Version >= 2 {
		sum, err = checksumHexbak(bak)
	} else {
		sum, err = checksumRecords(bak.Records)
	}
	if err != nil {
		return err
	}
	if sum != bak.Checksum {
		return ErrChecksumMismatch
	}
	if err := ValidateHexbakAssets(bak); err != nil {
		return err
	}
	if err := ValidateHexbakCreativeWorkOCR(bak); err != nil {
		return err
	}
	if err := ValidateHexbakProblemAttempts(bak); err != nil {
		return err
	}
	return nil
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
