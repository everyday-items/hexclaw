package usecase

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// ProfileStore 孩子档案读写 port（adapter = router agent store，档案存 agents.metadata）。
type ProfileStore interface {
	GetProfile(ctx context.Context, agentName string) (k12.ChildProfile, error)
	SaveProfile(ctx context.Context, agentName string, p k12.ChildProfile) error
}

// ArchiveRestorer atomically merges records and exact-replaces the K12 profile
// from one signed archive. Record IDs are idempotent/import-wins while records
// absent from the archive survive. Production implementations must make the two
// persistent writes one durability boundary and publish the in-memory profile
// only after that boundary commits.
type ArchiveRestorer interface {
	RestoreArchive(ctx context.Context, agentName string, recs []*records.AgentRecord, p *k12.ChildProfile) error
}

// GetProfile 读孩子档案。
func (d Deps) GetProfile(ctx context.Context, agentName string) (k12.ChildProfile, error) {
	if d.Profiles == nil {
		return k12.ChildProfile{}, fmt.Errorf("usecase: 未配置档案存储")
	}
	return d.Profiles.GetProfile(ctx, agentName)
}

// UpdateProfile 建档/改档（升学改年级）。
//
// 年级白名单收窄为小学 12 档（架构设计-v0.5.0《明确不做》#2：不做初中和高中辅导，
// 发布阻断）——初中/高中年级不可写入档案；超纲判定仍用 18 档全序，不受影响。
// 只改传入的非空字段（保留其他）。改年级 → 下次辅导边界随之重算。
func (d Deps) UpdateProfile(ctx context.Context, agentName string, p k12.ChildProfile) (k12.ChildProfile, error) {
	if d.Profiles == nil {
		return k12.ChildProfile{}, fmt.Errorf("usecase: 未配置档案存储")
	}
	if agentName == "" {
		return k12.ChildProfile{}, fmt.Errorf("%w: agentName 不可空", ErrInvalidInput)
	}
	if p.GradeTerm != "" && !k12.ValidProfileGradeTerm(p.GradeTerm) {
		return k12.ChildProfile{}, fmt.Errorf("%w: 非法年级学期 %q（须为小学 12 档：一年级上～六年级下）", ErrInvalidInput, p.GradeTerm)
	}
	if err := d.Profiles.SaveProfile(ctx, agentName, p); err != nil {
		return k12.ChildProfile{}, fmt.Errorf("usecase: 保存档案: %w", err)
	}
	return d.Profiles.GetProfile(ctx, agentName)
}
