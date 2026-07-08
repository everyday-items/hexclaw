package engineadapter

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// agentReadWriter 内存 Agent 路由（*router.Dispatcher 满足）。
type agentReadWriter interface {
	GetAgent(name string) (*router.AgentConfig, bool)
	UpdateAgent(cfg router.AgentConfig) error
}

// agentPersister Agent 持久化（router.Store 满足）。可为 nil（仅内存）。
type agentPersister interface {
	SaveAgent(ctx context.Context, agent *router.AgentConfig) error
}

// ProfileAdapter 把孩子档案读写映射到 agents.metadata（经 router）。
type ProfileAdapter struct {
	rw    agentReadWriter
	store agentPersister
}

// NewProfileAdapter 创建 adapter。rw = agentRouter（Dispatcher），store = agentStore（可 nil）。
func NewProfileAdapter(rw agentReadWriter, store agentPersister) *ProfileAdapter {
	return &ProfileAdapter{rw: rw, store: store}
}

var _ usecase.ProfileStore = (*ProfileAdapter)(nil)

// GetProfile 从实例 metadata 读孩子档案。
func (a *ProfileAdapter) GetProfile(_ context.Context, agentName string) (k12.ChildProfile, error) {
	cfg, ok := a.rw.GetAgent(agentName)
	if !ok {
		return k12.ChildProfile{}, fmt.Errorf("profile: 实例 %q 不存在", agentName)
	}
	return k12.ProfileFromMeta(cfg.Metadata), nil
}

// SaveProfile 只改 K12 档案键（保留其他 metadata），read-modify-write。
//
// 关键：ApplyProfileToMeta 克隆 map，绝不原地改 router 内部 map（GetAgent 返回内部指针，
// 直接改会绕过锁污染共享 map）。改内存 → 再从内存重读 → 落库（对齐 handleUpdateAgent 两阶段写）。
func (a *ProfileAdapter) SaveProfile(ctx context.Context, agentName string, p k12.ChildProfile) error {
	cfg, ok := a.rw.GetAgent(agentName)
	if !ok {
		return fmt.Errorf("profile: 实例 %q 不存在", agentName)
	}
	updated := *cfg                                            // 浅拷贝 struct
	updated.Metadata = k12.ApplyProfileToMeta(cfg.Metadata, p) // 克隆 map + 只改 K12 键
	if err := a.rw.UpdateAgent(updated); err != nil {
		return fmt.Errorf("profile: 更新内存路由: %w", err)
	}
	if a.store != nil {
		cur, _ := a.rw.GetAgent(agentName)
		if err := a.store.SaveAgent(ctx, cur); err != nil {
			return fmt.Errorf("profile: 持久化档案: %w", err)
		}
	}
	return nil
}
