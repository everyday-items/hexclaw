package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

const (
	RestoreMigrationCompleted  = "completed"
	RestoreMigrationRolledBack = "rolled_back"
)

var (
	ErrGuardianConfirmationRequired = errors.New("guardian confirmation required")
	ErrArchiveScopeMismatch         = errors.New("hexbak archive scope mismatch")
	ErrRestoreAsArchiveVersion      = errors.New("restore-as requires hexbak v2+")
)

// RestoreAsRequest 是跨 Tutor 恢复命令。SourceAgent/TargetAgent 与监护人确认均为
// 显式字段，服务端不从修改后的 archive header 猜归属。
type RestoreAsRequest struct {
	Archive           *Hexbak `json:"archive"`
	SourceAgent       string  `json:"source_agent"`
	TargetAgent       string  `json:"target_agent"`
	GuardianConfirmed bool    `json:"guardian_confirmed"`
	IdempotencyKey    string  `json:"idempotency_key"`
}

type RestoreAsRollbackRequest struct {
	MigrationID       string `json:"migration_id,omitempty"`
	TargetAgent       string `json:"target_agent"`
	GuardianConfirmed bool   `json:"guardian_confirmed"`
}

// RestoreAsPlan 是校验后交给共享 SQLite adapter 的不可变执行计划。OriginalArchive
// 保持原 checksum/owner；MigratedArchive 是独立的当前版本 copy，所有 owner 已重写。
type RestoreAsPlan struct {
	OriginalArchive       *Hexbak
	MigratedArchive       *Hexbak
	OriginalArchiveDigest string
	SourceAgent           string
	TargetAgent           string
	IdempotencyKey        string
	RequestedAt           int64
}

type RestoreAsResult struct {
	MigrationID              string  `json:"migration_id"`
	SourceAgent              string  `json:"source_agent,omitempty"`
	TargetAgent              string  `json:"target_agent"`
	Status                   string  `json:"status"`
	Restored                 int     `json:"restored"`
	OriginalArchiveDigest    string  `json:"original_archive_digest,omitempty"`
	MigratedChecksum         string  `json:"migrated_checksum,omitempty"`
	SnapshotDigest           string  `json:"snapshot_digest,omitempty"`
	JournalEntries           int     `json:"journal_entries"`
	OriginalArchivePreserved bool    `json:"original_archive_preserved"`
	Idempotent               bool    `json:"idempotent"`
	Snapshot                 *Hexbak `json:"snapshot,omitempty"`
}

// ArchiveMigrationRestorer owns the single durable transaction that writes target records,
// target profile, immutable original archive, pre-restore snapshot and append-only journal.
type ArchiveMigrationRestorer interface {
	RestoreArchiveAs(ctx context.Context, plan RestoreAsPlan) (RestoreAsResult, error)
	RollbackRestoreAs(ctx context.Context, req RestoreAsRollbackRequest) (RestoreAsResult, error)
}

func (d Deps) RestoreAs(ctx context.Context, req RestoreAsRequest) (RestoreAsResult, error) {
	if !req.GuardianConfirmed {
		return RestoreAsResult{}, fmt.Errorf("%w: restore-as 必须由监护人明确确认", ErrGuardianConfirmationRequired)
	}
	if d.ArchiveMigrator == nil {
		return RestoreAsResult{}, fmt.Errorf("%w: 未配置 restore-as 原子迁移能力", ErrInvalidInput)
	}
	source := strings.TrimSpace(req.SourceAgent)
	target := strings.TrimSpace(req.TargetAgent)
	key := strings.TrimSpace(req.IdempotencyKey)
	if source == "" || target == "" || key == "" {
		return RestoreAsResult{}, fmt.Errorf("%w: source_agent / target_agent / idempotency_key 不可空", ErrInvalidInput)
	}
	if len(key) > 200 {
		return RestoreAsResult{}, fmt.Errorf("%w: idempotency_key 过长", ErrInvalidInput)
	}
	if source == target {
		return RestoreAsResult{}, fmt.Errorf("%w: 同 Tutor 恢复请使用 /restore", ErrInvalidInput)
	}
	if req.Archive == nil {
		return RestoreAsResult{}, fmt.Errorf("%w: archive 不可空", ErrInvalidInput)
	}
	if req.Archive.Version < 2 {
		return RestoreAsResult{}, ErrRestoreAsArchiveVersion
	}
	if err := VerifyHexbak(req.Archive); err != nil {
		return RestoreAsResult{}, err
	}
	if req.Archive.AgentName != source {
		return RestoreAsResult{}, fmt.Errorf("%w: archive=%q request=%q", ErrArchiveScopeMismatch, req.Archive.AgentName, source)
	}
	if err := validateArchivedProfile(req.Archive); err != nil {
		return RestoreAsResult{}, err
	}
	for _, rec := range req.Archive.Records {
		if rec == nil || rec.AgentName != source {
			return RestoreAsResult{}, fmt.Errorf("%w: record owner 不属于 source_agent", ErrArchiveScopeMismatch)
		}
	}

	original := cloneHexbak(req.Archive)
	originalDigest, err := HexbakDigest(original)
	if err != nil {
		return RestoreAsResult{}, err
	}
	migrated, err := MigrateHexbakOwner(req.Archive, target)
	if err != nil {
		return RestoreAsResult{}, err
	}
	plan := RestoreAsPlan{
		OriginalArchive: original, MigratedArchive: migrated,
		OriginalArchiveDigest: originalDigest,
		SourceAgent:           source, TargetAgent: target, IdempotencyKey: key, RequestedAt: d.now(),
	}
	return d.ArchiveMigrator.RestoreArchiveAs(ctx, plan)
}

// MigrateHexbakOwner creates an independent current-version copy, rewrites every
// record envelope owner plus asset/OCR/Problem identity, and seals a new checksum.
// The persistence adapter calls this again inside its transaction;
// the usecase copy is only an immutable preflight plan and never mutates the source archive.
func MigrateHexbakOwner(source *Hexbak, targetAgent string) (*Hexbak, error) {
	targetAgent = strings.TrimSpace(targetAgent)
	if source == nil || targetAgent == "" {
		return nil, fmt.Errorf("%w: source archive / target_agent 不可空", ErrInvalidInput)
	}
	var sourceOCR []k12.CreativeWorkOCRArchiveEvidence
	var err error
	if source.Version >= 4 {
		if err := ValidateHexbakCreativeWorkOCR(source); err != nil {
			return nil, err
		}
		sourceOCR = append([]k12.CreativeWorkOCRArchiveEvidence(nil), source.CreativeWorkOCR...)
	} else {
		sourceOCR, err = materializeLegacyCreativeWorkOCR(source)
		if err != nil {
			return nil, err
		}
	}
	if err := ValidateHexbakProblemAttempts(source); err != nil {
		return nil, err
	}
	migrated := cloneHexbak(source)
	migrated.Version = HexbakVersion
	migrated.ArchiveID = ""
	migrated.AgentName = targetAgent
	for _, rec := range migrated.Records {
		if rec == nil {
			return nil, fmt.Errorf("%w: archive record 不可为 nil", ErrInvalidInput)
		}
		rec.AgentName = targetAgent
	}
	assets, err := migrateHexbakAssets(source, targetAgent, migrated.Records)
	if err != nil {
		return nil, err
	}
	migrated.Assets = assets
	assetMapping := creativeWorkOCRAssetMapping(source, migrated)
	migrated.ProblemAttempts, err = migrateHexbakProblemAttempts(
		source.ProblemAttempts, targetAgent, assetMapping,
	)
	if err != nil {
		return nil, err
	}
	jobIDs := creativeWorkOCRJobMapping(sourceOCR, targetAgent)
	if err := rewriteCreativeWorkOCRJobRefs(migrated.Records, jobIDs); err != nil {
		return nil, err
	}
	migrated.CreativeWorkOCR, err = migrateCreativeWorkOCREvidence(
		sourceOCR, targetAgent, assetMapping, jobIDs,
	)
	if err != nil {
		return nil, err
	}
	if source.ProblemSource != nil {
		problemSource, migrateErr := k12storage.MigrateProblemSourceArchiveV6Owner(
			source.AgentName, targetAgent, *source.ProblemSource, assetMapping,
		)
		if migrateErr != nil {
			if errors.Is(migrateErr, k12storage.ErrProblemSourceArchiveLiveWork) {
				return nil, fmt.Errorf("%w: %v", ErrHexbakProblemSourceLiveWork, migrateErr)
			}
			return nil, fmt.Errorf("%w: restore-as: %v", ErrHexbakProblemSource, migrateErr)
		}
		migrated.ProblemSource = &problemSource
	} else {
		migrated.ProblemSource = nil
	}
	if err := SealHexbak(migrated); err != nil {
		return nil, err
	}
	return migrated, nil
}

func (d Deps) RollbackRestoreAs(ctx context.Context, req RestoreAsRollbackRequest) (RestoreAsResult, error) {
	if !req.GuardianConfirmed {
		return RestoreAsResult{}, fmt.Errorf("%w: restore-as 回退必须由监护人明确确认", ErrGuardianConfirmationRequired)
	}
	if d.ArchiveMigrator == nil {
		return RestoreAsResult{}, fmt.Errorf("%w: 未配置 restore-as 原子迁移能力", ErrInvalidInput)
	}
	req.MigrationID = strings.TrimSpace(req.MigrationID)
	req.TargetAgent = strings.TrimSpace(req.TargetAgent)
	if req.MigrationID == "" || req.TargetAgent == "" {
		return RestoreAsResult{}, fmt.Errorf("%w: migration_id / target_agent 不可空", ErrInvalidInput)
	}
	return d.ArchiveMigrator.RollbackRestoreAs(ctx, req)
}

// HexbakDigest 是含原 checksum 的完整归档载荷 digest，用作不可变归档存储键。
func HexbakDigest(bak *Hexbak) (string, error) {
	if bak == nil {
		return "", fmt.Errorf("usecase: nil hexbak")
	}
	b, err := json.Marshal(bak)
	if err != nil {
		return "", fmt.Errorf("usecase: 序列化归档 digest: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func cloneHexbak(bak *Hexbak) *Hexbak {
	if bak == nil {
		return nil
	}
	out := *bak
	if bak.Profile != nil {
		p := *bak.Profile
		out.Profile = &p
	}
	if bak.Records != nil {
		out.Records = make([]*records.AgentRecord, len(bak.Records))
		for i, rec := range bak.Records {
			if rec == nil {
				continue
			}
			clone := *rec
			if rec.DueAt != nil {
				due := *rec.DueAt
				clone.DueAt = &due
			}
			out.Records[i] = &clone
		}
	}
	out.Assets = make([]HexbakAsset, len(bak.Assets))
	for i, item := range bak.Assets {
		out.Assets[i] = item
		out.Assets[i].Data = append([]byte(nil), item.Data...)
	}
	out.CreativeWorkOCR = append([]k12.CreativeWorkOCRArchiveEvidence(nil), bak.CreativeWorkOCR...)
	out.ProblemAttempts = cloneProblemAttemptSnapshots(bak.ProblemAttempts)
	if bak.ProblemSource != nil {
		cloned := k12storageCloneProblemSourceArchive(*bak.ProblemSource)
		out.ProblemSource = &cloned
	}
	return &out
}

func k12storageCloneProblemSourceArchive(
	source k12storage.ProblemSourceArchiveV6,
) k12storage.ProblemSourceArchiveV6 {
	raw, _ := json.Marshal(source)
	var out k12storage.ProblemSourceArchiveV6
	_ = json.Unmarshal(raw, &out)
	return out
}

func validateArchivedProfile(bak *Hexbak) error {
	if bak.Version >= 2 && bak.Profile != nil && bak.Profile.GradeTerm != "" && !k12.ValidProfileGradeTerm(bak.Profile.GradeTerm) {
		return fmt.Errorf("%w: 归档档案年级 %q 不在当前开放学段（仅小学一至六年级）", ErrInvalidInput, bak.Profile.GradeTerm)
	}
	return nil
}
